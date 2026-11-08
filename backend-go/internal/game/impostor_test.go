package game

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func startImp(t *testing.T, f *fixture, k int) {
	t.Helper()
	if f.data.words == nil {
		f.data.words = []string{"gato", "perro", "sushi"}
	}
	f.do(1, OpGameStart, map[string]any{"game": string(KindImpostor), "impostorCount": k})
}

func imp(f *fixture) *ImpostorState { return f.room.State.Game.Impostor }

func callVote(f *fixture, actor int64) {
	f.t.Helper()
	f.do(actor, OpVoteCall, nil)
}

func impVote(f *fixture, voter, target int64) Outcome {
	f.t.Helper()
	return f.do(voter, OpVoteSubmit, map[string]any{"votingRoundId": imp(f).VotingRoundID, "targetPlayerId": pid(target)})
}

// F34: 1 <= k < N, two players suffice, countdown persisted, roles assigned.
func TestImpostor_StartValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	f.data.words = []string{"gato"}
	f.fail(1, OpGameStart, map[string]any{"game": string(KindImpostor), "impostorCount": 0}, CodeInvalidInput)
	f.fail(1, OpGameStart, map[string]any{"game": string(KindImpostor), "impostorCount": 2}, CodeInvalidInput)
	f.do(1, OpGameStart, map[string]any{"game": string(KindImpostor)}) // default k=1
	s := imp(f)
	if len(s.ImpostorIDs) != 1 || len(s.Alive) != 2 || !s.CountdownEndsAt.Equal(f.now.Add(3*time.Second)) {
		t.Fatalf("state %+v", s)
	}
	if f.room.State.Game != nil && f.room.State.Game.Phase != PhaseDescription {
		t.Fatal("two players: the game must not end at start")
	}
	empty := newFixture(t, "ana", "beto")
	empty.fail(1, OpGameStart, map[string]any{"game": string(KindImpostor)}, CodeCatalogEmpty)
}

// History: words are not repeated until exhausted, then the history resets.
func TestImpostor_WordHistory(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	f.data.words = []string{"gato", "perro"}
	startImp(t, f, 1)
	first := imp(f).Word
	f.do(1, OpGameEnd, nil)
	startImp(t, f, 1)
	second := imp(f).Word
	if first == second {
		t.Fatal("word repeated before exhausting the catalog")
	}
	f.do(1, OpGameEnd, nil)
	startImp(t, f, 1)
	if len(f.room.State.ImpostorUsedWords) != 1 {
		t.Fatalf("history must reset when exhausted: %v", f.room.State.ImpostorUsedWords)
	}
}

// F35/C5: civil sees the word, impostor sees null, spectators get nothing, and
// the public view never contains the word or the impostor list.
func TestImpostor_RolesArePrivate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	f.data.words = []string{"jirafa"}
	startImp(t, f, 1)
	impID := imp(f).ImpostorIDs[0]
	for _, m := range f.room.Members {
		role, err := ImpostorRole(f.room, m.ID, f.room.State.Game.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if m.ID == impID && (role.Role != "impostor" || role.Word != nil) {
			t.Fatalf("impostor role %+v", role)
		}
		if m.ID != impID && (role.Role != "civil" || *role.Word != "jirafa") {
			t.Fatalf("civil role %+v", role)
		}
	}
	f.room.AddMember(&Member{ID: 9, Username: "tarde"})
	if _, err := ImpostorRole(f.room, 9, f.room.State.Game.RunID); !IsCode(err, CodeRoleUnavailable) {
		t.Fatalf("spectator: %v", err)
	}
	if _, err := ImpostorRole(f.room, 1, "old-run"); !IsCode(err, CodeStaleGame) {
		t.Fatalf("stale run: %v", err)
	}
	b, _ := json.Marshal(f.view())
	if strings.Contains(string(b), "jirafa") || strings.Contains(string(b), "impostorIds") || strings.Contains(string(b), "usedWords") {
		t.Fatalf("secret leaked in public view: %s", b)
	}
	for _, p := range f.view().Players {
		if p.ID == "9" && p.Participation != "spectator" {
			t.Fatal("late joiner must be a spectator")
		}
	}
	f.fail(9, OpVoteCall, nil, CodeForbidden)
}

// F36: personal cooldown 120 s, global cooldown 60 s after a vote ends.
func TestImpostor_Cooldowns(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro", "dani")
	startImp(t, f, 1)
	f.room.State.Game.Impostor.ImpostorIDs = []int64{4}
	callVote(f, 1)
	f.fail(2, OpVoteCall, nil, CodeStateConflict) // already voting
	// everyone votes a civilian-tie: 1->2, 2->1, 3->1? make a tie: 1->2,2->1,3->3,4->4
	impVote(f, 1, 2)
	impVote(f, 2, 1)
	impVote(f, 3, 3)
	impVote(f, 4, 4)
	if f.room.State.Game.Phase != PhaseDescription || !imp(f).LastVotingResult.Tie {
		t.Fatal("tie must eliminate nobody and return to description")
	}
	f.now = f.now.Add(59 * time.Second)
	_, err := Apply(t.Context(), f.env(), f.room, 2, f.cmd(OpVoteCall, nil))
	e, ok := AsError(err)
	if !ok || e.Code != CodeCooldownActive || e.Details["scope"] != "global" || e.RetryAfter != time.Second {
		t.Fatalf("global cooldown: %v", err)
	}
	f.now = f.now.Add(time.Second)
	f.fail(1, OpVoteCall, nil, CodeCooldownActive) // personal: 60 s since call < 120 s
	callVote(f, 2)
}

// F37: majority eliminates, timeout resolves once, late votes are rejected,
// victory for civilians and for impostors, result retained.
func TestImpostor_VotingAndVictory(t *testing.T) {
	t.Parallel()
	t.Run("civilians win", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, "ana", "beto", "caro")
		startImp(t, f, 1)
		f.room.State.Game.Impostor.ImpostorIDs = []int64{3}
		run := f.room.State.Game.RunID
		callVote(f, 1)
		impVote(f, 1, 3)
		impVote(f, 2, 3)
		out := impVote(f, 3, 3) // self-vote allowed
		if out.Reason != "game.finished" || f.room.State.Game != nil {
			t.Fatalf("reason %s", out.Reason)
		}
		res := f.view().LastGameResult
		if res.Winner != WinnerCivilians || res.GameRunID != run || res.ImpostorPlayerIDs[0] != "3" ||
			*res.FinalVoting.EliminatedPlayerID != "3" || res.Impostors[0].Name != "caro" {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("impostors win when they equal civilians", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, "ana", "beto", "caro")
		startImp(t, f, 1)
		f.room.State.Game.Impostor.ImpostorIDs = []int64{3}
		callVote(f, 1)
		impVote(f, 1, 2)
		impVote(f, 2, 2)
		impVote(f, 3, 2)
		if f.view().LastGameResult.Winner != WinnerImpostors {
			t.Fatal("1 impostor vs 1 civilian: impostors win")
		}
	})
	t.Run("timeout resolves once and rejects late votes", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, "ana", "beto", "caro", "dani", "eli")
		startImp(t, f, 1)
		f.room.State.Game.Impostor.ImpostorIDs = []int64{5}
		callVote(f, 1)
		round := imp(f).VotingRoundID
		impVote(f, 1, 2)
		if DeadlineDue(f.room, f.now.Add(59*time.Second)) {
			t.Fatal("deadline not due yet")
		}
		f.now = f.now.Add(60 * time.Second)
		if !DeadlineDue(f.room, f.now) || !AdvanceDue(f.env(), f.room) {
			t.Fatal("timeout must resolve")
		}
		if AdvanceDue(f.env(), f.room) {
			t.Fatal("resolution must happen exactly once")
		}
		r := imp(f).LastVotingResult
		if r.Reason != "timeout" || r.Tie || *r.EliminatedPlayerID != 2 || len(r.PendingAtClose) != 4 {
			t.Fatalf("timeout result %+v", r)
		}
		f.fail(3, OpVoteSubmit, map[string]any{"votingRoundId": round, "targetPlayerId": pid(1)}, CodeStateConflict)
	})
	t.Run("zero votes is a tie", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, "ana", "beto", "caro")
		startImp(t, f, 1)
		callVote(f, 1)
		f.now = f.now.Add(61 * time.Second)
		AdvanceDue(f.env(), f.room)
		if r := imp(f).LastVotingResult; !r.Tie || r.EliminatedPlayerID != nil || len(imp(f).Alive) != 3 {
			t.Fatalf("zero votes %+v", r)
		}
	})
}

func TestImpostor_VoteValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	startImp(t, f, 1)
	f.fail(1, OpVoteSubmit, map[string]any{"votingRoundId": "x", "targetPlayerId": pid(2)}, CodeStateConflict)
	callVote(f, 1)
	f.fail(1, OpVoteSubmit, map[string]any{"votingRoundId": "old", "targetPlayerId": pid(2)}, CodeStaleRound)
	f.fail(1, OpVoteSubmit, map[string]any{"votingRoundId": imp(f).VotingRoundID, "targetPlayerId": pid(9)}, CodeInvalidInput)
	impVote(f, 1, 2)
	f.fail(1, OpVoteSubmit, map[string]any{"votingRoundId": imp(f).VotingRoundID, "targetPlayerId": pid(3)}, CodeAlreadyVoted)
	if v := f.view().Game.Voting; v.VotedCount != 1 || len(v.PendingPlayerIDs) != 2 || v.Counts != nil {
		t.Fatalf("impostor progress must hide raw counts: %+v", v)
	}
}

// C12: departures invalidate votes by/for the player and keep voting open.
func TestImpostor_DepartureDuringVoting(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro", "dani", "eli")
	startImp(t, f, 1)
	f.room.State.Game.Impostor.ImpostorIDs = []int64{5}
	callVote(f, 1)
	impVote(f, 1, 3)
	impVote(f, 2, 3)
	impVote(f, 3, 1)
	RemoveMember(f.env(), f.room, 3, RemovalDisconnect)
	s := imp(f)
	if f.room.State.Game.Phase != PhaseVoting || len(s.Votes) != 0 {
		t.Fatalf("votes by and for the departed must be removed; phase=%s votes=%v", f.room.State.Game.Phase, s.Votes)
	}
	if s.LastDeparture.Reason != RemovalDisconnect || containsID(s.Alive, 3) {
		t.Fatal("departed participant must be eliminated")
	}
	before := s.LastVotingEndedAt
	RemoveMember(f.env(), f.room, 5, RemovalLeft) // impostor leaves: civilians win
	if f.room.State.Game != nil || f.room.State.LastGameResult.Winner != WinnerCivilians || before != nil {
		t.Fatal("impostor departure must end the game for civilians")
	}
}

func TestImpostor_NewRunInvalidatesOldDeadline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	startImp(t, f, 1)
	callVote(f, 1)
	startImp(t, f, 1)
	f.now = f.now.Add(2 * time.Minute)
	if AdvanceDue(f.env(), f.room) {
		t.Fatal("a deadline of the previous run must not act on the new one")
	}
}
