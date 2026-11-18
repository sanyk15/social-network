package feed

import (
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestDecodeJob(t *testing.T) {
	msg := redis.XMessage{
		ID:     "1-0",
		Values: map[string]any{"job": `{"type":"post.fanout","post_id":7,"author_user_id":3}`},
	}
	job, err := decodeJob(msg)
	if err != nil {
		t.Fatalf("decodeJob: %v", err)
	}
	if job.Type != JobPostFanout || job.PostID != 7 || job.AuthorUserID != 3 {
		t.Fatalf("unexpected job: %+v", job)
	}

	if _, err := decodeJob(redis.XMessage{ID: "2-0", Values: map[string]any{}}); err == nil {
		t.Fatal("expected an error for a message without the job field")
	}
	if _, err := decodeJob(redis.XMessage{ID: "3-0", Values: map[string]any{"job": `{"type":`}}); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
	if _, err := decodeJob(redis.XMessage{ID: "4-0", Values: map[string]any{"job": `{}`}}); err == nil {
		t.Fatal("expected an error for an empty job type")
	}
}

func TestConfigDefaults(t *testing.T) {
	var cfg Config
	cfg.withDefaults()

	if cfg.Size != 1000 {
		t.Errorf("Size = %d, want 1000", cfg.Size)
	}
	if cfg.FeedTTL <= 0 || cfg.FriendsTTL <= 0 || cfg.PostTTL <= 0 {
		t.Errorf("TTLs must be positive: %+v", cfg)
	}
	if cfg.Mode != ModeSync {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeSync)
	}
	if cfg.Prefix != "sn" {
		t.Errorf("Prefix = %q, want sn", cfg.Prefix)
	}
}
