package game

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// El Impostor. Reference: ElImpostorService and ImpostorGameState.

// Winners.
const (
	WinnerCivilians = "civilians"
	WinnerImpostors = "impostors"
)

func startImpostor(ctx context.Context, env Env, r *Room, k int) error {
	n := len(r.Members)
	if n == 0 {
		return Errf(CodeInvalidInput, "no hay jugadores en la sala")
	}
	if k < 1 {
		return Errf(CodeInvalidInput, "debe haber al menos 1 impostor")
	}
	if k >= n { // effective minimum: two players (Java rule, not three)
		return &Error{Code: CodeInvalidInput, Message: "debe haber al menos 1 civil",
			Details: map[string]any{"players": n, "impostorCount": k}}
	}
	words, err := env.Data.ImpostorWords(ctx)
	if err != nil {
		return fmt.Errorf("loading impostor words: %w", err)
	}
	if len(words) == 0 {
		return Errf(CodeCatalogEmpty, "no hay palabras disponibles para El Impostor")
	}
	used := r.State.ImpostorUsedWords
	var available []string
	for _, w := range words {
		if !slices.Contains(used, w) {
			available = append(available, w)
		}
	}
	if len(available) == 0 { // all used: reset the room history
		used = nil
		available = words
	}
	word := available[env.Rand.IntN(len(available))]
	if !slices.Contains(used, word) {
		used = append(used, word)
	}
	r.State.ImpostorUsedWords = used

	ids := make([]int64, n)
	roster := make([]RosterEntry, n)
	for i, m := range r.Members {
		ids[i] = m.ID
		roster[i] = RosterEntry{ID: m.ID, Name: m.Username}
	}
	shuffled := slices.Clone(ids)
	shuffle(env.Rand, shuffled)
	impostors := slices.Clone(shuffled[:k])
	slices.Sort(impostors)

	g := newGame(env, KindImpostor)
	g.Phase = PhaseDescription
	g.Impostor = &ImpostorState{
		Roster:          roster,
		ImpostorIDs:     impostors,
		Word:            word,
		ImpostorCount:   k,
		Alive:           ids,
		CountdownEndsAt: env.Now.Add(env.Rules.ImpostorCountdown),
	}
	r.State.Game = g // a new run: deadlines of any previous run cannot act (C12)
	return nil
}

func applyVoteCall(_ context.Context, env Env, r *Room, actor *Member, cmd Command) (Outcome, error) {
	g, err := requireGame(r, cmd, KindImpostor)
	if err != nil {
		return Outcome{}, err
	}
	imp := g.Impostor
	if !containsID(imp.Alive, actor.ID) {
		return Outcome{}, Errf(CodeForbidden, "solo un jugador vivo puede llamar a votación")
	}
	if g.Phase != PhaseDescription {
		return Outcome{}, Errf(CodeStateConflict, "no se puede llamar a votación en la fase actual")
	}
	for _, c := range imp.Calls {
		if c.PlayerID == actor.ID {
			if wait := env.Rules.ImpostorPersonalCooldown - env.Now.Sub(c.At); wait > 0 {
				return Outcome{}, cooldownErr("espera antes de volver a llamar a votación", wait, "personal")
			}
		}
	}
	if imp.LastVotingEndedAt != nil {
		if wait := env.Rules.ImpostorGlobalCooldown - env.Now.Sub(*imp.LastVotingEndedAt); wait > 0 {
			return Outcome{}, cooldownErr("espera antes de llamar otra votación", wait, "global")
		}
	}
	now := env.Now
	deadline := now.Add(env.Rules.ImpostorVotingDuration)
	g.Phase = PhaseVoting
	imp.VotingRoundID = env.id()
	imp.Votes = nil
	imp.Deadline = &deadline
	imp.LastCalledBy = &actor.ID
	imp.LastCalledAt = &now
	imp.Calls = slices.DeleteFunc(imp.Calls, func(c CallRecord) bool { return c.PlayerID == actor.ID })
	imp.Calls = append(imp.Calls, CallRecord{PlayerID: actor.ID, At: now})
	return Outcome{
		Result:  map[string]any{"votingRoundId": imp.VotingRoundID, "deadline": deadline},
		Changed: true, Reason: "voting.started",
	}, nil
}

func cooldownErr(msg string, wait time.Duration, scope string) *Error {
	return &Error{Code: CodeCooldownActive, Message: msg, RetryAfter: wait,
		Details: map[string]any{"scope": scope, "retryAfterMs": wait.Milliseconds()}}
}

func voteImpostor(env Env, r *Room, g *Game, actor *Member, cmd Command) (Outcome, error) {
	var p struct {
		VotingRoundID  string `json:"votingRoundId"`
		TargetPlayerID string `json:"targetPlayerId"`
	}
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return Outcome{}, err
	}
	imp := g.Impostor
	if g.Phase != PhaseVoting {
		return Outcome{}, Errf(CodeStateConflict, "no hay votación activa")
	}
	if p.VotingRoundID == "" {
		return Outcome{}, Errf(CodeInvalidInput, "votingRoundId es obligatorio")
	}
	if p.VotingRoundID != imp.VotingRoundID {
		return Outcome{}, &Error{Code: CodeStaleRound, Message: "la votación indicada ya no es la actual",
			Details: map[string]any{"currentVotingRoundId": imp.VotingRoundID}}
	}
	if !containsID(imp.Alive, actor.ID) {
		return Outcome{}, Errf(CodeForbidden, "solo los jugadores vivos pueden votar")
	}
	target, err := ParseID(p.TargetPlayerID, "targetPlayerId")
	if err != nil {
		return Outcome{}, err
	}
	if !containsID(imp.Alive, target) { // self-vote is allowed (Java)
		return Outcome{}, Errf(CodeInvalidInput, "el jugador votado no está entre los vivos")
	}
	if hasVoted(imp.Votes, actor.ID) {
		return Outcome{}, Errf(CodeAlreadyVoted, "ya has votado")
	}
	imp.Votes = append(imp.Votes, Vote{VoterID: actor.ID, TargetID: target})
	reason := "voting.progress"
	if len(pendingVoters(imp)) == 0 {
		reason = resolveImpostorVote(env, r, g, "all-voted")
	}
	return Outcome{Result: map[string]any{"recorded": true}, Changed: true, Reason: reason}, nil
}

func pendingVoters(imp *ImpostorState) []int64 {
	var pending []int64
	for _, id := range imp.Alive {
		if !hasVoted(imp.Votes, id) {
			pending = append(pending, id)
		}
	}
	return pending
}

// resolveImpostorVote closes the current vote exactly once (processVotingResult):
// zero votes or a tied maximum eliminate nobody; a unique maximum is eliminated.
// It returns the snapshot reason.
func resolveImpostorVote(env Env, r *Room, g *Game, reason string) string {
	imp := g.Impostor
	counts := map[int64]int{}
	for _, v := range imp.Votes {
		counts[v.TargetID]++
	}
	res := &VotingResult{
		ResultID: env.id(), ScopeID: imp.VotingRoundID, Reason: reason,
		PendingAtClose: pendingVoters(imp), ResolvedAt: env.Now,
	}
	best, atBest := 0, 0
	var top int64
	for _, id := range imp.Alive { // counts include zeros for every alive player
		c := counts[id]
		res.Counts = append(res.Counts, VoteCount{PlayerID: id, Votes: c})
		switch {
		case c > best:
			best, top, atBest = c, id, 1
		case c == best && c > 0:
			atBest++
		}
	}
	res.Tie = best == 0 || atBest > 1
	if !res.Tie {
		res.EliminatedPlayerID = &top
		res.EliminatedName = rosterName(imp, top)
		imp.Alive = removeID(imp.Alive, top)
		imp.Eliminated = append(imp.Eliminated, top)
	}
	imp.LastVotingResult = res
	imp.Votes = nil
	imp.Deadline = nil
	if winner := impostorWinner(imp); winner != "" {
		endImpostor(env, r, g, winner)
		return "game.finished"
	}
	ended := env.Now
	imp.LastVotingEndedAt = &ended // the global cooldown starts here
	g.Phase = PhaseDescription
	return "voting.resolved"
}

// impostorWinner implements checkEndCondition.
func impostorWinner(imp *ImpostorState) string {
	impAlive := 0
	for _, id := range imp.Alive {
		if containsID(imp.ImpostorIDs, id) {
			impAlive++
		}
	}
	civAlive := len(imp.Alive) - impAlive
	switch {
	case len(imp.Alive) == 0 || impAlive == 0:
		return WinnerCivilians
	case impAlive >= civAlive:
		return WinnerImpostors
	}
	return ""
}

func endImpostor(env Env, r *Room, g *Game, winner string) {
	imp := g.Impostor
	msg := "Los impostores igualaron en número — ¡ganaron!"
	if winner == WinnerCivilians {
		msg = "Todos los impostores fueron eliminados"
	}
	var names []RosterEntry
	for _, id := range imp.ImpostorIDs {
		names = append(names, RosterEntry{ID: id, Name: rosterName(imp, id)})
	}
	r.State.LastGameResult = &GameResult{
		ResultID: env.id(), GameRunID: g.RunID, Kind: KindImpostor, Winner: winner,
		ImpostorPlayerIDs: slices.Clone(imp.ImpostorIDs), Impostors: names, Message: msg,
		FinalVoting: imp.LastVotingResult, EndedAt: env.Now,
	}
	r.State.Game = nil // Java endGame sets currentGame=null; word history is kept
}

// impostorDeparture applies C11/C12 when an alive participant leaves: they
// are eliminated, their vote and the votes against them are invalidated, an
// open vote continues while voters remain pending, and victory is re-checked.
// Unlike Java, a departure does not restart the global cooldown.
func impostorDeparture(env Env, r *Room, g *Game, id int64, reason string) {
	imp := g.Impostor
	if !containsID(imp.Alive, id) {
		return // spectator or already eliminated
	}
	imp.Alive = removeID(imp.Alive, id)
	imp.Eliminated = append(imp.Eliminated, id)
	imp.LastDeparture = &Departure{PlayerID: id, Reason: reason, At: env.Now}
	if g.Phase == PhaseVoting {
		imp.Votes = slices.DeleteFunc(imp.Votes, func(v Vote) bool { return v.VoterID == id || v.TargetID == id })
		if len(pendingVoters(imp)) == 0 {
			resolveImpostorVote(env, r, g, "all-voted")
			return
		}
	}
	if winner := impostorWinner(imp); winner != "" {
		endImpostor(env, r, g, winner)
	}
}

func rosterName(imp *ImpostorState, id int64) string {
	for _, e := range imp.Roster {
		if e.ID == id {
			return e.Name
		}
	}
	return ""
}

// Role is the private role of one participant.
type Role struct {
	GameRunID string  `json:"gameRunId"`
	Role      string  `json:"role"`
	Word      *string `json:"word"`
}

// ImpostorRole returns the role of actorID; only roster participants of the
// given run get one (C5). Spectators get ROLE_UNAVAILABLE.
func ImpostorRole(r *Room, actorID int64, gameRunID string) (Role, error) {
	g := r.State.Game
	if g == nil || g.Kind != KindImpostor {
		return Role{}, Errf(CodeStateConflict, "no hay una partida de El Impostor activa")
	}
	if gameRunID == "" {
		return Role{}, Errf(CodeInvalidInput, "gameRunId es obligatorio")
	}
	if gameRunID != g.RunID {
		return Role{}, Errf(CodeStaleGame, "la partida indicada ya no es la actual")
	}
	imp := g.Impostor
	if rosterName(imp, actorID) == "" {
		return Role{}, Errf(CodeRoleUnavailable, "te uniste después del inicio: eres espectador hasta la próxima partida")
	}
	if containsID(imp.ImpostorIDs, actorID) {
		return Role{GameRunID: g.RunID, Role: "impostor"}, nil
	}
	w := imp.Word
	return Role{GameRunID: g.RunID, Role: "civil", Word: &w}, nil
}
