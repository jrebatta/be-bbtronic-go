//go:build integration

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/realtime"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/testdb"
)

// T12: handshake, auth, messages, errors and commands over WebSocket.
func TestWS_Protocol(t *testing.T) {
	db := testdb.New(t)
	inst := newInstance(t, db, nil)
	ana := register(t, inst, "ana")
	r := ana.createRoom()

	if _, resp, err := dialWS(t, inst, "https://evil.example"); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatal("foreign Origin must be rejected at the handshake")
	}

	// A first frame that is not auth → error + 1008, no data sent before.
	conn, resp, err := dialWS(t, inst, allowedOrigin)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("dial %v", err)
	}
	bad := &wsClient{t: t, conn: conn}
	bad.send(map[string]any{"v": 1, "type": "state.request"})
	if code, frames := bad.expectClose(3 * time.Second); code != websocket.StatusPolicyViolation || len(frames) != 1 || frames[0].Type != "error" {
		t.Fatalf("non-auth first frame: code=%d frames=%v", code, frames)
	}
	// Invalid token → error + 1008.
	conn, _, _ = dialWS(t, inst, allowedOrigin)
	bad = &wsClient{t: t, conn: conn}
	bad.send(map[string]any{"v": 1, "type": "auth", "payload": map[string]string{"accessToken": "x", "roomCode": r.Code, "epoch": r.Epoch}})
	if code, _ := bad.expectClose(3 * time.Second); code != websocket.StatusPolicyViolation {
		t.Fatalf("bad token close %d", code)
	}

	ws, ok := connectWS(t, inst, ana, r)
	var okp struct {
		ConnectionID        string `json:"connectionId"`
		RotateAfterSeconds  int    `json:"rotateAfterSeconds"`
		LeaseRenewalSeconds int    `json:"leaseRenewalSeconds"`
		PlayerID            string `json:"playerId"`
	}
	_ = json.Unmarshal(ok.Payload, &okp)
	if okp.ConnectionID == "" || okp.RotateAfterSeconds != 60 || okp.PlayerID != ana.id {
		t.Fatalf("auth.ok %+v", okp)
	}

	ws.send(map[string]any{"v": 1, "type": "nonsense", "requestId": "r1"})
	if f := ws.expect("error", 3*time.Second); f.RequestID != "r1" {
		t.Fatalf("unknown type error %+v", f)
	}
	_ = ws.conn.Write(t.Context(), websocket.MessageText, []byte("{not json"))
	ws.expect("error", 3*time.Second) // connection stays open

	ws.send(map[string]any{"v": 1, "type": "heartbeat", "requestId": "hb1", "payload": map[string]string{"connectionId": okp.ConnectionID}})
	if f := ws.expect("heartbeat.ack", 3*time.Second); f.RequestID != "hb1" {
		t.Fatal("heartbeat ack")
	}
	ws.send(map[string]any{"v": 1, "type": "state.request", "requestId": "s1"})
	snap := ws.expect("room.snapshot", 3*time.Second)

	// Command over WS shares the engine and receipts with REST.
	cmdID := newKey()
	ws.send(map[string]any{"v": 1, "type": "command", "requestId": cmdID, "payload": map[string]any{
		"commandId": cmdID, "epoch": r.Epoch, "operation": "room.activate", "expectedVersion": snap.StateVersion}})
	if f := ws.expect("command.result", 3*time.Second); f.RequestID != cmdID {
		t.Fatalf("command.result %+v", f)
	}
	if f := ws.expectSnapshotAfter(snap.StateVersion, 3*time.Second); f.Reason != "room.activated" {
		t.Fatalf("local commit must push a snapshot with its reason, got %q", f.Reason)
	}
	big := make([]byte, 20<<10)
	for i := range big {
		big[i] = 'x'
	}
	_ = ws.conn.Write(t.Context(), websocket.MessageText, big)
	if code, _ := ws.expectClose(3 * time.Second); code != websocket.StatusMessageTooBig {
		t.Fatalf("frame limit close %d", code)
	}
}

// T13: a socket on instance A converges on a change committed through
// instance B without any shared memory (PG revision polling).
func TestWS_CrossInstanceConvergence(t *testing.T) {
	db := testdb.New(t)
	a := newInstance(t, db, nil)
	b := newInstance(t, db, nil)
	ana := register(t, a, "ana")
	r := ana.createRoom()
	beto := register(t, b, "beto")
	beto.join(r)

	ws, _ := connectWS(t, a, ana, r)
	before := ana.state(r).StateVersion
	start := time.Now()
	if st, _ := beto.command(r, "member.ready", map[string]bool{"ready": true}); st != 200 {
		t.Fatalf("ready via B: %d", st)
	}
	f := ws.expectSnapshotAfter(before, 5*time.Second)
	t.Logf("cross-instance propagation: %s (poll 150–200 ms ±20%%)", time.Since(start).Round(time.Millisecond))
	var p struct {
		State snapshot `json:"state"`
	}
	_ = json.Unmarshal(f.Payload, &p)
	ready := false
	for _, pl := range p.State.Players {
		ready = ready || (pl.Username == "beto" && pl.Ready)
	}
	if !ready {
		t.Fatal("snapshot from A must reflect B's commit")
	}
}

// T14/T17: a kick committed on B revokes the kicked socket held by A.
func TestWS_RevocationAcrossInstances(t *testing.T) {
	db := testdb.New(t)
	a := newInstance(t, db, nil)
	b := newInstance(t, db, nil)
	ana := register(t, b, "ana")
	r := ana.createRoom()
	beto := register(t, a, "beto")
	beto.join(r)
	ws, _ := connectWS(t, a, beto, r)
	if st, _ := ana.command(r, "member.kick", map[string]string{"playerId": beto.id}); st != 200 {
		t.Fatalf("kick %d", st)
	}
	code, frames := ws.expectClose(5 * time.Second)
	revoked := false
	for _, f := range frames {
		revoked = revoked || f.Type == "access.revoked"
	}
	if code != websocket.StatusPolicyViolation || !revoked {
		t.Fatalf("kicked socket: code=%d revoked=%t", code, revoked)
	}
}

// Rotation before the platform limit (Vercel Hobby measured 300 s): the
// server asks the client to reconnect with 1012 and the client re-authenticates.
func TestWS_RotationAndReconnect(t *testing.T) {
	db := testdb.New(t)
	inst := newInstance(t, db, func(c *realtime.Config) { c.RotateAfter = 700 * time.Millisecond })
	ana := register(t, inst, "ana")
	r := ana.createRoom()
	for cycle := range 3 {
		ws, _ := connectWS(t, inst, ana, r)
		code, frames := ws.expectClose(5 * time.Second)
		if code != websocket.StatusServiceRestart || len(frames) == 0 || frames[len(frames)-1].Type != "server.reconnect" {
			t.Fatalf("cycle %d: code=%d frames=%v", cycle, code, frames)
		}
	}
	if s := ana.state(r); len(s.Players) != 1 {
		t.Fatal("rotation must not remove the member")
	}
}

// C13: closing the last socket shows "reconnecting"; reconnecting within the
// grace period keeps the member; silence beyond it removes the member.
func TestWS_PresenceGrace(t *testing.T) {
	db := testdb.New(t)
	inst := newInstance(t, db, nil)
	ana := register(t, inst, "ana")
	r := ana.createRoom()
	beto := register(t, inst, "beto")
	beto.join(r)
	watch, _ := connectWS(t, inst, ana, r)
	bws, _ := connectWS(t, inst, beto, r)
	_ = bws.conn.Close(websocket.StatusNormalClosure, "bye")

	status := func() string {
		for _, p := range ana.state(r).Players {
			if p.Username == "beto" {
				return p.ConnectionStatus
			}
		}
		return "gone"
	}
	deadline := time.Now().Add(3 * time.Second)
	for status() != "reconnecting" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if status() != "reconnecting" {
		t.Fatalf("after close: %s", status())
	}
	// The watcher on ana's instance finalizes the departure after the grace.
	deadline = time.Now().Add(5 * time.Second)
	for status() != "gone" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if status() != "gone" {
		t.Fatalf("member must leave after grace without traffic: %s", status())
	}
	watch.expect("room.snapshot", 3*time.Second)
}

// Graceful shutdown asks every socket to reconnect.
func TestWS_Shutdown(t *testing.T) {
	db := testdb.New(t)
	inst := newInstance(t, db, nil)
	ana := register(t, inst, "ana")
	r := ana.createRoom()
	ws, _ := connectWS(t, inst, ana, r)
	go inst.app.Hub.Shutdown(t.Context())
	if code, _ := ws.expectClose(5 * time.Second); code != websocket.StatusServiceRestart {
		t.Fatalf("shutdown close %d", code)
	}
}
