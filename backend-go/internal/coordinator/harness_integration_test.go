//go:build integration

package coordinator_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/coordinator"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/testdb"
)

type harness struct {
	t   *testing.T
	db  *testdb.DB
	svc *coordinator.Service

	mu  sync.Mutex
	now time.Time
	seq int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, db: testdb.New(t), now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	h.svc = h.newService()
	return h
}

// newService builds a fresh coordinator on the same database: it simulates
// another Vercel instance or a process restart (no shared memory).
func (h *harness) newService() *coordinator.Service {
	svc := coordinator.New(h.db.Store, coordinator.DefaultConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.SetClock(h.clock)
	return svc
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func (h *harness) key() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	return fmt.Sprintf("test-key-%016d", h.seq)
}

type player struct {
	coordinator.Actor
	token string
}

func (h *harness) register(name string) player {
	h.t.Helper()
	res, err := h.svc.Register(context.Background(), name)
	if err != nil {
		h.t.Fatalf("register %s: %v", name, err)
	}
	return h.auth(res.AccessToken)
}

func (h *harness) auth(token string) player {
	h.t.Helper()
	a, err := h.svc.Authenticate(context.Background(), token)
	if err != nil {
		h.t.Fatalf("authenticate: %v", err)
	}
	return player{Actor: a, token: token}
}

// refresh re-reads the actor (room membership changes after create/join).
func (h *harness) refresh(p player) player { return h.auth(p.token) }

type room struct {
	code, epoch string
}

func (h *harness) createRoom(p player) room {
	h.t.Helper()
	res, err := h.svc.CreateRoom(context.Background(), p.Actor, h.key())
	if err != nil {
		h.t.Fatalf("create room: %v", err)
	}
	return room{code: res.RoomCode, epoch: res.Epoch}
}

func (h *harness) join(p player, r room) coordinator.RoomResult {
	h.t.Helper()
	res, err := h.svc.JoinRoom(context.Background(), p.Actor, r.code, r.epoch, h.key())
	if err != nil {
		h.t.Fatalf("join %s: %v", p.Username, err)
	}
	return res
}

// lobby creates a room hosted by the first name with the others joined.
func (h *harness) lobby(names ...string) (room, []player) {
	h.t.Helper()
	ps := make([]player, len(names))
	for i, n := range names {
		ps[i] = h.register(n)
	}
	r := h.createRoom(ps[0])
	for _, p := range ps[1:] {
		h.join(p, r)
	}
	for i := range ps {
		ps[i] = h.refresh(ps[i])
	}
	return r, ps
}

func (h *harness) state(p player, r room) game.View {
	h.t.Helper()
	v, err := h.svc.State(context.Background(), p.Actor, r.code, r.epoch)
	if err != nil {
		h.t.Fatalf("state: %v", err)
	}
	return v
}

// request builds a command scoped to the current state (as a client would).
func (h *harness) request(p player, r room, op string, payload any) coordinator.CommandRequest {
	h.t.Helper()
	v := h.state(p, r)
	req := coordinator.CommandRequest{CommandID: h.key(), Epoch: r.epoch, Operation: op, ExpectedVersion: &v.StateVersion}
	if v.Game != nil {
		req.GameRunID = v.Game.ID
		if v.Game.RoundID != nil {
			req.RoundID = *v.Game.RoundID
		}
	}
	if payload != nil {
		b, _ := json.Marshal(payload)
		req.Payload = b
	}
	return req
}

func (h *harness) do(p player, r room, op string, payload any) coordinator.CommandResult {
	h.t.Helper()
	res, err := h.svc.Execute(context.Background(), p.Actor, r.code, h.request(p, r, op, payload))
	if err != nil {
		h.t.Fatalf("%s by %s: %v", op, p.Username, err)
	}
	return res
}

func (h *harness) fail(p player, r room, op string, payload any, want game.Code) {
	h.t.Helper()
	_, err := h.svc.Execute(context.Background(), p.Actor, r.code, h.request(p, r, op, payload))
	if !game.IsCode(err, want) {
		h.t.Fatalf("%s by %s: got %v, want %s", op, p.Username, err, want)
	}
}

func result(res coordinator.CommandResult) map[string]any {
	b, _ := json.Marshal(res.Result)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func version(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func playerID(p player) string { return game.FormatID(p.PlayerID) }
