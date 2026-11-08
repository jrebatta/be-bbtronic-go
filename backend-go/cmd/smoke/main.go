// Command smoke is the deployment test client (plan 9.2 / phase 8). It checks
// a running backend through its public contract only.
//
//	go run ./cmd/smoke -base https://<deployment>            # no database needed
//	go run ./cmd/smoke -base https://<deployment> -full      # needs a test DB behind the deployment
//
// A Vercel "Protection Bypass for Automation" secret may be provided via
// VERCEL_AUTOMATION_BYPASS_SECRET (sent as a header, never printed).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type smoke struct {
	base, origin, bypass string
	failures             int
}

func main() {
	base := flag.String("base", "http://localhost:8080", "deployment base URL")
	origin := flag.String("origin", "https://fe-bbtronic-vue.vercel.app", "an allowed Origin")
	full := flag.Bool("full", false, "run the database-backed flow (register, room, WS snapshot, command)")
	flag.Parse()
	s := &smoke{base: strings.TrimRight(*base, "/"), origin: *origin, bypass: os.Getenv("VERCEL_AUTOMATION_BYPASS_SECRET")}
	fmt.Printf("# smoke %s base=%s full=%t\n", time.Now().UTC().Format(time.RFC3339), s.base, *full)

	s.httpChecks()
	s.wsChecks()
	if *full {
		s.fullFlow()
	}
	fmt.Printf("# done failures=%d\n", s.failures)
	if s.failures > 0 {
		os.Exit(1)
	}
}

func (s *smoke) check(name string, ok bool, format string, args ...any) {
	st := "PASS"
	if !ok {
		st = "FAIL"
		s.failures++
	}
	fmt.Printf("%s %-26s %s\n", st, name, fmt.Sprintf(format, args...))
}

func (s *smoke) headers(h http.Header) http.Header {
	if h == nil {
		h = http.Header{}
	}
	if s.bypass != "" {
		h.Set("x-vercel-protection-bypass", s.bypass)
	}
	return h
}

func (s *smoke) req(method, path string, body any, h http.Header) (int, http.Header, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, method, s.base+path, rd)
	r.Header = s.headers(h)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return 0, nil, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Header, b
}

func errCode(b []byte) string {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	return e.Error.Code
}

func (s *smoke) httpChecks() {
	st, h, b := s.req("GET", "/healthz", nil, nil)
	s.check("http.healthz", st == 200 && bytes.Contains(b, []byte(`"ok"`)), "status=%d body=%s x-vercel-id=%q", st, bytes.TrimSpace(b), h.Get("x-vercel-id"))
	s.check("http.security-headers", h.Get("X-Content-Type-Options") == "nosniff" && h.Get("Cache-Control") == "no-store" && h.Get("X-Request-Id") != "",
		"nosniff=%q cache=%q requestId=%t", h.Get("X-Content-Type-Options"), h.Get("Cache-Control"), h.Get("X-Request-Id") != "")
	st, _, b = s.req("GET", "/api/v1/players/me", nil, nil)
	s.check("http.unauthenticated", st == 401 && errCode(b) == "UNAUTHENTICATED", "status=%d code=%s", st, errCode(b))
	st, _, b = s.req("GET", "/api/users/all", nil, nil)
	s.check("http.legacy-route-absent", st == 404, "GET /api/users/all status=%d code=%s", st, errCode(b))
	pre := http.Header{"Origin": {s.origin}, "Access-Control-Request-Method": {"POST"}}
	st, h, _ = s.req("OPTIONS", "/api/v1/rooms", nil, pre)
	s.check("http.cors-preflight", st == 204 && h.Get("Access-Control-Allow-Origin") == s.origin, "status=%d allow-origin=%q", st, h.Get("Access-Control-Allow-Origin"))
	st, _, b = s.req("POST", "/api/v1/players", map[string]string{"username": "x"}, http.Header{"Origin": {"https://evil.example"}})
	s.check("http.foreign-origin", st == 403 && errCode(b) == "FORBIDDEN", "status=%d code=%s", st, errCode(b))
	st, _, b = s.req("POST", "/api/v1/players", map[string]any{"username": "x", "admin": true}, nil)
	s.check("http.strict-json", st == 400 && errCode(b) == "INVALID_INPUT", "status=%d code=%s", st, errCode(b))
}

func (s *smoke) wsURL() string { return "ws" + strings.TrimPrefix(s.base, "http") + "/api/v1/realtime" }

func (s *smoke) dial(origin string) (*websocket.Conn, *http.Response, error) {
	h := s.headers(nil)
	if origin != "" {
		h.Set("Origin", origin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return websocket.Dial(ctx, s.wsURL(), &websocket.DialOptions{HTTPHeader: h})
}

func readFrame(c *websocket.Conn, timeout time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

func send(c *websocket.Conn, v any) error {
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, b)
}

func (s *smoke) wsChecks() {
	_, resp, err := s.dial("https://evil.example")
	st := 0
	if resp != nil {
		st = resp.StatusCode
	}
	s.check("ws.foreign-origin", err != nil && st == 403, "status=%d", st)

	c, resp, err := s.dial(s.origin)
	if err != nil {
		s.check("ws.upgrade", false, "err=%v", err)
		return
	}
	s.check("ws.upgrade", resp.StatusCode == 101, "status=%d through the full middleware chain", resp.StatusCode)
	_ = send(c, map[string]any{"v": 1, "type": "auth", "requestId": "a1",
		"payload": map[string]string{"accessToken": "malformed", "roomCode": "1234", "epoch": "x"}})
	f, _ := readFrame(c, 10*time.Second)
	_, err = readFrame(c, 10*time.Second)
	s.check("ws.auth-rejected", f["type"] == "error" && websocket.CloseStatus(err) == websocket.StatusPolicyViolation,
		"frame=%v close=%d", f["type"], websocket.CloseStatus(err))

	c, _, err = s.dial(s.origin)
	if err == nil {
		_ = send(c, map[string]any{"v": 1, "type": "state.request"})
		_, _ = readFrame(c, 10*time.Second)
		_, err = readFrame(c, 10*time.Second)
		s.check("ws.first-frame-must-auth", websocket.CloseStatus(err) == websocket.StatusPolicyViolation, "close=%d", websocket.CloseStatus(err))
	}
	// A well-formed token forces a database lookup: without a reachable DB
	// the server must degrade with a retryable error and close 1013.
	c, _, err = s.dial(s.origin)
	if err == nil {
		_ = send(c, map[string]any{"v": 1, "type": "auth", "requestId": "a2",
			"payload": map[string]string{"accessToken": strings.Repeat("A", 43), "roomCode": "1234", "epoch": "x"}})
		f, _ := readFrame(c, 20*time.Second)
		_, err = readFrame(c, 20*time.Second)
		code := ""
		if e, ok := f["payload"].(map[string]any); ok {
			if inner, ok := e["error"].(map[string]any); ok {
				code, _ = inner["code"].(string)
			}
		}
		fmt.Printf("INFO %-26s code=%s close=%d (UNAUTHENTICATED/1008 with a DB; DATABASE_UNAVAILABLE/1013 without)\n",
			"ws.token-lookup", code, websocket.CloseStatus(err))
	}
}

func key() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *smoke) fullFlow() {
	name := "smoke" + key()[:8]
	st, _, b := s.req("POST", "/api/v1/players", map[string]string{"username": name}, nil)
	var reg struct{ AccessToken string }
	_ = json.Unmarshal(b, &reg)
	s.check("full.register", st == 201 && reg.AccessToken != "", "status=%d", st)
	if reg.AccessToken == "" {
		return
	}
	auth := http.Header{"Authorization": {"Bearer " + reg.AccessToken}}
	h := auth.Clone()
	h.Set("Idempotency-Key", key())
	st, _, b = s.req("POST", "/api/v1/rooms", map[string]any{}, h)
	var room struct{ RoomCode, Epoch, StateVersion string }
	_ = json.Unmarshal(b, &room)
	s.check("full.create-room", st == 201 && room.RoomCode != "", "status=%d code=%s", st, room.RoomCode)

	c, _, err := s.dial(s.origin)
	if err != nil {
		s.check("full.ws", false, "err=%v", err)
		return
	}
	defer c.CloseNow()
	_ = send(c, map[string]any{"v": 1, "type": "auth", "payload": map[string]string{"accessToken": reg.AccessToken, "roomCode": room.RoomCode, "epoch": room.Epoch}})
	ok, _ := readFrame(c, 15*time.Second)
	snap, _ := readFrame(c, 15*time.Second)
	s.check("full.ws-auth-snapshot", ok["type"] == "auth.ok" && snap["type"] == "room.snapshot", "frames=%v,%v", ok["type"], snap["type"])
	cmd := key()
	start := time.Now()
	_ = send(c, map[string]any{"v": 1, "type": "command", "requestId": cmd, "payload": map[string]any{
		"commandId": cmd, "epoch": room.Epoch, "operation": "room.activate", "expectedVersion": snap["stateVersion"]}})
	gotResult, gotSnap := false, false
	for i := 0; i < 5 && !(gotResult && gotSnap); i++ {
		f, err := readFrame(c, 15*time.Second)
		if err != nil {
			break
		}
		gotResult = gotResult || f["type"] == "command.result"
		gotSnap = gotSnap || (f["type"] == "room.snapshot" && f["stateVersion"] != snap["stateVersion"])
	}
	s.check("full.ws-command", gotResult && gotSnap, "command.result=%t newSnapshot=%t in %s", gotResult, gotSnap, time.Since(start).Round(time.Millisecond))
	st, _, _ = s.req("DELETE", "/api/v1/players/me", nil, auth)
	s.check("full.logout", st == 204, "status=%d", st)
}
