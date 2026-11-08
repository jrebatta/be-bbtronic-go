package game

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- test doubles ----------------------------------------------------------

type fakeData struct {
	catalogs  map[Kind][]CatalogItem
	texts     map[int64]string
	words     []string
	templates []string
	nextQID   int64
	inserted  []DirectQuestion
	shown     map[int64]bool
	cleared   int
}

func newFakeData() *fakeData {
	return &fakeData{catalogs: map[Kind][]CatalogItem{}, texts: map[int64]string{}, shown: map[int64]bool{}, nextQID: 100}
}

func (f *fakeData) add(kind Kind, id int64, typ, text string) {
	f.catalogs[kind] = append(f.catalogs[kind], CatalogItem{ID: id, Type: typ})
	f.texts[id] = text
}

func (f *fakeData) CatalogItems(_ context.Context, k Kind) ([]CatalogItem, error) {
	return append([]CatalogItem(nil), f.catalogs[k]...), nil
}
func (f *fakeData) CatalogText(_ context.Context, _ Kind, id int64) (string, bool, error) {
	t, ok := f.texts[id]
	return t, ok, nil
}
func (f *fakeData) ImpostorWords(context.Context) ([]string, error)    { return f.words, nil }
func (f *fakeData) WaitingTemplates(context.Context) ([]string, error) { return f.templates, nil }
func (f *fakeData) InsertDirectQuestion(_ context.Context, _ int64, _ string, q DirectQuestion) (int64, error) {
	f.nextQID++
	q.ID = f.nextQID
	f.inserted = append(f.inserted, q)
	return f.nextQID, nil
}
func (f *fakeData) AddShownQuestion(_ context.Context, _, id int64) error {
	f.shown[id] = true
	return nil
}
func (f *fakeData) ClearShownQuestions(context.Context, int64) error {
	f.shown = map[int64]bool{}
	f.cleared++
	return nil
}

// seqRand returns the queued values (mod n), then 0.
type seqRand struct{ vals []int }

func (s *seqRand) IntN(n int) int {
	if len(s.vals) == 0 {
		return 0
	}
	v := s.vals[0]
	s.vals = s.vals[1:]
	return v % n
}

type fixture struct {
	t    testing.TB
	room *Room
	data *fakeData
	rnd  *seqRand
	now  time.Time
	ids  int
}

func newFixture(t testing.TB, names ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, data: newFakeData(), rnd: &seqRand{}, now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	host := int64(1)
	f.room = &Room{ID: 10, Code: "1234", Epoch: "epoch-1", HostID: &host, CreatorName: names[0]}
	for i, n := range names {
		f.room.AddMember(&Member{ID: int64(i + 1), Username: n})
	}
	return f
}

func (f *fixture) env() Env {
	return Env{Now: f.now, Rand: f.rnd, Rules: DefaultRules(), Data: f.data, NewID: func() string {
		f.ids++
		return fmt.Sprintf("id-%d", f.ids)
	}}
}

func (f *fixture) cmd(op string, payload any) Command {
	c := Command{Operation: op}
	if g := f.room.State.Game; g != nil {
		c.GameRunID, c.RoundID = g.RunID, g.RoundID
	}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			f.t.Fatal(err)
		}
		c.Payload = b
	}
	return c
}

func (f *fixture) do(actor int64, op string, payload any) Outcome {
	f.t.Helper()
	out, err := Apply(context.Background(), f.env(), f.room, actor, f.cmd(op, payload))
	if err != nil {
		f.t.Fatalf("%s by %d: unexpected error %v", op, actor, err)
	}
	return out
}

func (f *fixture) fail(actor int64, op string, payload any, want Code) {
	f.t.Helper()
	_, err := Apply(context.Background(), f.env(), f.room, actor, f.cmd(op, payload))
	if !IsCode(err, want) {
		f.t.Fatalf("%s by %d: got error %v, want code %s", op, actor, err, want)
	}
}

func (f *fixture) view() View {
	return BuildView(f.room, ViewOptions{Now: f.now, Grace: 20 * time.Second, Rules: DefaultRules()})
}

func pid(id int64) string { return FormatID(id) }

// --- generic operations ------------------------------------------------------

func TestApply_Permissions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		actor int64
		op    string
		want  Code
	}{
		{name: "non host cannot activate", actor: 2, op: OpRoomActivate, want: CodeForbidden},
		{name: "non host cannot start a game", actor: 2, op: OpGameStart, want: CodeForbidden},
		{name: "non host cannot reset ready", actor: 2, op: OpMembersResetReady, want: CodeForbidden},
		{name: "non member is rejected", actor: 99, op: OpMemberReady, want: CodeForbidden},
		{name: "unknown operation", actor: 1, op: "room.destroy", want: CodeInvalidInput},
		{name: "reset is blocked until reset_game_data is defined", actor: 1, op: OpRoomReset, want: CodeResetNotConfigured},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, "ana", "beto")
			f.fail(tt.actor, tt.op, nil, tt.want)
		})
	}
}

func TestApply_ReadyAndReset(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	out := f.do(2, OpMemberReady, map[string]any{"ready": true})
	if !out.Changed || !f.room.Member(2).Ready {
		t.Fatal("member.ready must mark only the actor")
	}
	if f.room.Member(1).Ready || f.room.Member(3).Ready {
		t.Fatal("member.ready must not affect other members (no impersonation)")
	}
	if again := f.do(2, OpMemberReady, nil); again.Changed {
		t.Fatal("repeated ready must be a no-op")
	}
	f.fail(2, OpMemberReady, map[string]any{"ready": false}, CodeInvalidInput)

	v := f.view()
	if v.Readiness.Ready != 1 || v.Readiness.Total != 3 || v.Readiness.AllReady {
		t.Fatalf("readiness = %+v", v.Readiness)
	}
	out = f.do(1, OpMembersResetReady, nil)
	if out.Result.(map[string]any)["resetCount"] != 1 || f.room.Member(2).Ready {
		t.Fatalf("resetReady result %+v", out.Result)
	}
}

func TestApply_ActivateIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	if out := f.do(1, OpRoomActivate, nil); !out.Changed || !f.room.Activated {
		t.Fatal("activate must set game_started")
	}
	if out := f.do(1, OpRoomActivate, nil); out.Changed {
		t.Fatal("second activate must not change state")
	}
}

func TestApply_Kick(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto", "caro")
	out := f.do(1, OpMemberKick, map[string]any{"playerId": pid(2)})
	if f.room.Member(2) != nil || len(out.RemovedMemberIDs) != 1 || out.RemovedMemberIDs[0] != 2 {
		t.Fatalf("kick did not remove member: %+v", out)
	}
	f.fail(1, OpMemberKick, map[string]any{"playerId": pid(2)}, CodeNotFound)
	f.fail(1, OpMemberKick, map[string]any{"playerId": pid(1)}, CodeInvalidInput)
	f.fail(1, OpMemberKick, map[string]any{"playerId": "abc"}, CodeInvalidInput)
	f.fail(3, OpMemberKick, map[string]any{"playerId": pid(1)}, CodeForbidden)
}

func TestRemoveMember_HostLeavesWithoutSuccession(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	RemoveMember(f.env(), f.room, 1, RemovalLeft)
	if f.room.HostID != nil {
		t.Fatal("host must not be transferred automatically")
	}
	if v := f.view(); v.Room.HostPlayerID != nil || v.Room.CreatorName != "ana" {
		t.Fatalf("view room = %+v", v.Room)
	}
	f.fail(2, OpRoomActivate, nil, CodeForbidden)
}

func TestApply_GameStartValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	f.fail(1, OpGameStart, map[string]any{"game": "ajedrez"}, CodeInvalidInput)
	f.fail(1, OpGameStart, map[string]any{"game": string(KindDirect), "impostorCount": 1}, CodeInvalidInput)
	f.fail(1, OpGameStart, map[string]any{"game": string(KindDirect), "extra": true}, CodeInvalidInput)
	f.fail(1, OpGameStart, map[string]any{"game": string(KindNeverEver)}, CodeCatalogEmpty)
}

func TestApply_GameEnd(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "ana", "beto")
	if out := f.do(1, OpGameEnd, nil); out.Changed {
		t.Fatal("ending without a game is a no-op")
	}
	f.do(1, OpGameStart, map[string]any{"game": string(KindDirect)})
	run := f.room.State.Game.RunID
	stale := f.cmd(OpGameEnd, nil)
	stale.GameRunID = "other"
	if _, err := Apply(context.Background(), f.env(), f.room, 1, stale); !IsCode(err, CodeStaleGame) {
		t.Fatalf("stale end: %v", err)
	}
	out := f.do(1, OpGameEnd, nil)
	if f.room.State.Game != nil || out.Result.(map[string]any)["endedGameRunId"] != run {
		t.Fatalf("end result %+v", out.Result)
	}
	if v := f.view(); v.Room.Status != "lobby" || v.Game != nil || len(v.Players) != 2 {
		t.Fatalf("after end: %+v", v.Room)
	}
}

func TestCleanText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{name: "plain", in: "¿Qué harías?", ok: true},
		{name: "trimmed", in: "  hola  ", ok: true},
		{name: "blank", in: "   ", ok: false},
		{name: "control char", in: "a\x00b", ok: false},
		{name: "newline allowed", in: "a\nb", ok: true},
		{name: "too long", in: strings.Repeat("ñ", 256), ok: false},
		{name: "max length", in: strings.Repeat("ñ", 255), ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := cleanText(tt.in, 255, "text")
			if (err == nil) != tt.ok {
				t.Fatalf("cleanText(%q) err=%v want ok=%t", tt.in, err, tt.ok)
			}
		})
	}
}

func TestNewID_Format(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 100 {
		id := NewID()
		if len(id) != 36 || id[14] != '4' || seen[id] {
			t.Fatalf("bad uuid %q", id)
		}
		seen[id] = true
	}
}
