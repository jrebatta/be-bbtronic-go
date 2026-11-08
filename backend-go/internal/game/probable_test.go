package game

import "testing"

func startProbable(t *testing.T, f *fixture, texts ...string) {
	t.Helper()
	for i, txt := range texts {
		f.data.add(KindMostLikely, int64(i+1), "normal", txt)
	}
	f.do(1, OpGameStart, map[string]any{"game": string(KindMostLikely)})
}

func instance(f *fixture) string { return f.room.State.Game.Catalog.Voting.InstanceID }

func vote(f *fixture, voter, target int64) Outcome {
	f.t.Helper()
	return f.do(voter, OpVoteSubmit, map[string]any{"questionInstanceId": instance(f), "targetPlayerId": pid(target)})
}

// F27–F30: {player} rendering, one vote per member, completion and winner.
func TestProbable_VotingCompletesWithWinner(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	startProbable(t, f, "¿Quién es más probable que {player} invite?")
	next(f, "NORMAL")
	cq := f.view().Game.CurrentQuestion
	if cq.ToPlayerName == nil || cq.Text != "¿Quién es más probable que "+*cq.ToPlayerName+" invite?" {
		t.Fatalf("render %+v", cq)
	}
	vote(f, 1, 2)
	f.fail(1, OpVoteSubmit, map[string]any{"questionInstanceId": instance(f), "targetPlayerId": pid(3)}, CodeAlreadyVoted)
	vote(f, 2, 2)
	v := f.view().Game.Voting
	if v.Complete || v.VotedCount != 2 || len(v.PendingPlayerIDs) != 1 || *v.LeaderPlayerID != "2" {
		t.Fatalf("partial %+v", v)
	}
	out := vote(f, 3, 3)
	if out.Reason != "voting.resolved" {
		t.Fatalf("reason %s", out.Reason)
	}
	gv := f.view().Game
	if !gv.Voting.Complete || *gv.Voting.WinnerPlayerID != "2" || gv.LastVotingResult.WinnerName != "beto" {
		t.Fatalf("result %+v", gv.Voting)
	}
	f.fail(1, OpVoteSubmit, map[string]any{"questionInstanceId": instance(f), "targetPlayerId": pid(1)}, CodeStateConflict)
}

// C10: ties resolve to the lowest member ID; zero votes means no winner.
func TestProbableTally_Tiebreak(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	res := ProbableTally(f.room, []Vote{{1, 3}, {2, 2}})
	if *res.WinnerPlayerID != 2 || !res.Tie {
		t.Fatalf("tie must go to lowest ID: %+v", res)
	}
	if res := ProbableTally(f.room, nil); res.WinnerPlayerID != nil {
		t.Fatal("no votes: no winner")
	}
}

func TestProbable_TypeExhaustedKeepsVotes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	startProbable(t, f, "uno", "dos")
	next(f, "normal")
	vote(f, 2, 1)
	f.fail(1, OpQuestionNext, map[string]any{"type": "picante"}, CodeQuestionTypeExhausted)
	if f.view().Game.Voting.VotedCount != 1 {
		t.Fatal("a failed next must not clear votes (Java defect fixed)")
	}
}

// F31: clear-votes keeps the question.
func TestProbable_ClearVotes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	startProbable(t, f, "uno", "dos")
	next(f, "normal")
	q := f.view().Game.CurrentQuestion.ID
	vote(f, 1, 2)
	vote(f, 2, 2)
	f.do(1, OpVotesClear, map[string]any{"questionInstanceId": instance(f)})
	gv := f.view().Game
	if gv.Voting.VotedCount != 0 || gv.Voting.Complete || gv.LastVotingResult != nil || gv.CurrentQuestion.ID != q {
		t.Fatalf("after clear %+v", gv.Voting)
	}
	f.fail(1, OpVotesClear, map[string]any{"questionInstanceId": "old"}, CodeStaleQuestion)
	f.fail(2, OpVotesClear, map[string]any{"questionInstanceId": instance(f)}, CodeForbidden)
}

// C11: a departure invalidates votes by and for the player; joiners vote while
// the vote is open; completed results are frozen.
func TestProbable_MembershipChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	startProbable(t, f, "uno", "dos")
	next(f, "normal")
	vote(f, 1, 3)
	vote(f, 2, 1)
	RemoveMember(f.env(), f.room, 3, RemovalLeft)
	v := f.room.State.Game.Catalog.Voting
	if len(v.Votes) != 1 || v.Complete {
		t.Fatalf("votes for the departed player must be removed: %+v", v)
	}
	f.room.AddMember(&Member{ID: 4, Username: "dani"})
	vote(f, 1, 2)
	if v.Complete {
		t.Fatal("the joiner is part of the open electorate")
	}
	vote(f, 4, 2)
	if !v.Complete || *v.Result.WinnerPlayerID != 2 {
		t.Fatalf("result %+v", v.Result)
	}
	f.room.AddMember(&Member{ID: 5, Username: "eli"})
	if !f.view().Game.Voting.Complete {
		t.Fatal("a completed vote stays frozen after later joins")
	}
}

func TestProbable_VoteValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	startProbable(t, f, "uno")
	f.fail(1, OpVoteSubmit, map[string]any{"questionInstanceId": "x", "targetPlayerId": pid(2)}, CodeStateConflict)
	next(f, "normal")
	f.fail(1, OpVoteSubmit, map[string]any{"questionInstanceId": instance(f), "targetPlayerId": pid(9)}, CodeInvalidInput)
	f.fail(1, OpVoteSubmit, map[string]any{"questionInstanceId": "old", "targetPlayerId": pid(2)}, CodeStaleQuestion)
}
