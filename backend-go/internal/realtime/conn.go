package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/apierror"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/coordinator"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
)

// Gate decides whether a new upgrade is allowed (rate limiting by IP).
type Gate func(r *http.Request) bool

// Handler returns the GET /api/v1/realtime handler.
func (h *Hub) Handler(gate Gate) http.Handler {
	allowed := map[string]bool{}
	var hosts []string
	for _, o := range h.cfg.AllowedOrigins {
		allowed[o] = true
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			hosts = append(hosts, u.Host)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exact Origin allowlist; non-browser clients send no Origin and must
		// still authenticate in the first frame.
		if origin := r.Header.Get("Origin"); origin != "" && !allowed[origin] {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if gate != nil && !gate(r) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "too many connections", http.StatusTooManyRequests)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns:  hosts,
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return // Accept already wrote the HTTP error
		}
		ws.SetReadLimit(h.cfg.FrameLimit)
		h.handlers.Add(1)
		defer h.handlers.Done()
		h.serve(ws)
	})
}

type outMsg struct {
	data        []byte
	closeCode   websocket.StatusCode
	closeReason string
}

type conn struct {
	hub    *Hub
	ws     *websocket.Conn
	actor  coordinator.Actor
	roomID int64
	code   string
	epoch  string

	connMu sync.Mutex
	connID string

	out     chan outMsg
	snapSig chan struct{}
	snap    atomic.Pointer[[]byte]
	sentVer atomic.Int64
	closing atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
}

func (c *conn) connectionID() string {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.connID
}

func (c *conn) setConnectionID(id string) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	c.connID = id
}

// serve runs the whole connection lifecycle on the handler goroutine. The
// hijacked socket gets its own context (not r.Context) bound to its life.
func (h *Hub) serve(ws *websocket.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	authCtx, authCancel := context.WithTimeout(ctx, h.cfg.AuthTimeout)
	typ, data, err := ws.Read(authCtx)
	authCancel()
	if err != nil {
		_ = ws.Close(CloseAuthTimeout, "auth timeout")
		return
	}
	var env Envelope
	var p AuthPayload
	if typ != websocket.MessageText || json.Unmarshal(data, &env) != nil || env.Type != TypeAuth ||
		json.Unmarshal(env.Payload, &p) != nil {
		h.rejectAuth(ctx, ws, env.RequestID, game.Errf(game.CodeInvalidInput, "el primer mensaje debe ser auth"))
		return
	}
	opCtx, opCancel := context.WithTimeout(ctx, h.cfg.CommandTimeout)
	actor, err := h.svc.Authenticate(opCtx, p.AccessToken)
	if err != nil {
		opCancel()
		h.rejectAuth(ctx, ws, env.RequestID, err)
		return
	}
	lease, mc, err := h.svc.OpenConnection(opCtx, actor, p.RoomCode, p.Epoch, game.LeaseKindWebSocket, "")
	opCancel()
	if err != nil {
		h.rejectAuth(ctx, ws, env.RequestID, err)
		return
	}
	connCtx, connCancel := context.WithCancel(ctx)
	c := &conn{
		hub: h, ws: ws, actor: actor, roomID: mc.RoomID, code: mc.RoomCode, epoch: mc.Epoch, connID: lease.ConnectionID,
		out: make(chan outMsg, h.cfg.SendQueue), snapSig: make(chan struct{}, 1), ctx: connCtx, cancel: connCancel,
	}
	defer c.finish()
	if !h.register(c) {
		c.reconnect("shutdown")
		c.writeLoop()
		return
	}
	c.send(encode(TypeAuthOK, Envelope{RequestID: env.RequestID}, map[string]any{
		"connectionId": lease.ConnectionID, "roomCode": mc.RoomCode, "epoch": mc.Epoch,
		"stateVersion": versionString(mc.StateVersion), "serverTime": lease.ServerTime,
		"leaseUntil": lease.LeaseUntil, "playerId": game.FormatID(actor.PlayerID),
		"rotateAfterSeconds": int(h.cfg.RotateAfter / time.Second), "leaseRenewalSeconds": int(h.cfg.LeaseRenew / time.Second),
	}))
	// Registered before loading: commits during the load mark the room dirty
	// and the watcher re-checks the revision (plan 7.3).
	if err := c.sendFreshSnapshot("sync"); err != nil {
		c.closeWith(CloseTryAgain, "snapshot unavailable", nil)
	}

	go c.writeLoop()
	rotate := time.AfterFunc(h.cfg.RotateAfter, func() { c.reconnect("rotate") })
	defer rotate.Stop()
	c.readLoop()
}

func (h *Hub) rejectAuth(ctx context.Context, ws *websocket.Conn, requestID string, err error) {
	_, body, internal := apierror.Classify(err)
	if internal {
		h.log.Error("realtime auth failed", "err", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, encode(TypeError, Envelope{RequestID: requestID}, map[string]any{"error": body}))
	code := CloseRevoked
	if body.Retryable {
		code = CloseTryAgain
	}
	_ = ws.Close(code, string(body.Code))
}

func (c *conn) finish() {
	c.cancel()
	c.hub.unregister(c)
	// Best effort: release the lease so presence flips quickly. If the
	// platform kills the function, the lease simply expires (plan 7.4).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.hub.svc.CloseConnection(ctx, c.actor, c.roomID, c.connectionID()); err != nil {
		c.hub.log.Warn("closing lease failed", "room", c.roomID, "err", err)
	}
	c.ws.CloseNow()
}

// send enqueues a control/response frame; a full queue means a slow client:
// close it and let it resync (plan 5.5).
func (c *conn) send(b []byte) {
	if c.closing.Load() {
		return
	}
	select {
	case c.out <- outMsg{data: b}:
	default:
		c.closeWith(CloseSlowConsumer, "slow consumer", nil)
	}
}

// pushSnapshot stores the latest snapshot (coalesced: a full replacement
// supersedes older pending ones) and wakes the writer.
func (c *conn) pushSnapshot(version int64, msg []byte) {
	if c.closing.Load() || version < c.sentVer.Load() {
		return
	}
	c.snap.Store(&msg)
	c.sentVer.Store(version)
	select {
	case c.snapSig <- struct{}{}:
	default:
	}
}

func (c *conn) sendFreshSnapshot(reason string) error {
	ctx, cancel := context.WithTimeout(c.ctx, c.hub.cfg.CommandTimeout)
	defer cancel()
	snap, err := c.hub.svc.Snapshot(ctx, c.roomID)
	if err != nil {
		return err
	}
	if snap == nil || !snap.Members[c.actor.PlayerID] {
		c.revoke("left")
		return nil
	}
	msg := encode(TypeRoomSnapshot, Envelope{
		EventID: snap.Epoch + ":" + versionString(snap.Version), RoomCode: snap.View.Room.Code,
		Epoch: snap.Epoch, StateVersion: versionString(snap.Version), Reason: reason,
	}, map[string]any{"state": snap.View})
	c.sentVer.Store(0) // explicit request: same version is re-sent to rebuild the UI
	c.pushSnapshot(snap.Version, msg)
	return nil
}

// closeWith sends an optional final frame and closes with code.
func (c *conn) closeWith(code websocket.StatusCode, reason string, final []byte) {
	if !c.closing.CompareAndSwap(false, true) {
		return
	}
	select {
	case c.out <- outMsg{data: final, closeCode: code, closeReason: reason}:
	default: // queue full: close immediately
		go func() { _ = c.ws.Close(code, reason) }()
		c.cancel()
	}
}

func (c *conn) revoke(why string) {
	c.closeWith(CloseRevoked, "access revoked", encode(TypeAccessRevoked, Envelope{}, map[string]any{"reason": why}))
}

func (c *conn) reconnect(why string) {
	c.closeWith(CloseRotate, why, encode(TypeServerReconnect, Envelope{}, map[string]any{
		"reason": why, "retryAfterMs": 0, "lastVersion": versionString(c.sentVer.Load()),
	}))
}

// writeLoop is the single writer of the socket.
func (c *conn) writeLoop() {
	write := func(b []byte) bool {
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		defer cancel()
		return c.ws.Write(ctx, websocket.MessageText, b) == nil
	}
	for {
		select {
		case <-c.ctx.Done():
			return
		case m := <-c.out:
			if m.data != nil && !write(m.data) {
				c.cancel()
				return
			}
			if m.closeCode != 0 {
				_ = c.ws.Close(m.closeCode, m.closeReason)
				c.cancel()
				return
			}
		case <-c.snapSig:
			if p := c.snap.Swap(nil); p != nil && !write(*p) {
				c.cancel()
				return
			}
		}
	}
}

// readLoop processes client frames until the socket ends.
func (c *conn) readLoop() {
	// Cancelling a Read context closes the socket abruptly in coder/websocket,
	// so idleness is detected by a watchdog that closes cleanly with 4001.
	idle := time.AfterFunc(c.hub.cfg.HeartbeatTimeout, func() {
		c.closeWith(CloseHeartbeatTimeout, "heartbeat timeout", nil)
	})
	defer idle.Stop()
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		idle.Reset(c.hub.cfg.HeartbeatTimeout)
		if typ != websocket.MessageText {
			c.sendError("", game.Errf(game.CodeInvalidInput, "solo se aceptan mensajes de texto JSON"))
			continue
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil || env.V != ProtocolVersion {
			c.sendError("", game.Errf(game.CodeInvalidInput, "mensaje JSON inválido o versión no soportada"))
			continue
		}
		switch env.Type {
		case TypeHeartbeat:
			c.heartbeat(env)
		case TypeStateRequest:
			if err := c.sendFreshSnapshot("sync"); err != nil {
				c.sendError(env.RequestID, err)
			}
		case TypeCommand:
			c.command(env)
		default:
			c.sendError(env.RequestID, game.Errf(game.CodeInvalidInput, "tipo de mensaje desconocido"))
		}
	}
}

func (c *conn) heartbeat(env Envelope) {
	if !c.actor.TokenExpiresAt.After(c.hub.svc.Now()) {
		c.revoke("token-expired")
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.hub.cfg.CommandTimeout)
	defer cancel()
	res, err := c.hub.svc.RenewConnection(ctx, c.actor, c.roomID, c.connectionID())
	if game.IsCode(err, game.CodeLeaseNotFound) {
		// Lease lapsed (e.g. grace started): re-open under the room lock.
		var mc coordinator.MemberContext
		res, mc, err = c.hub.svc.OpenConnection(ctx, c.actor, c.code, c.epoch, game.LeaseKindWebSocket, "")
		if err == nil {
			c.setConnectionID(res.ConnectionID)
			_ = mc
		}
	}
	if err != nil {
		if game.IsCode(err, game.CodeForbidden) || game.IsCode(err, game.CodeRoomNotFound) || game.IsCode(err, game.CodeRoomExpired) {
			c.revoke("left")
			return
		}
		c.sendError(env.RequestID, err)
		return
	}
	c.send(encode(TypeHeartbeatAck, Envelope{RequestID: env.RequestID}, map[string]any{
		"connectionId": res.ConnectionID, "leaseUntil": res.LeaseUntil, "serverTime": res.ServerTime,
		"stateVersion": versionString(c.sentVer.Load()),
	}))
}

func (c *conn) command(env Envelope) {
	var req coordinator.CommandRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		c.sendError(env.RequestID, game.Errf(game.CodeInvalidInput, "comando inválido"))
		return
	}
	if req.CommandID == "" {
		req.CommandID = env.RequestID
	}
	if req.Epoch == "" {
		req.Epoch = c.epoch
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.hub.cfg.CommandTimeout)
	defer cancel()
	res, err := c.hub.svc.Execute(ctx, c.actor, c.code, req)
	if err != nil {
		c.sendError(req.CommandID, err)
		return
	}
	c.send(encode(TypeCommandResult, Envelope{RequestID: req.CommandID}, res))
}

func (c *conn) sendError(requestID string, err error) {
	_, body, internal := apierror.Classify(err)
	if internal {
		c.hub.log.Error("realtime request failed", "room", c.roomID, "err", err)
	}
	c.send(encode(TypeError, Envelope{RequestID: requestID}, map[string]any{"error": body}))
}
