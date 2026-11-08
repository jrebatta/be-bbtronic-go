// Package store is the PostgreSQL persistence of BBtronic (pgx/v5, explicit
// parameterized SQL, no ORM). It reuses the tables created by the Spring Boot
// application plus the additive columns of migrations/0001_go_runtime.sql.
//
// Lock order (to avoid deadlocks): game_session row first, then users rows in
// ascending ID. Lease renewals lock a single users row and nothing else.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig configures the connection pool for a serverless environment.
type PoolConfig struct {
	URL            string
	MaxConns       int32
	MinConns       int32
	ConnectTimeout time.Duration
	// ExecMode: "" (pgx default: cache_statement), "cache_describe", "describe_exec", "exec", "simple_protocol".
	ExecMode string
}

// Store wraps the pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open creates the pool lazily (MinConns=0 does not connect at start-up).
func Open(ctx context.Context, c PoolConfig) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(c.URL)
	if err != nil {
		// Never wrap the DSN parse error text: it may echo the URL.
		return nil, errors.New("invalid DATABASE_URL")
	}
	cfg.MaxConns = c.MaxConns
	cfg.MinConns = c.MinConns
	cfg.MaxConnLifetime = 10 * time.Minute
	cfg.MaxConnLifetimeJitter = time.Minute
	cfg.MaxConnIdleTime = time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	if c.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = c.ConnectTimeout
	}
	switch c.ExecMode {
	case "":
	case "cache_statement":
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	case "cache_describe":
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	case "describe_exec":
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	case "exec":
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	case "simple_protocol":
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	default:
		return nil, fmt.Errorf("unknown PG_QUERY_EXEC_MODE %q", c.ExecMode)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks connectivity (readiness).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool exposes the pool to the admin tooling.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Tx is a transaction handle carrying all repository methods.
type Tx struct {
	tx pgx.Tx
}

// CommitThenFail makes InTx commit the work done so far and then return Err.
// Used when a reconciliation (deadline, expiry) must be confirmed even though
// the incoming command is rejected (plan 5.3 step 5).
type CommitThenFail struct{ Err error }

func (e *CommitThenFail) Error() string { return e.Err.Error() }
func (e *CommitThenFail) Unwrap() error { return e.Err }

const maxTxAttempts = 3

// InTx runs fn in a READ COMMITTED transaction, retrying the whole function on
// serialization failures and deadlocks (40001, 40P01).
func (s *Store) InTx(ctx context.Context, fn func(*Tx) error) error {
	return s.run(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
}

// ReadTx runs fn in a short REPEATABLE READ read-only transaction so a
// snapshot spanning several tables is consistent (plan 8.2).
func (s *Store) ReadTx(ctx context.Context, fn func(*Tx) error) error {
	return s.run(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

func (s *Store) run(ctx context.Context, opts pgx.TxOptions, fn func(*Tx) error) error {
	var err error
	for attempt := 1; attempt <= maxTxAttempts; attempt++ {
		err = s.once(ctx, opts, fn)
		if !retryable(err) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Duration(attempt*attempt) * 10 * time.Millisecond)
	}
	return err
}

func (s *Store) once(ctx context.Context, opts pgx.TxOptions, fn func(*Tx) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("%w: begin: %w", ErrUnavailable, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx)) // rollback error is irrelevant once err is set
		}
	}()
	fnErr := fn(&Tx{tx: tx})
	var ctf *CommitThenFail
	if fnErr != nil && !errors.As(fnErr, &ctf) {
		return fnErr
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		// A failed COMMIT has an unknown outcome for the caller; idempotency
		// receipts make a retry with the same key safe.
		return fmt.Errorf("%w: commit: %w", ErrCommitUnknown, cerr)
	}
	if ctf != nil {
		return ctf.Err
	}
	return nil
}

// Sentinel infrastructure errors.
var (
	ErrUnavailable   = errors.New("database unavailable")
	ErrCommitUnknown = errors.New("commit outcome unknown")
	ErrNotFound      = errors.New("not found")
)

func retryable(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (pg.Code == "40001" || pg.Code == "40P01")
}

// IsUniqueViolation reports SQLSTATE 23505.
func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// Savepoint runs fn inside a SAVEPOINT; on error the savepoint is rolled back
// and the outer transaction stays usable.
func (t *Tx) Savepoint(ctx context.Context, fn func() error) error {
	sp, err := t.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	if err := fn(); err != nil {
		if rerr := sp.Rollback(ctx); rerr != nil {
			return errors.Join(err, fmt.Errorf("rollback to savepoint: %w", rerr))
		}
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}
