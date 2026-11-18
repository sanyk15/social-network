// Command feedrebuild rebuilds feed caches from PostgreSQL.
//
// One user:
//
//	go run ./cmd/feedrebuild -user-id 42
//
// Every user (in id order, in batches):
//
//	go run ./cmd/feedrebuild -all -batch 500
//
// The command reads the same configuration as the server (DATABASE_URL,
// REDIS_URL, FEED_SIZE, FEED_CACHE_TTL) and is the offline counterpart of
// POST /admin/feed/rebuild.
package main

import (
	"context"
	"flag"
	"log"
	"time"

	"social-network/internal/cache"
	"social-network/internal/config"
	"social-network/internal/feed"
	"social-network/internal/store"
)

func main() {
	userID := flag.Int64("user-id", 0, "rebuild the feed of one user")
	all := flag.Bool("all", false, "rebuild the feeds of every user")
	batch := flag.Int("batch", 500, "users per batch with -all")
	dsn := flag.String("dsn", "", "Postgres DSN (default: DATABASE_URL)")
	redisURL := flag.String("redis", "", "Redis URL (default: REDIS_URL)")
	size := flag.Int("size", 0, "feed size (default: FEED_SIZE or 1000)")
	ttl := flag.Duration("ttl", 0, "feed cache TTL (default: FEED_CACHE_TTL or 10m)")
	flag.Parse()

	if *userID <= 0 && !*all {
		log.Fatal("either -user-id or -all is required")
	}

	cfg := config.Load()
	if *dsn == "" {
		*dsn = cfg.DatabaseURL
	}
	if *redisURL == "" {
		*redisURL = cfg.RedisURL
	}
	if *size == 0 {
		*size = cfg.FeedSize
	}
	if *ttl == 0 {
		*ttl = cfg.FeedCacheTTL
	}

	ctx := context.Background()
	st, err := store.New(ctx, store.Config{MasterURL: *dsn, MaxConns: cfg.DBMaxConns})
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer st.Close()

	rdb, err := cache.NewRedis(ctx, *redisURL)
	if err != nil {
		log.Fatalf("connect to redis: %v", err)
	}
	defer rdb.Close()

	svc := feed.NewService(st, rdb, nil, feed.Config{Size: *size, FeedTTL: *ttl})

	if *userID > 0 {
		start := time.Now()
		if err := svc.Rebuild(ctx, *userID); err != nil {
			log.Fatalf("rebuild user %d: %v", *userID, err)
		}
		status, _ := svc.Status(ctx, *userID)
		log.Printf("rebuilt feed of user %d: %d posts, took %s", *userID, status.Size, time.Since(start).Round(time.Millisecond))
		return
	}

	start := time.Now()
	done, err := svc.RebuildAll(ctx, *batch, func(n int) {
		log.Printf("rebuilt %d feeds", n)
	})
	if err != nil {
		log.Fatalf("rebuild all after %d feeds: %v", done, err)
	}
	log.Printf("done: %d feeds in %s", done, time.Since(start).Round(time.Millisecond))
}
