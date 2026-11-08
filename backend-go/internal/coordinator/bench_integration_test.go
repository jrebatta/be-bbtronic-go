//go:build integration

package coordinator_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/coordinator"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/testdb"
)

// BenchmarkExecute measures one full command transaction (lock, receipt,
// rules, persistence, commit) against a local PostgreSQL. On Neon the
// absolute numbers are dominated by network round trips.
func BenchmarkExecute(b *testing.B) {
	h := &harness{t: b, now: time.Now().UTC()}
	h.db = testdb.New(b)
	h.svc = h.newService()
	r, ps := h.lobby("ana", "beto")
	h.do(ps[0], r, game.OpGameStart, map[string]any{"game": "preguntas-directas"})
	v := h.state(ps[0], r)
	ctx := context.Background()
	i := 0
	b.ReportAllocs()
	for b.Loop() {
		i++
		req := coordinator.CommandRequest{
			CommandID: fmt.Sprintf("bench-key-%016d", i), Epoch: r.epoch, GameRunID: v.Game.ID, RoundID: *v.Game.RoundID,
			Operation: game.OpQuestionSubmit,
			Payload:   []byte(fmt.Sprintf(`{"toPlayerId":%q,"text":"pregunta %d","anonymous":false}`, playerID(ps[0]), i)),
		}
		if _, err := h.svc.Execute(ctx, ps[1].Actor, r.code, req); err != nil {
			b.Fatal(err)
		}
		if i%150 == 0 { // stay under MaxQuestionsPerRound
			h.do(ps[0], r, game.OpRoundNew, nil)
			v = h.state(ps[0], r)
		}
	}
}

// BenchmarkSnapshot measures the consistent read used by GET state and the
// realtime fan-out.
func BenchmarkSnapshot(b *testing.B) {
	h := &harness{t: b, now: time.Now().UTC()}
	h.db = testdb.New(b)
	h.svc = h.newService()
	r, ps := h.lobby("ana", "beto", "caro", "dani")
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.svc.State(ctx, ps[0].Actor, r.code, r.epoch); err != nil {
			b.Fatal(err)
		}
	}
}
