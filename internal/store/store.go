package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

type User struct {
	ID        int64    `json:"id"`
	Username  string   `json:"username"`
	FirstName string   `json:"first_name"`
	LastName  string   `json:"last_name"`
	BirthDate string   `json:"birth_date"`
	Gender    string   `json:"gender"`
	Interests []string `json:"interests"`
	City      string   `json:"city"`
}

type Credential struct {
	ID           int64
	PasswordHash string
}

type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) CreateUser(ctx context.Context, u User, passwordHash string) (int64, error) {
	const q = `
		INSERT INTO users (username, password_hash, first_name, last_name, birth_date, gender, interests, city)
		VALUES ($1, $2, $3, $4, $5::date, $6, $7, $8)
		RETURNING id`

	var id int64
	err := s.pool.QueryRow(ctx, q,
		u.Username, passwordHash, u.FirstName, u.LastName, u.BirthDate, u.Gender, u.Interests, u.City,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, ErrConflict
		}
		return 0, err
	}
	return id, nil
}

func (s *Store) GetUserByID(ctx context.Context, id int64) (User, error) {
	const q = `
		SELECT id, username, first_name, last_name, birth_date::text, gender, interests, city
		FROM users
		WHERE id = $1`

	var u User
	err := s.pool.QueryRow(ctx, q, id).Scan(
		&u.ID, &u.Username, &u.FirstName, &u.LastName, &u.BirthDate, &u.Gender, &u.Interests, &u.City,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s *Store) GetCredentialByUsername(ctx context.Context, username string) (Credential, error) {
	const q = `SELECT id, password_hash FROM users WHERE username = $1`

	var c Credential
	err := s.pool.QueryRow(ctx, q, username).Scan(&c.ID, &c.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	return c, err
}

func (s *Store) CreateSession(ctx context.Context, token string, userID int64, ttl time.Duration) error {
	const q = `INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, $3)`

	_, err := s.pool.Exec(ctx, q, token, userID, time.Now().Add(ttl))
	return err
}

func (s *Store) UserIDByToken(ctx context.Context, token string) (int64, error) {
	const q = `SELECT user_id FROM sessions WHERE token = $1 AND expires_at > now()`

	var userID int64
	err := s.pool.QueryRow(ctx, q, token).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return userID, err
}
