// Command seedfeed fills friendships and posts for a subset of already seeded
// users. It is the ДЗ-4 counterpart of cmd/seed (which fills users).
//
//	docker compose exec pg-master psql -U social -d social ...   # users already seeded
//	go run ./cmd/seedfeed -users 1000 -friends 20 -posts 100000
//
// Post texts come from the course file (https://github.com/OtusTeam/highload/
// blob/master/homework/posts.txt), one post per line. If the file is missing,
// the seeder generates lorem ipsum so the command still works offline.
//
// Posts are inserted in created_at order, so post ids (BIGSERIAL) grow with the
// timeline — that is what the feed cache relies on.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"social-network/internal/cache"
	"social-network/internal/config"
	"social-network/internal/store"
)

var lorem = []string{
	"Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua.",
	"Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat.",
	"Duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur.",
	"Excepteur sint occaecat cupidatat non proident, sunt in culpa qui officia deserunt mollit anim id est laborum.",
	"Sed ut perspiciatis unde omnis iste natus error sit voluptatem accusantium doloremque laudantium.",
	"Nemo enim ipsam voluptatem quia voluptas sit aspernatur aut odit aut fugit.",
	"Neque porro quisquam est, qui dolorem ipsum quia dolor sit amet, consectetur, adipisci velit.",
	"At vero eos et accusamus et iusto odio dignissimos ducimus qui blanditiis praesentium voluptatum.",
}

const maxTextRunes = 4096

func main() {
	users := flag.Int("users", 1000, "seed friends and posts for user ids in [1, users]")
	friendsPerUser := flag.Int("friends", 20, "approximate number of friends per user")
	posts := flag.Int("posts", 100_000, "number of posts to insert")
	postsFile := flag.String("posts-file", "posts.txt", "file with one post text per line (fallback: ../posts.txt, then lorem ipsum)")
	batch := flag.Int("batch", 10_000, "rows per COPY batch")
	seed := flag.Int64("seed", 42, "random seed")
	truncate := flag.Bool("truncate", false, "truncate posts and friends before seeding")
	redisURL := flag.String("redis", os.Getenv("REDIS_URL"), "Redis URL; when set, feed caches are dropped after seeding so they rebuild from the new data")
	dsn := flag.String("dsn", "", "Postgres DSN (default: DATABASE_URL)")
	flag.Parse()

	if *users <= 0 || *friendsPerUser < 0 || *posts < 0 {
		log.Fatal("users must be positive; friends and posts must be non-negative")
	}
	if *batch <= 0 {
		log.Fatal("batch must be positive")
	}

	ctx := context.Background()
	cfg := config.Load()
	if *dsn == "" {
		*dsn = cfg.DatabaseURL
	}

	st, err := store.New(ctx, store.Config{MasterURL: *dsn, MaxConns: cfg.DBMaxConns})
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	maxID, err := st.MaxUserID(ctx)
	if err != nil {
		log.Fatalf("read max user id: %v", err)
	}
	if maxID == 0 {
		log.Fatal("users table is empty: run cmd/seed first")
	}
	if int64(*users) > maxID {
		log.Printf("only %d users exist, clamping -users to %d", maxID, maxID)
		*users = int(maxID)
	}

	if *truncate {
		if err := st.TruncateFeedData(ctx); err != nil {
			log.Fatalf("truncate: %v", err)
		}
		log.Print("truncated posts and friends")
	}

	rnd := rand.New(rand.NewSource(*seed))

	start := time.Now()
	friendCount, err := seedFriends(ctx, st, rnd, *users, *friendsPerUser, *batch)
	if err != nil {
		log.Fatalf("seed friends: %v", err)
	}
	log.Printf("inserted %d friendship pairs in %s", friendCount, time.Since(start).Round(time.Millisecond))

	texts := loadTexts(*postsFile)
	if texts == nil {
		log.Print("posts file not found, generating lorem ipsum texts")
	}

	start = time.Now()
	inserted, err := seedPosts(ctx, st, rnd, *users, *posts, texts, *batch)
	if err != nil {
		log.Fatalf("seed posts: %v", err)
	}
	log.Printf("inserted %d posts in %s", inserted, time.Since(start).Round(time.Millisecond))

	log.Print("analyzing posts and friends")
	if err := st.AnalyzeFeedData(ctx); err != nil {
		log.Fatalf("analyze: %v", err)
	}

	// Direct database inserts bypass fan-out, so every cached feed is stale
	// now. Dropping the cache makes the next read rebuild from the new data.
	if *redisURL == "" {
		log.Print("REDIS_URL is empty: feed caches were not dropped (they rebuild after the TTL)")
	} else {
		rdb, err := cache.NewRedis(ctx, *redisURL)
		if err != nil {
			log.Fatalf("connect to redis: %v", err)
		}
		defer rdb.Close()
		dropped, err := clearFeedCache(ctx, rdb)
		if err != nil {
			log.Fatalf("drop feed caches: %v", err)
		}
		log.Printf("dropped %d feed cache keys", dropped)
	}

	log.Printf("done: %d users, %d friendships, %d posts", *users, friendCount, inserted)
}

// clearFeedCache drops feed, friend and post keys. The job stream is kept: it
// has consumer-group state, and its messages reference the old posts anyway
// (the feed rebuild will ignore them because the posts no longer exist).
func clearFeedCache(ctx context.Context, rdb *redis.Client) (int, error) {
	deleted := 0
	patterns := []string{"sn:feed:*", "sn:friends:*", "sn:post:*"}

	for _, pattern := range patterns {
		iter := rdb.Scan(ctx, 0, pattern, 1000).Iterator()
		keys := make([]string, 0, 500)
		for iter.Next(ctx) {
			key := iter.Val()
			if key == "sn:feed:jobs" {
				continue
			}
			keys = append(keys, key)
			if len(keys) == cap(keys) {
				deleted += int(rdb.Unlink(ctx, keys...).Val())
				keys = keys[:0]
			}
		}
		if err := iter.Err(); err != nil {
			return deleted, err
		}
		if len(keys) > 0 {
			deleted += int(rdb.Unlink(ctx, keys...).Val())
		}
	}
	return deleted, nil
}

func seedFriends(ctx context.Context, st *store.Store, rnd *rand.Rand, users, perUser, batch int) (int, error) {
	if perUser == 0 || users < 2 {
		return 0, nil
	}
	if perUser > users-1 {
		perUser = users - 1
	}

	type pair struct{ a, b int64 }
	seen := make(map[pair]struct{}, users*perUser)
	pairs := make([]store.FriendPair, 0, batch)
	total := 0

	flush := func() error {
		if err := st.InsertFriendsBatch(ctx, pairs); err != nil {
			return err
		}
		total += len(pairs)
		pairs = pairs[:0]
		return nil
	}

	for user := 1; user <= users; user++ {
		added := 0
		// A bounded number of attempts avoids an infinite loop when the
		// user already has almost everyone as a friend.
		for attempt := 0; added < perUser && attempt < perUser*10; attempt++ {
			other := 1 + rnd.Intn(users)
			if other == user {
				continue
			}
			a, b := int64(user), int64(other)
			if a > b {
				a, b = b, a
			}
			p := pair{a: a, b: b}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			pairs = append(pairs, store.FriendPair{UserID: a, FriendID: b})
			added++
			if len(pairs) >= batch {
				if err := flush(); err != nil {
					return total, err
				}
			}
		}
	}
	if len(pairs) > 0 {
		if err := flush(); err != nil {
			return total, err
		}
	}
	return total, nil
}

func seedPosts(ctx context.Context, st *store.Store, rnd *rand.Rand, users, count int, texts []string, batch int) (int, error) {
	if count == 0 {
		return 0, nil
	}

	const step = 100 * time.Millisecond
	from := time.Now().Add(-time.Duration(count) * step)

	rows := make([]store.PostSeed, 0, batch)
	inserted := 0
	for i := 0; i < count; i++ {
		text := ""
		if len(texts) > 0 {
			text = texts[i%len(texts)]
		}
		if text == "" {
			text = generatedText(i)
		}
		rows = append(rows, store.PostSeed{
			AuthorID:  int64(1 + rnd.Intn(users)),
			Text:      text,
			CreatedAt: from.Add(time.Duration(i) * step),
		})
		if len(rows) >= batch {
			if err := st.InsertPostsBatch(ctx, rows); err != nil {
				return inserted, err
			}
			inserted += len(rows)
			rows = rows[:0]
			log.Printf("inserted %d/%d posts", inserted, count)
		}
	}
	if len(rows) > 0 {
		if err := st.InsertPostsBatch(ctx, rows); err != nil {
			return inserted, err
		}
		inserted += len(rows)
	}
	return inserted, nil
}

func generatedText(i int) string {
	return fmt.Sprintf("%s %s", lorem[i%len(lorem)], lorem[(i*7+3)%len(lorem)])
}

// loadTexts reads one post per line. It returns nil when no file is found, so
// the caller can fall back to generated text.
func loadTexts(path string) []string {
	for _, candidate := range []string{path, "../" + path, "../posts.txt", "posts.txt"} {
		texts, err := readLines(candidate)
		if err == nil {
			log.Printf("loaded %d post texts from %s", len(texts), candidate)
			return texts
		}
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("read %s: %v", candidate, err)
		}
	}
	return nil
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)

	texts := make([]string, 0, 10_000)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > maxTextRunes {
			line = string([]rune(line)[:maxTextRunes])
		}
		texts = append(texts, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return nil, errors.New("no non-empty lines")
	}
	return texts, nil
}
