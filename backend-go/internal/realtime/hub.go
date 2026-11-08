package realtime

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/coordinator"
)

// Config tunes the realtime layer (plan 7, 9.4).
type Config struct {
	AllowedOrigins   []string
	FrameLimit       int64
	AuthTimeout      time.Duration
	RotateAfter      time.Duration // < platform max duration (measured 300 s on Vercel Hobby)
	LeaseRenew       time.Duration // advertised heartbeat cadence
	HeartbeatTimeout time.Duration // close when no frame for this long
	PollActive       time.Duration
	PollLobby        time.Duration
	SendQueue        int
	CommandTimeout   time.Duration
}

// DefaultConfig returns the candidate values (rotation at 270 s: Vercel
// Hobby closed sockets at 300 s in the Preview measurement).
func DefaultConfig() Config {
	return Config{
		FrameLimit: 16 << 10, AuthTimeout: 5 * time.Second, RotateAfter: 270 * time.Second,
		LeaseRenew: 15 * time.Second, HeartbeatTimeout: 45 * time.Second,
		PollActive: 2 * time.Second, PollLobby: 5 * time.Second, SendQueue: 32, CommandTimeout: 10 * time.Second,
	}
}

type roomSubs struct {
	conns       map[*conn]struct{}
	sentVersion int64
	dirty       bool
	reason      string
	active      bool // room has a game: poll faster
}

// Hub holds the local sockets of this process, grouped by room.
type Hub struct {
	svc *coordinator.Service
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	rooms    map[int64]*roomSubs
	running  bool
	closing  bool
	wake     chan struct{}
	stop     chan struct{}
	handlers sync.WaitGroup
	watcher  sync.WaitGroup
}

// NewHub creates the hub and registers it as the coordinator notifier.
func NewHub(svc *coordinator.Service, cfg Config, log *slog.Logger) *Hub {
	h := &Hub{svc: svc, cfg: cfg, log: log, rooms: map[int64]*roomSubs{},
		wake: make(chan struct{}, 1), stop: make(chan struct{})}
	svc.SetNotifier(h)
	return h
}

// RoomChanged is called after a local commit: wake the watcher immediately.
func (h *Hub) RoomChanged(roomID int64, reason string) {
	h.mu.Lock()
	rs := h.rooms[roomID]
	if rs != nil {
		rs.dirty = true
		rs.reason = reason
	}
	h.mu.Unlock()
	if rs != nil {
		h.signal()
	}
}

func (h *Hub) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *Hub) register(c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false
	}
	rs := h.rooms[c.roomID]
	if rs == nil {
		rs = &roomSubs{conns: map[*conn]struct{}{}}
		h.rooms[c.roomID] = rs
	}
	rs.conns[c] = struct{}{}
	if !h.running {
		h.running = true
		h.watcher.Add(1)
		go h.watch()
	}
	return true
}

func (h *Hub) unregister(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rs := h.rooms[c.roomID]; rs != nil {
		delete(rs.conns, c)
		if len(rs.conns) == 0 {
			delete(h.rooms, c.roomID)
		}
	}
}

// ActiveConnections returns the number of local sockets.
func (h *Hub) ActiveConnections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, rs := range h.rooms {
		n += len(rs.conns)
	}
	return n
}

// watch is the single per-process watcher. It exists only while at least
// one local socket exists and never overlaps cycles.
func (h *Hub) watch() {
	defer h.watcher.Done()
	backoff := time.Duration(0)
	for {
		ids, interval := h.plan()
		if len(ids) == 0 {
			h.mu.Lock()
			if len(h.rooms) == 0 {
				h.running = false
				h.mu.Unlock()
				return
			}
			h.mu.Unlock()
			continue
		}
		wait := jitter(interval) + backoff
		timer := time.NewTimer(wait)
		select {
		case <-h.stop:
			timer.Stop()
			return
		case <-h.wake:
			timer.Stop()
		case <-timer.C:
		}
		if err := h.cycle(); err != nil {
			backoff = min(max(2*backoff, time.Second), 30*time.Second)
			h.log.Warn("realtime watcher degraded", "err", err, "backoffMs", backoff.Milliseconds())
			continue
		}
		backoff = 0
	}
}

func (h *Hub) plan() ([]int64, time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	interval := h.cfg.PollLobby
	ids := make([]int64, 0, len(h.rooms))
	for id, rs := range h.rooms {
		ids = append(ids, id)
		if rs.active || rs.dirty {
			interval = min(interval, h.cfg.PollActive)
		}
		if rs.dirty {
			interval = 0
		}
	}
	return ids, interval
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d - d/5 + time.Duration(rand.Int64N(int64(d/5)*2+1)) // ±20 %
}

// cycle polls revisions of the local rooms in one batch, reconciles due
// deadlines/presence and pushes fresh snapshots where versions changed.
func (h *Hub) cycle() error {
	ids, _ := h.plan()
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	heads, err := h.svc.RoomHeads(ctx, ids)
	if err != nil {
		return err // keep the last confirmed state; no empty/fake snapshot
	}
	for _, id := range ids {
		head := heads[id]
		if head == nil {
			h.revokeRoom(id, "room-closed")
			continue
		}
		if h.svc.Due(head) {
			if _, err := h.svc.ReconcileRoom(ctx, id); err != nil {
				h.log.Warn("reconcile failed", "room", id, "err", err)
			}
		}
		h.mu.Lock()
		rs := h.rooms[id]
		need := rs != nil && (rs.dirty || head.Version != rs.sentVersion)
		h.mu.Unlock()
		if need || h.svc.Due(head) {
			if err := h.push(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// push loads one consistent snapshot and fans it out to the room's sockets,
// re-validating membership and token expiry for each.
func (h *Hub) push(ctx context.Context, roomID int64) error {
	h.mu.Lock()
	reason := "sync"
	if rs := h.rooms[roomID]; rs != nil {
		if rs.reason != "" {
			reason = rs.reason
		}
		rs.dirty = false
		rs.reason = ""
	}
	h.mu.Unlock()

	snap, err := h.svc.Snapshot(ctx, roomID)
	if err != nil {
		h.RoomChanged(roomID, reason) // retry on next cycle
		return err
	}
	if snap == nil {
		h.revokeRoom(roomID, "room-closed")
		return nil
	}
	msg := encode(TypeRoomSnapshot, Envelope{
		EventID: snap.Epoch + ":" + versionString(snap.Version), RoomCode: snap.View.Room.Code,
		Epoch: snap.Epoch, StateVersion: versionString(snap.Version), Reason: reason,
	}, map[string]any{"state": snap.View})
	now := h.svc.Now()

	h.mu.Lock()
	rs := h.rooms[roomID]
	var targets []*conn
	if rs != nil {
		rs.sentVersion = snap.Version
		rs.active = snap.View.Game != nil
		for c := range rs.conns {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()
	for _, c := range targets {
		switch {
		case !snap.Members[c.actor.PlayerID]:
			why := "left"
			if reason == "member.kicked" {
				why = "kicked"
			}
			c.revoke(why)
		case !c.actor.TokenExpiresAt.After(now):
			c.revoke("token-expired")
		default:
			c.pushSnapshot(snap.Version, msg)
		}
	}
	return nil
}

func (h *Hub) revokeRoom(roomID int64, why string) {
	h.mu.Lock()
	var targets []*conn
	if rs := h.rooms[roomID]; rs != nil {
		for c := range rs.conns {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()
	for _, c := range targets {
		c.revoke(why)
	}
}

// Shutdown asks every local socket to reconnect (1012), waits for handlers
// to finish within ctx and stops the watcher. Hijacked connections are not
// closed by http.Server.Shutdown, hence this explicit step.
func (h *Hub) Shutdown(ctx context.Context) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return
	}
	h.closing = true
	var all []*conn
	for _, rs := range h.rooms {
		for c := range rs.conns {
			all = append(all, c)
		}
	}
	h.mu.Unlock()
	for _, c := range all {
		c.reconnect("shutdown")
	}
	done := make(chan struct{})
	go func() {
		h.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	close(h.stop)
	h.watcher.Wait()
}
