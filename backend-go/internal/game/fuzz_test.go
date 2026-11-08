package game

import (
	"context"
	"encoding/json"
	"testing"
)

// FuzzApply feeds arbitrary operations and payloads to every engine: rules
// must reject or apply, never panic, and never break the room invariants.
func FuzzApply(f *testing.F) {
	for _, op := range []string{OpGameStart, OpQuestionNext, OpQuestionSubmit, OpVoteSubmit, OpVoteCall, OpMemberKick, OpVotesClear} {
		f.Add(op, `{"game":"el-impostor","impostorCount":1}`, int64(1))
		f.Add(op, `{"type":"x","toPlayerId":"2","text":"hola","anonymous":true}`, int64(2))
		f.Add(op, `{"targetPlayerId":"9999999999999999999999","votingRoundId":""}`, int64(1))
	}
	f.Fuzz(func(t *testing.T, op, payload string, actor int64) {
		fx := newFixture(t, "ana", "beto", "caro")
		fx.data.words = []string{"gato"}
		fx.data.add(KindMostLikely, 1, "x", "{player}")
		for _, k := range []Kind{KindDirect, KindMostLikely, KindImpostor} {
			raw, _ := json.Marshal(map[string]any{"game": k})
			if _, err := Apply(context.Background(), fx.env(), fx.room, 1, Command{Operation: OpGameStart, Payload: raw}); err != nil {
				t.Fatal(err)
			}
			cmd := fx.cmd(op, nil)
			cmd.Payload = json.RawMessage(payload)
			_, _ = Apply(context.Background(), fx.env(), fx.room, actor, cmd)
			if len(fx.room.Members) == 0 {
				continue
			}
			_ = BuildView(fx.room, ViewOptions{Now: fx.now, Rules: DefaultRules()})
		}
	})
}
