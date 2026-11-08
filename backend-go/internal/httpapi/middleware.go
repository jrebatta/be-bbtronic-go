package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID returns the request ID stored in ctx.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Hijack/deadlines (WebSocket upgrade).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// withObservability adds a request ID, recovers panics and logs one line per
// request (method, route pattern, status, latency) — never bodies, query
// strings or Authorization.
func withObservability(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-Id", id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				log.ErrorContext(ctx, "panic in handler", "requestId", id, "panic", p, "stack", string(debug.Stack()))
				writeError(rec, r, errInternal)
			}
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			log.InfoContext(ctx, "http request", "requestId", id, "method", r.Method, "route", route,
				"status", rec.status, "durationMs", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}

// withSecurityHeaders sets defensive headers on API responses.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// withCORS implements an exact Origin allowlist. Requests carrying a
// non-allowed Origin are rejected (defense in depth; CORS is not auth).
// Requests without Origin (non-browser clients) still need a bearer token.
func withCORS(allowed map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Origin")
		if !allowed[origin] {
			writeError(w, r, errOriginDenied)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withRESTLimits applies body size and per-request deadlines to REST routes
// only (never to the WebSocket route: hijacked connections manage their own).
func withRESTLimits(bodyLimit int64, timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(timeout)) // unsupported writers ignore it
		_ = rc.SetWriteDeadline(time.Now().Add(timeout + 2*time.Second))
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ClientIP returns the client address. Proxy headers are trusted only when
// the deployment platform sets them (TRUST_PROXY_HEADERS, e.g. on Vercel).
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
