#!/usr/bin/env bash
# End-to-end проверка ленты друзей против запущенного стека.
#
#   docker compose up -d --build
#   load/feed-smoke.sh
#
# Требуются curl и jq. redis-cli нужен только для проверки перестройки кэша
# «изнутри» (можно указать REDIS_CLI="docker compose exec -T redis redis-cli").
#
# Переменные:
#   BASE_URL       по умолчанию http://localhost:8080
#   REDIS_CLI      по умолчанию redis-cli
#   CHECK_CAP      0 — пропустить проверку 1000 постов (по умолчанию 1)
set -uo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
REDIS_CLI="${REDIS_CLI:-redis-cli}"
CHECK_CAP="${CHECK_CAP:-1}"

BODY_FILE=$(mktemp)
trap 'rm -f "$BODY_FILE"' EXIT

LAST_CODE=""
LAST_BODY=""
FAILS=0

pass() { printf '  ok   %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*"; FAILS=$((FAILS + 1)); }
section() { printf '\n== %s ==\n' "$*"; }

check() { # got want message
	if [ "$1" = "$2" ]; then pass "$3"; else fail "$3 (got '$1', want '$2')"; fi
}

http() { # method path token [body]
	local method=$1 path=$2 token=${3:-} body=${4:-}
	local args=(-sS -o "$BODY_FILE" -w '%{http_code}' -X "$method" "$BASE_URL$path")
	if [ -n "$token" ]; then args+=(-H "Authorization: Bearer $token"); fi
	if [ -n "$body" ]; then args+=(-H "Content-Type: application/json" -d "$body"); fi
	LAST_CODE=$(curl "${args[@]}")
	LAST_BODY=$(cat "$BODY_FILE")
}

register_user() { # username -> id
	local username=$1
	http POST /user/register "" "{
		\"username\": \"$username\",
		\"password\": \"secret123\",
		\"first_name\": \"Test\",
		\"last_name\": \"User\",
		\"birth_date\": \"1990-01-01\",
		\"gender\": \"other\",
		\"interests\": [\"testing\"],
		\"city\": \"Moscow\"
	}"
	if [ "$LAST_CODE" != "201" ]; then
		echo "register $username failed: $LAST_CODE $LAST_BODY" >&2
		exit 1
	fi
	jq -r .id <<<"$LAST_BODY"
}

login_user() { # username -> token
	http POST /login "" "{\"username\": \"$1\", \"password\": \"secret123\"}"
	if [ "$LAST_CODE" != "200" ]; then
		echo "login $1 failed: $LAST_CODE $LAST_BODY" >&2
		exit 1
	fi
	jq -r .token <<<"$LAST_BODY"
}

create_post() { # token text -> id
	http POST /post/create "$1" "$(jq -nc --arg t "$2" '{text: $t}')"
	if [ "$LAST_CODE" != "200" ]; then
		echo "create post failed: $LAST_CODE $LAST_BODY" >&2
		exit 1
	fi
	jq -r .id <<<"$LAST_BODY"
}

feed_body() { # token [limit]
	http GET "/post/feed?limit=${2:-1000}" "$1"
}

feed_count() { # token post_id -> 0|1
	feed_body "$1"
	jq --argjson id "$2" '[.[] | select(.id == $id)] | length' <<<"$LAST_BODY"
}

feed_text() { # token post_id
	feed_body "$1"
	jq -r --argjson id "$2" '[.[] | select(.id == $id)][0].text // ""' <<<"$LAST_BODY"
}

expect_count() { # token post_id want message [tries]
	local tries=${5:-40}
	for _ in $(seq 1 "$tries"); do
		if [ "$(feed_count "$1" "$2")" = "$3" ]; then pass "$4"; return; fi
		sleep 0.05
	done
	fail "$4 (still $(feed_count "$1" "$2"), want $3)"
}

expect_text() { # token post_id want message [tries]
	local tries=${5:-40}
	for _ in $(seq 1 "$tries"); do
		if [ "$(feed_text "$1" "$2")" = "$3" ]; then pass "$4"; return; fi
		sleep 0.05
	done
	fail "$4 (still '$(feed_text "$1" "$2")', want '$3')"
}

expect_code() { # method path token body want message [tries]
	local tries=${7:-40}
	for _ in $(seq 1 "$tries"); do
		http "$1" "$2" "$3" "$4"
		if [ "$LAST_CODE" = "$5" ]; then pass "$6"; return; fi
		sleep 0.05
	done
	fail "$6 (still $LAST_CODE, want $5)"
}

suffix="$(date +%s)$RANDOM"
alice="smoke_alice_$suffix"
bob="smoke_bob_$suffix"

section "users"
alice_id=$(register_user "$alice")
alice_token=$(login_user "$alice")
bob_id=$(register_user "$bob")
bob_token=$(login_user "$bob")
pass "alice=$alice_id bob=$bob_id"

section "post is not visible before friendship"
post_id=$(create_post "$alice_token" "hello from alice $suffix")
expect_count "$bob_token" "$post_id" 0 "bob does not see alice's post yet"

section "friend add invalidates the feed"
http POST /friend/add "$bob_token" "{\"user_id\": $alice_id}"
check "$LAST_CODE" 200 "POST /friend/add"
expect_count "$bob_token" "$post_id" 1 "bob sees alice's post after friend add"

section "post update invalidates the cached payload"
new_text="edited by alice $suffix"
http POST /post/update "$alice_token" "{\"id\": $post_id, \"text\": \"$new_text\"}"
check "$LAST_CODE" 200 "POST /post/update"
expect_text "$bob_token" "$post_id" "$new_text" "bob sees the new text"

section "post delete removes the post from the feed"
http POST /post/delete "$alice_token" "{\"id\": $post_id}"
check "$LAST_CODE" 200 "POST /post/delete"
expect_count "$bob_token" "$post_id" 0 "deleted post is not in the feed"
expect_code GET "/post/get/$post_id" "$bob_token" "" 404 "deleted post is not readable"

section "replica-style path forms from the spec"
post2=$(create_post "$alice_token" "second post $suffix")
http PUT "/post/delete/0" "$alice_token" ""
check "$LAST_CODE" 400 "PUT /post/delete/{id} validates the id"
http GET "/post/get/$post2" "$bob_token"
check "$LAST_CODE" 200 "GET /post/get/{id}"
http PUT "/friend/set/$alice_id" "$bob_token" ""
check "$LAST_CODE" 200 "PUT /friend/set/{user_id}"

section "automatic rebuild from PostgreSQL"
if $REDIS_CLI DEL "sn:feed:$bob_id" "sn:feed:ready:$bob_id" >/dev/null 2>&1; then
	expect_count "$bob_token" "$post2" 1 "cache dropped -> feed rebuilt from the database"
	http GET "/admin/feed/status?user_id=$bob_id" "$bob_token"
	check "$(jq -r .ready <<<"$LAST_BODY")" true "ready flag is back"
else
	printf '  skip: redis-cli cannot reach Redis (set REDIS_CLI)\n'
fi

section "explicit rebuild from the database"
http POST /admin/feed/rebuild "$bob_token" "{\"user_id\": $bob_id}"
check "$LAST_CODE" 200 "POST /admin/feed/rebuild"
http GET "/admin/feed/status?user_id=$bob_id" "$bob_token"
check "$(jq -r .ready <<<"$LAST_BODY")" true "feed is materialised after rebuild"
check "$(jq -r .size <<<"$LAST_BODY")" 1 "feed holds the one live post"

section "friend delete invalidates the feed"
http POST /friend/delete "$bob_token" "{\"user_id\": $alice_id}"
check "$LAST_CODE" 200 "POST /friend/delete"
expect_count "$bob_token" "$post2" 0 "alice's posts are gone after friend delete"

if [ "$CHECK_CAP" = "1" ]; then
	section "feed keeps the latest 1000 posts"
	first=""
	last=""
	for i in $(seq 1 1005); do
		pid=$(create_post "$alice_token" "cap $suffix $i")
		[ -z "$first" ] && first=$pid
		last=$pid
	done
	pass "created 1005 posts"

	http POST /friend/add "$bob_token" "{\"user_id\": $alice_id}"
	check "$LAST_CODE" 200 "re-add friend"
	feed_body "$bob_token" 1000
	check "$(jq 'length' <<<"$LAST_BODY")" 1000 "the feed returns exactly 1000 posts"
	check "$(jq --argjson id "$last" '[.[] | select(.id == $id)] | length' <<<"$LAST_BODY")" 1 "newest post is present"
	check "$(jq --argjson id "$first" '[.[] | select(.id == $id)] | length' <<<"$LAST_BODY")" 0 "oldest post was evicted"

	http GET "/admin/feed/status?user_id=$bob_id" "$bob_token"
	check "$(jq -r .size <<<"$LAST_BODY")" 1000 "cached feed holds exactly 1000 ids"
fi

section "stats"
http GET /admin/feed/stats "$bob_token"
echo "$LAST_BODY" | jq . 2>/dev/null || echo "$LAST_BODY"

printf '\n'
if [ "$FAILS" -gt 0 ]; then
	printf 'FAILED: %d check(s)\n' "$FAILS"
	exit 1
fi
printf 'OK: all checks passed\n'
