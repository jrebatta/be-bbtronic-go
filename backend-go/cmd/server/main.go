// Command server is the BBtronic HTTP + WebSocket backend. It is the
// entrypoint detected by the Vercel Go preset (cmd/server/main.go) and
// listens on PORT. Start-up never deletes data, seeds or migrates (C1).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/app"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	log := app.NewLogger(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           a.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// No global Read/WriteTimeout: they would also hit hijacked
		// WebSockets. REST routes set per-request deadlines instead.
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("bbtronic listening", "port", cfg.Port, "env", cfg.AppEnv, "build", cfg.BuildID, "realtime", cfg.RealtimeMode)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		a.Close(context.Background())
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	a.Hub.Shutdown(shutdownCtx) // server.reconnect + close 1012 to every socket
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	a.Close(shutdownCtx)
	return nil
}
