package game

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Rand is the randomness source used by the engines (injected for tests).
type Rand interface {
	IntN(n int) int
}

// DefaultRand returns the process-wide math/rand/v2 source. Game randomness
// is not a security boundary; secrets (tokens) use crypto/rand elsewhere.
func DefaultRand() Rand { return defaultRand{} }

type defaultRand struct{}

func (defaultRand) IntN(n int) int { return mrand.IntN(n) }

// NewID returns a random RFC 4122 version 4 UUID string.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err)) // never returns an error on supported platforms
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:])
}

// Rules holds the tunable game constants. Defaults reproduce the Java values.
type Rules struct {
	ImpostorVotingDuration   time.Duration // ElImpostorService.VOTING_DURATION_SECONDS
	ImpostorPersonalCooldown time.Duration // PERSONAL_COOLDOWN_SECONDS (real value 120 s)
	ImpostorGlobalCooldown   time.Duration // GLOBAL_COOLDOWN_SECONDS
	ImpostorCountdown        time.Duration // 3-2-1 countdown before the game starts
	QuestionTextMaxRunes     int
	MaxQuestionsPerRound     int
	CatalogTypeMaxRunes      int
}

// DefaultRules returns the rules verified in the Spring Boot code.
func DefaultRules() Rules {
	return Rules{
		ImpostorVotingDuration:   60 * time.Second,
		ImpostorPersonalCooldown: 120 * time.Second,
		ImpostorGlobalCooldown:   60 * time.Second,
		ImpostorCountdown:        3 * time.Second,
		// question.question has no @Column length: Hibernate's default
		// varchar(255). Confirm against the real catalog before raising it.
		QuestionTextMaxRunes: 255,
		MaxQuestionsPerRound: 200,
		CatalogTypeMaxRunes:  64,
	}
}

// Data is the I/O the engines need, implemented by the store inside the
// command transaction.
type Data interface {
	CatalogItems(ctx context.Context, kind Kind) ([]CatalogItem, error)
	CatalogText(ctx context.Context, kind Kind, id int64) (text string, found bool, err error)
	ImpostorWords(ctx context.Context) ([]string, error)
	WaitingTemplates(ctx context.Context) ([]string, error)
	InsertDirectQuestion(ctx context.Context, roomID int64, roundID string, q DirectQuestion) (int64, error)
	AddShownQuestion(ctx context.Context, roomID, questionID int64) error
	ClearShownQuestions(ctx context.Context, roomID int64) error
}

// Env carries the injected dependencies of one transition.
type Env struct {
	Now   time.Time
	Rand  Rand
	Rules Rules
	Data  Data
	NewID func() string
}

func (e Env) id() string {
	if e.NewID != nil {
		return e.NewID()
	}
	return NewID()
}

// Command is a validated client intention.
type Command struct {
	Operation string
	GameRunID string
	RoundID   string
	Payload   json.RawMessage
}

// Outcome describes the effect of an applied command.
type Outcome struct {
	Result  any
	Changed bool
	Reason  string
	// RemovedMemberIDs must be deleted from users after the transition (kick).
	RemovedMemberIDs []int64
}

// Operations.
const (
	OpRoomActivate      = "room.activate"
	OpRoomReset         = "room.reset"
	OpMemberReady       = "member.ready"
	OpMembersResetReady = "members.resetReady"
	OpMemberKick        = "member.kick"
	OpGameStart         = "game.start"
	OpGameEnd           = "game.end"
	OpRoundNew          = "round.new"
	OpRoundCheckReady   = "round.checkReady"
	OpQuestionSubmit    = "question.submit"
	OpQuestionNext      = "question.next"
	OpVoteSubmit        = "vote.submit"
	OpVoteCall          = "vote.call"
	OpVotesClear        = "votes.clear"
)

type opSpec struct {
	hostOnly bool
	// versioned operations require expectedVersion (host control, 5.3).
	versioned bool
	apply     func(ctx context.Context, env Env, r *Room, actor *Member, cmd Command) (Outcome, error)
}

var operations map[string]opSpec

func init() {
	operations = map[string]opSpec{
		OpRoomActivate:      {hostOnly: true, versioned: true, apply: applyRoomActivate},
		OpRoomReset:         {hostOnly: true, versioned: true, apply: applyRoomReset},
		OpMemberReady:       {apply: applyMemberReady},
		OpMembersResetReady: {hostOnly: true, versioned: true, apply: applyMembersResetReady},
		OpMemberKick:        {hostOnly: true, versioned: true, apply: applyMemberKick},
		OpGameStart:         {hostOnly: true, versioned: true, apply: applyGameStart},
		OpGameEnd:           {hostOnly: true, versioned: true, apply: applyGameEnd},
		OpRoundNew:          {hostOnly: true, versioned: true, apply: applyRoundNew},
		OpRoundCheckReady:   {hostOnly: true, versioned: true, apply: applyRoundCheckReady},
		OpQuestionSubmit:    {apply: applyQuestionSubmit},
		OpQuestionNext:      {hostOnly: true, versioned: true, apply: applyQuestionNext},
		OpVoteSubmit:        {apply: applyVoteSubmit},
		OpVoteCall:          {apply: applyVoteCall},
		OpVotesClear:        {hostOnly: true, versioned: true, apply: applyVotesClear},
	}
}

// KnownOperation reports whether op exists.
func KnownOperation(op string) bool { _, ok := operations[op]; return ok }

// RequiresExpectedVersion reports whether op must carry expectedVersion.
func RequiresExpectedVersion(op string) bool { return operations[op].versioned }

// Apply validates permissions and applies cmd on behalf of actorID. On error
// the room may be partially modified: callers must discard it (rollback).
func Apply(ctx context.Context, env Env, r *Room, actorID int64, cmd Command) (Outcome, error) {
	spec, ok := operations[cmd.Operation]
	if !ok {
		return Outcome{}, Errf(CodeInvalidInput, "operación desconocida")
	}
	actor := r.Member(actorID)
	if actor == nil {
		return Outcome{}, Errf(CodeForbidden, "no perteneces a esta sala")
	}
	if spec.hostOnly && !r.IsHost(actorID) {
		return Outcome{}, Errf(CodeForbidden, "solo el anfitrión puede realizar esta acción")
	}
	return spec.apply(ctx, env, r, actor, cmd)
}

// AdvanceDue materializes game transitions whose deadline has passed
// (Impostor voting timeout). It is idempotent and returns true on change.
func AdvanceDue(env Env, r *Room) bool {
	g := r.State.Game
	if g == nil || g.Kind != KindImpostor || g.Phase != PhaseVoting {
		return false
	}
	imp := g.Impostor
	if imp.Deadline == nil || env.Now.Before(*imp.Deadline) {
		return false
	}
	resolveImpostorVote(env, r, g, "timeout")
	return true
}

// DeadlineDue reports whether AdvanceDue would change the room at now.
func DeadlineDue(r *Room, now time.Time) bool {
	d := r.NextDeadline()
	return d != nil && !now.Before(*d)
}

// Removal reasons.
const (
	RemovalLeft       = "left"
	RemovalKicked     = "kicked"
	RemovalDisconnect = "disconnect"
)

// RemoveMember removes a member and reconciles every engine (C11/C12): bags
// drop the player, open votes by/for the player are invalidated, Impostor
// eliminates an alive participant and re-evaluates the round.
func RemoveMember(env Env, r *Room, id int64, reason string) bool {
	if r.Member(id) == nil {
		return false
	}
	idx := -1
	for i, m := range r.Members {
		if m.ID == id {
			idx = i
		}
	}
	r.Members = append(r.Members[:idx], r.Members[idx+1:]...)
	if r.IsHost(id) {
		r.HostID = nil // no automatic succession (plan 4.6)
	}
	if g := r.State.Game; g != nil {
		switch {
		case g.Catalog != nil:
			reconcileCatalogDeparture(env, r, g, id)
		case g.Impostor != nil:
			impostorDeparture(env, r, g, id, reason)
		}
	}
	return true
}

// --- generic room operations ---------------------------------------------

func applyRoomActivate(_ context.Context, _ Env, r *Room, _ *Member, _ Command) (Outcome, error) {
	changed := !r.Activated
	r.Activated = true
	return Outcome{Result: map[string]any{"active": true}, Changed: changed, Reason: "room.activated"}, nil
}

func applyRoomReset(context.Context, Env, *Room, *Member, Command) (Outcome, error) {
	// The Java endpoint calls the stored procedure reset_game_data(code),
	// whose definition is not in the repository. Its semantics must not be
	// invented (plan 6.2/8.1): blocked until the definition is reviewed.
	return Outcome{}, Errf(CodeResetNotConfigured,
		"el reinicio de datos no está disponible: falta la definición verificada de reset_game_data")
}

func applyMemberReady(_ context.Context, _ Env, _ *Room, actor *Member, cmd Command) (Outcome, error) {
	var p struct {
		Ready *bool `json:"ready"`
	}
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return Outcome{}, err
	}
	if p.Ready != nil && !*p.Ready {
		return Outcome{}, Errf(CodeInvalidInput, "solo se puede marcar ready=true; el reinicio colectivo lo hace el anfitrión")
	}
	changed := !actor.Ready
	actor.Ready = true
	return Outcome{
		Result:  map[string]any{"playerId": FormatID(actor.ID), "ready": true},
		Changed: changed, Reason: "member.ready",
	}, nil
}

func applyMembersResetReady(_ context.Context, _ Env, r *Room, _ *Member, _ Command) (Outcome, error) {
	n := resetReady(r)
	return Outcome{Result: map[string]any{"resetCount": n}, Changed: n > 0, Reason: "members.readyReset"}, nil
}

func resetReady(r *Room) int {
	n := 0
	for _, m := range r.Members {
		if m.Ready {
			m.Ready = false
			n++
		}
	}
	return n
}

func applyMemberKick(_ context.Context, env Env, r *Room, actor *Member, cmd Command) (Outcome, error) {
	var p struct {
		PlayerID string `json:"playerId"`
	}
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return Outcome{}, err
	}
	target, err := ParseID(p.PlayerID, "playerId")
	if err != nil {
		return Outcome{}, err
	}
	if target == actor.ID {
		return Outcome{}, Errf(CodeInvalidInput, "el anfitrión no puede expulsarse; usa la salida de jugador")
	}
	if r.Member(target) == nil {
		return Outcome{}, Errf(CodeNotFound, "el jugador no está en la sala")
	}
	RemoveMember(env, r, target, RemovalKicked)
	return Outcome{
		Result:  map[string]any{"playerId": FormatID(target), "removed": true},
		Changed: true, Reason: "member.kicked", RemovedMemberIDs: []int64{target},
	}, nil
}

func applyGameStart(ctx context.Context, env Env, r *Room, _ *Member, cmd Command) (Outcome, error) {
	var p struct {
		Game          Kind `json:"game"`
		ImpostorCount *int `json:"impostorCount"`
	}
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return Outcome{}, err
	}
	if !p.Game.Valid() {
		return Outcome{}, Errf(CodeInvalidInput, "juego desconocido")
	}
	if p.ImpostorCount != nil && p.Game != KindImpostor {
		return Outcome{}, Errf(CodeInvalidInput, "impostorCount solo aplica a el-impostor")
	}
	var err error
	switch {
	case p.Game == KindDirect:
		err = startDirect(ctx, env, r)
	case p.Game.IsCatalog():
		err = startCatalog(ctx, env, r, p.Game)
	case p.Game == KindImpostor:
		k := 1 // Java default: body.getOrDefault("impostorCount", 1)
		if p.ImpostorCount != nil {
			k = *p.ImpostorCount
		}
		err = startImpostor(ctx, env, r, k)
	}
	if err != nil {
		return Outcome{}, err
	}
	g := r.State.Game
	res := map[string]any{"gameRunId": g.RunID, "phase": g.Phase, "roundId": nullString(g.RoundID)}
	if g.Impostor != nil {
		res["countdownEndsAt"] = g.Impostor.CountdownEndsAt
	}
	return Outcome{Result: res, Changed: true, Reason: "game.started"}, nil
}

func applyGameEnd(_ context.Context, _ Env, r *Room, _ *Member, cmd Command) (Outcome, error) {
	g := r.State.Game
	if g == nil {
		return Outcome{Result: map[string]any{"endedGameRunId": nil}, Reason: "game.ended"}, nil
	}
	if _, err := requireGame(r, cmd); err != nil {
		return Outcome{}, err
	}
	r.State.Game = nil // lobby; deadlines of this run can no longer act
	r.Direct = nil
	return Outcome{Result: map[string]any{"endedGameRunId": g.RunID}, Changed: true, Reason: "game.ended"}, nil
}

func applyQuestionNext(ctx context.Context, env Env, r *Room, _ *Member, cmd Command) (Outcome, error) {
	g, err := requireGame(r, cmd)
	if err != nil {
		return Outcome{}, err
	}
	switch {
	case g.Kind == KindDirect:
		return nextDirect(ctx, env, r, g, cmd)
	case g.Kind.IsCatalog():
		return nextCatalog(ctx, env, r, g, cmd)
	}
	return Outcome{}, Errf(CodeWrongGame, "el juego activo no tiene preguntas")
}

func applyVoteSubmit(_ context.Context, env Env, r *Room, actor *Member, cmd Command) (Outcome, error) {
	g, err := requireGame(r, cmd)
	if err != nil {
		return Outcome{}, err
	}
	switch g.Kind {
	case KindMostLikely:
		return voteProbable(env, r, g, actor, cmd)
	case KindImpostor:
		return voteImpostor(env, r, g, actor, cmd)
	}
	return Outcome{}, Errf(CodeWrongGame, "el juego activo no tiene votaciones")
}

// --- helpers ---------------------------------------------------------------

// requireGame checks there is an active game and that the command targets it.
func requireGame(r *Room, cmd Command, kinds ...Kind) (*Game, error) {
	g := r.State.Game
	if g == nil {
		return nil, Errf(CodeStateConflict, "no hay un juego activo")
	}
	if cmd.GameRunID == "" {
		return nil, Errf(CodeInvalidInput, "gameRunId es obligatorio")
	}
	if cmd.GameRunID != g.RunID {
		return nil, &Error{Code: CodeStaleGame, Message: "la partida indicada ya no es la actual",
			Details: map[string]any{"currentGameRunId": g.RunID}}
	}
	if len(kinds) > 0 {
		ok := false
		for _, k := range kinds {
			ok = ok || g.Kind == k
		}
		if !ok {
			return nil, Errf(CodeWrongGame, "la operación no corresponde al juego activo")
		}
	}
	return g, nil
}

// requireRound checks the command targets the current round.
func requireRound(g *Game, cmd Command) error {
	if cmd.RoundID == "" {
		return Errf(CodeInvalidInput, "roundId es obligatorio")
	}
	if cmd.RoundID != g.RoundID {
		return &Error{Code: CodeStaleRound, Message: "la ronda indicada ya no es la actual",
			Details: map[string]any{"currentRoundId": g.RoundID}}
	}
	return nil
}

func newGame(env Env, kind Kind) *Game {
	return &Game{RunID: env.id(), Kind: kind, StartedAt: env.Now}
}

// decodePayload strictly decodes a command payload; null/empty is allowed.
func decodePayload(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return Errf(CodeInvalidInput, "payload inválido para la operación")
	}
	if dec.More() {
		return Errf(CodeInvalidInput, "payload inválido para la operación")
	}
	return nil
}

// ParseID parses a decimal string identifier.
func ParseID(s, field string) (int64, error) {
	if s == "" {
		return 0, Errf(CodeInvalidInput, "%s es obligatorio", field)
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, Errf(CodeInvalidInput, "%s no es un identificador válido", field)
	}
	return id, nil
}

// FormatID renders an identifier as a decimal string (JS-safe).
func FormatID(id int64) string { return strconv.FormatInt(id, 10) }

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// shuffle is a Fisher–Yates shuffle (Collections.shuffle equivalent).
func shuffle[T any](r Rand, s []T) {
	for i := len(s) - 1; i > 0; i-- {
		j := r.IntN(i + 1)
		s[i], s[j] = s[j], s[i]
	}
}

// cleanText validates user text: trimmed, non-empty, bounded, no control
// characters except newlines and tabs.
func cleanText(s string, maxRunes int, field string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", Errf(CodeInvalidInput, "%s no puede estar vacío", field)
	}
	n := 0
	for _, c := range s {
		n++
		if c == '�' || (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			return "", Errf(CodeInvalidInput, "%s contiene caracteres no permitidos", field)
		}
	}
	if n > maxRunes {
		return "", &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("%s supera %d caracteres", field, maxRunes),
			Details: map[string]any{"maxLength": maxRunes}}
	}
	return s, nil
}
