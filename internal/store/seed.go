package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// UserSeed is one row prepared for bulk loading. It mirrors the users table
// columns that the seeder fills; id and created_at are left to the database.
type UserSeed struct {
	Username     string
	PasswordHash string
	FirstName    string
	LastName     string
	BirthDate    time.Time
	Gender       string
	Interests    []string
	City         string
}

// InsertUsersBatch inserts a batch of profiles using COPY. The caller is
// responsible for splitting the input into reasonably sized batches.
func (s *Store) InsertUsersBatch(ctx context.Context, users []UserSeed) error {
	if len(users) == 0 {
		return nil
	}

	rows := make([][]any, len(users))
	for i, u := range users {
		rows[i] = []any{
			u.Username,
			u.PasswordHash,
			u.FirstName,
			u.LastName,
			pgtype.Date{Time: u.BirthDate, Valid: true},
			u.Gender,
			u.Interests,
			u.City,
		}
	}

	_, err := s.pool.CopyFrom(ctx,
		pgx.Identifier{"users"},
		[]string{"username", "password_hash", "first_name", "last_name", "birth_date", "gender", "interests", "city"},
		pgx.CopyFromRows(rows),
	)
	return err
}

// TruncateUsers removes all profiles and dependent sessions.
func (s *Store) TruncateUsers(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `TRUNCATE users RESTART IDENTITY CASCADE`)
	return err
}

// AnalyzeUsers refreshes planner statistics after a bulk load.
func (s *Store) AnalyzeUsers(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `ANALYZE users`)
	return err
}
