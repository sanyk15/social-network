package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// JobType identifies a queue job. Feed fan-out is the only writer of the
// stream; refresh jobs are produced by the CLI/admin rebuild path.
type JobType string

const (
	JobPostFanout  JobType = "post.fanout"
	JobPostRemove  JobType = "post.remove"
	JobFeedRefresh JobType = "feed.refresh"
)

// Job is the value stored in the stream. A single JSON field keeps the stream
// schema readable in redis-cli (XRANGE) without a field-per-type design.
type Job struct {
	Type         JobType `json:"type"`
	PostID       int64   `json:"post_id,omitempty"`
	AuthorUserID int64   `json:"author_user_id,omitempty"`
	UserID       int64   `json:"user_id,omitempty"`
}

const (
	jobMaxLen      = 100_000
	jobReadCount   = 64
	jobBlock       = 2 * time.Second
	jobMaxAttempts = 3
	jobReclaimIdle = 60 * time.Second
)

// Queue is a Redis Streams queue with one consumer group. Messages are
// acknowledged even after a failed handler: a stale feed is repaired by the
// database rebuild, so a poison message must not block the stream forever.
type Queue struct {
	rdb    *redis.Client
	stream string
	group  string
	log    *log.Logger
}

func NewQueue(rdb *redis.Client, prefix string) *Queue {
	if prefix == "" {
		prefix = "sn"
	}
	return &Queue{
		rdb:    rdb,
		stream: prefix + ":feed:jobs",
		group:  "feed-workers",
		log:    log.Default(),
	}
}

func (q *Queue) Stream() string { return q.stream }
func (q *Queue) Group() string  { return q.group }

// EnsureGroup creates the consumer group from the current end of the stream.
// Old jobs are not replayed: the feed cache is rebuilt from the database when
// it is missing, so a lost job is not a correctness problem.
func (q *Queue) EnsureGroup(ctx context.Context) error {
	err := q.rdb.XGroupCreateMkStream(ctx, q.stream, q.group, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

func (q *Queue) Enqueue(ctx context.Context, job Job) error {
	raw, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	return q.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: q.stream,
		MaxLen: jobMaxLen,
		Approx: true,
		Values: map[string]any{"job": string(raw)},
	}).Err()
}

// Run consumes jobs until ctx is cancelled. Each worker has its own consumer
// name so Redis can track pending entries per process.
func (q *Queue) Run(ctx context.Context, consumer string, handle func(context.Context, Job) error) {
	lastReclaim := time.Now()
	for {
		if ctx.Err() != nil {
			return
		}
		if time.Since(lastReclaim) >= 30*time.Second {
			q.reclaim(ctx, consumer, handle)
			lastReclaim = time.Now()
		}

		res, err := q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    q.group,
			Consumer: consumer,
			Streams:  []string{q.stream, ">"},
			Count:    jobReadCount,
			Block:    jobBlock,
		}).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			q.log.Printf("feed queue: read: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		for _, stream := range res {
			for _, msg := range stream.Messages {
				q.process(ctx, msg, handle)
			}
		}
	}
}

// reclaim retries messages that were delivered to a worker that died before
// acknowledging them.
func (q *Queue) reclaim(ctx context.Context, consumer string, handle func(context.Context, Job) error) {
	pending, err := q.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: q.stream,
		Group:  q.group,
		Start:  "-",
		End:    "+",
		Count:  100,
		Idle:   jobReclaimIdle,
	}).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) && ctx.Err() == nil {
			q.log.Printf("feed queue: reclaim inspect: %v", err)
		}
		return
	}
	if len(pending) == 0 {
		return
	}

	ids := make([]string, len(pending))
	for i, p := range pending {
		ids[i] = p.ID
	}
	msgs, err := q.rdb.XClaim(ctx, &redis.XClaimArgs{
		Stream:   q.stream,
		Group:    q.group,
		Consumer: consumer,
		MinIdle:  jobReclaimIdle,
		Messages: ids,
	}).Result()
	if err != nil {
		if ctx.Err() == nil {
			q.log.Printf("feed queue: reclaim: %v", err)
		}
		return
	}
	for _, msg := range msgs {
		q.process(ctx, msg, handle)
	}
}

func (q *Queue) process(ctx context.Context, msg redis.XMessage, handle func(context.Context, Job) error) {
	job, err := decodeJob(msg)
	if err == nil {
		for attempt := 1; attempt <= jobMaxAttempts; attempt++ {
			if err = handle(ctx, job); err == nil {
				break
			}
			q.log.Printf("feed queue: job %s (id %s) attempt %d: %v", job.Type, msg.ID, attempt, err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
			}
		}
	}
	if err != nil {
		q.log.Printf("feed queue: job failed permanently (id %s): %v", msg.ID, err)
	}

	ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.rdb.XAck(ackCtx, q.stream, q.group, msg.ID).Err(); err != nil {
		q.log.Printf("feed queue: ack %s: %v", msg.ID, err)
	}
}

func decodeJob(msg redis.XMessage) (Job, error) {
	raw, ok := msg.Values["job"].(string)
	if !ok {
		return Job{}, fmt.Errorf("message %s has no job field", msg.ID)
	}
	var job Job
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		return Job{}, fmt.Errorf("message %s: %w", msg.ID, err)
	}
	if job.Type == "" {
		return Job{}, fmt.Errorf("message %s: empty job type", msg.ID)
	}
	return job, nil
}

type QueueStats struct {
	Stream  string `json:"stream"`
	Group   string `json:"group"`
	Length  int64  `json:"length"`
	Pending int64  `json:"pending"`
}

// Stats reports the stream length and the number of pending (unacknowledged)
// messages — enough to show queue mode working during a demo.
func (q *Queue) Stats(ctx context.Context) (QueueStats, error) {
	out := QueueStats{Stream: q.stream, Group: q.group}
	length, err := q.rdb.XLen(ctx, q.stream).Result()
	if err != nil {
		return out, err
	}
	out.Length = length

	pending, err := q.rdb.XPending(ctx, q.stream, q.group).Result()
	if err != nil {
		return out, err
	}
	out.Pending = pending.Count
	return out, nil
}
