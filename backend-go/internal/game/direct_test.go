package game

import (
	"context"
	"testing"
)

func startDirectGame(t *testing.T, f *fixture) {
	t.Helper()
	f.do(1, OpGameStart, map[string]any{"game": string(KindDirect)})
}

func submit(f *fixture, from, to int64, text string, anon bool) string {
	f.t.Helper()
	out := f.do(from, OpQuestionSubmit, map[string]any{"toPlayerId": pid(to), "text": text, "anonymous": anon})
	return out.Result.(map[string]any)["questionId"].(string)
}

func readyAll(f *fixture) {
	for _, m := range f.room.Members {
		f.do(m.ID, OpMemberReady, nil)
	}
}

// F17: start creates a WAITING_QUESTIONS round, resets ready and the shown set.
func TestDirect_Start(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	f.do(2, OpMemberReady, nil)
	startDirectGame(t, f)
	g := f.room.State.Game
	if g.Kind != KindDirect || g.Phase != PhaseWaitingQuestions || g.RoundID == "" || !f.room.Activated {
		t.Fatalf("game = %+v", g)
	}
	if f.room.Member(2).Ready || f.data.cleared != 1 {
		t.Fatal("start must reset ready and shown questions")
	}
}

// F19: only during WAITING_QUESTIONS, author from the token, validated recipient.
func TestDirect_Submit(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	startDirectGame(t, f)
	submit(f, 2, 1, "¿Pregunta?", true)
	if q := f.data.inserted[0]; q.From != "beto" || q.To != "ana" || !q.Anonymous {
		t.Fatalf("stored question %+v: real author must be persisted", q)
	}
	f.fail(2, OpQuestionSubmit, map[string]any{"toPlayerId": pid(9), "text": "x", "anonymous": false}, CodeInvalidInput)
	f.fail(2, OpQuestionSubmit, map[string]any{"toPlayerId": pid(1), "text": " ", "anonymous": false}, CodeInvalidInput)
	f.fail(2, OpQuestionSubmit, map[string]any{"toPlayerId": pid(1), "text": "x"}, CodeInvalidInput)

	stale := f.cmd(OpQuestionSubmit, map[string]any{"toPlayerId": pid(1), "text": "x", "anonymous": false})
	stale.RoundID = "old-round"
	if _, err := Apply(context.Background(), f.env(), f.room, 2, stale); !IsCode(err, CodeStaleRound) {
		t.Fatalf("stale round: %v", err)
	}
	if v := f.view(); v.Game.CurrentQuestion != nil || v.Game.Progress.TotalQuestions != 1 {
		t.Fatal("texts must not be published before selection")
	}
}

// F15: check-ready requires everyone ready and at least one question; the
// lowest-ID question becomes current.
func TestDirect_CheckReady(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	f.data.templates = []string{"Tengo toda la noche {usuarios}..."}
	startDirectGame(t, f)

	f.do(1, OpMemberReady, nil)
	out := f.do(1, OpRoundCheckReady, nil)
	res := out.Result.(map[string]any)
	if res["allReady"] != false || res["waitingMessage"] != "Tengo toda la noche beto, caro..." || out.Changed {
		t.Fatalf("pending result %+v", res)
	}
	if ids := res["pendingPlayerIds"].([]string); len(ids) != 2 || ids[0] != "2" {
		t.Fatalf("pending ids %v", ids)
	}

	readyAll(f)
	f.fail(1, OpRoundCheckReady, nil, CodeNoQuestions)

	f.room.State.Game.Phase = PhaseWaitingQuestions
	q1 := submit(f, 2, 3, "primera", false)
	submit(f, 3, 2, "segunda", false)
	out = f.do(1, OpRoundCheckReady, nil)
	if !out.Changed || f.room.State.Game.Phase != PhaseInProgress {
		t.Fatalf("round must start: %+v", out)
	}
	v := f.view()
	if cq := v.Game.CurrentQuestion; cq == nil || cq.ID != q1 || cq.Number != 1 || cq.Total != 2 {
		t.Fatalf("current question %+v", v.Game.CurrentQuestion)
	}
	f.fail(2, OpQuestionSubmit, map[string]any{"toPlayerId": pid(1), "text": "tarde", "anonymous": false}, CodeStateConflict)
}

func TestDirect_CheckReadyDefaultMessage(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	out := f.do(1, OpRoundCheckReady, nil)
	if msg := out.Result.(map[string]any)["waitingMessage"]; msg != "Aún faltan usuarios por estar listos: ana, beto" {
		t.Fatalf("message %q", msg)
	}
}

// F21/C3: next avoids the previous recipient when possible, never repeats,
// stores the real current ID and ends as COMPLETED.
func TestDirect_NextAvoidsRecipientAndCompletes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	startDirectGame(t, f)
	submit(f, 2, 3, "a caro 1", false) // id 101 (first by ID)
	submit(f, 1, 3, "a caro 2", true)  // id 102
	submit(f, 3, 2, "a beto", false)   // id 103
	readyAll(f)
	f.do(1, OpRoundCheckReady, nil) // current 101 -> caro

	out := f.do(1, OpQuestionNext, nil)
	if got := out.Result.(map[string]any)["questionId"]; got != "103" {
		t.Fatalf("next must avoid caro: got %v", got)
	}
	out = f.do(1, OpQuestionNext, nil) // only 102 left, recipient repeat allowed
	if got := out.Result.(map[string]any)["questionId"]; got != "102" {
		t.Fatalf("got %v", got)
	}
	v := f.view()
	cq := v.Game.CurrentQuestion
	if cq.ID != "102" || cq.FromPlayerName != nil || !*cq.Anonymous || cq.Number != 3 {
		t.Fatalf("anonymous projection %+v", cq)
	}
	if f.data.inserted[1].From != "ana" {
		t.Fatal("the stored author must never be altered")
	}
	out = f.do(1, OpQuestionNext, nil)
	if out.Result.(map[string]any)["exhausted"] != true || f.room.State.Game.Phase != PhaseCompleted {
		t.Fatal("exhaustion must complete the round")
	}
	if v := f.view(); v.Game.CurrentQuestion == nil || v.Game.CurrentQuestion.ID != "102" {
		t.Fatal("the last question stays visible after completion")
	}
	if again := f.do(1, OpQuestionNext, nil); again.Changed {
		t.Fatal("next after completion is a no-op")
	}
}

func TestDirect_NextRequiresInProgress(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	startDirectGame(t, f)
	f.fail(1, OpQuestionNext, nil, CodeStateConflict)
	f.fail(2, OpQuestionNext, nil, CodeForbidden)
}

// F18: a new round starts empty; previous questions are preserved in storage.
func TestDirect_NewRound(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	startDirectGame(t, f)
	submit(f, 2, 1, "vieja", false)
	oldRound := f.room.State.Game.RoundID
	out := f.do(1, OpRoundNew, nil)
	g := f.room.State.Game
	if g.RoundID == oldRound || g.Phase != PhaseWaitingQuestions || out.Result.(map[string]any)["roundId"] != g.RoundID {
		t.Fatalf("round %+v", g)
	}
	if len(f.room.Direct.Questions) != 0 || len(f.data.inserted) != 1 {
		t.Fatal("new round must not show previous questions nor delete them")
	}
}

func TestDirect_RecipientLeftKeepsName(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	startDirectGame(t, f)
	submit(f, 2, 3, "para caro", false)
	readyAll(f)
	f.do(1, OpRoundCheckReady, nil)
	RemoveMember(f.env(), f.room, 3, RemovalLeft)
	cq := f.view().Game.CurrentQuestion
	if cq.ToPlayerID != nil || cq.ToPlayerName == nil || *cq.ToPlayerName != "caro" {
		t.Fatalf("recipient projection %+v", cq)
	}
}
