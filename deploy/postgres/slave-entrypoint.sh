#!/bin/bash
# Entrypoint for a physical streaming replica.
#
# On the first start it waits for the master, takes a base backup into PGDATA,
# writes primary_conninfo (including application_name used by
# synchronous_standby_names) and starts PostgreSQL in hot-standby mode.
# On later starts it reuses the existing data directory.
set -euo pipefail

: "${PGDATA:=/var/lib/postgresql/data}"
: "${MASTER_HOST:=pg-master}"
: "${REPLICATION_USER:=replicator}"
: "${REPLICATION_PASSWORD:=replpass}"
: "${REPLICA_NAME:=replica}"

# The Debian postgres image ships gosu, the Alpine one ships su-exec.
if command -v gosu >/dev/null 2>&1; then
    RUN_AS=(gosu postgres)
elif command -v su-exec >/dev/null 2>&1; then
    RUN_AS=(su-exec postgres)
else
    echo "neither gosu nor su-exec is available" >&2
    exit 1
fi

mkdir -p "$PGDATA" /var/run/postgresql
chown postgres:postgres /var/run/postgresql

# A successful base backup is marked explicitly: a half-written PGDATA can
# already contain PG_VERSION, and starting such a cluster would fail.
if [ ! -s "$PGDATA/PG_VERSION" ] || [ ! -f "$PGDATA/.social-initialized" ]; then
    chown -R postgres:postgres "$PGDATA"
    chmod 700 "$PGDATA"

    echo "waiting for master ${MASTER_HOST} ..."
    for _ in $(seq 1 60); do
        if PGPASSWORD="$REPLICATION_PASSWORD" pg_isready \
            -h "$MASTER_HOST" -p 5432 -U "$REPLICATION_USER" -d postgres >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done

    attempt=0
    while :; do
        attempt=$((attempt + 1))
        echo "pg_basebackup from ${MASTER_HOST} (attempt ${attempt})"

        find "$PGDATA" -mindepth 1 -maxdepth 1 -exec rm -rf {} +

        if PGPASSWORD="$REPLICATION_PASSWORD" "${RUN_AS[@]}" pg_basebackup \
            -h "$MASTER_HOST" -p 5432 -U "$REPLICATION_USER" \
            -D "$PGDATA" -Fp -Xs -c fast -P; then
            break
        fi

        if [ "$attempt" -ge 5 ]; then
            echo "pg_basebackup failed after ${attempt} attempts" >&2
            exit 1
        fi
        sleep 3
    done

    cat >> "$PGDATA/postgresql.auto.conf" <<EOF
primary_conninfo = 'host=$MASTER_HOST port=5432 user=$REPLICATION_USER password=$REPLICATION_PASSWORD application_name=$REPLICA_NAME'
EOF
    touch "$PGDATA/standby.signal" "$PGDATA/.social-initialized"
    chown -R postgres:postgres "$PGDATA"
    chmod 700 "$PGDATA"
fi

exec "${RUN_AS[@]}" postgres \
    -D "$PGDATA" \
    -c hot_standby=on \
    -c hot_standby_feedback=on \
    -c wal_keep_size=1GB \
    -c listen_addresses='*'
