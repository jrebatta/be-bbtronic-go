//go:build integration

package coordinator_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/coordinator"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
)

// F02: unique global names, uniform validation, hashed token only.
func TestRegister(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.register("Ana María")
	if _, err := h.svc.Register(ctx, "Ana María"); !game.IsCode(err, game.CodeNameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	for _, bad := range []string{"", "   ", "a\x00b", strings.Repeat("x", 33), "<script>"} {
		if _, err := h.svc.Register(ctx, bad); !game.IsCode(err, game.CodeInvalidInput) {
			t.Fatalf("name %q accepted: %v", bad, err)
		}
	}
	if n := h.db.Count(t, `SELECT count(*) FROM users WHERE id = $1 AND session_token IS NULL AND length(token_hash) = 32`, p.PlayerID); n != 1 {
		t.Fatal("only the SHA-256 of the token may be stored")
	}
	if _, err := h.svc.Authenticate(ctx, "not-a-token"); !game.IsCode(err, game.CodeUnauthenticated) {
		t.Fatal("malformed token must be rejected")
	}

	// Concurrent registrations of the same name create exactly one player.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 10 {
		wg.Go(func() {
			if _, err := h.svc.Register(ctx, "Beto"); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if ok != 1 || h.db.Count(t, `SELECT count(*) FROM users WHERE username = 'Beto'`) != 1 {
		t.Fatalf("concurrent duplicate registration: %d successes", ok)
	}
}

// An orphan registration (never joined) frees its name after 15 minutes;
// legacy Java rows are never reclaimed.
func TestRegister_OrphanAndLegacyNames(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.register("caro")
	h.advance(16 * time.Minute)
	if _, err := h.svc.Register(ctx, "caro"); err != nil {
		t.Fatalf("orphan name not reclaimed: %v", err)
	}
	h.db.Exec(t, `INSERT INTO users (username, ready, session_token) VALUES ('legacy', false, 'java-uuid')`)
	h.advance(24 * time.Hour)
	if _, err := h.svc.Register(ctx, "legacy"); !game.IsCode(err, game.CodeNameTaken) {
		t.Fatalf("legacy row must not be reclaimed: %v", err)
	}
}

// F06/F08: create → host member; idempotent retry; single membership; join.
func TestCreateAndJoin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ana := h.register("ana")
	key := h.key()
	r1, err := h.svc.CreateRoom(ctx, ana.Actor, key)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := h.svc.CreateRoom(ctx, ana.Actor, key)
	if err != nil || r2.RoomCode != r1.RoomCode || !r2.Replayed {
		t.Fatalf("retry with the same key must replay: %+v %v", r2, err)
	}
	if n := h.db.Count(t, `SELECT count(*) FROM game_session WHERE epoch IS NOT NULL`); n != 1 {
		t.Fatalf("retry created %d rooms", n)
	}
	if _, err := h.svc.CreateRoom(ctx, ana.Actor, h.key()); !game.IsCode(err, game.CodeAlreadyInRoom) {
		t.Fatalf("second membership: %v", err)
	}
	r := room{code: r1.RoomCode, epoch: r1.Epoch}
	ana = h.refresh(ana)
	v := h.state(ana, r)
	if *v.Room.HostPlayerID != playerID(ana) || len(v.Players) != 1 || !v.Players[0].IsHost || v.Room.CreatorName != "ana" {
		t.Fatalf("host view %+v", v.Room)
	}

	beto := h.register("beto")
	if _, err := h.svc.JoinRoom(ctx, beto.Actor, "0000", "", h.key()); !game.IsCode(err, game.CodeRoomNotFound) {
		t.Fatalf("invalid code: %v", err)
	}
	if _, err := h.svc.JoinRoom(ctx, beto.Actor, r.code, "00000000-0000-4000-8000-000000000000", h.key()); !game.IsCode(err, game.CodeStaleEpoch) {
		t.Fatalf("stale epoch: %v", err)
	}
	res := h.join(beto, r)
	if res.Participation != "player" || h.join(beto, r).RoomCode != r.code {
		t.Fatal("join must be idempotent for a member")
	}

	// Two players joining concurrently both become members.
	c, d := h.register("caro"), h.register("dani")
	var wg sync.WaitGroup
	for _, p := range []player{c, d} {
		wg.Go(func() { h.join(p, r) })
	}
	wg.Wait()
	if v := h.state(h.refresh(ana), r); len(v.Players) != 4 {
		t.Fatalf("members after concurrent joins: %d", len(v.Players))
	}
}

// F07/F11/F14 + C2: the snapshot is member-only and contains no secrets.
func TestState_PrivacyAndAuthorization(t *testing.T) {
	h := newHarness(t)
	r, ps := h.lobby("ana", "beto")
	b, _ := json.Marshal(h.state(ps[0], r))
	for _, secret := range []string{ps[0].token, ps[1].token, "token", "session"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("snapshot leaks %q", secret)
		}
	}
	outsider := h.register("eve")
	if _, err := h.svc.State(context.Background(), outsider.Actor, r.code, r.epoch); !game.IsCode(err, game.CodeForbidden) {
		t.Fatalf("outsider read: %v", err)
	}
	if _, err := h.svc.State(context.Background(), ps[0].Actor, r.code, ""); !game.IsCode(err, game.CodeInvalidInput) {
		t.Fatalf("epoch required: %v", err)
	}
}

// F05/F09/F16: own ready, host-only kick, kicked token stops working.
func TestReadyKick(t *testing.T) {
	h := newHarness(t)
	r, ps := h.lobby("ana", "beto", "caro")
	h.do(ps[1], r, game.OpMemberReady, map[string]any{"ready": true})
	v := h.state(ps[0], r)
	if v.Readiness.Ready != 1 || !v.Players[1].Ready || v.Players[2].Ready {
		t.Fatalf("readiness %+v", v.Readiness)
	}
	h.fail(ps[1], r, game.OpMemberKick, map[string]any{"playerId": playerID(ps[2])}, game.CodeForbidden)
	h.do(ps[0], r, game.OpMemberKick, map[string]any{"playerId": playerID(ps[2])})
	if _, err := h.svc.Authenticate(context.Background(), ps[2].token); !game.IsCode(err, game.CodeUnauthenticated) {
		t.Fatalf("kicked token must stop working (Java deletes the user): %v", err)
	}
	if v := h.state(ps[0], r); len(v.Players) != 2 {
		t.Fatal("kicked member still listed")
	}
	h.do(ps[0], r, game.OpMembersResetReady, nil)
	if v := h.state(ps[0], r); v.Readiness.Ready != 0 {
		t.Fatal("reset ready")
	}
}

// F04: logout leaves, frees the name, is idempotent; host leaves without
// succession; the last member closes the room logically (no deletion).
func TestLogout(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r, ps := h.lobby("ana", "beto")
	if err := h.svc.Logout(ctx, ps[0].Actor); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Logout(ctx, ps[0].Actor); err != nil {
		t.Fatalf("repeated logout must be a no-op: %v", err)
	}
	v := h.state(ps[1], r)
	if v.Room.HostPlayerID != nil || len(v.Players) != 1 {
		t.Fatalf("after host logout %+v", v.Room)
	}
	if _, err := h.svc.Register(ctx, "ana"); err != nil {
		t.Fatalf("logout must free the name: %v", err)
	}
	if err := h.svc.Logout(ctx, ps[1].Actor); err != nil {
		t.Fatal(err)
	}
	if n := h.db.Count(t, `SELECT count(*) FROM game_session WHERE session_code IS NULL AND runtime_state->>'closedCode' = $1 AND closed_at IS NOT NULL`, r.code); n != 1 {
		t.Fatal("empty room must be closed, not deleted")
	}
}

// T11: replay returns the confirmed result; another payload with the same
// key is rejected; 20 concurrent retries produce a single effect.
func TestIdempotency(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r, ps := h.lobby("ana", "beto")
	req := h.request(ps[0], r, game.OpRoomActivate, nil)
	first, err := h.svc.Execute(ctx, ps[0].Actor, r.code, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.svc.Execute(ctx, ps[0].Actor, r.code, req)
	if err != nil || !again.Replayed || again.CommittedVersion != first.CommittedVersion {
		t.Fatalf("replay %+v %v", again, err)
	}
	other := req
	other.Payload = json.RawMessage(`{"x":1}`)
	if _, err := h.svc.Execute(ctx, ps[0].Actor, r.code, other); !game.IsCode(err, game.CodeIdempotencyKeyReused) {
		t.Fatalf("key reuse: %v", err)
	}

	before := version(t, h.state(ps[0], r).StateVersion)
	ready := h.request(ps[1], r, game.OpMemberReady, nil)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := h.svc.Execute(ctx, ps[1].Actor, r.code, ready); err != nil {
				t.Errorf("concurrent retry: %v", err)
			}
		})
	}
	wg.Wait()
	if after := version(t, h.state(ps[0], r).StateVersion); after != before+1 {
		t.Fatalf("20 retries must commit once: version %d -> %d", before, after)
	}
}

// T10/F21: two hosts' tabs sending next with the same version → one wins.
func TestConcurrentControlCommands(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r, ps := h.lobby("ana", "beto")
	reqs := []coordinator.CommandRequest{
		h.request(ps[0], r, game.OpGameStart, map[string]any{"game": "preguntas-directas"}),
		h.request(ps[0], r, game.OpGameStart, map[string]any{"game": "preguntas-directas"}),
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range reqs {
		wg.Go(func() { _, errs[i] = h.svc.Execute(ctx, ps[0].Actor, r.code, reqs[i]) })
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case game.IsCode(err, game.CodeStateConflict):
			conflicts++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

// F10: reset stays blocked until reset_game_data is defined.
func TestRoomResetBlocked(t *testing.T) {
	h := newHarness(t)
	r, ps := h.lobby("ana")
	h.fail(ps[0], r, game.OpRoomReset, nil, game.CodeResetNotConfigured)
}

// C1/C9: legacy Java rows are never modified and do not block codes; idle
// rooms expire logically and release their code.
func TestLegacyRowsAndExpiry(t *testing.T) {
	h := newHarness(t)
	h.db.Exec(t, `INSERT INTO game_session (session_code, creator_name, game_started) VALUES ('1234', 'java', false)`)
	h.db.Exec(t, `INSERT INTO users (username, ready, session_token, game_session_id) SELECT 'javauser', false, 'tok', id FROM game_session WHERE creator_name = 'java'`)
	r, ps := h.lobby("ana", "beto")
	if n := h.db.Count(t, `SELECT count(*) FROM game_session WHERE creator_name = 'java' AND epoch IS NULL AND closed_at IS NULL`); n != 1 {
		t.Fatal("legacy room modified")
	}
	if n := h.db.Count(t, `SELECT count(*) FROM users WHERE username = 'javauser' AND token_hash IS NULL`); n != 1 {
		t.Fatal("legacy user modified")
	}
	h.advance(2*time.Hour + time.Minute)
	_, err := h.svc.Execute(context.Background(), ps[0].Actor, r.code, h.requestNoState(r, game.OpRoomActivate, "1"))
	if !game.IsCode(err, game.CodeRoomExpired) {
		t.Fatalf("idle room: %v", err)
	}
	if n := h.db.Count(t, `SELECT count(*) FROM game_session WHERE session_code IS NULL AND runtime_state->>'closedCode' = $1 AND closed_at IS NOT NULL`, r.code); n != 1 {
		t.Fatal("expiry must be committed as a logical close")
	}
	// Members of the closed room may create a new one.
	if _, err := h.svc.CreateRoom(context.Background(), h.refresh(ps[1]).Actor, h.key()); err != nil {
		t.Fatalf("membership in a closed room must not block: %v", err)
	}
}

func (h *harness) requestNoState(r room, op, ver string) coordinator.CommandRequest {
	return coordinator.CommandRequest{CommandID: h.key(), Epoch: r.epoch, Operation: op, ExpectedVersion: &ver}
}

// fixedRand always draws the same value: every room gets the same code.
type fixedRand struct{}

func (fixedRand) IntN(n int) int { return 821 % n }

// The real database has UNIQUE(session_code): closing a room releases its
// code (session_code = NULL, kept in runtime_state.closedCode) so it can be
// reused without deleting rooms.
func TestClosedRoomReleasesCode(t *testing.T) {
	h := newHarness(t)
	h.svc.SetRand(fixedRand{})
	ana := h.register("ana")
	first := h.createRoom(ana)
	if first.code != "1821" {
		t.Fatalf("code %s", first.code)
	}
	beto := h.register("beto")
	if _, err := h.svc.CreateRoom(context.Background(), beto.Actor, h.key()); !game.IsCode(err, game.CodeLimitReached) {
		t.Fatalf("a code held by an active room must not be reused: %v", err)
	}
	if err := h.svc.Logout(context.Background(), h.refresh(ana).Actor); err != nil { // last member leaves → room closed
		t.Fatal(err)
	}
	second := h.createRoom(beto)
	if second.code != first.code || second.epoch == first.epoch {
		t.Fatalf("released code must be reusable by a new generation: %+v %+v", first, second)
	}
	if n := h.db.Count(t, `SELECT count(*) FROM game_session WHERE runtime_state->>'closedCode' = '1821' AND session_code IS NULL`); n != 1 {
		t.Fatal("closed room must keep its code in runtime_state")
	}
}
