package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Post is a user post. ID is used as the feed ordering key: BIGSERIAL grows with
// insertion time, so "the latest 1000 posts" is "the 1000 greatest ids".
type Post struct {
	ID           int64     `json:"id"`
	Text         string    `json:"text"`
	AuthorUserID int64     `json:"author_user_id"`
	CreatedAt    time.Time `json:"created_at"`
}

const postColumns = `id, text, author_id, created_at`

// CreatePost inserts a post on the master. The caller is responsible for text
// validation; the table has a CHECK as a second line of defence.
func (s *Store) CreatePost(ctx context.Context, authorID int64, text string) (Post, error) {
	const q = `INSERT INTO posts (author_id, text) VALUES ($1, $2) RETURNING ` + postColumns

	var p Post
	err := s.master.QueryRow(ctx, q, authorID, text).
		Scan(&p.ID, &p.Text, &p.AuthorUserID, &p.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return Post{}, ErrNotFound
		}
		return Post{}, err
	}
	return p, nil
}

// GetPost reads one live post. Like other point reads it goes to a replica and
// is retried on the master when the replica has not caught up yet.
func (s *Store) GetPost(ctx context.Context, id int64) (Post, error) {
	const q = `SELECT ` + postColumns + ` FROM posts WHERE id = $1 AND deleted_at IS NULL`

	t := s.readTargetFrom(ctx)
	p, err := scanPost(t.pool.QueryRow(ctx, q, id))
	if s.fallbackNeeded(t, err) {
		s.masterFallbacks.Add(1)
		p, err = scanPost(s.master.QueryRow(ctx, q, id))
	}
	return p, err
}

// GetPostsByIDs returns live posts with the given ids. The result is unordered;
// callers restore the order they need. A replica lag right after a write makes
// ids missing, so the query is repeated on the master in that case.
func (s *Store) GetPostsByIDs(ctx context.Context, ids []int64) ([]Post, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	const q = `SELECT ` + postColumns + ` FROM posts WHERE id = ANY($1) AND deleted_at IS NULL`

	t := s.readTargetFrom(ctx)
	posts, err := queryPosts(ctx, t.pool, q, ids)
	if err != nil {
		return nil, err
	}
	if t.replica != nil && s.readFallback && len(posts) < len(ids) {
		s.masterFallbacks.Add(1)
		return queryPosts(ctx, s.master, q, ids)
	}
	return posts, nil
}

// UpdatePost changes the text of a live post owned by authorID.
func (s *Store) UpdatePost(ctx context.Context, id, authorID int64, text string) (Post, error) {
	const q = `
		UPDATE posts
		SET text = $3, updated_at = now()
		WHERE id = $1 AND author_id = $2 AND deleted_at IS NULL
		RETURNING ` + postColumns

	var p Post
	err := s.master.QueryRow(ctx, q, id, authorID, text).
		Scan(&p.ID, &p.Text, &p.AuthorUserID, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Post{}, ErrNotFound
	}
	return p, err
}

// DeletePost soft-deletes a live post owned by authorID.
func (s *Store) DeletePost(ctx context.Context, id, authorID int64) error {
	const q = `
		UPDATE posts
		SET deleted_at = now()
		WHERE id = $1 AND author_id = $2 AND deleted_at IS NULL`

	tag, err := s.master.Exec(ctx, q, id, authorID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AddFriend inserts an undirected friendship. It reports whether the pair was
// new: adding an existing friend is a no-op, not an error.
func (s *Store) AddFriend(ctx context.Context, userID, friendID int64) (bool, error) {
	const q = `
		INSERT INTO friends (user_id, friend_id)
		VALUES ($1, $2)
		ON CONFLICT (user_id, friend_id) DO NOTHING`

	// The pair is stored in canonical order (user_id < friend_id). Sorting in
	// Go avoids the SQL type ambiguity of LEAST($1, $2) with pgx parameters.
	a, b := userID, friendID
	if a > b {
		a, b = b, a
	}

	tag, err := s.master.Exec(ctx, q, a, b)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return false, ErrNotFound
		}
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RemoveFriend deletes an undirected friendship.
func (s *Store) RemoveFriend(ctx context.Context, userID, friendID int64) error {
	const q = `DELETE FROM friends WHERE user_id = $1 AND friend_id = $2`

	a, b := userID, friendID
	if a > b {
		a, b = b, a
	}

	_, err := s.master.Exec(ctx, q, a, b)
	return err
}

// FriendsOf returns the undirected friend list. Feed fan-out needs it right
// after a friendship is written, so it reads the master (and the feed service
// caches the result in Redis).
func (s *Store) FriendsOf(ctx context.Context, userID int64) ([]int64, error) {
	const q = `
		SELECT CASE WHEN user_id = $1 THEN friend_id ELSE user_id END
		FROM friends
		WHERE user_id = $1 OR friend_id = $1
		ORDER BY 1`

	rows, err := s.master.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0, 64)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FeedPostIDs rebuilds a feed from the database: the latest limit post ids of
// the given authors. It reads the master so a rebuild never misses a post that
// has already been committed (and whose fan-out may still be in flight).
func (s *Store) FeedPostIDs(ctx context.Context, authorIDs []int64, limit int) ([]int64, error) {
	const q = `
		SELECT id
		FROM posts
		WHERE author_id = ANY($1) AND deleted_at IS NULL
		ORDER BY id DESC
		LIMIT $2`

	rows, err := s.master.Query(ctx, q, authorIDs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FeedPosts reads a page of the feed straight from the database: the latest
// live posts of the given authors. It is the fallback when the cache is
// unavailable, and it uses the same ordering as the cached feed (id desc).
func (s *Store) FeedPosts(ctx context.Context, authorIDs []int64, limit, offset int) ([]Post, error) {
	const q = `
		SELECT ` + postColumns + `
		FROM posts
		WHERE author_id = ANY($1) AND deleted_at IS NULL
		ORDER BY id DESC
		LIMIT $2 OFFSET $3`

	return queryPosts(ctx, s.master, q, authorIDs, limit, offset)
}

// UserIDsAfter pages through users for a full feed rebuild.
func (s *Store) UserIDsAfter(ctx context.Context, afterID int64, limit int) ([]int64, error) {
	const q = `SELECT id FROM users WHERE id > $1 ORDER BY id LIMIT $2`

	rows, err := s.master.Query(ctx, q, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func scanPost(row pgx.Row) (Post, error) {
	var p Post
	err := row.Scan(&p.ID, &p.Text, &p.AuthorUserID, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Post{}, ErrNotFound
	}
	return p, err
}

func queryPosts(ctx context.Context, q *pgxpool.Pool, sql string, args ...any) ([]Post, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	posts := make([]Post, 0, 16)
	for rows.Next() {
		var p Post
		if err := rows.Scan(&p.ID, &p.Text, &p.AuthorUserID, &p.CreatedAt); err != nil {
			return nil, err
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}
