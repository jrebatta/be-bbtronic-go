//go:build integration

package e2e

import (
	"strings"
	"testing"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/testdb"
)

// T01: the REST v1 contract end to end (status codes, envelopes, string IDs,
// idempotency, authorization, no secrets).
func TestREST_Contract(t *testing.T) {
	db := testdb.New(t)
	inst := newInstance(t, db, nil)
	ana := register(t, inst, "ana")
	if ana.token == "" || ana.id == "" {
		t.Fatal("register must return the token to its owner only")
	}
	var dup errEnvelope
	if st := (&client{t: t, base: inst.srv.URL}).call("POST", "/api/v1/players", map[string]string{"username": "ana"}, nil, &dup); st != 409 || dup.Error.Code != "NAME_TAKEN" || dup.RequestID == "" {
		t.Fatalf("duplicate: %d %v", st, dup)
	}

	var me struct {
		Player struct{ ID, Username string }
		Room   *struct{ Code, Epoch string }
	}
	if st := ana.call("GET", "/api/v1/players/me", nil, nil, &me); st != 200 || me.Player.ID != ana.id || me.Room != nil {
		t.Fatalf("me %d %+v", st, me)
	}
	if st := ana.call("POST", "/api/v1/rooms", map[string]any{}, nil, nil); st != 400 {
		t.Fatalf("missing Idempotency-Key -> %d", st)
	}
	key := newKey()
	var r1, r2 struct {
		RoomCode, Epoch, StateVersion, PlayerID string
		Replayed                                bool
	}
	ana.call("POST", "/api/v1/rooms", map[string]any{}, map[string]string{"Idempotency-Key": key}, &r1)
	if st := ana.call("POST", "/api/v1/rooms", map[string]any{}, map[string]string{"Idempotency-Key": key}, &r2); st != 201 || !r2.Replayed || r2.RoomCode != r1.RoomCode {
		t.Fatalf("create replay %d %+v", st, r2)
	}
	r := roomRef{r1.RoomCode, r1.Epoch}
	beto := register(t, inst, "beto")
	beto.join(r)

	s := ana.state(r)
	if len(s.Players) != 2 || *s.Room.HostPlayerID != ana.id || s.StateVersion == "" {
		t.Fatalf("state %+v", s)
	}
	var raw map[string]any
	ana.call("GET", "/api/v1/rooms/"+r.Code+"/state?epoch="+r.Epoch, nil, nil, &raw)
	if body := strings.ToLower(jsonString(raw)); strings.Contains(body, ana.token) || strings.Contains(body, beto.token) || strings.Contains(body, "token") {
		t.Fatal("snapshot contains tokens")
	}

	// Commands: header must match commandId; host-only; version conflict.
	body := map[string]any{"commandId": newKey(), "epoch": r.Epoch, "operation": "room.activate", "expectedVersion": s.StateVersion}
	if st := ana.call("POST", "/api/v1/rooms/"+r.Code+"/commands", body, map[string]string{"Idempotency-Key": newKey()}, nil); st != 400 {
		t.Fatalf("mismatched key -> %d", st)
	}
	if st, out := beto.command(r, "room.activate", nil); st != 403 || out["error"].(map[string]any)["code"] != "FORBIDDEN" {
		t.Fatalf("non-host -> %d %v", st, out)
	}
	stale := map[string]any{"commandId": newKey(), "epoch": r.Epoch, "operation": "room.activate", "expectedVersion": "1"}
	var conflict errEnvelope
	if st := ana.call("POST", "/api/v1/rooms/"+r.Code+"/commands", stale, map[string]string{"Idempotency-Key": stale["commandId"].(string)}, &conflict); st != 409 ||
		conflict.Error.Code != "STATE_CONFLICT" || conflict.Error.Details["currentVersion"] == nil {
		t.Fatalf("stale version -> %d %v", st, conflict)
	}
	if st, out := ana.command(r, "room.activate", nil); st != 200 || out["committedVersion"] == nil || out["replayed"] != false {
		t.Fatalf("activate %d %v", st, out)
	}
	if st, out := ana.command(r, "room.reset", nil); st != 501 || out["error"].(map[string]any)["code"] != "RESET_NOT_CONFIGURED" {
		t.Fatalf("reset %d %v", st, out)
	}

	// Role endpoint: no Impostor game → 409.
	if st := ana.call("GET", "/api/v1/rooms/"+r.Code+"/games/current/role?epoch="+r.Epoch+"&gameRunId=x", nil, nil, nil); st != 409 {
		t.Fatalf("role without game -> %d", st)
	}
	// Outsider cannot read the room.
	eve := register(t, inst, "eve")
	if st := eve.call("GET", "/api/v1/rooms/"+r.Code+"/state?epoch="+r.Epoch, nil, nil, nil); st != 403 {
		t.Fatalf("outsider -> %d", st)
	}
	// HTTP-fallback lease lifecycle (N13–N15).
	var lease struct{ ConnectionID string }
	if st := beto.call("POST", "/api/v1/rooms/"+r.Code+"/connections", map[string]string{"epoch": r.Epoch}, map[string]string{"Idempotency-Key": newKey()}, &lease); st != 201 {
		t.Fatalf("open lease %d", st)
	}
	if st := beto.call("PUT", "/api/v1/rooms/"+r.Code+"/connections/"+lease.ConnectionID, map[string]string{"epoch": r.Epoch}, nil, nil); st != 200 {
		t.Fatalf("renew lease %d", st)
	}
	if st := beto.call("DELETE", "/api/v1/rooms/"+r.Code+"/connections/"+lease.ConnectionID+"?epoch="+r.Epoch, nil, nil, nil); st != 204 {
		t.Fatalf("close lease %d", st)
	}
	// Logout: 204, repeated 204, token dead.
	if st := beto.call("DELETE", "/api/v1/players/me", nil, nil, nil); st != 204 {
		t.Fatalf("logout %d", st)
	}
	if st := beto.call("DELETE", "/api/v1/players/me", nil, nil, nil); st != 204 {
		t.Fatalf("second logout %d", st)
	}
	if st := beto.call("GET", "/api/v1/players/me", nil, nil, nil); st != 401 {
		t.Fatalf("token after logout -> %d", st)
	}
}

type errEnvelope struct {
	Error struct {
		Code      string         `json:"code"`
		Retryable bool           `json:"retryable"`
		Details   map[string]any `json:"details"`
	} `json:"error"`
	RequestID string `json:"requestId"`
}

func jsonString(v any) string {
	var b strings.Builder
	writeJSON(&b, v)
	return b.String()
}

func writeJSON(b *strings.Builder, v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			b.WriteString(k)
			b.WriteByte(':')
			writeJSON(b, val)
		}
	case []any:
		for _, val := range x {
			writeJSON(b, val)
		}
	case string:
		b.WriteString(x)
	}
	b.WriteByte(' ')
}
