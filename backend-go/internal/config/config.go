// Package config loads and validates the environment (plan 9.4). Errors name
// the offending variable, never its value; secrets are never logged.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the validated runtime configuration.
type Config struct {
	AppEnv         string
	Port           string
	DatabaseURL    string
	PGMaxConns     int32
	PGMinConns     int32
	PGConnectTO    time.Duration
	PGQueryTO      time.Duration
	PGExecMode     string
	AllowedOrigins []string
	TokenTTL       time.Duration
	OpsHealthToken string
	RealtimeMode   string

	PollActive       time.Duration
	PollLobby        time.Duration
	WSRotate         time.Duration
	PresenceRenew    time.Duration
	PresenceLease    time.Duration
	DisconnectGrace  time.Duration
	RoomIdle         time.Duration
	ReceiptRetention time.Duration
	MaxConnsPerUser  int
	MaxRoomMembers   int
	HTTPBodyLimit    int64
	WSFrameLimit     int64
	TrustProxy       bool
	LogLevel         string
	BuildID          string
}

// Environments.
const (
	EnvDevelopment = "development"
	EnvTest        = "test"
	EnvPreview     = "preview"
	EnvProduction  = "production"
)

// Load reads the configuration from getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	str := func(name, def string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return def
	}
	integer := func(name string, def, lo, hi int64) int64 {
		v := getenv(name)
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < lo || n > hi {
			errs = append(errs, fmt.Errorf("%s must be an integer in [%d, %d]", name, lo, hi))
			return def
		}
		return n
	}
	seconds := func(name string, def, lo, hi int64) time.Duration {
		return time.Duration(integer(name, def, lo, hi)) * time.Second
	}
	millis := func(name string, def, lo, hi int64) time.Duration {
		return time.Duration(integer(name, def, lo, hi)) * time.Millisecond
	}

	c := Config{
		AppEnv:           str("APP_ENV", ""),
		Port:             str("PORT", "8080"),
		DatabaseURL:      getenv("DATABASE_URL"),
		PGMaxConns:       int32(integer("PG_MAX_CONNS", 2, 1, 50)),
		PGMinConns:       int32(integer("PG_MIN_CONNS", 0, 0, 10)),
		PGConnectTO:      seconds("PG_CONNECT_TIMEOUT", 5, 1, 60),
		PGQueryTO:        seconds("PG_QUERY_TIMEOUT", 5, 1, 60),
		PGExecMode:       str("PG_QUERY_EXEC_MODE", ""),
		TokenTTL:         seconds("AUTH_TOKEN_TTL_SECONDS", 86400, 300, 30*86400),
		OpsHealthToken:   getenv("OPS_HEALTH_TOKEN"),
		RealtimeMode:     str("REALTIME_MODE", "websocket"),
		PollActive:       millis("STATE_POLL_ACTIVE_MS", 2000, 250, 60000),
		PollLobby:        millis("STATE_POLL_LOBBY_MS", 5000, 250, 120000),
		WSRotate:         seconds("WS_ROTATE_SECONDS", 270, 30, 3600),
		PresenceRenew:    seconds("PRESENCE_RENEW_SECONDS", 15, 5, 300),
		PresenceLease:    seconds("PRESENCE_LEASE_SECONDS", 45, 10, 900),
		DisconnectGrace:  seconds("DISCONNECT_GRACE_SECONDS", 20, 0, 600),
		RoomIdle:         seconds("ROOM_IDLE_SECONDS", 7200, 60, 7*86400),
		ReceiptRetention: time.Duration(integer("RECEIPT_RETENTION_HOURS", 24, 1, 24*30)) * time.Hour,
		MaxConnsPerUser:  int(integer("MAX_CONNECTIONS_PER_PLAYER", 4, 1, 20)),
		MaxRoomMembers:   int(integer("MAX_ROOM_MEMBERS", 30, 2, 200)),
		HTTPBodyLimit:    integer("HTTP_BODY_LIMIT", 64<<10, 1<<10, 1<<20),
		WSFrameLimit:     integer("WS_FRAME_LIMIT", 16<<10, 1<<10, 1<<20),
		TrustProxy:       str("TRUST_PROXY_HEADERS", "false") == "true",
		LogLevel:         str("LOG_LEVEL", "info"),
		BuildID:          str("BUILD_ID", str("VERCEL_GIT_COMMIT_SHA", "dev")),
	}

	switch c.AppEnv {
	case EnvDevelopment, EnvTest, EnvPreview, EnvProduction:
	case "":
		errs = append(errs, errors.New("APP_ENV is required (development|test|preview|production)"))
	default:
		errs = append(errs, errors.New("APP_ENV must be development|test|preview|production"))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	} else if err := checkDSN(c.DatabaseURL, c.AppEnv); err != nil {
		errs = append(errs, err)
	}
	if c.PGMinConns > c.PGMaxConns {
		errs = append(errs, errors.New("PG_MIN_CONNS must not exceed PG_MAX_CONNS"))
	}
	if c.RealtimeMode != "websocket" && c.RealtimeMode != "polling" {
		errs = append(errs, errors.New("REALTIME_MODE must be websocket|polling"))
	}
	if c.PresenceRenew >= c.PresenceLease {
		errs = append(errs, errors.New("PRESENCE_RENEW_SECONDS must be lower than PRESENCE_LEASE_SECONDS"))
	}
	origins, err := parseOrigins(getenv("ALLOWED_ORIGINS"), c.AppEnv)
	if err != nil {
		errs = append(errs, err)
	}
	c.AllowedOrigins = origins
	if t := c.OpsHealthToken; t != "" && len(t) < 32 {
		errs = append(errs, errors.New("OPS_HEALTH_TOKEN must have at least 32 characters"))
	}
	return c, errors.Join(errs...)
}

// FromEnv loads from the process environment.
func FromEnv() (Config, error) { return Load(os.Getenv) }

// checkDSN enforces verified TLS outside development/test (plan 8.2) without
// ever echoing the DSN.
func checkDSN(dsn, env string) error {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return errors.New("DATABASE_URL must be a postgres:// URL")
	}
	if env == EnvPreview || env == EnvProduction {
		if mode := u.Query().Get("sslmode"); mode != "verify-full" {
			return errors.New("DATABASE_URL must use sslmode=verify-full in preview/production")
		}
	}
	return nil
}

func parseOrigins(raw, env string) ([]string, error) {
	var out []string
	for o := range strings.SplitSeq(raw, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || u.Host == "" || u.Path != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("ALLOWED_ORIGINS must be a comma-separated list of scheme://host[:port] origins")
		}
		if u.Scheme == "http" && (env == EnvProduction || env == EnvPreview) && u.Hostname() != "localhost" {
			return nil, errors.New("ALLOWED_ORIGINS must use https outside development")
		}
		out = append(out, u.Scheme+"://"+u.Host)
	}
	return out, nil
}
