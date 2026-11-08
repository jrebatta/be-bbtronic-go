package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is an embedded, versioned SQL file.
type Migration struct {
	Version string
	SQL     string
}

// Migrations returns the embedded migrations in order.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", e.Name(), err)
		}
		out = append(out, Migration{Version: strings.TrimSuffix(e.Name(), ".sql"), SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// migrationLockKey serializes concurrent migrators (pg_advisory_xact_lock).
const migrationLockKey = 0x0bb7_2026

// Migrate applies pending migrations, each in its own transaction. It is only
// called by the admin tool after explicit confirmation — never at start-up.
func (s *Store) Migrate(ctx context.Context, log func(format string, args ...any)) ([]string, error) {
	ms, err := Migrations()
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS go_schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("creating go_schema_migrations: %w", err)
	}
	var applied []string
	for _, m := range ms {
		done := false
		err := s.InTx(ctx, func(tx *Tx) error {
			if _, err := tx.tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
				return fmt.Errorf("advisory lock: %w", err)
			}
			var exists bool
			if err := tx.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM go_schema_migrations WHERE version = $1)`, m.Version).Scan(&exists); err != nil {
				return fmt.Errorf("checking migration: %w", err)
			}
			if exists {
				done = true
				return nil
			}
			if _, err := tx.tx.Exec(ctx, m.SQL); err != nil {
				return fmt.Errorf("applying %s: %w", m.Version, err)
			}
			_, err := tx.tx.Exec(ctx, `INSERT INTO go_schema_migrations (version) VALUES ($1)`, m.Version)
			return err
		})
		if err != nil {
			return applied, err
		}
		if done {
			log("migration %s already applied", m.Version)
			continue
		}
		log("migration %s applied", m.Version)
		applied = append(applied, m.Version)
	}
	return applied, nil
}
