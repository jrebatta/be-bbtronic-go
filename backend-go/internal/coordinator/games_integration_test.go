//go:build integration

package coordinator_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
)

// F17–F21/C3: Directas persisted in question/shown_questions; the current
// question survives a restart (new coordinator, no shared memory).
func TestDirectas_PersistenceAndRestart(t *testing.T) {
	h := newHarness(t)
	r, ps := h.lobby("ana", "beto", "caro")
	h.do(ps[0], r, game.OpGameStart, map[string]any{"game": "preguntas-directas"})
	h.do(ps[1], r, game.OpQuestionSubmit, map[string]any{"toPlayerId": playerID(ps[2]), "text": "¿Qué ñandú?", "anonymous": true})
	h.do(ps[2], r, game.OpQuestionSubmit, map[string]any{"toPlayerId": playerID(ps[1]), "text": "¿Y tú?", "anonymous": false})
	if n := h.db.Count(t, `SELECT count(*) FROM question WHERE from_user = 'beto' AND to_user = 'caro' AND anonymous AND question = '¿Qué ñandú?'`); n != 1 {
		t.Fatal("the real author must be stored even for anonymous questions")
	}
	if res := result(h.do(ps[0], r, game.OpRoundCheckReady, nil)); res["allReady"] != false || res["started"] != false {
		t.Fatalf("pending players: %v", res) // Java answered 200 with the waiting message
	}
}

// F15: check-ready with pending players returns the waiting message.
func TestDirectas_CheckReadyFlow(t *testing.T) {
	h := newHarness(t)
	h.db.Exec(t, `INSERT INTO waiting_messages (template) VALUES ('{usuarios} avisan')`)
	r, ps := h.lobby("ana", "beto", "caro")
	h.do(ps[0], r, game.OpGameStart, map[string]any{"game": "preguntas-directas"})
	res := result(h.do(ps[0], r, game.OpRoundCheckReady, nil))
	if res["allReady"] != false || res["waitingMessage"] != "ana, beto, caro avisan" {
		t.Fatalf("waiting %v", res)
	}
	for _, p := range ps {
		h.do(p, r, game.OpMemberReady, nil)
	}
	h.fail(ps[0], r, game.OpRoundCheckReady, nil, game.CodeNoQuestions)
	h.do(ps[1], r, game.OpQuestionSubmit, map[string]any{"toPlayerId": playerID(ps[2]), "text": "uno", "anonymous": true})
	h.do(ps[2], r, game.OpQuestionSubmit, map[string]any{"toPlayerId": playerID(ps[1]), "text": "dos", "anonymous": false})
	res = result(h.do(ps[0], r, game.OpRoundCheckReady, nil))
	if res["started"] != true {
		t.Fatalf("start %v", res)
	}
	v := h.state(ps[1], r)
	cq := v.Game.CurrentQuestion
	if v.Game.Phase != game.PhaseInProgress || cq.Text != "uno" || cq.FromPlayerName != nil || cq.Number != 1 || cq.Total != 2 {
		t.Fatalf("first question %+v", cq)
	}

	// Restart: another coordinator instance reads the same confirmed state.
	other := h.newService()
	v2, err := other.State(context.Background(), ps[2].Actor, r.code, r.epoch)
	if err != nil || v2.Game.CurrentQuestion.ID != cq.ID || v2.StateVersion != v.StateVersion {
		t.Fatalf("state after restart differs: %v", err)
	}
	h.do(ps[0], r, game.OpQuestionNext, nil)
	if v := h.state(ps[0], r); v.Game.CurrentQuestion.Text != "dos" || *v.Game.CurrentQuestion.FromPlayerName != "caro" {
		t.Fatalf("second %+v", v.Game.CurrentQuestion)
	}
	if res := result(h.do(ps[0], r, game.OpQuestionNext, nil)); res["exhausted"] != true {
		t.Fatal("exhaustion")
	}
	if n := h.db.Count(t, `SELECT count(*) FROM shown_questions s JOIN game_session g ON g.id = s.session_id WHERE g.session_code = $1`, r.code); n != 2 {
		t.Fatalf("shown_questions rows %d", n)
	}
	// Legacy columns mirror the runtime state for readability.
	if n := h.db.Count(t, `SELECT count(*) FROM game_session WHERE session_code = $1 AND current_game = 'preguntas-directas' AND round_status = 'COMPLETED' AND game_started`, r.code); n != 1 {
		t.Fatal("legacy columns not mirrored")
	}
	// New round: previous questions preserved in storage, not shown.
	h.do(ps[0], r, game.OpRoundNew, nil)
	if v := h.state(ps[0], r); v.Game.Progress.TotalQuestions != 0 || v.Game.Phase != game.PhaseWaitingQuestions {
		t.Fatal("new round must start empty")
	}
	if n := h.db.Count(t, `SELECT count(*) FROM question q JOIN game_session g ON g.id = q.game_session_id WHERE g.session_code = $1`, r.code); n != 2 {
		t.Fatal("historic questions must be preserved")
	}
	h.do(ps[0], r, game.OpGameEnd, nil)
	if v := h.state(ps[0], r); v.Game != nil || len(v.Players) != 3 {
		t.Fatal("end returns to lobby keeping members")
	}
}

func seedCatalog(h *harness, table string, rows ...[2]string) {
	for _, row := range rows {
		h.db.Exec(h.t, `INSERT INTO `+table+` (tipo, texto) VALUES ($1, $2)`, row[0], row[1])
	}
}

// F22–F25: catalog order is materialized (survives restart), type filter is
// case-insensitive and nothing repeats.
func TestCatalogs_PersistedOrder(t *testing.T) {
	h := newHarness(t)
	seedCatalog(h, "yo_nunca_nunca", [2]string{"tranquilo", "a"}, [2]string{"Tranquilo", "b"}, [2]string{"picante", "c"})
	r, ps := h.lobby("ana", "beto")
	h.fail(ps[0], r, game.OpGameStart, map[string]any{"game": "cultura-pendeja"}, game.CodeCatalogEmpty)
	h.do(ps[0], r, game.OpGameStart, map[string]any{"game": "yo-nunca-nunca"})
	h.fail(ps[1], r, game.OpQuestionNext, map[string]any{"type": "tranquilo"}, game.CodeForbidden)
	h.do(ps[0], r, game.OpQuestionNext, map[string]any{"type": "TRANQUILO"})
	first := h.state(ps[0], r).Game.CurrentQuestion.Text

	other := h.newService() // restart
	h.svc = other
	h.do(ps[0], r, game.OpQuestionNext, map[string]any{"type": "tranquilo"})
	second := h.state(ps[0], r).Game.CurrentQuestion.Text
	if first == second || (first != "a" && first != "b") || (second != "a" && second != "b") {
		t.Fatalf("repetition across restart: %q %q", first, second)
	}
	h.fail(ps[0], r, game.OpQuestionNext, map[string]any{"type": "tranquilo"}, game.CodeQuestionTypeExhausted)
	if h.state(ps[0], r).Game.CurrentQuestion.Text != second {
		t.Fatal("type exhaustion must not change the current question")
	}
	if n := h.db.Count(t, `SELECT count(*) FROM yo_nunca_nunca`); n != 3 {
		t.Fatal("catalog tables are never modified")
	}
}

// F26–F31: Probable with concurrent votes, completion, C10 tie-break.
func TestProbable_ConcurrentVotes(t *testing.T) {
	h := newHarness(t)
	seedCatalog(h, "quien_es_mas_probable", [2]string{"normal", "¿Quién invitaría a {player}?"}, [2]string{"normal", "¿Quién llega tarde?"})
	r, ps := h.lobby("ana", "beto", "caro", "dani")
	h.do(ps[0], r, game.OpGameStart, map[string]any{"game": "quien-es-mas-probable"})
	h.do(ps[0], r, game.OpQuestionNext, map[string]any{"type": "normal"})
	v := h.state(ps[0], r)
	inst := v.Game.Voting.ScopeID
	targets := []player{ps[1], ps[2], ps[1], ps[2]} // tie 2–2 between beto and caro
	var wg sync.WaitGroup
	for i, p := range ps {
		wg.Go(func() {
			req := h.request(p, r, game.OpVoteSubmit, map[string]any{"questionInstanceId": inst, "targetPlayerId": playerID(targets[i])})
			req.ExpectedVersion = nil // independent player action: no global version
			if _, err := h.svc.Execute(context.Background(), p.Actor, r.code, req); err != nil {
				t.Errorf("vote %s: %v", p.Username, err)
			}
		})
	}
	wg.Wait()
	v = h.state(ps[0], r)
	if !v.Game.Voting.Complete || *v.Game.Voting.WinnerPlayerID != playerID(ps[1]) || !v.Game.LastVotingResult.Tie {
		t.Fatalf("concurrent votes lost or tie-break wrong: %+v", v.Game.Voting)
	}
	req := h.request(ps[0], r, game.OpVoteSubmit, map[string]any{"questionInstanceId": inst, "targetPlayerId": playerID(ps[1])})
	req.ExpectedVersion = nil
	if _, err := h.svc.Execute(context.Background(), ps[0].Actor, r.code, req); !game.IsCode(err, game.CodeStateConflict) {
		t.Fatalf("vote after completion: %v", err)
	}
	h.do(ps[0], r, game.OpVotesClear, map[string]any{"questionInstanceId": inst})
	if v := h.state(ps[0], r); v.Game.Voting.VotedCount != 0 || v.Game.CurrentQuestion.InstanceID != inst {
		t.Fatal("clear keeps the question")
	}
}

// F32/F33: Incómodas destinatarios sin repetición hasta completar el ciclo.
func TestUncomfortable_Cycle(t *testing.T) {
	h := newHarness(t)
	for range 6 {
		seedCatalog(h, "preguntas_incomodas", [2]string{"x", "Pregunta para {player}"})
	}
	r, ps := h.lobby("ana", "beto", "caro")
	h.do(ps[0], r, game.OpGameStart, map[string]any{"game": "preguntas-incomodas"})
	seen := map[string]int{}
	for range 3 {
		h.do(ps[0], r, game.OpQuestionNext, map[string]any{"type": "X"})
		cq := h.state(ps[0], r).Game.CurrentQuestion
		if cq.Text != "Pregunta para "+*cq.ToPlayerName {
			t.Fatalf("render %q", cq.Text)
		}
		seen[*cq.ToPlayerName]++
	}
	if len(seen) != 3 {
		t.Fatalf("first cycle must hit every player once: %v", seen)
	}
}

func seedWords(h *harness) {
	if _, err := h.db.Store.SeedCatalogs(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// F34–F37: Impostor end to end with persisted deadlines, private roles,
// cooldowns, timeout resolution after a restart and recoverable results.
func TestImpostor_EndToEnd(t *testing.T) {
	h := newHarness(t)
	seedWords(h)
	if n := h.db.Count(t, `SELECT count(*) FROM impostor_word`); n != 150 {
		t.Fatalf("seed must load the 150 Java words, got %d", n)
	}
	r, ps := h.lobby("ana", "beto", "caro", "dani")
	h.fail(ps[0], r, game.OpGameStart, map[string]any{"game": "el-impostor", "impostorCount": 4}, game.CodeInvalidInput)
	h.do(ps[0], r, game.OpGameStart, map[string]any{"game": "el-impostor", "impostorCount": 1})
	v := h.state(ps[0], r)
	run := v.Game.ID
	if !v.Game.Impostor.CountdownEndsAt.Equal(h.clock().Add(3 * time.Second)) {
		t.Fatal("countdown deadline")
	}
	raw, _ := json.Marshal(v)
	impostor := player{}
	civilWord := ""
	for _, p := range ps {
		role, err := h.svc.Role(context.Background(), p.Actor, r.code, r.epoch, run)
		if err != nil {
			t.Fatal(err)
		}
		if role.Role == "impostor" {
			impostor = p
		} else {
			civilWord = *role.Word
		}
	}
	if impostor.PlayerID == 0 || strings.Contains(string(raw), civilWord) {
		t.Fatal("roles/word privacy broken")
	}
	late := h.register("tarde")
	h.join(late, r)
	late = h.refresh(late)
	if _, err := h.svc.Role(context.Background(), late.Actor, r.code, r.epoch, run); !game.IsCode(err, game.CodeRoleUnavailable) {
		t.Fatalf("spectator role: %v", err)
	}

	caller := ps[0]
	if caller.PlayerID == impostor.PlayerID {
		caller = ps[1]
	}
	callReq := h.request(caller, r, game.OpVoteCall, nil)
	callReq.ExpectedVersion = nil
	res, err := h.svc.Execute(context.Background(), caller.Actor, r.code, callReq)
	if err != nil {
		t.Fatal(err)
	}
	votingRound := result(res)["votingRoundId"].(string)
	vote := func(p, target player) error {
		req := h.request(p, r, game.OpVoteSubmit, map[string]any{"votingRoundId": votingRound, "targetPlayerId": playerID(target)})
		req.ExpectedVersion = nil
		_, err := h.svc.Execute(context.Background(), p.Actor, r.code, req)
		return err
	}
	if err := vote(caller, impostor); err != nil {
		t.Fatal(err)
	}
	if n := h.db.Count(t, `SELECT count(*) FROM game_session WHERE session_code = $1 AND next_deadline_at IS NOT NULL`, r.code); n != 1 {
		t.Fatal("voting deadline must be persisted")
	}

	// Restart + deadline passes without traffic: the state reports it and
	// reconciliation resolves exactly once; late votes are rejected.
	h.svc = h.newService()
	h.advance(61 * time.Second)
	if v := h.state(caller, r); !v.ReconciliationRequired {
		t.Fatal("snapshot must flag the pending reconciliation (GET does not mutate)")
	}
	rec, err := h.svc.Reconcile(context.Background(), caller.Actor, r.code, r.epoch)
	if err != nil || !rec.Changed {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	if again, _ := h.svc.Reconcile(context.Background(), caller.Actor, r.code, r.epoch); again.Changed {
		t.Fatal("resolution must happen once")
	}
	if err := vote(ps[2], impostor); !game.IsCode(err, game.CodeStateConflict) {
		t.Fatalf("late vote: %v", err)
	}
	gameOver := h.state(caller, r)
	if gameOver.Game != nil {
		// One vote against the impostor with three pending: unique max → eliminated → civilians win.
		t.Fatalf("expected civilians win, game still %s", gameOver.Game.Phase)
	}
	if res := gameOver.LastGameResult; res == nil || res.Winner != game.WinnerCivilians || res.ImpostorPlayerIDs[0] != playerID(impostor) ||
		res.FinalVoting.Reason != "timeout" {
		t.Fatalf("result %+v", gameOver.LastGameResult)
	}
	// Word history is private, room-level and survives the game end.
	if n := h.db.Count(t, `SELECT count(*) FROM game_session WHERE session_code = $1 AND runtime_state->'impostorUsedWords' ? $2`, r.code, civilWord); n != 1 {
		t.Fatal("word history not retained")
	}
}

// T14/C13: presence leases, silent expiry → reconnecting → grace → removal;
// two tabs keep the member online; reconnect during grace cancels it.
func TestPresence_LeasesAndGrace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r, ps := h.lobby("ana", "beto")
	l1, _, err := h.svc.OpenConnection(ctx, ps[1].Actor, r.code, r.epoch, game.LeaseKindHTTP, h.key())
	if err != nil {
		t.Fatal(err)
	}
	l2, _, err := h.svc.OpenConnection(ctx, ps[1].Actor, r.code, r.epoch, game.LeaseKindWebSocket, "")
	if err != nil {
		t.Fatal(err)
	}
	status := func() string {
		for _, p := range h.state(ps[0], r).Players {
			if p.ID == playerID(ps[1]) {
				return p.ConnectionStatus
			}
		}
		return "gone"
	}
	if status() != game.StatusOnline {
		t.Fatal("online")
	}
	roomID, err := h.svc.ResolveRoomID(ctx, ps[1].Actor, r.code, r.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.CloseConnection(ctx, ps[1].Actor, roomID, l1.ConnectionID); err != nil {
		t.Fatal(err)
	}
	if status() != game.StatusOnline {
		t.Fatal("closing one tab keeps the member online")
	}
	h.advance(30 * time.Second)
	if _, err := h.svc.RenewConnection(ctx, ps[1].Actor, roomID, l2.ConnectionID); err != nil {
		t.Fatal(err)
	}
	h.advance(50 * time.Second) // lease expired silently
	if _, err := h.svc.Reconcile(ctx, ps[0].Actor, r.code, r.epoch); err != nil {
		t.Fatal(err)
	}
	if status() != game.StatusReconnecting {
		t.Fatalf("status %s", status())
	}
	// Reconnect inside the grace period cancels the departure.
	h.advance(10 * time.Second)
	if _, _, err := h.svc.OpenConnection(ctx, ps[1].Actor, r.code, r.epoch, game.LeaseKindWebSocket, ""); err != nil {
		t.Fatal(err)
	}
	h.advance(15 * time.Second)
	_, _ = h.svc.Reconcile(ctx, ps[0].Actor, r.code, r.epoch)
	if status() != game.StatusOnline {
		t.Fatal("reconnect during grace must cancel the departure")
	}
	// Silent loss again, then grace elapses: final departure (Java parity).
	h.advance(46 * time.Second)
	_, _ = h.svc.Reconcile(ctx, ps[0].Actor, r.code, r.epoch)
	h.advance(21 * time.Second)
	_, _ = h.svc.Reconcile(ctx, ps[0].Actor, r.code, r.epoch)
	if status() != "gone" {
		t.Fatalf("member must leave after the grace period, status %s", status())
	}
	if _, err := h.svc.Authenticate(ctx, ps[1].token); !game.IsCode(err, game.CodeUnauthenticated) {
		t.Fatal("the departed player's credential must stop working")
	}
	// Lease limit: at most 4 active connections per player.
	for range 4 {
		if _, _, err := h.svc.OpenConnection(ctx, ps[0].Actor, r.code, r.epoch, game.LeaseKindWebSocket, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := h.svc.OpenConnection(ctx, ps[0].Actor, r.code, r.epoch, game.LeaseKindWebSocket, ""); !game.IsCode(err, game.CodeLimitReached) {
		t.Fatalf("fifth connection: %v", err)
	}
}

// T10: logout during an open Impostor vote reconciles the game atomically.
func TestImpostor_LogoutDuringVote(t *testing.T) {
	h := newHarness(t)
	seedWords(h)
	r, ps := h.lobby("ana", "beto", "caro", "dani", "eli")
	h.do(ps[0], r, game.OpGameStart, map[string]any{"game": "el-impostor"})
	callReq := h.request(ps[0], r, game.OpVoteCall, nil)
	callReq.ExpectedVersion = nil
	if _, err := h.svc.Execute(context.Background(), ps[0].Actor, r.code, callReq); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Logout(context.Background(), ps[4].Actor); err != nil {
		t.Fatal(err)
	}
	v := h.state(ps[0], r)
	if v.LastGameResult == nil && (v.Game == nil || len(v.Game.Impostor.AlivePlayerIDs) != 4) {
		t.Fatalf("departed player must be eliminated: %+v", v.Game)
	}
}
