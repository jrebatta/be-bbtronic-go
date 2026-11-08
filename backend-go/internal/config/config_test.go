package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoad(t *testing.T) {
	t.Parallel()
	base := func() map[string]string {
		return map[string]string{"APP_ENV": "development", "DATABASE_URL": "postgres://u@127.0.0.1:5432/db?sslmode=disable"}
	}
	tests := []struct {
		name    string
		mutate  func(m map[string]string)
		wantErr string
	}{
		{name: "defaults", mutate: func(map[string]string) {}},
		{name: "missing env", mutate: func(m map[string]string) { delete(m, "APP_ENV") }, wantErr: "APP_ENV is required"},
		{name: "missing database", mutate: func(m map[string]string) { delete(m, "DATABASE_URL") }, wantErr: "DATABASE_URL is required"},
		{name: "jdbc url rejected", mutate: func(m map[string]string) { m["DATABASE_URL"] = "jdbc:postgresql://h/db" }, wantErr: "postgres:// URL"},
		{name: "production requires verify-full", mutate: func(m map[string]string) {
			m["APP_ENV"] = "production"
			m["DATABASE_URL"] = "postgres://u:secretpw@db.example/db?sslmode=require"
		}, wantErr: "verify-full"},
		{name: "bad origin", mutate: func(m map[string]string) { m["ALLOWED_ORIGINS"] = "https://a.example/path" }, wantErr: "ALLOWED_ORIGINS"},
		{name: "http origin in production", mutate: func(m map[string]string) {
			m["APP_ENV"] = "production"
			m["DATABASE_URL"] = "postgres://u@db.example/db?sslmode=verify-full"
			m["ALLOWED_ORIGINS"] = "http://fe.example"
		}, wantErr: "https"},
		{name: "renew must be below lease", mutate: func(m map[string]string) { m["PRESENCE_RENEW_SECONDS"] = "60" }, wantErr: "PRESENCE_RENEW_SECONDS"},
		{name: "out of range", mutate: func(m map[string]string) { m["PG_MAX_CONNS"] = "0" }, wantErr: "PG_MAX_CONNS"},
		{name: "short ops token", mutate: func(m map[string]string) { m["OPS_HEALTH_TOKEN"] = "short" }, wantErr: "OPS_HEALTH_TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := base()
			tt.mutate(m)
			c, err := Load(env(m))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if c.WSRotate != 270*time.Second || c.PGMaxConns != 2 || c.PresenceLease != 45*time.Second || c.DisconnectGrace != 20*time.Second {
					t.Fatalf("defaults %+v", c)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "secretpw") {
				t.Fatal("errors must never echo secret values")
			}
		})
	}
}

func TestLoad_Origins(t *testing.T) {
	t.Parallel()
	c, err := Load(env(map[string]string{
		"APP_ENV": "development", "DATABASE_URL": "postgres://u@localhost/db",
		"ALLOWED_ORIGINS": " https://fe-bbtronic-vue.vercel.app , http://localhost:5173 ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedOrigins) != 2 || c.AllowedOrigins[0] != "https://fe-bbtronic-vue.vercel.app" {
		t.Fatalf("origins %v", c.AllowedOrigins)
	}
}
