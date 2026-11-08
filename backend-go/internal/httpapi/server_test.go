package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/coordinator"
)

// newTestHandler builds the API with a coordinator whose store is never
// reached by the requests below (they fail before any I/O).
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := coordinator.New(nil, coordinator.DefaultConfig(), log)
	return New(svc, log, Options{
		AllowedOrigins: []string{"https://fe.example"}, BodyLimit: 64, RequestTimeout: time.Second, BuildID: "test",
		RegisterLimiter: NewLimiter(60, 2),
	})
}

func do(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type errorResp struct {
	Error struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
	RequestID string `json:"requestId"`
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) errorResp {
	t.Helper()
	var e errorResp
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q: %v", w.Body.String(), err)
	}
	return e
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	w := do(t, newTestHandler(t), "GET", "/healthz", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) || w.Header().Get("X-Request-Id") == "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("security headers missing")
	}
}

func TestCORS(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	w := do(t, h, "OPTIONS", "/api/v1/rooms", "", map[string]string{
		"Origin": "https://fe.example", "Access-Control-Request-Method": "POST",
	})
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "https://fe.example" ||
		!strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key") {
		t.Fatalf("preflight %d %v", w.Code, w.Header())
	}
	w = do(t, h, "POST", "/api/v1/players", `{"username":"x"}`, map[string]string{"Origin": "https://evil.example"})
	if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" || decodeErr(t, w).Error.Code != "FORBIDDEN" {
		t.Fatalf("foreign origin %d", w.Code)
	}
}

func TestAuthRequired(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/players/me"}, {"POST", "/api/v1/rooms"}, {"GET", "/api/v1/rooms/1234/state?epoch=x"},
		{"POST", "/api/v1/rooms/1234/commands"}, {"GET", "/api/v1/rooms/1234/games/current/role"},
	} {
		w := do(t, h, tc.method, tc.path, "", nil)
		if w.Code != 401 || decodeErr(t, w).Error.Code != "UNAUTHENTICATED" || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("%s %s -> %d", tc.method, tc.path, w.Code)
		}
	}
	w := do(t, h, "GET", "/api/v1/players/me", "", map[string]string{"Authorization": "Bearer not-a-real-token"})
	if w.Code != 401 {
		t.Fatalf("malformed bearer -> %d", w.Code)
	}
	// Logout is idempotent and does not reveal token validity.
	if w := do(t, h, "DELETE", "/api/v1/players/me", "", map[string]string{"Authorization": "Bearer nope"}); w.Code != 204 {
		t.Fatalf("logout invalid token -> %d", w.Code)
	}
}

func TestRegisterValidation(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	tests := []struct {
		name, body, code string
		status           int
	}{
		{"unknown field", `{"username":"a","admin":true}`, "INVALID_INPUT", 400},
		{"missing username", `{}`, "INVALID_INPUT", 400},
		{"not json", `username=a`, "INVALID_INPUT", 400},
		{"too large", `{"username":"` + strings.Repeat("a", 100) + `"}`, "PAYLOAD_TOO_LARGE", 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/players", strings.NewReader(tt.body))
			r.RemoteAddr = "10.0.0." + tt.name[:1] + ":1"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status || decodeErr(t, w).Error.Code != tt.code {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestRegisterRateLimit(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	var last *httptest.ResponseRecorder
	for range 3 {
		r := httptest.NewRequest("POST", "/api/v1/players", strings.NewReader(`{}`))
		r.RemoteAddr = "192.0.2.7:1234"
		last = httptest.NewRecorder()
		h.ServeHTTP(last, r)
	}
	if last.Code != 429 || last.Header().Get("Retry-After") == "" || !decodeErr(t, last).Error.Retryable {
		t.Fatalf("third request -> %d", last.Code)
	}
}

func TestUnknownRouteAndReadyzDisabled(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	if w := do(t, h, "GET", "/api/users/all", "", nil); w.Code != 404 {
		t.Fatalf("legacy route must not exist: %d", w.Code)
	}
	if w := do(t, h, "GET", "/readyz", "", nil); w.Code != 404 {
		t.Fatalf("readyz without OPS_HEALTH_TOKEN must be disabled: %d", w.Code)
	}
}

func TestLimiter(t *testing.T) {
	t.Parallel()
	l := NewLimiter(60, 2)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("1")
	}
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("2")
	}
	if ok, wait := l.Allow("k"); ok || wait <= 0 {
		t.Fatal("3 must be limited")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("refill")
	}
	if ok, _ := l.Allow("other"); !ok {
		t.Fatal("keys are independent")
	}
}

func TestClientIP(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.1.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if ClientIP(r, false) != "10.1.1.1" || ClientIP(r, true) != "203.0.113.9" {
		t.Fatal("proxy headers must be trusted only when configured")
	}
}
