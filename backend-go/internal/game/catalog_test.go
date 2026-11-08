package game

import (
	"testing"
)

func next(f *fixture, typ string) map[string]any {
	f.t.Helper()
	return f.do(1, OpQuestionNext, map[string]any{"type": typ}).Result.(map[string]any)
}

// F22–F25: catalog games shuffle, filter type case-insensitively, never repeat,
// distinguish type exhaustion from total exhaustion.
func TestCatalog_YoNuncaNuncaAndCultura(t *testing.T) {
	t.Parallel()
	for _, kind := range []Kind{KindNeverEver, KindTrivia} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, "ana", "beto")
			f.data.add(kind, 1, "tranquilo", "t1")
			f.data.add(kind, 2, "Picante", "p1")
			f.data.add(kind, 3, "tranquilo", "t2")
			f.rnd.vals = []int{0, 0} // Fisher–Yates with j=0: [1,2,3] -> [2,3,1]
			f.do(1, OpGameStart, map[string]any{"game": string(kind)})
			g := f.room.State.Game
			if g.Phase != PhaseInProgress || g.RoundID == "" || len(g.Catalog.Remaining) != 3 {
				t.Fatalf("start %+v", g)
			}

			seen := map[string]bool{}
			for range 2 {
				res := next(f, "TRANQUILO")
				id := res["questionId"].(string)
				if seen[id] {
					t.Fatalf("question %s repeated", id)
				}
				seen[id] = true
			}
			cq := f.view().Game.CurrentQuestion
			if cq.Type != "tranquilo" || (cq.Text != "t1" && cq.Text != "t2") {
				t.Fatalf("current %+v", cq)
			}
			f.fail(1, OpQuestionNext, map[string]any{"type": "tranquilo"}, CodeQuestionTypeExhausted)
			if f.view().Game.CurrentQuestion.ID != cq.ID {
				t.Fatal("type exhaustion must not mutate the current question")
			}
			if res := next(f, "picante"); res["questionId"] != "2" {
				t.Fatalf("picante %v", res)
			}
			if res := next(f, "picante"); res["exhausted"] != true || f.room.State.Game.Phase != PhaseCompleted {
				t.Fatal("total exhaustion must complete")
			}
			f.fail(1, OpQuestionNext, map[string]any{}, CodeInvalidInput)
		})
	}
}

func TestCatalog_RestartClearsPreviousGame(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	f.data.add(KindNeverEver, 1, "a", "uno")
	f.data.add(KindNeverEver, 2, "a", "dos")
	f.do(1, OpGameStart, map[string]any{"game": string(KindNeverEver)})
	next(f, "a")
	f.do(1, OpGameStart, map[string]any{"game": string(KindNeverEver)})
	g := f.room.State.Game
	if g.Catalog.Current != nil || len(g.Catalog.Remaining) != 2 {
		t.Fatal("restart must not keep the previous question (C14)")
	}
}

func TestCatalog_SkipsDeletedRows(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana")
	f.data.add(KindTrivia, 1, "a", "uno")
	f.data.add(KindTrivia, 2, "a", "dos")
	f.do(1, OpGameStart, map[string]any{"game": string(KindTrivia)})
	delete(f.data.texts, f.room.State.Game.Catalog.Remaining[0].ID)
	res := next(f, "a")
	if res["exhausted"] != false {
		t.Fatal("a deleted catalog row must be skipped")
	}
}

// F32/F33: Incómodas always draws a recipient from a bag without repetition.
func TestUncomfortable_BagWithoutRepetition(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	for i := int64(1); i <= 6; i++ {
		f.data.add(KindUncomfortable, i, "x", "Pregunta para {player}: ¿{player}?")
	}
	f.do(2, OpMemberReady, nil)
	f.do(1, OpGameStart, map[string]any{"game": string(KindUncomfortable)})
	if f.room.Member(2).Ready {
		t.Fatal("Incómodas start resets ready")
	}
	counts := map[string]int{}
	for i := range 6 {
		next(f, "x")
		cq := f.view().Game.CurrentQuestion
		name := *cq.ToPlayerName
		if cq.Text != "Pregunta para "+name+": ¿"+name+"?" {
			t.Fatalf("render %q", cq.Text)
		}
		counts[name]++
		if i == 2 {
			for n, c := range counts {
				if c != 1 {
					t.Fatalf("player %s drawn %d times in the first cycle", n, c)
				}
			}
		}
	}
	if counts["ana"] != 2 || counts["beto"] != 2 || counts["caro"] != 2 {
		t.Fatalf("two full cycles expected: %v", counts)
	}
}

// C11: departed members leave the bag; new members join the cycle.
func TestUncomfortable_BagReconcilesMembership(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	for i := int64(1); i <= 4; i++ {
		f.data.add(KindUncomfortable, i, "x", "{player}")
	}
	f.do(1, OpGameStart, map[string]any{"game": string(KindUncomfortable)})
	RemoveMember(f.env(), f.room, 2, RemovalLeft)
	f.room.AddMember(&Member{ID: 3, Username: "caro"})
	got := map[string]bool{}
	for range 2 {
		next(f, "x")
		got[*f.view().Game.CurrentQuestion.ToPlayerName] = true
	}
	if got["beto"] || !got["ana"] || !got["caro"] {
		t.Fatalf("drawn %v", got)
	}
}
