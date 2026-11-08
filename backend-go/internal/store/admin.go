package store

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// AdminPlayer is an inventory row (A1 / Java GET /api/users/all) without
// any credential.
type AdminPlayer struct {
	ID        int64
	Username  string
	RoomCode  *string
	Ready     bool
	CreatedAt *time.Time
	Online    bool
}

// ListPlayers returns the player inventory; onlineOnly keeps players with a
// valid lease (Java /online listed everybody, C2/F03).
func (s *Store) ListPlayers(ctx context.Context, onlineOnly bool, now time.Time) ([]AdminPlayer, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id, u.username, g.session_code, u.ready, u.created_at,
			EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(u.connection_leases, '[]'::jsonb)) l
			        WHERE (l->>'until')::timestamptz > $1)
		FROM users u LEFT JOIN game_session g ON g.id = u.game_session_id
		ORDER BY u.id`, now)
	if err != nil {
		return nil, fmt.Errorf("listing players: %w", err)
	}
	defer rows.Close()
	var out []AdminPlayer
	for rows.Next() {
		var p AdminPlayer
		if err := rows.Scan(&p.ID, &p.Username, &p.RoomCode, &p.Ready, &p.CreatedAt, &p.Online); err != nil {
			return nil, fmt.Errorf("scanning player: %w", err)
		}
		if !onlineOnly || p.Online {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// CheckReport summarizes whether a database is ready for the Go backend.
type CheckReport struct {
	ServerVersion    string
	MissingTables    []string
	MigrationApplied bool
	Counts           map[string]int64
}

// requiredTables are the Spring Boot tables the Go backend reuses.
var requiredTables = []string{"game_session", "users", "question", "shown_questions",
	"yo_nunca_nunca", "cultura_pendeja", "quien_es_mas_probable", "preguntas_incomodas",
	"impostor_word", "waiting_messages"}

// Check is a read-only readiness diagnosis: connectivity, Java tables,
// migration status and catalog sizes.
func (s *Store) Check(ctx context.Context) (CheckReport, error) {
	r := CheckReport{Counts: map[string]int64{}}
	if err := s.pool.QueryRow(ctx, `SHOW server_version`).Scan(&r.ServerVersion); err != nil {
		return r, fmt.Errorf("connecting: %w", err)
	}
	for _, t := range requiredTables {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, t).Scan(&exists); err != nil {
			return r, fmt.Errorf("checking %s: %w", t, err)
		}
		if !exists {
			r.MissingTables = append(r.MissingTables, t)
			continue
		}
		var n int64
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+t).Scan(&n); err != nil { // t from constant list
			return r, fmt.Errorf("counting %s: %w", t, err)
		}
		r.Counts[t] = n
	}
	var hasTable bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('go_schema_migrations') IS NOT NULL`).Scan(&hasTable); err != nil {
		return r, fmt.Errorf("checking migrations: %w", err)
	}
	if hasTable {
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM go_schema_migrations WHERE version = '0001_go_runtime')`).
			Scan(&r.MigrationApplied); err != nil {
			return r, fmt.Errorf("checking migrations: %w", err)
		}
	}
	return r, nil
}

// InspectSchema prints the read-only catalog queries of plan 8.1. The body of
// reset_game_data is printed only when showRoutine is set (it may contain
// sensitive logic and must be reviewed before being versioned).
func (s *Store) InspectSchema(ctx context.Context, w io.Writer, showRoutine bool) error {
	sections := []struct {
		title string
		sql   string
	}{
		{"server", `SELECT version(), current_setting('server_encoding'), current_setting('TimeZone'),
			(SELECT datcollate FROM pg_database WHERE datname = current_database())`},
		{"columns", `SELECT table_schema, table_name, column_name, data_type, udt_name, is_nullable,
			COALESCE(column_default, ''), COALESCE(character_maximum_length::text, '')
			FROM information_schema.columns WHERE table_schema NOT IN ('pg_catalog','information_schema')
			ORDER BY table_schema, table_name, ordinal_position`},
		{"constraints", `SELECT n.nspname, c.relname, con.conname, con.contype::text, pg_get_constraintdef(con.oid)
			FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname NOT IN ('pg_catalog','information_schema') ORDER BY 1, 2, 3`},
		{"indexes", `SELECT schemaname, tablename, indexname, indexdef FROM pg_indexes
			WHERE schemaname NOT IN ('pg_catalog','information_schema') ORDER BY 1, 2, 3`},
		{"row counts (estimates)", `SELECT n.nspname, c.relname, c.reltuples::bigint FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind = 'r'
			AND n.nspname NOT IN ('pg_catalog','information_schema') ORDER BY 1, 2`},
		{"triggers", `SELECT event_object_table, trigger_name, action_timing, event_manipulation
			FROM information_schema.triggers ORDER BY 1, 2`},
		{"reset_game_data", `SELECT n.nspname, p.proname, p.prokind::text, pg_get_function_identity_arguments(p.oid)
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE p.proname = 'reset_game_data'`},
	}
	if showRoutine {
		sections = append(sections, struct {
			title string
			sql   string
		}{"reset_game_data definition", `SELECT pg_get_functiondef(p.oid) FROM pg_proc p WHERE p.proname = 'reset_game_data'`})
	}
	for _, sec := range sections {
		fmt.Fprintf(w, "\n## %s\n", sec.title)
		rows, err := s.pool.Query(ctx, sec.sql)
		if err != nil {
			return fmt.Errorf("inspecting %s: %w", sec.title, err)
		}
		n := 0
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				rows.Close()
				return fmt.Errorf("reading %s: %w", sec.title, err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				parts[i] = fmt.Sprint(v)
			}
			fmt.Fprintln(w, strings.Join(parts, " | "))
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating %s: %w", sec.title, err)
		}
		if n == 0 {
			fmt.Fprintln(w, "(no rows)")
		}
	}
	return nil
}
