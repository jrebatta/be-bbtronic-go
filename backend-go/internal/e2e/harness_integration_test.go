//go:build integration

// Package e2e runs the real HTTP handler and WebSocket hub over httptest
// servers. Two servers on the same PostgreSQL database stand in for two
// Vercel instances with no shared memory.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/app"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/config"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/realtime"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/testdb"
)

const allowedOrigin = "https://fe.example"

type instance struct {
	app *app.App
	srv *httptest.Server
}

func testConfig() config.Config {
	return config.Config{
		AppEnv: config.EnvTest, PGQueryTO: 5 * time.Second, TokenTTL: time.Hour, RealtimeMode: "websocket",
		AllowedOrigins: []string{allowedOrigin}, PollActive: 150 * time.Millisecond, PollLobby: 200 * time.Millisecond,
		WSRotate: time.Minute, PresenceRenew: time.Second, PresenceLease: 3 * time.Second, DisconnectGrace: time.Second,
		RoomIdle: time.Hour, ReceiptRetention: time.Hour, MaxConnsPerUser: 4, MaxRoomMembers: 30,
		HTTPBodyLimit: 64 << 10, WSFrameLimit: 16 << 10, BuildID: "e2e",
	}
}

func newInstance(t *testing.T, db *testdb.DB, tune func(*realtime.Config)) *instance {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := app.WireWith(db.Store, testConfig(), log, tune)
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		a.Hub.Shutdown(ctx)
		srv.Close()
	})
	return &instance{app: a, srv: srv}
}

var keySeq atomic.Int64

func newKey() string { return fmt.Sprintf("e2e-key-%020d", keySeq.Add(1)) }

// client is a REST client bound to one instance and one player.
type client struct {
	t     *testing.T
	base  string
	token string
	id    string
	name  string
}

func (c *client) call(method, path string, body any, headers map[string]string, out any) int {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: decoding %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode
}

func register(t *testing.T, inst *instance, name string) *client {
	t.Helper()
	c := &client{t: t, base: inst.srv.URL, name: name}
	var res struct {
		Player      struct{ ID, Username string }
		AccessToken string
	}
	if st := c.call("POST", "/api/v1/players", map[string]string{"username": name}, nil, &res); st != 201 {
		t.Fatalf("register %s: %d", name, st)
	}
	c.token, c.id = res.AccessToken, res.Player.ID
	return c
}

// on returns the same player talking to another instance.
func (c *client) on(inst *instance) *client {
	cp := *c
	cp.base = inst.srv.URL
	return &cp
}

type roomRef struct{ Code, Epoch string }

func (c *client) createRoom() roomRef {
	c.t.Helper()
	var res struct{ RoomCode, Epoch string }
	if st := c.call("POST", "/api/v1/rooms", map[string]any{}, map[string]string{"Idempotency-Key": newKey()}, &res); st != 201 {
		c.t.Fatalf("create room: %d", st)
	}
	return roomRef{res.RoomCode, res.Epoch}
}

func (c *client) join(r roomRef) {
	c.t.Helper()
	if st := c.call("POST", "/api/v1/rooms/"+r.Code+"/members", map[string]string{"epoch": r.Epoch}, map[string]string{"Idempotency-Key": newKey()}, nil); st != 200 {
		c.t.Fatalf("join: %d", st)
	}
}

type snapshot struct {
	StateVersion string `json:"stateVersion"`
	Room         struct {
		Code, Epoch, Status string
		HostPlayerID        *string
	} `json:"room"`
	Players []struct {
		ID, Username, ConnectionStatus, Participation string
		Ready                                         bool
	} `json:"players"`
	Game *struct {
		ID      string  `json:"id"`
		Kind    string  `json:"kind"`
		RoundID *string `json:"roundId"`
		Phase   string  `json:"phase"`
	} `json:"game"`
}

func (c *client) state(r roomRef) snapshot {
	c.t.Helper()
	var s snapshot
	if st := c.call("GET", "/api/v1/rooms/"+r.Code+"/state?epoch="+r.Epoch, nil, nil, &s); st != 200 {
		c.t.Fatalf("state: %d", st)
	}
	return s
}

// command sends a command scoped to the current snapshot and returns status + body.
func (c *client) command(r roomRef, op string, payload any) (int, map[string]any) {
	c.t.Helper()
	s := c.state(r)
	id := newKey()
	body := map[string]any{"commandId": id, "epoch": r.Epoch, "operation": op, "expectedVersion": s.StateVersion}
	if s.Game != nil {
		body["gameRunId"] = s.Game.ID
		if s.Game.RoundID != nil {
			body["roundId"] = *s.Game.RoundID
		}
	}
	if payload != nil {
		body["payload"] = payload
	}
	var out map[string]any
	st := c.call("POST", "/api/v1/rooms/"+r.Code+"/commands", body, map[string]string{"Idempotency-Key": id}, &out)
	return st, out
}

// --- WebSocket client ------------------------------------------------------------

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
}

type frame struct {
	V            int             `json:"v"`
	Type         string          `json:"type"`
	RequestID    string          `json:"requestId"`
	StateVersion string          `json:"stateVersion"`
	Reason       string          `json:"reason"`
	Payload      json.RawMessage `json:"payload"`
}

func dialWS(t *testing.T, inst *instance, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	u := "ws" + strings.TrimPrefix(inst.srv.URL, "http") + "/api/v1/realtime"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
}

func connectWS(t *testing.T, inst *instance, c *client, r roomRef) (*wsClient, frame) {
	t.Helper()
	conn, _, err := dialWS(t, inst, allowedOrigin)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetReadLimit(1 << 20)
	ws := &wsClient{t: t, conn: conn}
	t.Cleanup(func() { conn.CloseNow() })
	ws.send(map[string]any{"v": 1, "type": "auth", "requestId": "auth-1",
		"payload": map[string]string{"accessToken": c.token, "roomCode": r.Code, "epoch": r.Epoch}})
	ok := ws.expect("auth.ok", 5*time.Second)
	ws.expect("room.snapshot", 5*time.Second)
	return ws, ok
}

func (w *wsClient) send(v any) {
	w.t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.conn.Write(ctx, websocket.MessageText, b); err != nil {
		w.t.Fatalf("ws write: %v", err)
	}
}

func (w *wsClient) read(timeout time.Duration) (frame, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := w.conn.Read(ctx)
	if err != nil {
		return frame{}, err
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return frame{}, fmt.Errorf("decoding %q: %w", data, err)
	}
	return f, nil
}

// expect reads frames until one of type typ arrives.
func (w *wsClient) expect(typ string, timeout time.Duration) frame {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		f, err := w.read(time.Until(deadline))
		if err != nil {
			w.t.Fatalf("waiting for %s: %v", typ, err)
		}
		if f.Type == typ {
			return f
		}
	}
}

// expectSnapshotAfter waits for a snapshot whose version exceeds v.
func (w *wsClient) expectSnapshotAfter(v string, timeout time.Duration) frame {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		f := w.expect("room.snapshot", time.Until(deadline))
		if len(f.StateVersion) > len(v) || (len(f.StateVersion) == len(v) && f.StateVersion > v) {
			return f
		}
	}
}

// expectClose reads until the socket closes and returns the close code.
func (w *wsClient) expectClose(timeout time.Duration) (websocket.StatusCode, []frame) {
	w.t.Helper()
	var frames []frame
	deadline := time.Now().Add(timeout)
	for {
		f, err := w.read(time.Until(deadline))
		if err != nil {
			return websocket.CloseStatus(err), frames
		}
		frames = append(frames, f)
	}
}
