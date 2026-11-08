package game

import (
	"strconv"
	"time"
)

// View is the public room snapshot (contract v1, plan 6.3). It is identical
// for every member: it never contains tokens, the Impostor word, impostor
// identities before the game ends, raw Impostor votes or unselected question
// texts. Private data goes through [ImpostorRole] only.
type View struct {
	V                      int             `json:"v"`
	Room                   RoomView        `json:"room"`
	StateVersion           string          `json:"stateVersion"`
	ServerTime             time.Time       `json:"serverTime"`
	Players                []PlayerView    `json:"players"`
	Readiness              Readiness       `json:"readiness"`
	Game                   *GameView       `json:"game"`
	LastGameResult         *GameResultView `json:"lastGameResult"`
	PresenceObservedAt     time.Time       `json:"presenceObservedAt"`
	ReconciliationRequired bool            `json:"reconciliationRequired"`
}

// RoomView describes the room.
type RoomView struct {
	Code         string  `json:"code"`
	Epoch        string  `json:"epoch"`
	HostPlayerID *string `json:"hostPlayerId"`
	CreatorName  string  `json:"creatorName"`
	Status       string  `json:"status"` // lobby | active | closed
	Activated    bool    `json:"activated"`
}

// PlayerView is a public member entry.
type PlayerView struct {
	ID               string `json:"id"`
	Username         string `json:"username"`
	Ready            bool   `json:"ready"`
	IsHost           bool   `json:"isHost"`
	ConnectionStatus string `json:"connectionStatus"`
	Participation    string `json:"participation"` // player | spectator
}

// Readiness aggregates ready flags (Java round-info usersReady/totalUsers).
type Readiness struct {
	Ready    int  `json:"ready"`
	Total    int  `json:"total"`
	AllReady bool `json:"allReady"`
}

// GameView is the public projection of the active game.
type GameView struct {
	ID               string            `json:"id"`
	Kind             Kind              `json:"kind"`
	RoundID          *string           `json:"roundId"`
	Phase            string            `json:"phase"`
	StartedAt        time.Time         `json:"startedAt"`
	Deadline         *time.Time        `json:"deadline"`
	CurrentQuestion  *QuestionView     `json:"currentQuestion"`
	Progress         *ProgressView     `json:"progress,omitempty"`
	Voting           *VotingView       `json:"voting"`
	LastVotingResult *VotingResultView `json:"lastVotingResult"`
	Impostor         *ImpostorView     `json:"impostor,omitempty"`
}

// QuestionView is the selected question. Anonymous Directas questions have a
// null fromPlayerName: the UI chooses the label (C3, never mutates the row).
type QuestionView struct {
	InstanceID     string  `json:"instanceId,omitempty"`
	ID             string  `json:"id"`
	Type           string  `json:"type,omitempty"`
	Text           string  `json:"text"`
	FromPlayerName *string `json:"fromPlayerName,omitempty"`
	ToPlayerID     *string `json:"toPlayerId,omitempty"`
	ToPlayerName   *string `json:"toPlayerName,omitempty"`
	Anonymous      *bool   `json:"anonymous,omitempty"`
	Number         int     `json:"number,omitempty"`
	Total          int     `json:"total,omitempty"`
}

// ProgressView reports round progress.
type ProgressView struct {
	TotalQuestions  int            `json:"totalQuestions"`
	ShownQuestions  int            `json:"shownQuestions"`
	Remaining       *int           `json:"remaining,omitempty"`
	RemainingByType map[string]int `json:"remainingByType,omitempty"`
}

// VotingView is the open vote progress. Probable exposes partial counts and
// the current leader (Java getVoteResults could be read at any time);
// Impostor never exposes raw counts before resolution.
type VotingView struct {
	ScopeID          string          `json:"scopeId"`
	VotedCount       int             `json:"votedCount"`
	TotalVoters      int             `json:"totalVoters"`
	PendingPlayerIDs []string        `json:"pendingPlayerIds"`
	Complete         bool            `json:"complete"`
	Counts           []VoteCountView `json:"counts,omitempty"`
	LeaderPlayerID   *string         `json:"leaderPlayerId,omitempty"`
	WinnerPlayerID   *string         `json:"winnerPlayerId,omitempty"`
	Deadline         *time.Time      `json:"deadline,omitempty"`
}

// VoteCountView is a public count.
type VoteCountView struct {
	PlayerID string `json:"playerId"`
	Votes    int    `json:"votes"`
}

// VotingResultView is a closed vote.
type VotingResultView struct {
	ResultID           string          `json:"resultId"`
	ScopeID            string          `json:"scopeId"`
	Counts             []VoteCountView `json:"counts"`
	Tie                bool            `json:"tie"`
	WinnerPlayerID     *string         `json:"winnerPlayerId,omitempty"`
	WinnerName         string          `json:"winnerName,omitempty"`
	EliminatedPlayerID *string         `json:"eliminatedPlayerId,omitempty"`
	EliminatedName     string          `json:"eliminatedName,omitempty"`
	Reason             string          `json:"reason"`
	PendingAtClose     []string        `json:"pendingAtClose,omitempty"`
	ResolvedAt         time.Time       `json:"resolvedAt"`
}

// ImpostorView is the public Impostor state (no word, no impostor IDs, no
// usedWords — C5).
type ImpostorView struct {
	ImpostorCount       int            `json:"impostorCount"`
	ParticipantIDs      []string       `json:"participantIds"`
	AlivePlayerIDs      []string       `json:"alivePlayerIds"`
	EliminatedPlayerIDs []string       `json:"eliminatedPlayerIds"`
	CountdownEndsAt     time.Time      `json:"countdownEndsAt"`
	VotingRoundID       *string        `json:"votingRoundId"`
	LastVotingCalledBy  *string        `json:"lastVotingCalledBy"`
	LastVotingCalledAt  *time.Time     `json:"lastVotingCalledAt"`
	LastVotingEndedAt   *time.Time     `json:"lastVotingEndedAt"`
	GlobalCooldownUntil *time.Time     `json:"globalCooldownUntil"`
	PersonalCooldowns   []CooldownView `json:"personalCooldowns"`
	LastDeparture       *DepartureView `json:"lastDeparture,omitempty"`
}

// CooldownView is a per-player vote.call availability.
type CooldownView struct {
	PlayerID    string    `json:"playerId"`
	AvailableAt time.Time `json:"availableAt"`
}

// DepartureView is a public departure notice.
type DepartureView struct {
	PlayerID string    `json:"playerId"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}

// GameResultView is the retained terminal result.
type GameResultView struct {
	ResultID          string            `json:"resultId"`
	GameRunID         string            `json:"gameRunId"`
	Kind              Kind              `json:"kind"`
	Winner            string            `json:"winner"`
	ImpostorPlayerIDs []string          `json:"impostorPlayerIds"`
	Impostors         []RosterView      `json:"impostors"`
	Message           string            `json:"message"`
	FinalVoting       *VotingResultView `json:"finalVoting"`
	EndedAt           time.Time         `json:"endedAt"`
}

// RosterView is a participant name kept for results.
type RosterView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ViewOptions carries data computed outside the aggregate.
type ViewOptions struct {
	Now   time.Time
	Grace time.Duration
	Rules Rules
}

// BuildView projects the room into its public snapshot.
func BuildView(r *Room, o ViewOptions) View {
	v := View{
		V:                  1,
		StateVersion:       strconv.FormatInt(r.Version, 10),
		ServerTime:         o.Now.UTC(),
		PresenceObservedAt: o.Now.UTC(),
		Players:            []PlayerView{},
		Room: RoomView{
			Code: r.Code, Epoch: r.Epoch, CreatorName: r.CreatorName,
			HostPlayerID: idPtr(r.HostID), Activated: r.Activated, Status: "lobby",
		},
	}
	g := r.State.Game
	switch {
	case r.ClosedAt != nil:
		v.Room.Status = "closed"
	case g != nil:
		v.Room.Status = "active"
	}
	for _, m := range r.Members {
		part := "player"
		if g != nil && g.Impostor != nil && rosterName(g.Impostor, m.ID) == "" {
			part = "spectator" // late joiner (C11)
		}
		v.Players = append(v.Players, PlayerView{
			ID: FormatID(m.ID), Username: m.Username, Ready: m.Ready, IsHost: r.IsHost(m.ID),
			ConnectionStatus: m.ConnectionStatus(o.Now), Participation: part,
		})
		v.Readiness.Total++
		if m.Ready {
			v.Readiness.Ready++
		}
	}
	v.Readiness.AllReady = v.Readiness.Total > 0 && v.Readiness.Ready == v.Readiness.Total
	if g != nil {
		v.Game = buildGameView(r, g, o)
	}
	if res := r.State.LastGameResult; res != nil {
		v.LastGameResult = gameResultView(res)
	}
	v.ReconciliationRequired = DeadlineDue(r, o.Now) || PresenceDue(r, o.Now, o.Grace)
	return v
}

func buildGameView(r *Room, g *Game, o ViewOptions) *GameView {
	gv := &GameView{ID: g.RunID, Kind: g.Kind, Phase: g.Phase, StartedAt: g.StartedAt}
	if g.RoundID != "" {
		gv.RoundID = &g.RoundID
	}
	switch {
	case g.Direct != nil:
		directView(r, g, gv)
	case g.Catalog != nil:
		catalogView(r, g, gv)
	case g.Impostor != nil:
		impostorView(g, gv, o)
	}
	return gv
}

func directView(r *Room, g *Game, gv *GameView) {
	total, shown := 0, 0
	if r.Direct != nil {
		total, shown = len(r.Direct.Questions), len(r.Direct.Shown)
	}
	gv.Progress = &ProgressView{TotalQuestions: total, ShownQuestions: shown}
	q := r.currentDirect()
	if q == nil {
		return
	}
	anon := q.Anonymous
	qv := &QuestionView{ID: FormatID(q.ID), Text: q.Text, Anonymous: &anon, Number: shown, Total: total}
	if !q.Anonymous {
		from := q.From
		qv.FromPlayerName = &from
	}
	to := q.To
	qv.ToPlayerName = &to
	if m := r.MemberByName(q.To); m != nil { // null if the recipient already left
		id := FormatID(m.ID)
		qv.ToPlayerID = &id
	}
	gv.CurrentQuestion = qv
	_ = g
}

func catalogView(r *Room, g *Game, gv *GameView) {
	cs := g.Catalog
	remaining := len(cs.Remaining)
	byType := map[string]int{}
	for _, it := range cs.Remaining {
		byType[it.Type]++
	}
	gv.Progress = &ProgressView{Remaining: &remaining, RemainingByType: byType}
	if q := cs.Current; q != nil {
		qv := &QuestionView{InstanceID: q.InstanceID, ID: FormatID(q.ID), Type: q.Type, Text: q.Text}
		if q.ToPlayerID != nil {
			name := q.ToPlayerName
			qv.ToPlayerName = &name
			if r.Member(*q.ToPlayerID) != nil {
				qv.ToPlayerID = idPtr(q.ToPlayerID)
			}
		}
		gv.CurrentQuestion = qv
	}
	if v := cs.Voting; v != nil {
		vv := &VotingView{ScopeID: v.InstanceID, VotedCount: len(v.Votes), Complete: v.Complete, PendingPlayerIDs: []string{}}
		if v.Complete && v.Result != nil {
			vv.TotalVoters = len(v.Votes)
			vv.Counts = countsView(v.Result.Counts)
			vv.WinnerPlayerID = idPtr(v.Result.WinnerPlayerID)
			vv.LeaderPlayerID = vv.WinnerPlayerID
		} else {
			vv.TotalVoters = len(r.Members)
			for _, m := range r.Members {
				if !hasVoted(v.Votes, m.ID) {
					vv.PendingPlayerIDs = append(vv.PendingPlayerIDs, FormatID(m.ID))
				}
			}
			tally := ProbableTally(r, v.Votes)
			vv.Counts = countsView(tally.Counts)
			vv.LeaderPlayerID = idPtr(tally.WinnerPlayerID)
		}
		gv.Voting = vv
	}
	if lr := cs.LastVotingResult; lr != nil {
		gv.LastVotingResult = votingResultView(lr)
	}
}

func impostorView(g *Game, gv *GameView, o ViewOptions) {
	imp := g.Impostor
	iv := &ImpostorView{
		ImpostorCount:       imp.ImpostorCount,
		AlivePlayerIDs:      idsView(imp.Alive),
		EliminatedPlayerIDs: idsView(imp.Eliminated),
		CountdownEndsAt:     imp.CountdownEndsAt,
		LastVotingCalledBy:  idPtr(imp.LastCalledBy),
		LastVotingCalledAt:  imp.LastCalledAt,
		LastVotingEndedAt:   imp.LastVotingEndedAt,
		PersonalCooldowns:   []CooldownView{},
	}
	for _, e := range imp.Roster {
		iv.ParticipantIDs = append(iv.ParticipantIDs, FormatID(e.ID))
	}
	if imp.VotingRoundID != "" {
		iv.VotingRoundID = &imp.VotingRoundID
	}
	if imp.LastVotingEndedAt != nil {
		t := imp.LastVotingEndedAt.Add(o.Rules.ImpostorGlobalCooldown)
		iv.GlobalCooldownUntil = &t
	}
	for _, c := range imp.Calls {
		iv.PersonalCooldowns = append(iv.PersonalCooldowns, CooldownView{
			PlayerID: FormatID(c.PlayerID), AvailableAt: c.At.Add(o.Rules.ImpostorPersonalCooldown)})
	}
	if d := imp.LastDeparture; d != nil {
		iv.LastDeparture = &DepartureView{PlayerID: FormatID(d.PlayerID), Reason: d.Reason, At: d.At}
	}
	gv.Impostor = iv
	if g.Phase == PhaseVoting {
		gv.Deadline = imp.Deadline
		gv.Voting = &VotingView{
			ScopeID: imp.VotingRoundID, VotedCount: len(imp.Votes), TotalVoters: len(imp.Alive),
			PendingPlayerIDs: idsView(pendingVoters(imp)), Deadline: imp.Deadline,
		}
	}
	if imp.LastVotingResult != nil {
		gv.LastVotingResult = votingResultView(imp.LastVotingResult)
	}
}

func votingResultView(r *VotingResult) *VotingResultView {
	return &VotingResultView{
		ResultID: r.ResultID, ScopeID: r.ScopeID, Counts: countsView(r.Counts), Tie: r.Tie,
		WinnerPlayerID: idPtr(r.WinnerPlayerID), WinnerName: r.WinnerName,
		EliminatedPlayerID: idPtr(r.EliminatedPlayerID), EliminatedName: r.EliminatedName,
		Reason: r.Reason, PendingAtClose: idsView(r.PendingAtClose), ResolvedAt: r.ResolvedAt,
	}
}

func gameResultView(r *GameResult) *GameResultView {
	gv := &GameResultView{
		ResultID: r.ResultID, GameRunID: r.GameRunID, Kind: r.Kind, Winner: r.Winner,
		ImpostorPlayerIDs: idsView(r.ImpostorPlayerIDs), Message: r.Message, EndedAt: r.EndedAt,
		Impostors: []RosterView{},
	}
	for _, e := range r.Impostors {
		gv.Impostors = append(gv.Impostors, RosterView{ID: FormatID(e.ID), Name: e.Name})
	}
	if r.FinalVoting != nil {
		gv.FinalVoting = votingResultView(r.FinalVoting)
	}
	return gv
}

func countsView(cs []VoteCount) []VoteCountView {
	out := make([]VoteCountView, 0, len(cs))
	for _, c := range cs {
		out = append(out, VoteCountView{PlayerID: FormatID(c.PlayerID), Votes: c.Votes})
	}
	return out
}

func idsView(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, FormatID(id))
	}
	return out
}

func idPtr(id *int64) *string {
	if id == nil {
		return nil
	}
	s := FormatID(*id)
	return &s
}
