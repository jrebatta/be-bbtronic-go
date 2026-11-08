// Package app wires the components (explicit constructor injection, no DI
// framework) for the server binary and for end-to-end tests.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/config"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/coordinator"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/httpapi"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/realtime"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/store"
)

// App is a fully wired backend.
type App struct {
	Store       *store.Store
	Coordinator *coordinator.Service
	Hub         *realtime.Hub
	Handler     http.Handler
}

// NewLogger returns a JSON slog logger on stdout (Vercel captures stdout).
func NewLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// New builds the application. The pool connects lazily (MinConns=0).
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	st, err := store.Open(ctx, store.PoolConfig{
		URL: cfg.DatabaseURL, MaxConns: cfg.PGMaxConns, MinConns: cfg.PGMinConns,
		ConnectTimeout: cfg.PGConnectTO, ExecMode: cfg.PGExecMode,
	})
	if err != nil {
		return nil, err
	}
	return Wire(st, cfg, log), nil
}

// Wire assembles the components around an existing store.
func Wire(st *store.Store, cfg config.Config, log *slog.Logger) *App {
	return WireWith(st, cfg, log, nil)
}

// WireWith is Wire with a hook to adjust the realtime configuration (tests).
func WireWith(st *store.Store, cfg config.Config, log *slog.Logger, tune func(*realtime.Config)) *App {
	ccfg := coordinator.DefaultConfig()
	ccfg.TokenTTL = cfg.TokenTTL
	ccfg.DisconnectGrace = cfg.DisconnectGrace
	ccfg.LeaseTTL = cfg.PresenceLease
	ccfg.RoomIdle = cfg.RoomIdle
	ccfg.ReceiptRetention = cfg.ReceiptRetention
	ccfg.MaxConnectionsPerPlayer = cfg.MaxConnsPerUser
	ccfg.MaxRoomMembers = cfg.MaxRoomMembers
	svc := coordinator.New(st, ccfg, log)

	rcfg := realtime.DefaultConfig()
	rcfg.AllowedOrigins = cfg.AllowedOrigins
	rcfg.FrameLimit = cfg.WSFrameLimit
	rcfg.RotateAfter = cfg.WSRotate
	rcfg.LeaseRenew = cfg.PresenceRenew
	rcfg.HeartbeatTimeout = cfg.PresenceLease
	rcfg.PollActive = cfg.PollActive
	rcfg.PollLobby = cfg.PollLobby
	if tune != nil {
		tune(&rcfg)
	}
	hub := realtime.NewHub(svc, rcfg, log)

	opts := httpapi.Options{
		AllowedOrigins: cfg.AllowedOrigins, BodyLimit: cfg.HTTPBodyLimit, RequestTimeout: cfg.PGQueryTO * 2,
		OpsHealthToken: cfg.OpsHealthToken, TrustProxy: cfg.TrustProxy, BuildID: cfg.BuildID,
		RegisterLimiter: httpapi.NewLimiter(10, 10),
		CommandLimiter:  httpapi.NewLimiter(600, 40),
	}
	if cfg.RealtimeMode == "websocket" {
		wsLimiter := httpapi.NewLimiter(30, 20)
		opts.Realtime = hub.Handler(func(r *http.Request) bool {
			ok, _ := wsLimiter.Allow(httpapi.ClientIP(r, cfg.TrustProxy))
			return ok
		})
	}
	return &App{Store: st, Coordinator: svc, Hub: hub, Handler: httpapi.New(svc, log, opts)}
}

// Close releases the pool.
func (a *App) Close(context.Context) { a.Store.Close() }
