package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
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

// Config describes the database topology. MasterURL is required; ReplicaURLs
// may be empty, in which case all reads go to the master.
type Config struct {
	MasterURL    string
	ReplicaURLs  []string
	MaxConns     int32
	ReadFallback bool
}

type replica struct {
	name    string
	pool    *pgxpool.Pool
	healthy atomic.Bool
	lastErr atomic.Value
}

func (r *replica) lastError() string {
	if v := r.lastErr.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// readTarget is the pool chosen for one read-only request. It is stored in the
// request context, so authorization and the data query of a single request go
// to the same replica. This is the Go counterpart of Spring's
// @Transactional(readOnly = true).
type readTarget struct {
	pool    *pgxpool.Pool
	replica *replica
	name    string
}

type readTargetKey struct{}

type Store struct {
	master       *pgxpool.Pool
	replicas     []*replica
	next         atomic.Uint64
	readFallback bool

	replicaReads    atomic.Uint64
	masterReads     atomic.Uint64
	masterFallbacks atomic.Uint64

	cancel context.CancelFunc
	done   chan struct{}
}

func New(ctx context.Context, cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.MasterURL) == "" {
		return nil, errors.New("master database URL is required")
	}

	master, err := newPool(ctx, cfg.MasterURL, cfg.MaxConns, "social-app")
	if err != nil {
		return nil, fmt.Errorf("master pool: %w", err)
	}
	if err := master.Ping(ctx); err != nil {
		master.Close()
		return nil, fmt.Errorf("master ping: %w", err)
	}

	s := &Store{master: master, readFallback: cfg.ReadFallback}
	for i, url := range cfg.ReplicaURLs {
		pool, err := newPool(ctx, url, cfg.MaxConns, fmt.Sprintf("social-app-read-%d", i+1))
		if err != nil {
			s.closePools()
			return nil, fmt.Errorf("replica %d pool: %w", i+1, err)
		}
		name := fmt.Sprintf("%s:%d", pool.Config().ConnConfig.Host, pool.Config().ConnConfig.Port)
		r := &replica{name: name, pool: pool}
		if err := pingReplica(ctx, r); err != nil {
			log.Printf("replica %s is not available yet: %v", name, err)
		}
		s.replicas = append(s.replicas, r)
	}

	healthCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.healthLoop(healthCtx)

	return s, nil
}

func newPool(ctx context.Context, databaseURL string, maxConns int32, appName string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = make(map[string]string)
	}
	// Fail fast when a node disappears (the failover lab kills the master).
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	// LIKE 'prefix%' can use a btree index only when the planner knows the
	// pattern at plan time. Force custom plans so cached prepared statements
	// keep using the search index instead of falling back to a generic plan.
	cfg.ConnConfig.RuntimeParams["plan_cache_mode"] = "force_custom_plan"
	cfg.ConnConfig.RuntimeParams["application_name"] = appName

	return pgxpool.NewWithConfig(ctx, cfg)
}

func pingReplica(ctx context.Context, r *replica) error {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	err := r.pool.Ping(pingCtx)
	wasHealthy := r.healthy.Swap(err == nil)
	if err != nil {
		r.lastErr.Store(err.Error())
	} else {
		r.lastErr.Store("")
	}

	switch {
	case err == nil && !wasHealthy:
		log.Printf("replica %s is healthy", r.name)
	case err != nil && wasHealthy:
		log.Printf("replica %s is unhealthy: %v", r.name, err)
	}
	return err
}

func (s *Store) healthLoop(ctx context.Context) {
	defer close(s.done)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, r := range s.replicas {
				_ = pingReplica(ctx, r)
			}
		}
	}
}

func (s *Store) closePools() {
	for _, r := range s.replicas {
		r.pool.Close()
	}
	if s.master != nil {
		s.master.Close()
	}
}

func (s *Store) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.done != nil {
		<-s.done
	}
	s.closePools()
}

// WithReadOnly binds the request to one healthy replica. Handlers call it once
// at the start of a read request; all store reads using the returned context
// stick to that replica. If no replica is healthy, reads fall back to master.
func (s *Store) WithReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, readTargetKey{}, s.pickReadTarget())
}

func (s *Store) pickReadTarget() readTarget {
	if n := uint64(len(s.replicas)); n > 0 {
		start := s.next.Add(1) - 1
		for i := uint64(0); i < n; i++ {
			r := s.replicas[(start+i)%n]
			if r.healthy.Load() {
				s.replicaReads.Add(1)
				return readTarget{pool: r.pool, replica: r, name: r.name}
			}
		}
	}

	s.masterReads.Add(1)
	return readTarget{pool: s.master, name: "master"}
}

func (s *Store) readTargetFrom(ctx context.Context) readTarget {
	if t, ok := ctx.Value(readTargetKey{}).(readTarget); ok {
		return t
	}
	return s.pickReadTarget()
}

// fallbackNeeded reports whether a "not found" read should be retried on the
// master. A replica can lag right after a write; without this fallback a
// freshly registered user could get 401/404 from a read endpoint.
func (s *Store) fallbackNeeded(t readTarget, err error) bool {
	return s.readFallback && t.replica != nil && errors.Is(err, ErrNotFound)
}

func (s *Store) PingMaster(ctx context.Context) error {
	return s.master.Ping(ctx)
}

type RoutingStats struct {
	ReplicaReads    uint64 `json:"replica_reads"`
	MasterReads     uint64 `json:"master_reads"`
	MasterFallbacks uint64 `json:"master_fallbacks"`
}

func (s *Store) Stats() RoutingStats {
	return RoutingStats{
		ReplicaReads:    s.replicaReads.Load(),
		MasterReads:     s.masterReads.Load(),
		MasterFallbacks: s.masterFallbacks.Load(),
	}
}

type StandbyStatus struct {
	ApplicationName string `json:"application_name"`
	State           string `json:"state"`
	SyncState       string `json:"sync_state"`
	LagBytes        *int64 `json:"lag_bytes,omitempty"`
}

type MasterStatus struct {
	Name       string          `json:"name"`
	InRecovery bool            `json:"in_recovery"`
	CurrentLSN string          `json:"current_lsn,omitempty"`
	Replicas   []StandbyStatus `json:"replicas,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type ReplicaStatus struct {
	Name       string   `json:"name"`
	Healthy    bool     `json:"healthy"`
	InRecovery *bool    `json:"in_recovery,omitempty"`
	ReplayLSN  string   `json:"replay_lsn,omitempty"`
	LagBytes   *int64   `json:"lag_bytes,omitempty"`
	LagSeconds *float64 `json:"lag_seconds,omitempty"`
	LastError  string   `json:"last_error,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type ReplicationStatus struct {
	Master   MasterStatus    `json:"master"`
	Replicas []ReplicaStatus `json:"replicas"`
	Routing  RoutingStats    `json:"routing"`
}

// ReplicationStatus is a lab/diagnostics snapshot: master position, connected
// standbys and per-replica replay lag. It is not used by the request path.
func (s *Store) ReplicationStatus(ctx context.Context) ReplicationStatus {
	out := ReplicationStatus{Routing: s.Stats()}
	out.Master.Name = "master"

	masterCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var inRecovery bool
	var lsn string
	if err := s.master.QueryRow(masterCtx,
		`SELECT pg_is_in_recovery(), pg_current_wal_lsn()::text`).Scan(&inRecovery, &lsn); err != nil {
		out.Master.Error = err.Error()
	} else {
		out.Master.InRecovery = inRecovery
		out.Master.CurrentLSN = lsn

		rows, err := s.master.Query(masterCtx, `
			SELECT application_name, state, sync_state,
			       pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)::bigint
			FROM pg_stat_replication
			ORDER BY application_name`)
		if err != nil {
			out.Master.Error = err.Error()
		} else {
			defer rows.Close()
			for rows.Next() {
				var st StandbyStatus
				if err := rows.Scan(&st.ApplicationName, &st.State, &st.SyncState, &st.LagBytes); err != nil {
					out.Master.Error = err.Error()
					break
				}
				out.Master.Replicas = append(out.Master.Replicas, st)
			}
			if err := rows.Err(); err != nil {
				out.Master.Error = err.Error()
			}
		}
	}

	for _, r := range s.replicas {
		rs := ReplicaStatus{Name: r.name, Healthy: r.healthy.Load()}
		if !rs.Healthy {
			rs.LastError = r.lastError()
		}

		replicaCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		var replicaInRecovery bool
		var replayLSN *string
		var lagSeconds *float64
		err := r.pool.QueryRow(replicaCtx, `
			SELECT pg_is_in_recovery(),
			       pg_last_wal_replay_lsn()::text,
			       EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp())::float8`).
			Scan(&replicaInRecovery, &replayLSN, &lagSeconds)
		cancel()
		if err != nil {
			rs.Error = err.Error()
			out.Replicas = append(out.Replicas, rs)
			continue
		}

		rs.InRecovery = &replicaInRecovery
		if replayLSN != nil {
			rs.ReplayLSN = *replayLSN
		}
		rs.LagSeconds = lagSeconds

		if lsn != "" && replayLSN != nil {
			lagCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			var lag int64
			err := r.pool.QueryRow(lagCtx,
				`SELECT pg_wal_lsn_diff($1::pg_lsn, $2::pg_lsn)::bigint`,
				lsn, *replayLSN).Scan(&lag)
			cancel()
			if err == nil {
				rs.LagBytes = &lag
			}
		}

		out.Replicas = append(out.Replicas, rs)
	}

	return out
}

func (s *Store) CreateUser(ctx context.Context, u User, passwordHash string) (int64, error) {
	const q = `
		INSERT INTO users (username, password_hash, first_name, last_name, birth_date, gender, interests, city)
		VALUES ($1, $2, $3, $4, $5::date, $6, $7, $8)
		RETURNING id`

	var id int64
	err := s.master.QueryRow(ctx, q,
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

	t := s.readTargetFrom(ctx)
	u, err := scanUser(t.pool.QueryRow(ctx, q, id))
	if s.fallbackNeeded(t, err) {
		s.masterFallbacks.Add(1)
		u, err = scanUser(s.master.QueryRow(ctx, q, id))
	}
	return u, err
}

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(
		&u.ID, &u.Username, &u.FirstName, &u.LastName, &u.BirthDate, &u.Gender, &u.Interests, &u.City,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// escapeLikePrefix turns user input into a literal prefix pattern: wildcard
// characters are escaped and a trailing '%' is appended.
func escapeLikePrefix(s string) string {
	return likeEscaper.Replace(s) + "%"
}

// SearchUsers returns profiles whose first and last names start with the given
// prefixes, ordered by id. An empty result is a valid answer, so it is not
// retried on the master (unlike point reads).
func (s *Store) SearchUsers(ctx context.Context, firstName, lastName string) ([]User, error) {
	const q = `
		SELECT id, username, first_name, last_name, birth_date::text, gender, interests, city
		FROM users
		WHERE first_name LIKE $1 ESCAPE '\'
		  AND last_name  LIKE $2 ESCAPE '\'
		ORDER BY id`

	t := s.readTargetFrom(ctx)
	rows, err := t.pool.Query(ctx, q, escapeLikePrefix(firstName), escapeLikePrefix(lastName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]User, 0)
	for rows.Next() {
		var u User
		if err := rows.Scan(
			&u.ID, &u.Username, &u.FirstName, &u.LastName, &u.BirthDate, &u.Gender, &u.Interests, &u.City,
		); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) GetCredentialByUsername(ctx context.Context, username string) (Credential, error) {
	const q = `SELECT id, password_hash FROM users WHERE username = $1`

	t := s.readTargetFrom(ctx)
	c, err := scanCredential(t.pool.QueryRow(ctx, q, username))
	if s.fallbackNeeded(t, err) {
		s.masterFallbacks.Add(1)
		c, err = scanCredential(s.master.QueryRow(ctx, q, username))
	}
	return c, err
}

func scanCredential(row pgx.Row) (Credential, error) {
	var c Credential
	err := row.Scan(&c.ID, &c.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	return c, err
}

func (s *Store) CreateSession(ctx context.Context, token string, userID int64, ttl time.Duration) error {
	const q = `INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, $3)`

	_, err := s.master.Exec(ctx, q, token, userID, time.Now().Add(ttl))
	return err
}

func (s *Store) UserIDByToken(ctx context.Context, token string) (int64, error) {
	const q = `SELECT user_id FROM sessions WHERE token = $1 AND expires_at > now()`

	t := s.readTargetFrom(ctx)
	userID, err := scanUserID(t.pool.QueryRow(ctx, q, token))
	if s.fallbackNeeded(t, err) {
		s.masterFallbacks.Add(1)
		userID, err = scanUserID(s.master.QueryRow(ctx, q, token))
	}
	return userID, err
}

func scanUserID(row pgx.Row) (int64, error) {
	var userID int64
	err := row.Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return userID, err
}

// InsertWriteLoad appends one row to the lab write-load table. It always goes
// to the master.
func (s *Store) InsertWriteLoad(ctx context.Context, payload string) (int64, error) {
	const q = `INSERT INTO write_load (payload) VALUES ($1) RETURNING id`

	var id int64
	err := s.master.QueryRow(ctx, q, payload).Scan(&id)
	return id, err
}

// CountWriteLoad reads the write-load table from the master so the result
// reflects every acknowledged write.
func (s *Store) CountWriteLoad(ctx context.Context) (int64, error) {
	var count int64
	err := s.master.QueryRow(ctx, `SELECT count(*) FROM write_load`).Scan(&count)
	return count, err
}
