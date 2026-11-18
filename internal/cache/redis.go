// Package cache owns the Redis client used by the feed cache and queue.
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// optionsFromURL builds the client options. Kept separate from NewRedis so the
// retry policy can be asserted in tests without a live Redis.
func optionsFromURL(url string) (*redis.Options, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	opt.DialTimeout = 2 * time.Second
	opt.ReadTimeout = 2 * time.Second
	opt.WriteTimeout = 2 * time.Second
	opt.MaxRetries = 1
	// go-redis >= v9.20 retries a failed dial 5 times by default, which costs
	// seconds per request while Redis is down. The cache is not a hard
	// dependency: the read path falls back to PostgreSQL, so one dial attempt
	// is enough. Retrying here only delays the fallback.
	opt.DialerRetries = 1
	opt.DialerRetryTimeout = 0
	return opt, nil
}

// NewRedis parses a redis:// URL and waits for the server to answer. Docker
// Compose starts Redis with a healthcheck, but a native run may start the app
// first, so the constructor retries for a few seconds instead of failing.
func NewRedis(ctx context.Context, url string) (*redis.Client, error) {
	opt, err := optionsFromURL(url)
	if err != nil {
		return nil, err
	}

	client := redis.NewClient(opt)

	deadline := time.Now().Add(15 * time.Second)
	for {
		err = client.Ping(ctx).Err()
		if err == nil {
			return client, nil
		}
		if ctx.Err() != nil {
			client.Close()
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			client.Close()
			return nil, fmt.Errorf("redis at %s is not reachable: %w", opt.Addr, err)
		}
		select {
		case <-ctx.Done():
			client.Close()
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
