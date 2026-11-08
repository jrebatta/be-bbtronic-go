package game

import "context"

// Quién es más probable voting. Reference: QuienEsMasProbableService
// registerVote/getVoteResults/checkAllVoted/clearVotes.

func voteProbable(env Env, r *Room, g *Game, actor *Member, cmd Command) (Outcome, error) {
	var p struct {
		QuestionInstanceID string `json:"questionInstanceId"`
		TargetPlayerID     string `json:"targetPlayerId"`
	}
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return Outcome{}, err
	}
	v, err := currentVoting(g, p.QuestionInstanceID)
	if err != nil {
		return Outcome{}, err
	}
	if v.Complete {
		return Outcome{}, Errf(CodeStateConflict, "la votación de esta pregunta ya terminó")
	}
	target, err := ParseID(p.TargetPlayerID, "targetPlayerId")
	if err != nil {
		return Outcome{}, err
	}
	if r.Member(target) == nil {
		return Outcome{}, Errf(CodeInvalidInput, "el jugador votado no está en la sala")
	}
	for _, b := range v.Votes {
		if b.VoterID == actor.ID {
			return Outcome{}, Errf(CodeAlreadyVoted, "ya has votado")
		}
	}
	v.Votes = append(v.Votes, Vote{VoterID: actor.ID, TargetID: target})
	reason := "voting.progress"
	if closeProbableIfComplete(env, r, g.Catalog) {
		reason = "voting.resolved"
	}
	return Outcome{Result: map[string]any{"recorded": true, "complete": v.Complete}, Changed: true, Reason: reason}, nil
}

func applyVotesClear(_ context.Context, _ Env, r *Room, _ *Member, cmd Command) (Outcome, error) {
	g, err := requireGame(r, cmd, KindMostLikely)
	if err != nil {
		return Outcome{}, err
	}
	var p struct {
		QuestionInstanceID string `json:"questionInstanceId"`
	}
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return Outcome{}, err
	}
	v, err := currentVoting(g, p.QuestionInstanceID)
	if err != nil {
		return Outcome{}, err
	}
	v.Votes, v.Complete, v.Result = nil, false, nil
	if lr := g.Catalog.LastVotingResult; lr != nil && lr.ScopeID == v.InstanceID {
		g.Catalog.LastVotingResult = nil
	}
	return Outcome{Result: map[string]any{"cleared": true}, Changed: true, Reason: "votes.cleared"}, nil
}

func currentVoting(g *Game, instanceID string) (*Voting, error) {
	cs := g.Catalog
	if cs.Voting == nil || cs.Current == nil {
		return nil, Errf(CodeStateConflict, "no hay una pregunta en votación")
	}
	if instanceID == "" {
		return nil, Errf(CodeInvalidInput, "questionInstanceId es obligatorio")
	}
	if instanceID != cs.Voting.InstanceID {
		return nil, &Error{Code: CodeStaleQuestion, Message: "la pregunta indicada ya no es la actual",
			Details: map[string]any{"currentQuestionInstanceId": cs.Voting.InstanceID}}
	}
	return cs.Voting, nil
}

// closeProbableIfComplete freezes the result once every current member voted
// (Java checkAllVoted against current users; electorate frozen afterwards, C11).
func closeProbableIfComplete(env Env, r *Room, cs *CatalogState) bool {
	v := cs.Voting
	if v == nil || v.Complete || len(v.Votes) == 0 || len(r.Members) == 0 {
		return false
	}
	for _, m := range r.Members {
		if !hasVoted(v.Votes, m.ID) {
			return false
		}
	}
	res := ProbableTally(r, v.Votes)
	res.ResultID = env.id()
	res.ScopeID = v.InstanceID
	res.Reason = "all-voted"
	res.ResolvedAt = env.Now
	v.Complete = true
	v.Result = res
	cs.LastVotingResult = res
	return true
}

func hasVoted(votes []Vote, id int64) bool {
	for _, b := range votes {
		if b.VoterID == id {
			return true
		}
	}
	return false
}

// ProbableTally counts votes per member. Winner: maximum votes; ties are
// broken by the lowest member ID (join order) — explicit new rule C10, since
// the Java HashMap iteration order was not a stable rule.
func ProbableTally(r *Room, votes []Vote) *VotingResult {
	counts := map[int64]int{}
	for _, v := range votes {
		counts[v.TargetID]++
	}
	res := &VotingResult{}
	var winner int64
	best, atBest := 0, 0
	for _, m := range r.Members { // sorted by ID: the first maximum wins ties
		c := counts[m.ID]
		res.Counts = append(res.Counts, VoteCount{PlayerID: m.ID, Votes: c})
		switch {
		case c > best:
			best, winner, atBest = c, m.ID, 1
		case c == best && c > 0:
			atBest++
		}
	}
	if best > 0 {
		res.WinnerPlayerID = &winner
		res.WinnerName = r.Member(winner).Username
		res.Tie = atBest > 1
	}
	return res
}

// reconcileProbableDeparture invalidates the departed member's vote and the
// votes cast for them while the vote is open, then re-checks completion.
func reconcileProbableDeparture(env Env, r *Room, cs *CatalogState, id int64) {
	v := cs.Voting
	if v == nil || v.Complete {
		return
	}
	kept := v.Votes[:0]
	for _, b := range v.Votes {
		if b.VoterID != id && b.TargetID != id {
			kept = append(kept, b)
		}
	}
	v.Votes = kept
	closeProbableIfComplete(env, r, cs)
}
