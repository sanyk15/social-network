// Package feed implements the friends feed: fan-out on write into Redis sorted
// sets, cache-aside reads and rebuild from PostgreSQL.
//
// Data model (all keys are per user):
//
//	sn:feed:{uid}        ZSET  member=post id, score=post id (BIGSERIAL ≈ time)
//	sn:feed:ready:{uid}  STR   "1" — the ZSET was rebuilt from the database
//	sn:feed:rebuild:{uid} STR  "1" — a rebuild is in progress
//	sn:feed:dirty:{uid}  STR   "1" — a write arrived during a rebuild
//	sn:friends:{uid}     SET   cached friend ids for fan-out
//	sn:post:{id}         STR   cached post JSON
//
// A feed holds at most Size (1000) ids. Fan-out on write keeps it warm; a cache
// miss rebuilds it from the database, so the cache is only ever an accelerator.
package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"social-network/internal/store"
)

// Mode selects how feed fan-out is executed.
type Mode string

const (
	// ModeSync updates friend feeds inside the write request. Deterministic:
	// the next feed read (even from another user) already sees the post.
	ModeSync Mode = "sync"
	// ModeQueue publishes fan-out jobs to a Redis Stream. The write request
	// returns immediately; workers update the feeds a moment later.
	ModeQueue Mode = "queue"
)

// Config tunes the feed cache. Zero values are replaced by defaults.
type Config struct {
	Size       int           // maximum ids in one feed (ДЗ: 1000)
	FeedTTL    time.Duration // feed ZSET + ready flag lifetime
	FriendsTTL time.Duration // friend list cache lifetime
	PostTTL    time.Duration // post payload cache lifetime
	IncludeOwn bool          // include the user's own posts in the feed
	Mode       Mode
	Prefix     string
}

func (c *Config) withDefaults() {
	if c.Size <= 0 {
		c.Size = 1000
	}
	if c.FeedTTL <= 0 {
		c.FeedTTL = 10 * time.Minute
	}
	if c.FriendsTTL <= 0 {
		c.FriendsTTL = 10 * time.Minute
	}
	if c.PostTTL <= 0 {
		c.PostTTL = time.Hour
	}
	if c.Mode != ModeQueue {
		c.Mode = ModeSync
	}
	if c.Prefix == "" {
		c.Prefix = "sn"
	}
}

type Stats struct {
	FeedHits        uint64 `json:"feed_hits"`
	FeedMisses      uint64 `json:"feed_misses"`
	FeedRebuilds    uint64 `json:"feed_rebuilds"`
	FeedFallbacks   uint64 `json:"feed_db_fallbacks"`
	PostCacheWrites uint64 `json:"post_cache_writes"`
	FanoutPosts     uint64 `json:"fanout_posts"`
	FeedRemovals    uint64 `json:"feed_removals"`
	JobsProcessed   uint64 `json:"jobs_processed"`
	JobsFailed      uint64 `json:"jobs_failed"`
}

type Service struct {
	store *store.Store
	rdb   *redis.Client
	queue *Queue
	cfg   Config
	log   *log.Logger

	// counters are updated from HTTP handlers and workers simultaneously.
	hits, misses, rebuilds, fallbacks, cacheWrites, fanouts, removals, jobsOK, jobsFailed atomic.Uint64
}

func NewService(st *store.Store, rdb *redis.Client, q *Queue, cfg Config) *Service {
	cfg.withDefaults()
	return &Service{store: st, rdb: rdb, queue: q, cfg: cfg, log: log.Default()}
}

func (s *Service) Mode() Mode             { return s.cfg.Mode }
func (s *Service) Size() int              { return s.cfg.Size }
func (s *Service) FeedTTL() time.Duration { return s.cfg.FeedTTL }
func (s *Service) IncludeOwn() bool       { return s.cfg.IncludeOwn }

// QueueStats reports the queue depth when queue mode is enabled.
func (s *Service) QueueStats(ctx context.Context) (*QueueStats, error) {
	if s.queue == nil {
		return nil, nil
	}
	st, err := s.queue.Stats(ctx)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// Key layout. Parsing keys in one place keeps the Lua scripts and Go code in
// sync; the prefix makes the cache namespace obvious in redis-cli.
type keySet struct{ p string }

func (k keySet) feed(uid int64) string       { return k.p + ":feed:" + i64(uid) }
func (k keySet) ready(uid int64) string      { return k.p + ":feed:ready:" + i64(uid) }
func (k keySet) rebuilding(uid int64) string { return k.p + ":feed:rebuild:" + i64(uid) }
func (k keySet) dirty(uid int64) string      { return k.p + ":feed:dirty:" + i64(uid) }
func (k keySet) friends(uid int64) string    { return k.p + ":friends:" + i64(uid) }
func (k keySet) post(id int64) string        { return k.p + ":post:" + i64(id) }

func (s *Service) keys() keySet { return keySet{p: s.cfg.Prefix} }

func i64(v int64) string { return strconv.FormatInt(v, 10) }

// fanoutScript adds one post to a user's feed. It is skipped while the feed is
// being rebuilt (the rebuild will pick the post up from the database, or the
// dirty flag makes it run again). A missing ready flag means the feed is not
// materialised: the post will be there when the feed is first built from the
// database, so there is nothing to do.
//
// KEYS: 1 feed, 2 rebuilding, 3 dirty, 4 ready
// ARGV: 1 score, 2 member, 3 max size, 4 ttl seconds
const fanoutLua = `
if redis.call('EXISTS', KEYS[2]) == 1 then
  redis.call('SET', KEYS[3], '1', 'EX', 60)
  return 0
end
if redis.call('EXISTS', KEYS[4]) == 0 then
  return -1
end
redis.call('ZADD', KEYS[1], ARGV[1], ARGV[2])
redis.call('ZREMRANGEBYRANK', KEYS[1], 0, -(tonumber(ARGV[3]) + 1))
redis.call('EXPIRE', KEYS[1], ARGV[4])
redis.call('EXPIRE', KEYS[4], ARGV[4])
return 1
`

// removeLua drops a deleted post from a user's feed with the same
// rebuild-window protection as fanoutLua.
//
// KEYS: 1 feed, 2 rebuilding, 3 dirty
// ARGV: 1 member
const removeLua = `
if redis.call('EXISTS', KEYS[2]) == 1 then
  redis.call('SET', KEYS[3], '1', 'EX', 60)
  return 0
end
redis.call('ZREM', KEYS[1], ARGV[1])
return 1
`

// invalidateLua drops a feed that no longer matches the database (friend
// added/removed). If a rebuild is running, the dirty flag forces it to re-read
// the database, so the invalidation cannot be overwritten by a stale rebuild.
//
// KEYS: 1 feed, 2 ready, 3 rebuilding, 4 dirty
const invalidateLua = `
if redis.call('EXISTS', KEYS[3]) == 1 then
  redis.call('SET', KEYS[4], '1', 'EX', 60)
end
redis.call('DEL', KEYS[1], KEYS[2])
return 1
`

// Feed returns one page of the user's cached feed. A cache miss (or a Redis
// failure) is served from PostgreSQL, so the cache never becomes a hard
// dependency for reads.
func (s *Service) Feed(ctx context.Context, userID int64, offset, limit int) ([]store.Post, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > s.cfg.Size {
		limit = s.cfg.Size
	}

	posts, err := s.feedFromCache(ctx, userID, offset, limit)
	if err == nil {
		return posts, nil
	}

	s.fallbacks.Add(1)
	s.log.Printf("feed: cache path failed for user %d, reading the database: %v", userID, err)
	return s.feedFromDB(ctx, userID, offset, limit)
}

func (s *Service) feedFromCache(ctx context.Context, userID int64, offset, limit int) ([]store.Post, error) {
	k := s.keys()
	ready, err := s.rdb.Exists(ctx, k.ready(userID)).Result()
	if err != nil {
		return nil, err
	}
	if ready == 0 {
		s.misses.Add(1)
		if err := s.Rebuild(ctx, userID); err != nil {
			return nil, fmt.Errorf("rebuild: %w", err)
		}
	} else {
		s.hits.Add(1)
	}

	ids, err := s.rdb.ZRevRange(ctx, k.feed(userID), int64(offset), int64(offset+limit-1)).Result()
	if err != nil {
		return nil, err
	}
	s.touch(ctx, userID)
	if len(ids) == 0 {
		return []store.Post{}, nil
	}
	return s.postsByIDs(ctx, ids)
}

func (s *Service) feedFromDB(ctx context.Context, userID int64, offset, limit int) ([]store.Post, error) {
	authors, err := s.audience(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.store.FeedPosts(ctx, authors, limit, offset)
}

// postsByIDs resolves post ids to payloads: Redis first, the database for the
// misses (their payloads are written back into the cache).
func (s *Service) postsByIDs(ctx context.Context, ids []string) ([]store.Post, error) {
	k := s.keys()
	keys := make([]string, len(ids))
	parsed := make([]int64, len(ids))
	for i, raw := range ids {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err == nil {
			parsed[i] = id
			keys[i] = k.post(id)
		}
	}

	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}

	byID := make(map[int64]store.Post, len(ids))
	missing := make([]int64, 0, len(ids))
	for i, v := range vals {
		if parsed[i] == 0 {
			continue
		}
		if v == nil {
			missing = append(missing, parsed[i])
			continue
		}
		raw, ok := v.(string)
		if !ok {
			missing = append(missing, parsed[i])
			continue
		}
		var p store.Post
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			missing = append(missing, parsed[i])
			continue
		}
		byID[p.ID] = p
	}

	if len(missing) > 0 {
		dbPosts, err := s.store.GetPostsByIDs(ctx, missing)
		if err != nil {
			return nil, err
		}
		s.cachePosts(ctx, dbPosts)
		for _, p := range dbPosts {
			byID[p.ID] = p
		}
	}

	out := make([]store.Post, 0, len(ids))
	for _, id := range parsed {
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// Rebuild materialises a user's feed from PostgreSQL. It is idempotent and
// safe to run concurrently: a per-user marker serialises rebuilds, and the
// dirty flag makes a rebuild re-read the database when a fan-out lands in the
// window between the SQL query and the Redis write.
func (s *Service) Rebuild(ctx context.Context, userID int64) error {
	const rounds = 3
	for i := 0; i < rounds; i++ {
		dirty, err := s.rebuildOnce(ctx, userID)
		if err != nil {
			return err
		}
		if !dirty {
			s.rebuilds.Add(1)
			return nil
		}
	}

	// The feed is under constant write pressure; drop the ready flag so the
	// next read rebuilds again. The feed stays correct, just not cached.
	k := s.keys()
	if err := s.rdb.Del(ctx, k.ready(userID)).Err(); err != nil {
		return err
	}
	s.rebuilds.Add(1)
	return nil
}

func (s *Service) rebuildOnce(ctx context.Context, userID int64) (dirty bool, err error) {
	k := s.keys()
	acquired, err := s.rdb.SetNX(ctx, k.rebuilding(userID), "1", 30*time.Second).Result()
	if err != nil {
		return false, err
	}
	if !acquired {
		ready, err := s.waitReady(ctx, userID, 2*time.Second)
		if err != nil {
			return false, err
		}
		if ready {
			return false, nil
		}
		// The concurrent rebuild is stuck; build the feed ourselves. Our
		// write is idempotent, so overlapping work is harmless.
	}
	if acquired {
		defer func() {
			// Use a background context: a cancelled request must not leave the
			// marker behind.
			s.rdb.Del(context.Background(), k.rebuilding(userID))
		}()
		// A stale dirty flag would make us rebuild twice for nothing.
		if err := s.rdb.Del(ctx, k.dirty(userID)).Err(); err != nil {
			return false, err
		}
	}

	friends, err := s.friendIDs(ctx, userID)
	if err != nil {
		return false, err
	}
	authors := friends
	if s.cfg.IncludeOwn {
		authors = append(authors, userID)
	}

	ids, err := s.store.FeedPostIDs(ctx, authors, s.cfg.Size)
	if err != nil {
		return false, err
	}

	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, k.feed(userID))
	if len(ids) > 0 {
		members := make([]redis.Z, len(ids))
		for i, id := range ids {
			members[i] = redis.Z{Score: float64(id), Member: i64(id)}
		}
		pipe.ZAdd(ctx, k.feed(userID), members...)
	}
	pipe.Expire(ctx, k.feed(userID), s.cfg.FeedTTL)
	pipe.Set(ctx, k.ready(userID), "1", s.cfg.FeedTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}

	if !acquired {
		return false, nil
	}

	// Close the rebuild window: remove the marker, then consume a dirty flag
	// set by a fan-out that arrived while the marker was up.
	if err := s.rdb.Del(ctx, k.rebuilding(userID)).Err(); err != nil {
		return false, err
	}
	flag, err := s.rdb.GetDel(ctx, k.dirty(userID)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, err
	}
	return flag == "1", nil
}

func (s *Service) waitReady(ctx context.Context, userID int64, timeout time.Duration) (bool, error) {
	k := s.keys()
	deadline := time.Now().Add(timeout)
	for {
		n, err := s.rdb.Exists(ctx, k.ready(userID)).Result()
		if err != nil {
			return false, err
		}
		if n == 1 {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// touch keeps a hot feed alive: TTLs slide while the user reads the feed.
func (s *Service) touch(ctx context.Context, userID int64) {
	k := s.keys()
	pipe := s.rdb.Pipeline()
	pipe.Expire(ctx, k.feed(userID), s.cfg.FeedTTL)
	pipe.Expire(ctx, k.ready(userID), s.cfg.FeedTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		s.log.Printf("feed: touch user %d: %v", userID, err)
	}
}

// GetPost is a cache-aside read of a single post.
func (s *Service) GetPost(ctx context.Context, id int64) (store.Post, error) {
	k := s.keys()
	raw, err := s.rdb.Get(ctx, k.post(id)).Result()
	switch {
	case err == nil:
		var p store.Post
		if json.Unmarshal([]byte(raw), &p) == nil {
			return p, nil
		}
	case !errors.Is(err, redis.Nil):
		s.log.Printf("feed: post cache read %d: %v", id, err)
	}

	p, err := s.store.GetPost(ctx, id)
	if err != nil {
		return store.Post{}, err
	}
	s.cachePosts(ctx, []store.Post{p})
	return p, nil
}

// CreatePost writes the post and fans it out to the author's friends (and, by
// default, to the author's own feed). A cache failure never fails the write:
// the database is the source of truth and the feed rebuilds on the next read.
func (s *Service) CreatePost(ctx context.Context, authorID int64, text string) (store.Post, error) {
	p, err := s.store.CreatePost(ctx, authorID, text)
	if err != nil {
		return store.Post{}, err
	}
	s.cachePosts(ctx, []store.Post{p})

	if s.cfg.Mode == ModeQueue && s.queue != nil {
		job := Job{Type: JobPostFanout, PostID: p.ID, AuthorUserID: p.AuthorUserID}
		if err := s.queue.Enqueue(ctx, job); err != nil {
			s.log.Printf("feed: enqueue fanout for post %d: %v", p.ID, err)
			s.fanout(ctx, p.AuthorUserID, p.ID)
		}
	} else {
		s.fanout(ctx, p.AuthorUserID, p.ID)
	}
	return p, nil
}

// UpdatePost changes the text and drops the cached payload. Feed membership is
// untouched: the feed is ordered by post id, not by edit time.
func (s *Service) UpdatePost(ctx context.Context, id, authorID int64, text string) (store.Post, error) {
	p, err := s.store.UpdatePost(ctx, id, authorID, text)
	if err != nil {
		return store.Post{}, err
	}
	if err := s.rdb.Del(ctx, s.keys().post(id)).Err(); err != nil {
		s.log.Printf("feed: invalidate post %d: %v", id, err)
	}
	return p, nil
}

// DeletePost soft-deletes the post and removes it from the cached feeds of the
// author and their friends.
func (s *Service) DeletePost(ctx context.Context, id, authorID int64) error {
	if err := s.store.DeletePost(ctx, id, authorID); err != nil {
		return err
	}
	if err := s.rdb.Del(ctx, s.keys().post(id)).Err(); err != nil {
		s.log.Printf("feed: invalidate post %d: %v", id, err)
	}

	if s.cfg.Mode == ModeQueue && s.queue != nil {
		job := Job{Type: JobPostRemove, PostID: id, AuthorUserID: authorID}
		if err := s.queue.Enqueue(ctx, job); err != nil {
			s.log.Printf("feed: enqueue removal for post %d: %v", id, err)
			if err := s.removeFromFeeds(ctx, authorID, id); err != nil {
				s.log.Printf("feed: remove post %d: %v", id, err)
			}
		}
	} else {
		if err := s.removeFromFeeds(ctx, authorID, id); err != nil {
			s.log.Printf("feed: remove post %d: %v", id, err)
		}
	}
	return nil
}

// AddFriend stores an undirected friendship and invalidates both feeds: the
// next read rebuilds them from the database, which now includes each other's
// posts.
func (s *Service) AddFriend(ctx context.Context, userID, friendID int64) (bool, error) {
	created, err := s.store.AddFriend(ctx, userID, friendID)
	if err != nil {
		return false, err
	}
	s.invalidateFriendship(ctx, userID, friendID)
	return created, nil
}

// RemoveFriend deletes the friendship and invalidates both feeds.
func (s *Service) RemoveFriend(ctx context.Context, userID, friendID int64) error {
	if err := s.store.RemoveFriend(ctx, userID, friendID); err != nil {
		return err
	}
	s.invalidateFriendship(ctx, userID, friendID)
	return nil
}

func (s *Service) invalidateFriendship(ctx context.Context, a, b int64) {
	k := s.keys()
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, k.friends(a), k.friends(b))
	pipe.Eval(ctx, invalidateLua,
		[]string{k.feed(a), k.ready(a), k.rebuilding(a), k.dirty(a)})
	pipe.Eval(ctx, invalidateLua,
		[]string{k.feed(b), k.ready(b), k.rebuilding(b), k.dirty(b)})
	if _, err := pipe.Exec(ctx); err != nil {
		s.log.Printf("feed: invalidate friendship %d-%d: %v", a, b, err)
	}
}

// fanout pushes one post id into the feeds of the author's friends.
func (s *Service) fanout(ctx context.Context, authorID, postID int64) {
	if err := s.fanoutPost(ctx, authorID, postID); err != nil {
		s.log.Printf("feed: fanout post %d of user %d: %v", postID, authorID, err)
		return
	}
	s.fanouts.Add(1)
}

func (s *Service) fanoutPost(ctx context.Context, authorID, postID int64) error {
	targets, err := s.audience(ctx, authorID)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	k := s.keys()
	ttl := int64(s.cfg.FeedTTL.Seconds())
	pipe := s.rdb.Pipeline()
	for _, uid := range targets {
		pipe.Eval(ctx, fanoutLua,
			[]string{k.feed(uid), k.rebuilding(uid), k.dirty(uid), k.ready(uid)},
			i64(postID), i64(postID), s.cfg.Size, ttl)
	}
	_, err = pipe.Exec(ctx)
	return err
}

// removeFromFeeds drops a deleted post from the author's and friends' feeds.
func (s *Service) removeFromFeeds(ctx context.Context, authorID, postID int64) error {
	targets, err := s.audience(ctx, authorID)
	if err != nil {
		return err
	}
	k := s.keys()
	pipe := s.rdb.Pipeline()
	for _, uid := range targets {
		pipe.Eval(ctx, removeLua,
			[]string{k.feed(uid), k.rebuilding(uid), k.dirty(uid)},
			i64(postID))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	s.removals.Add(1)
	return nil
}

// audience is the set of feeds a post belongs to: the author's friends and,
// optionally, the author.
func (s *Service) audience(ctx context.Context, authorID int64) ([]int64, error) {
	friends, err := s.friendIDs(ctx, authorID)
	if err != nil {
		return nil, err
	}
	if !s.cfg.IncludeOwn {
		return friends, nil
	}
	return append(friends, authorID), nil
}

// friendIDs serves the friend list from Redis with a database fallback. It is
// on the write path, so the master is the source (a read replica could lag
// behind a just-accepted friendship).
func (s *Service) friendIDs(ctx context.Context, userID int64) ([]int64, error) {
	k := s.keys()
	exists, err := s.rdb.Exists(ctx, k.friends(userID)).Result()
	if err != nil {
		return s.store.FriendsOf(ctx, userID)
	}
	if exists == 1 {
		vals, err := s.rdb.SMembers(ctx, k.friends(userID)).Result()
		if err == nil {
			ids := make([]int64, 0, len(vals))
			for _, v := range vals {
				if id, err := strconv.ParseInt(v, 10, 64); err == nil {
					ids = append(ids, id)
				}
			}
			return ids, nil
		}
	}

	ids, err := s.store.FriendsOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	s.cacheFriends(ctx, userID, ids)
	return ids, nil
}

func (s *Service) cacheFriends(ctx context.Context, userID int64, ids []int64) {
	if len(ids) == 0 {
		// Redis has no empty sets; the next lookup simply hits the database.
		return
	}
	k := s.keys()
	members := make([]any, len(ids))
	for i, id := range ids {
		members[i] = i64(id)
	}
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, k.friends(userID))
	pipe.SAdd(ctx, k.friends(userID), members...)
	pipe.Expire(ctx, k.friends(userID), s.cfg.FriendsTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		s.log.Printf("feed: cache friends of %d: %v", userID, err)
	}
}

// cachePosts writes post payloads to Redis. Failures are logged, not returned:
// the database read is already done.
func (s *Service) cachePosts(ctx context.Context, posts []store.Post) {
	if len(posts) == 0 {
		return
	}
	k := s.keys()
	pipe := s.rdb.Pipeline()
	for _, p := range posts {
		raw, err := json.Marshal(p)
		if err != nil {
			s.log.Printf("feed: marshal post %d: %v", p.ID, err)
			continue
		}
		pipe.Set(ctx, k.post(p.ID), raw, s.cfg.PostTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		s.log.Printf("feed: cache posts: %v", err)
		return
	}
	s.cacheWrites.Add(uint64(len(posts)))
}

// StartWorkers runs the feed queue consumers (queue mode only).
func (s *Service) StartWorkers(ctx context.Context, n int) {
	if s.queue == nil {
		return
	}
	if n <= 0 {
		n = 2
	}
	if err := s.queue.EnsureGroup(ctx); err != nil {
		s.log.Printf("feed: queue group: %v", err)
		return
	}
	for i := 0; i < n; i++ {
		go s.queue.Run(ctx, fmt.Sprintf("worker-%d", i+1), s.handleJob)
	}
	s.log.Printf("feed: %d queue workers started on %s", n, s.queue.stream)
}

func (s *Service) handleJob(ctx context.Context, job Job) error {
	var err error
	switch job.Type {
	case JobPostFanout:
		err = s.fanoutPost(ctx, job.AuthorUserID, job.PostID)
	case JobPostRemove:
		err = s.removeFromFeeds(ctx, job.AuthorUserID, job.PostID)
	case JobFeedRefresh:
		err = s.Rebuild(ctx, job.UserID)
	default:
		err = fmt.Errorf("unknown job type %q", job.Type)
	}
	if err != nil {
		s.jobsFailed.Add(1)
		return err
	}
	s.jobsOK.Add(1)
	return nil
}

// RebuildAll rebuilds every user's feed from the database in id order. It is
// the entry point for "перестройка кэшей из СУБД".
func (s *Service) RebuildAll(ctx context.Context, batch int, progress func(done int)) (int, error) {
	if batch <= 0 {
		batch = 500
	}
	done := 0
	var after int64
	for {
		ids, err := s.store.UserIDsAfter(ctx, after, batch)
		if err != nil {
			return done, err
		}
		if len(ids) == 0 {
			return done, nil
		}
		for _, id := range ids {
			if err := s.Rebuild(ctx, id); err != nil {
				return done, fmt.Errorf("user %d: %w", id, err)
			}
			done++
			after = id
		}
		if progress != nil {
			progress(done)
		}
		if err := ctx.Err(); err != nil {
			return done, err
		}
	}
}

type Status struct {
	UserID    int64 `json:"user_id"`
	Ready     bool  `json:"ready"`
	Size      int64 `json:"size"`
	FeedTTLms int64 `json:"feed_ttl_ms"`
}

// Status exposes the cache state of one feed for the demo and the report.
func (s *Service) Status(ctx context.Context, userID int64) (Status, error) {
	k := s.keys()
	pipe := s.rdb.Pipeline()
	ready := pipe.Exists(ctx, k.ready(userID))
	size := pipe.ZCard(ctx, k.feed(userID))
	ttl := pipe.PTTL(ctx, k.feed(userID))
	if _, err := pipe.Exec(ctx); err != nil {
		return Status{}, err
	}
	return Status{
		UserID:    userID,
		Ready:     ready.Val() == 1,
		Size:      size.Val(),
		FeedTTLms: ttl.Val().Milliseconds(),
	}, nil
}

func (s *Service) Stats(ctx context.Context) Stats {
	out := Stats{
		FeedHits:        s.hits.Load(),
		FeedMisses:      s.misses.Load(),
		FeedRebuilds:    s.rebuilds.Load(),
		FeedFallbacks:   s.fallbacks.Load(),
		PostCacheWrites: s.cacheWrites.Load(),
		FanoutPosts:     s.fanouts.Load(),
		FeedRemovals:    s.removals.Load(),
		JobsProcessed:   s.jobsOK.Load(),
		JobsFailed:      s.jobsFailed.Load(),
	}
	return out
}
