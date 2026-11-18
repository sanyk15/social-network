package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// PostSeed is one post prepared for bulk loading. ID is left to the sequence.
type PostSeed struct {
	AuthorID  int64
	Text      string
	CreatedAt time.Time
}

// FriendPair is a canonical (user_id < friend_id) friendship.
type FriendPair struct {
	UserID   int64
	FriendID int64
}

// InsertPostsBatch bulk-loads posts with COPY. CreatedAt is explicit so the
// seeded feed looks like a timeline; ids still grow with insertion order.
func (s *Store) InsertPostsBatch(ctx context.Context, posts []PostSeed) error {
	if len(posts) == 0 {
		return nil
	}
	rows := make([][]any, len(posts))
	for i, p := range posts {
		rows[i] = []any{p.AuthorID, p.Text, pgtype.Timestamptz{Time: p.CreatedAt, Valid: true}}
	}
	_, err := s.master.CopyFrom(ctx,
		pgx.Identifier{"posts"},
		[]string{"author_id", "text", "created_at"},
		pgx.CopyFromRows(rows),
	)
	return err
}

// InsertFriendsBatch bulk-loads canonical friendship pairs.
func (s *Store) InsertFriendsBatch(ctx context.Context, pairs []FriendPair) error {
	if len(pairs) == 0 {
		return nil
	}
	rows := make([][]any, len(pairs))
	for i, p := range pairs {
		rows[i] = []any{p.UserID, p.FriendID}
	}
	_, err := s.master.CopyFrom(ctx,
		pgx.Identifier{"friends"},
		[]string{"user_id", "friend_id"},
		pgx.CopyFromRows(rows),
	)
	return err
}

// TruncateFeedData clears posts and friendships before reseeding.
func (s *Store) TruncateFeedData(ctx context.Context) error {
	_, err := s.master.Exec(ctx, `TRUNCATE posts, friends RESTART IDENTITY`)
	return err
}

// AnalyzeFeedData refreshes planner statistics after a bulk load.
func (s *Store) AnalyzeFeedData(ctx context.Context) error {
	_, err := s.master.Exec(ctx, `ANALYZE posts; ANALYZE friends`)
	return err
}

// MaxUserID returns the greatest user id, so the seeder can clamp the number
// of users it links together.
func (s *Store) MaxUserID(ctx context.Context) (int64, error) {
	var id *int64
	if err := s.master.QueryRow(ctx, `SELECT max(id) FROM users`).Scan(&id); err != nil {
		return 0, err
	}
	if id == nil {
		return 0, nil
	}
	return *id, nil
}
