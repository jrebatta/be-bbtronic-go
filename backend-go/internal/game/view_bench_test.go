package game

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// largeRoom builds a worst-case-ish room: 30 members (MAX_ROOM_MEMBERS),
// an Impostor game mid-vote, and a 1000-item catalog history in state.
func largeRoom(b *testing.B, kind Kind) *Room {
	b.Helper()
	names := make([]string, 30)
	for i := range names {
		names[i] = fmt.Sprintf("jugador-%02d", i)
	}
	f := newFixture(b, names...)
	f.data.words = []string{"gato", "perro"}
	for i := int64(1); i <= 1000; i++ {
		f.data.add(KindMostLikely, i, []string{"normal", "picante"}[i%2], "¿Quién es más probable que {player} haga algo?")
	}
	env := f.env()
	switch kind {
	case KindImpostor:
		if err := startImpostor(b.Context(), env, f.room, 5); err != nil {
			b.Fatal(err)
		}
		g := f.room.State.Game
		g.Phase = PhaseVoting
		d := f.now.Add(time.Minute)
		g.Impostor.Deadline, g.Impostor.VotingRoundID = &d, "vr"
		for _, m := range f.room.Members[:20] {
			g.Impostor.Votes = append(g.Impostor.Votes, Vote{VoterID: m.ID, TargetID: 1})
		}
	case KindMostLikely:
		if err := startCatalog(b.Context(), env, f.room, KindMostLikely); err != nil {
			b.Fatal(err)
		}
	}
	return f.room
}

func BenchmarkBuildViewAndEncode(b *testing.B) {
	for _, kind := range []Kind{KindImpostor, KindMostLikely} {
		r := largeRoom(b, kind)
		o := ViewOptions{Now: time.Now(), Grace: 20 * time.Second, Rules: DefaultRules()}
		b.Run(string(kind), func(b *testing.B) {
			b.ReportAllocs()
			var n int
			for b.Loop() {
				v := BuildView(r, o)
				out, err := json.Marshal(v)
				if err != nil {
					b.Fatal(err)
				}
				n = len(out)
			}
			b.ReportMetric(float64(n), "snapshot-bytes")
		})
	}
}

func BenchmarkStateEncodeDecode(b *testing.B) {
	r := largeRoom(b, KindMostLikely)
	raw, _ := json.Marshal(r.State)
	b.ReportAllocs()
	for b.Loop() {
		out, err := json.Marshal(r.State)
		if err != nil {
			b.Fatal(err)
		}
		var s State
		if err := json.Unmarshal(out, &s); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(raw)), "runtime_state-bytes")
}
