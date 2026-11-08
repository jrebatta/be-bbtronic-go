// Package game contains the BBtronic domain: rooms, membership, presence and
// the six game engines. Everything here is pure with respect to I/O except the
// small [Data] interface used to read catalogs and write Preguntas Directas
// rows; time and randomness are injected through [Env] so rules are testable.
//
// The types persisted in game_session.runtime_state carry explicit JSON tags
// and a schema version. They are private server state: never serialize them
// to clients — use [BuildView] instead.
package game

import (
	"slices"
	"time"
)

// Kind identifies a game mode. Values are the original strings used by the
// Spring Boot backend and stored in game_session.current_game.
type Kind string

// Game kinds.
const (
	KindDirect        Kind = "preguntas-directas"
	KindNeverEver     Kind = "yo-nunca-nunca"
	KindTrivia        Kind = "cultura-pendeja"
	KindMostLikely    Kind = "quien-es-mas-probable"
	KindUncomfortable Kind = "preguntas-incomodas"
	KindImpostor      Kind = "el-impostor"
)

// Kinds lists every supported game in a stable order.
var Kinds = []Kind{KindDirect, KindNeverEver, KindTrivia, KindMostLikely, KindUncomfortable, KindImpostor}

// Valid reports whether k is a known game kind.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// IsCatalog reports whether the game draws its questions from a catalog table.
func (k Kind) IsCatalog() bool {
	return k == KindNeverEver || k == KindTrivia || k == KindMostLikely || k == KindUncomfortable
}

// Phase values. Round phases keep the Java strings; Impostor phases keep the
// lowercase strings of ImpostorGameState.
const (
	PhaseWaitingQuestions = "WAITING_QUESTIONS"
	PhaseInProgress       = "IN_PROGRESS"
	PhaseCompleted        = "COMPLETED"

	PhaseDescription = "description"
	PhaseVoting      = "voting"
)

// StateSchemaVersion is the version of [State] written by this code.
const StateSchemaVersion = 1

// Room is the aggregate loaded (and row-locked) by the coordinator for each
// mutation. Members are always sorted by ID (join order).
type Room struct {
	ID          int64
	Code        string
	Epoch       string
	HostID      *int64
	CreatorName string
	Activated   bool // game_session.game_started
	Version     int64
	CreatedAt   time.Time
	LastActive  time.Time
	ClosedAt    *time.Time

	State   State
	Members []*Member

	// Direct holds the Preguntas Directas rows of the current round. It is
	// loaded only when the active game is Preguntas Directas.
	Direct *DirectRound
}

// Member is a player that belongs to the room (users row with game_session_id).
type Member struct {
	ID             int64
	Username       string
	Ready          bool
	Leases         []Lease
	DisconnectedAt *time.Time
}

// Lease is one live connection (WebSocket or HTTP fallback tab) of a member.
type Lease struct {
	ID    string    `json:"id"`
	Kind  string    `json:"kind"`
	Until time.Time `json:"until"`
}

// Lease kinds.
const (
	LeaseKindWebSocket = "ws"
	LeaseKindHTTP      = "http"
)

// State is the canonical JSON stored in game_session.runtime_state. It only
// contains data that has no column/table of its own.
type State struct {
	SchemaVersion int `json:"schemaVersion"`
	// Game is the active game; nil means lobby.
	Game *Game `json:"game,omitempty"`
	// LastGameResult is the most recent terminal result (Impostor), retained
	// so it can be recovered after the transient notification was missed.
	LastGameResult *GameResult `json:"lastGameResult,omitempty"`
	// ImpostorUsedWords is the room-level private word history (Java
	// sessionUsedWords). It survives game.end and is reset when exhausted.
	ImpostorUsedWords []string `json:"impostorUsedWords,omitempty"`
	// ClosedCode keeps the public code of a closed room: session_code is set
	// to NULL on close so the code can be reused under the real database's
	// UNIQUE index on game_session.session_code (rooms are never deleted).
	ClosedCode string `json:"closedCode,omitempty"`
}

// Game is the active game run. Exactly one of the kind-specific pointers is set.
type Game struct {
	RunID     string    `json:"runId"`
	Kind      Kind      `json:"kind"`
	RoundID   string    `json:"roundId,omitempty"`
	Phase     string    `json:"phase"`
	StartedAt time.Time `json:"startedAt"`

	Direct   *DirectState   `json:"direct,omitempty"`
	Catalog  *CatalogState  `json:"catalog,omitempty"`
	Impostor *ImpostorState `json:"impostor,omitempty"`
}

// DirectState is the Preguntas Directas state that has no column of its own.
// Questions live in the question table; shown ones in shown_questions.
type DirectState struct {
	// CurrentQuestionID is the question actually selected (fixes the Java
	// index/random mismatch, C3).
	CurrentQuestionID *int64 `json:"currentQuestionId,omitempty"`
}

// DirectRound is the loaded content of the current Preguntas Directas round.
type DirectRound struct {
	Questions []DirectQuestion // sorted by ID
	Shown     map[int64]bool
}

// DirectQuestion is a question row of the current round.
type DirectQuestion struct {
	ID        int64
	From      string
	To        string
	Text      string
	Anonymous bool
}

// CatalogItem is a remaining catalog question reference (id + tipo). Texts are
// read from the catalog table only when the question is selected.
type CatalogItem struct {
	ID   int64  `json:"id"`
	Type string `json:"t"`
}

// CatalogState is shared by Yo Nunca Nunca, Cultura Pendeja, Quién es más
// probable and Preguntas Incómodas.
type CatalogState struct {
	// Remaining is the materialized shuffled order still available.
	Remaining []CatalogItem    `json:"remaining"`
	Current   *CatalogQuestion `json:"current,omitempty"`
	// Served holds member IDs already drawn in the current {player} cycle.
	Served []int64 `json:"served,omitempty"`
	// Voting is the open or closed vote of the current Probable question.
	Voting *Voting `json:"voting,omitempty"`
	// LastVotingResult is the most recent closed Probable vote.
	LastVotingResult *VotingResult `json:"lastVotingResult,omitempty"`
}

// CatalogQuestion is the selected catalog question with its rendered text.
type CatalogQuestion struct {
	InstanceID   string `json:"instanceId"`
	ID           int64  `json:"id"`
	Type         string `json:"type"`
	Text         string `json:"text"`
	ToPlayerID   *int64 `json:"toPlayerId,omitempty"`
	ToPlayerName string `json:"toPlayerName,omitempty"`
}

// Vote is one ballot. For Probable the scope is the question instance; for
// Impostor it is the voting round.
type Vote struct {
	VoterID  int64 `json:"voter"`
	TargetID int64 `json:"target"`
}

// Voting is the Quién es más probable vote of one question instance.
type Voting struct {
	InstanceID string        `json:"instanceId"`
	Votes      []Vote        `json:"votes,omitempty"`
	Complete   bool          `json:"complete,omitempty"`
	Result     *VotingResult `json:"result,omitempty"`
}

// VoteCount is the number of votes received by a player.
type VoteCount struct {
	PlayerID int64 `json:"playerId"`
	Votes    int   `json:"votes"`
}

// VotingResult is a closed vote, retained for recovery.
type VotingResult struct {
	ResultID           string      `json:"resultId"`
	ScopeID            string      `json:"scopeId"`
	Counts             []VoteCount `json:"counts"`
	WinnerPlayerID     *int64      `json:"winnerPlayerId,omitempty"`
	WinnerName         string      `json:"winnerName,omitempty"`
	EliminatedPlayerID *int64      `json:"eliminatedPlayerId,omitempty"`
	EliminatedName     string      `json:"eliminatedName,omitempty"`
	Tie                bool        `json:"tie"`
	Reason             string      `json:"reason"`
	PendingAtClose     []int64     `json:"pendingAtClose,omitempty"`
	ResolvedAt         time.Time   `json:"resolvedAt"`
}

// RosterEntry keeps the display name of a game participant so results stay
// readable after the player leaves.
type RosterEntry struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// CallRecord is the last vote.call of a player (personal cooldown).
type CallRecord struct {
	PlayerID int64     `json:"playerId"`
	At       time.Time `json:"at"`
}

// Departure records an Impostor participant removed by leaving the room.
type Departure struct {
	PlayerID int64     `json:"playerId"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}

// ImpostorState is the private El Impostor state. Word and ImpostorIDs are
// secrets: they are only exposed through the role endpoint (owner only) and
// the terminal result (impostor IDs, as in Java).
type ImpostorState struct {
	Roster          []RosterEntry `json:"roster"`
	ImpostorIDs     []int64       `json:"impostorIds"`
	Word            string        `json:"word"`
	ImpostorCount   int           `json:"impostorCount"`
	Alive           []int64       `json:"alive"`
	Eliminated      []int64       `json:"eliminated,omitempty"`
	CountdownEndsAt time.Time     `json:"countdownEndsAt"`

	VotingRoundID string     `json:"votingRoundId,omitempty"`
	Deadline      *time.Time `json:"deadline,omitempty"`
	Votes         []Vote     `json:"votes,omitempty"`

	LastCalledBy      *int64       `json:"lastCalledBy,omitempty"`
	LastCalledAt      *time.Time   `json:"lastCalledAt,omitempty"`
	LastVotingEndedAt *time.Time   `json:"lastVotingEndedAt,omitempty"`
	Calls             []CallRecord `json:"calls,omitempty"`

	LastVotingResult *VotingResult `json:"lastVotingResult,omitempty"`
	LastDeparture    *Departure    `json:"lastDeparture,omitempty"`
}

// GameResult is a terminal game result.
type GameResult struct {
	ResultID          string        `json:"resultId"`
	GameRunID         string        `json:"gameRunId"`
	Kind              Kind          `json:"kind"`
	Winner            string        `json:"winner"`
	ImpostorPlayerIDs []int64       `json:"impostorPlayerIds"`
	Impostors         []RosterEntry `json:"impostors"`
	Message           string        `json:"message"`
	FinalVoting       *VotingResult `json:"finalVoting,omitempty"`
	EndedAt           time.Time     `json:"endedAt"`
}

// Member returns the member with the given ID or nil.
func (r *Room) Member(id int64) *Member {
	for _, m := range r.Members {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// MemberByName returns the member with the exact username or nil.
func (r *Room) MemberByName(name string) *Member {
	for _, m := range r.Members {
		if m.Username == name {
			return m
		}
	}
	return nil
}

// IsHost reports whether id is the current host.
func (r *Room) IsHost(id int64) bool { return r.HostID != nil && *r.HostID == id }

// NextDeadline returns the earliest game deadline that requires a server-side
// transition (persisted in game_session.next_deadline_at).
func (r *Room) NextDeadline() *time.Time {
	g := r.State.Game
	if g == nil || g.Impostor == nil || g.Phase != PhaseVoting {
		return nil
	}
	return g.Impostor.Deadline
}

// AddMember appends m keeping members sorted by ID.
func (r *Room) AddMember(m *Member) {
	r.Members = append(r.Members, m)
	slices.SortFunc(r.Members, func(a, b *Member) int { return cmpInt64(a.ID, b.ID) })
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func containsID(ids []int64, id int64) bool { return slices.Contains(ids, id) }

func removeID(ids []int64, id int64) []int64 {
	return slices.DeleteFunc(ids, func(v int64) bool { return v == id })
}
