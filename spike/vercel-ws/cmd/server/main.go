// Command server is the minimal Vercel feasibility spike for BBtronic:
// net/http + github.com/coder/websocket on the official Go preset.
//
// Endpoints:
//
//	GET /healthz  JSON with instance identity (no secrets)
//	GET /ws       WebSocket echo; text frames are echoed back as JSON
//
// WebSocket text commands: "info" returns instance stats; "broadcast:<text>"
// fans out to every socket held by this same instance; anything else is echoed.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

const readLimit = 16 << 10 // 16 KiB, the planned WS_FRAME_LIMIT

var (
	instanceID = randomHex(6)
	startedAt  = time.Now()
)

type server struct {
	log            *slog.Logger
	allowedOrigins map[string]bool
	originHosts    []string

	mu    sync.Mutex
	conns map[string]*websocket.Conn

	total atomic.Int64
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s := &server{log: log, conns: map[string]*websocket.Conn{}, allowedOrigins: map[string]bool{}}
	for _, o := range strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			log.Error("invalid ALLOWED_ORIGINS entry ignored", "entry", o)
			continue
		}
		s.allowedOrigins[u.Scheme+"://"+u.Host] = true
		s.originHosts = append(s.originHosts, u.Host)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /{$}", s.health)
	mux.HandleFunc("GET /ws", s.ws)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Info("shutdown signal received", "instance", instanceID, "activeConns", s.active())
		s.closeAll(websocket.StatusServiceRestart, "server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("http shutdown", "err", err)
		}
	}()

	log.Info("spike listening",
		"instance", instanceID, "port", port, "go", runtime.Version(),
		"region", os.Getenv("VERCEL_REGION"), "env", os.Getenv("VERCEL_ENV"),
		"originsConfigured", len(s.allowedOrigins))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"instanceId":  instanceID,
		"uptimeMs":    time.Since(startedAt).Milliseconds(),
		"goVersion":   runtime.Version(),
		"region":      os.Getenv("VERCEL_REGION"),
		"vercelEnv":   os.Getenv("VERCEL_ENV"),
		"commit":      os.Getenv("VERCEL_GIT_COMMIT_SHA"),
		"activeConns": s.active(),
		"totalConns":  s.total.Load(),
		"serverTime":  time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (s *server) ws(w http.ResponseWriter, r *http.Request) {
	// Exact Origin allowlist (scheme+host). Requests without Origin
	// (non-browser clients) are accepted; browsers always send it.
	if origin := r.Header.Get("Origin"); origin != "" && !s.allowedOrigins[origin] {
		s.log.Warn("origin rejected", "origin", origin)
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  s.originHosts,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Error("websocket accept failed", "err", err)
		return
	}
	c.SetReadLimit(readLimit)

	id := randomHex(4)
	opened := time.Now()
	s.mu.Lock()
	s.conns[id] = c
	s.mu.Unlock()
	s.total.Add(1)
	s.log.Info("ws open", "instance", instanceID, "conn", id, "active", s.active())

	// The hijacked connection outlives r.Context() semantics on some
	// platforms; use our own context bound to the connection lifetime.
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.conns, id)
		s.mu.Unlock()
		s.log.Info("ws closed", "instance", instanceID, "conn", id,
			"durationMs", time.Since(opened).Milliseconds(), "active", s.active())
	}()

	s.send(ctx, c, map[string]any{"type": "hello", "instanceId": instanceID, "connId": id})
	var seq int64
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			s.log.Info("ws read ended", "conn", id, "status", websocket.CloseStatus(err), "err", err.Error())
			return
		}
		if typ != websocket.MessageText {
			_ = c.Close(websocket.StatusUnsupportedData, "text frames only")
			return
		}
		seq++
		msg := string(data)
		switch {
		case msg == "info":
			s.send(ctx, c, map[string]any{"type": "info", "instanceId": instanceID, "active": s.active(),
				"connAgeMs": time.Since(opened).Milliseconds(), "seq": seq})
		case strings.HasPrefix(msg, "broadcast:"):
			n := s.broadcast(ctx, map[string]any{"type": "broadcast", "from": id, "data": strings.TrimPrefix(msg, "broadcast:"), "instanceId": instanceID})
			s.send(ctx, c, map[string]any{"type": "broadcast.ack", "delivered": n, "seq": seq})
		default:
			s.send(ctx, c, map[string]any{"type": "echo", "data": msg, "instanceId": instanceID, "connId": id, "seq": seq})
		}
	}
}

func (s *server) send(ctx context.Context, c *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Write(wctx, websocket.MessageText, b); err != nil {
		s.log.Warn("ws write failed", "err", err.Error())
	}
}

func (s *server) broadcast(ctx context.Context, v any) int {
	s.mu.Lock()
	targets := make([]*websocket.Conn, 0, len(s.conns))
	for _, c := range s.conns {
		targets = append(targets, c)
	}
	s.mu.Unlock()
	for _, c := range targets {
		s.send(ctx, c, v)
	}
	return len(targets)
}

func (s *server) closeAll(code websocket.StatusCode, reason string) {
	s.mu.Lock()
	targets := make([]*websocket.Conn, 0, len(s.conns))
	for _, c := range s.conns {
		targets = append(targets, c)
	}
	s.mu.Unlock()
	for _, c := range targets {
		_ = c.Close(code, reason)
	}
}

func (s *server) active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
