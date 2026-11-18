package cache

import (
	"testing"
)

// TestOptionsDialRetries guards the fallback latency of the feed read path.
//
// The cache is not a hard dependency: when Redis is down, GET /post/feed must
// reach PostgreSQL quickly instead of stalling. go-redis >= v9.20 retries a
// failed dial 5 times by default (DialerRetries), which turned a ~40 ms
// fallback into ~1.9 s — measured with k6 at 1 VU against a stopped Redis.
// NewRedis must keep a single dial attempt.
func TestOptionsDialRetries(t *testing.T) {
	opt, err := optionsFromURL("redis://127.0.0.1:6379/0")
	if err != nil {
		t.Fatalf("optionsFromURL: %v", err)
	}
	if opt.DialerRetries > 1 {
		t.Errorf("DialerRetries = %d, want <= 1: extra dial attempts stall the PostgreSQL fallback", opt.DialerRetries)
	}
	if opt.DialTimeout <= 0 {
		t.Errorf("DialTimeout = %v, want positive", opt.DialTimeout)
	}
}

func TestOptionsFromURLRejectsGarbage(t *testing.T) {
	if _, err := optionsFromURL("not-a-redis-url"); err == nil {
		t.Fatal("expected an error for an invalid URL")
	}
}
