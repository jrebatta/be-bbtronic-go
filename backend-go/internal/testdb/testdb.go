// Package testdb provisions throwaway PostgreSQL databases for integration
// tests. It refuses any non-local server so tests can never touch Neon or a
// production host, creates a uniquely named database per call, applies the
// inferred Spring Boot baseline plus the real Go migrations, and drops the
// database at cleanup.
//
// Configure with BBTRONIC_TEST_DATABASE_URL, e.g.
//
//	postgres://bbtronic_test@127.0.0.1:55432/postgres?sslmode=disable
package testdb

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/store"
)

//go:embed baseline_hibernate_inferred.sql
var baseline string

// EnvVar names the admin URL of the local test server.
const EnvVar = "BBTRONIC_TEST_DATABASE_URL"

// DB is a provisioned test database.
type DB struct {
	Store *store.Store
	URL   string
}

// New creates an isolated database or skips the test when not configured.
func New(t testing.TB) *DB {
	t.Helper()
	admin := os.Getenv(EnvVar)
	if admin == "" {
		t.Skipf("%s not set: integration test skipped", EnvVar)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("%s is not a URL", EnvVar)
	}
	if h := u.Hostname(); !localHost(h) {
		t.Fatalf("%s must point to a local server, got host %q: refusing to run", EnvVar, h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "bbt_test_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connecting to test server: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE template0 ENCODING 'UTF8'"); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	_ = conn.Close(ctx)

	dbURL := *u
	dbURL.Path = "/" + name
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		c, err := pgx.Connect(cctx, admin)
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer c.Close(cctx)
		if _, err := c.Exec(cctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("dropping %s: %v", name, err)
		}
	})

	c, err := pgx.Connect(ctx, dbURL.String())
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	if _, err := c.Exec(ctx, baseline); err != nil {
		t.Fatalf("applying inferred baseline: %v", err)
	}
	_ = c.Close(ctx)

	st, err := store.Open(ctx, store.PoolConfig{URL: dbURL.String(), MaxConns: 20, ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx, func(string, ...any) {}); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return &DB{Store: st, URL: dbURL.String()}
}

// localHost accepts loopback hosts, plus private RFC 1918 addresses only when
// BBTRONIC_TEST_DB_ALLOW_PRIVATE=1 (e.g. a test cluster reached from WSL).
func localHost(h string) bool {
	if h == "127.0.0.1" || h == "localhost" || h == "::1" {
		return true
	}
	if os.Getenv("BBTRONIC_TEST_DB_ALLOW_PRIVATE") != "1" {
		return false
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsPrivate()
}

// Exec runs SQL on the test database (fixtures).
func (d *DB) Exec(t testing.TB, sql string, args ...any) {
	t.Helper()
	if _, err := d.Store.Pool().Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture SQL failed: %v", err)
	}
}

// Count returns SELECT count(*) of a query.
func (d *DB) Count(t testing.TB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := d.Store.Pool().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	return n
}
