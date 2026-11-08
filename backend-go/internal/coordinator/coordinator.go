// Package coordinator orchestrates every state change of BBtronic: it
// authenticates the actor, locks the room in PostgreSQL, checks idempotency
// receipts and versions, materializes due deadlines, applies the pure game
// rules, persists and commits — and only then notifies local realtime
// subscribers. PostgreSQL is the only authority; no Go memory is shared state.
package coordinator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/auth"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/store"
)

// Config holds the operational parameters (plan 9.4).
type Config struct {
	TokenTTL                time.Duration
	OrphanRegistration      time.Duration
	DisconnectGrace         time.Duration
	LeaseTTL                time.Duration
	RoomIdle                time.Duration
	ReceiptRetention        time.Duration
	MaxConnectionsPerPlayer int
	MaxRoomMembers          int
	Rules                   game.Rules
}

// DefaultConfig returns the candidate values of the plan.
func DefaultConfig() Config {
	return Config{
		TokenTTL:                24 * time.Hour,
		OrphanRegistration:      15 * time.Minute,
		DisconnectGrace:         20 * time.Second,
		LeaseTTL:                45 * time.Second,
		RoomIdle:                2 * time.Hour,
		ReceiptRetention:        24 * time.Hour,
		MaxConnectionsPerPlayer: 4,
		MaxRoomMembers:          30,
		Rules:                   game.DefaultRules(),
	}
}

// Notifier is told about committed room changes (local realtime hub).
type Notifier interface {
	RoomChanged(roomID int64, reason string)
}

// Service is the coordinator.
type Service struct {
	store *store.Store
	cfg   Config
	log   *slog.Logger

	mu       sync.RWMutex
	now      func() time.Time
	rand     game.Rand
	notifier Notifier
}

// New builds a Service.
func New(st *store.Store, cfg Config, log *slog.Logger) *Service {
	return &Service{store: st, cfg: cfg, log: log, now: time.Now, rand: game.DefaultRand()}
}

// Config returns the configuration.
func (s *Service) Config() Config { return s.cfg }

// SetNotifier registers the realtime notifier.
func (s *Service) SetNotifier(n Notifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifier = n
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// SetRand replaces the randomness source (tests).
func (s *Service) SetRand(r game.Rand) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rand = r
}

// Now returns the current server time in UTC.
func (s *Service) Now() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.now().UTC()
}

func (s *Service) random() game.Rand {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rand
}

func (s *Service) notify(roomID int64, reason string) {
	s.mu.RLock()
	n := s.notifier
	s.mu.RUnlock()
	if n != nil {
		n.RoomChanged(roomID, reason)
	}
}

func (s *Service) env(now time.Time, tx *store.Tx) game.Env {
	return game.Env{Now: now, Rand: s.random(), Rules: s.cfg.Rules, Data: tx.Data()}
}

// Ping checks the database (readiness).
func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

// --- players -----------------------------------------------------------------

// Actor is the authenticated caller.
type Actor struct {
	PlayerID       int64
	Username       string
	RoomID         *int64
	TokenExpiresAt time.Time
}

// RegisterResult is returned once to the token owner.
type RegisterResult struct {
	PlayerID    int64
	Username    string
	AccessToken string
	ExpiresAt   time.Time
}

const usernameMaxRunes = 32

// ValidateUsername applies the uniform rule for every entry point (fixes the
// Java inconsistency between register/create/join): trimmed, 1–32 characters,
// letters/digits/spaces and common punctuation, no control characters.
func ValidateUsername(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", game.Errf(game.CodeInvalidInput, "el nombre de usuario es obligatorio")
	}
	n := 0
	for _, c := range name {
		n++
		if unicode.IsControl(c) || c == '�' || !(unicode.IsLetter(c) || unicode.IsDigit(c) || unicode.IsMark(c) || strings.ContainsRune(" ._-'", c)) {
			return "", game.Errf(game.CodeInvalidInput, "el nombre de usuario contiene caracteres no permitidos")
		}
	}
	if n > usernameMaxRunes {
		return "", game.Errf(game.CodeInvalidInput, "el nombre de usuario supera %d caracteres", usernameMaxRunes)
	}
	return name, nil
}

// Register creates a player and issues its bearer token (N1).
func (s *Service) Register(ctx context.Context, username string) (RegisterResult, error) {
	name, err := ValidateUsername(username)
	if err != nil {
		return RegisterResult{}, err
	}
	token, hash, err := auth.NewToken()
	if err != nil {
		return RegisterResult{}, fmt.Errorf("generating token: %w", err)
	}
	now := s.Now()
	exp := now.Add(s.cfg.TokenTTL)
	var id int64
	err = s.store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		id, err = tx.CreatePlayer(ctx, name, hash, exp, now, now.Add(-s.cfg.OrphanRegistration))
		return err
	})
	if errors.Is(err, store.ErrNameTaken) {
		return RegisterResult{}, game.Errf(game.CodeNameTaken, "el nombre de usuario ya está en uso")
	}
	if err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{PlayerID: id, Username: name, AccessToken: token, ExpiresAt: exp}, nil
}

// Authenticate resolves a bearer token.
func (s *Service) Authenticate(ctx context.Context, token string) (Actor, error) {
	if err := auth.Validate(token); err != nil {
		return Actor{}, game.Errf(game.CodeUnauthenticated, "credencial inválida")
	}
	p, err := s.store.PlayerByTokenHash(ctx, auth.Hash(token))
	if errors.Is(err, store.ErrNotFound) {
		return Actor{}, game.Errf(game.CodeUnauthenticated, "credencial inválida o revocada")
	}
	if err != nil {
		return Actor{}, err
	}
	if !p.TokenExpiresAt.After(s.Now()) {
		return Actor{}, game.Errf(game.CodeUnauthenticated, "credencial expirada")
	}
	return Actor{PlayerID: p.ID, Username: p.Username, RoomID: p.RoomID, TokenExpiresAt: p.TokenExpiresAt}, nil
}

// RoomRef identifies the actor's active room.
type RoomRef struct {
	Code  string `json:"code"`
	Epoch string `json:"epoch"`
}

// Me returns the actor's active room, if any (N2).
func (s *Service) Me(ctx context.Context, a Actor) (*RoomRef, error) {
	if a.RoomID == nil {
		return nil, nil //nolint:nilnil // no room is a valid answer
	}
	var ref *RoomRef
	err := s.store.ReadTx(ctx, func(tx *store.Tx) error {
		ok, code, epoch, err := tx.RoomIsActive(ctx, *a.RoomID)
		if ok {
			ref = &RoomRef{Code: code, Epoch: epoch}
		}
		return err
	})
	return ref, err
}

// Logout removes the player: it leaves its room (games reconciled) and the
// users row is deleted, freeing the name (N3, Java logoutUser).
func (s *Service) Logout(ctx context.Context, a Actor) error { return s.logout(ctx, a, 0) }

func (s *Service) logout(ctx context.Context, a Actor, attempt int) error {
	var changedRoom int64
	err := s.store.InTx(ctx, func(tx *store.Tx) error {
		changedRoom = 0
		// Lock order is room → users: read the membership without a lock,
		// lock the room (which locks its members), then re-check.
		roomID, err := tx.PlayerRoom(ctx, a.PlayerID)
		if errors.Is(err, store.ErrNotFound) {
			return nil // already gone: idempotent
		}
		if err != nil {
			return err
		}
		if roomID != nil {
			room, err := tx.LoadRoom(ctx, "", *roomID, true)
			switch {
			case errors.Is(err, store.ErrNotFound): // closed or legacy room: plain delete
			case err != nil:
				return err
			case room.Member(a.PlayerID) == nil:
				return errMembershipMoved // joined elsewhere meanwhile: retry
			default:
				now := s.Now()
				orig := snapshotMembers(room)
				env := s.env(now, tx)
				s.advance(env, room)
				game.RemoveMember(env, room, a.PlayerID, game.RemovalLeft)
				room.Version++
				room.LastActive = now
				changedRoom = room.ID
				return s.persist(ctx, tx, room, orig, now) // deletes the users row
			}
		}
		if _, err := tx.LockPlayer(ctx, a.PlayerID); errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		return tx.DeleteUser(ctx, a.PlayerID)
	})
	if errors.Is(err, errMembershipMoved) && attempt < 3 {
		return s.logout(ctx, a, attempt+1)
	}
	if err == nil && changedRoom != 0 {
		s.notify(changedRoom, "member.left")
	}
	return err
}

// --- rooms --------------------------------------------------------------------

// RoomResult is returned by create/join (N4/N5).
type RoomResult struct {
	RoomCode      string `json:"roomCode"`
	Epoch         string `json:"epoch"`
	PlayerID      string `json:"playerId"`
	StateVersion  string `json:"stateVersion"`
	Participation string `json:"participation"`
	Replayed      bool   `json:"replayed"`
}

const scopePlayer = "player"

// CreateRoom creates a room with the actor as host (N4).
func (s *Service) CreateRoom(ctx context.Context, a Actor, key string) (RoomResult, error) {
	if err := ValidateIdempotencyKey(key); err != nil {
		return RoomResult{}, err
	}
	reqHash := hashParts("rooms.create")
	var res RoomResult
	var roomID int64
	err := s.store.InTx(ctx, func(tx *store.Tx) error {
		roomID = 0
		now := s.Now()
		p, err := tx.LockPlayer(ctx, a.PlayerID)
		if errors.Is(err, store.ErrNotFound) {
			return game.Errf(game.CodeUnauthenticated, "credencial revocada")
		}
		if err != nil {
			return err
		}
		if replayed, err := s.replay(ctx, tx, scopePlayer, p.ID, key, reqHash, now, &res); replayed || err != nil {
			res.Replayed = replayed
			return err
		}
		if err := s.ensureNotInRoom(ctx, tx, p, 0); err != nil {
			return err
		}
		room := &game.Room{
			Epoch: game.NewID(), HostID: &p.ID, CreatorName: p.Username, Version: 1,
			CreatedAt: now, LastActive: now, State: game.State{SchemaVersion: game.StateSchemaVersion},
		}
		inserted := false
		for range 40 { // 9000 codes; collisions only with other active rooms (C9)
			room.Code = strconv.Itoa(1000 + s.random().IntN(9000))
			if err := tx.CloseIdleRoomWithCode(ctx, room.Code, now.Add(-s.cfg.RoomIdle)); err != nil {
				return err
			}
			if inserted, err = tx.InsertRoom(ctx, room); err != nil {
				return err
			}
			if inserted {
				break
			}
		}
		if !inserted {
			return game.Errf(game.CodeLimitReached, "no hay códigos de sala disponibles; inténtalo más tarde")
		}
		if err := tx.AttachMember(ctx, p.ID, room.ID); err != nil {
			return err
		}
		res = RoomResult{RoomCode: room.Code, Epoch: room.Epoch, PlayerID: game.FormatID(p.ID),
			StateVersion: strconv.FormatInt(room.Version, 10), Participation: "player"}
		roomID = room.ID
		return s.putReceipt(ctx, tx, scopePlayer, p.ID, key, reqHash, "rooms.create", room.Version, res, now)
	})
	if err == nil && roomID != 0 {
		s.notify(roomID, "room.created")
	}
	return res, err
}

// JoinRoom adds the actor to an existing room (N5). epoch is optional.
func (s *Service) JoinRoom(ctx context.Context, a Actor, code, epoch, key string) (RoomResult, error) {
	if err := ValidateIdempotencyKey(key); err != nil {
		return RoomResult{}, err
	}
	if err := ValidateRoomCode(code); err != nil {
		return RoomResult{}, err
	}
	reqHash := hashParts("rooms.join", code, epoch)
	var res RoomResult
	var changedRoom int64
	err := s.store.InTx(ctx, func(tx *store.Tx) error {
		changedRoom = 0
		now := s.Now()
		room, err := s.lockRoom(ctx, tx, code, epoch, now)
		if err != nil {
			return err
		}
		p, err := tx.LockPlayer(ctx, a.PlayerID)
		if errors.Is(err, store.ErrNotFound) {
			return game.Errf(game.CodeUnauthenticated, "credencial revocada")
		}
		if err != nil {
			return err
		}
		if replayed, err := s.replay(ctx, tx, scopePlayer, p.ID, key, reqHash, now, &res); replayed || err != nil {
			res.Replayed = replayed
			return err
		}
		participation := func() string {
			if g := room.State.Game; g != nil && g.Impostor != nil {
				for _, e := range g.Impostor.Roster {
					if e.ID == p.ID {
						return "player"
					}
				}
				return "spectator" // late Impostor joiner (C11)
			}
			return "player"
		}
		if p.RoomID != nil && *p.RoomID == room.ID {
			res = RoomResult{RoomCode: room.Code, Epoch: room.Epoch, PlayerID: game.FormatID(p.ID),
				StateVersion: strconv.FormatInt(room.Version, 10), Participation: participation()}
			return nil // already a member: idempotent success
		}
		if err := s.ensureNotInRoom(ctx, tx, p, room.ID); err != nil {
			return err
		}
		if len(room.Members) >= s.cfg.MaxRoomMembers {
			return game.Errf(game.CodeRoomFull, "la sala alcanzó el máximo de %d jugadores", s.cfg.MaxRoomMembers)
		}
		orig := snapshotMembers(room)
		env := s.env(now, tx)
		s.advance(env, room)
		if err := tx.AttachMember(ctx, p.ID, room.ID); err != nil {
			return err
		}
		room.AddMember(&game.Member{ID: p.ID, Username: p.Username})
		orig[p.ID] = &game.Member{ID: p.ID} // attached clean; avoid re-save
		room.Version++
		room.LastActive = now
		if err := s.persist(ctx, tx, room, orig, now); err != nil {
			return err
		}
		res = RoomResult{RoomCode: room.Code, Epoch: room.Epoch, PlayerID: game.FormatID(p.ID),
			StateVersion: strconv.FormatInt(room.Version, 10), Participation: participation()}
		changedRoom = room.ID
		return s.putReceipt(ctx, tx, scopePlayer, p.ID, key, reqHash, "rooms.join", room.Version, res, now)
	})
	if err == nil && changedRoom != 0 {
		s.notify(changedRoom, "member.joined")
	}
	return res, err
}

// ensureNotInRoom rejects a second simultaneous membership. A membership in a
// closed/legacy room is released.
func (s *Service) ensureNotInRoom(ctx context.Context, tx *store.Tx, p *store.Player, allowRoomID int64) error {
	if p.RoomID == nil || *p.RoomID == allowRoomID {
		return nil
	}
	active, code, _, err := tx.RoomIsActive(ctx, *p.RoomID)
	if err != nil {
		return err
	}
	if active {
		return &game.Error{Code: game.CodeAlreadyInRoom, Message: "ya perteneces a otra sala; sal de ella primero",
			Details: map[string]any{"roomCode": code}}
	}
	return nil
}

// lockRoom loads and locks an active room, checks the epoch and applies the
// logical idle expiry (committed even though the request then fails).
func (s *Service) lockRoom(ctx context.Context, tx *store.Tx, code, epoch string, now time.Time) (*game.Room, error) {
	room, err := tx.LoadRoom(ctx, code, 0, true)
	if errors.Is(err, store.ErrNotFound) {
		return nil, game.Errf(game.CodeRoomNotFound, "la sala no existe o ya cerró")
	}
	if err != nil {
		return nil, err
	}
	if epoch != "" && epoch != room.Epoch {
		return nil, game.Errf(game.CodeStaleEpoch, "la sala con ese código pertenece a otra generación")
	}
	if room.LastActive.Add(s.cfg.RoomIdle).Before(now) {
		room.ClosedAt = &now
		if err := tx.SaveRoom(ctx, room); err != nil {
			return nil, err
		}
		return nil, &store.CommitThenFail{Err: game.Errf(game.CodeRoomExpired, "la sala expiró por inactividad")}
	}
	return room, nil
}

// --- state, role, commands ------------------------------------------------------

// errNotMember is returned when the actor does not belong to the room.
var errNotMember = game.Errf(game.CodeForbidden, "no perteneces a esta sala")

var errMembershipMoved = errors.New("membership changed concurrently")

// State returns the public snapshot for a member (N6). GET never mutates.
func (s *Service) State(ctx context.Context, a Actor, code, epoch string) (game.View, error) {
	var v game.View
	err := s.store.ReadTx(ctx, func(tx *store.Tx) error {
		room, err := s.readRoom(ctx, tx, code, epoch, a)
		if err != nil {
			return err
		}
		v = s.view(room)
		return nil
	})
	return v, err
}

func (s *Service) readRoom(ctx context.Context, tx *store.Tx, code, epoch string, a Actor) (*game.Room, error) {
	if err := ValidateRoomCode(code); err != nil {
		return nil, err
	}
	room, err := tx.LoadRoom(ctx, code, 0, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, game.Errf(game.CodeRoomNotFound, "la sala no existe o ya cerró")
	}
	if err != nil {
		return nil, err
	}
	if epoch == "" {
		return nil, game.Errf(game.CodeInvalidInput, "epoch es obligatorio")
	}
	if epoch != room.Epoch {
		return nil, game.Errf(game.CodeStaleEpoch, "la sala con ese código pertenece a otra generación")
	}
	if room.Member(a.PlayerID) == nil {
		return nil, errNotMember
	}
	return room, nil
}

func (s *Service) view(room *game.Room) game.View {
	return game.BuildView(room, game.ViewOptions{Now: s.Now(), Grace: s.cfg.DisconnectGrace, Rules: s.cfg.Rules})
}

// Role returns the private Impostor role of the actor (N8).
func (s *Service) Role(ctx context.Context, a Actor, code, epoch, gameRunID string) (game.Role, error) {
	var role game.Role
	err := s.store.ReadTx(ctx, func(tx *store.Tx) error {
		room, err := s.readRoom(ctx, tx, code, epoch, a)
		if err != nil {
			return err
		}
		role, err = game.ImpostorRole(room, a.PlayerID, gameRunID)
		return err
	})
	return role, err
}

// CommandRequest is the wire command (plan 6.2).
type CommandRequest struct {
	CommandID       string          `json:"commandId"`
	Epoch           string          `json:"epoch"`
	GameRunID       string          `json:"gameRunId,omitempty"`
	RoundID         string          `json:"roundId,omitempty"`
	ExpectedVersion *string         `json:"expectedVersion,omitempty"`
	Operation       string          `json:"operation"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

// CommandResult is the confirmation after commit.
type CommandResult struct {
	CommandID        string `json:"commandId"`
	Operation        string `json:"operation"`
	CommittedVersion string `json:"committedVersion"`
	Replayed         bool   `json:"replayed"`
	Result           any    `json:"result"`
}

// Execute runs a command (N7 and WS "command").
func (s *Service) Execute(ctx context.Context, a Actor, code string, req CommandRequest) (CommandResult, error) {
	expected, err := validateCommand(code, req)
	if err != nil {
		return CommandResult{}, err
	}
	reqHash, err := commandHash(req)
	if err != nil {
		return CommandResult{}, err
	}
	var res CommandResult
	var changedRoom int64
	reason := "sync"
	err = s.store.InTx(ctx, func(tx *store.Tx) error {
		changedRoom = 0
		now := s.Now()
		room, err := s.lockRoom(ctx, tx, code, req.Epoch, now)
		if err != nil {
			return err
		}
		if room.Member(a.PlayerID) == nil {
			return errNotMember
		}
		scope := roomScope(room)
		if replayed, err := s.replay(ctx, tx, scope, a.PlayerID, req.CommandID, reqHash, now, &res); replayed || err != nil {
			res.Replayed = replayed
			return err
		}
		orig := snapshotMembers(room)
		env := s.env(now, tx)
		advanced := s.advance(env, room)
		fail := func(err error) error {
			if !advanced {
				return err
			}
			room.Version++
			if perr := s.persist(ctx, tx, room, orig, now); perr != nil {
				return perr
			}
			changedRoom = room.ID
			return &store.CommitThenFail{Err: err}
		}
		if game.RequiresExpectedVersion(req.Operation) {
			if expected == nil {
				return game.Errf(game.CodeInvalidInput, "expectedVersion es obligatorio para esta operación")
			}
			if *expected != room.Version || advanced {
				current := room.Version
				if advanced {
					current++
				}
				return fail(&game.Error{Code: game.CodeStateConflict, Message: "el estado cambió; sincroniza antes de repetir la acción",
					Details: map[string]any{"currentVersion": strconv.FormatInt(current, 10)}})
			}
		}
		cmd := game.Command{Operation: req.Operation, GameRunID: req.GameRunID, RoundID: req.RoundID, Payload: req.Payload}
		var out game.Outcome
		if !advanced {
			// Nothing to keep on failure: the whole transaction rolls back.
			// Skipping SAVEPOINT/RELEASE saves two round trips per command
			// (BenchmarkExecute, see MIGRATION_PROGRESS.md).
			if out, err = game.Apply(ctx, env, room, a.PlayerID, cmd); err != nil {
				return err
			}
		} else {
			// A reconciliation must be committed even if the command fails:
			// isolate the command's own SQL effects in a savepoint.
			pre, err := room.Clone()
			if err != nil {
				return err
			}
			applyErr := tx.Savepoint(ctx, func() error {
				var err error
				out, err = game.Apply(ctx, env, room, a.PlayerID, cmd)
				return err
			})
			if applyErr != nil {
				room = pre
				return fail(applyErr)
			}
		}
		if out.Changed || advanced {
			room.Version++
			changedRoom = room.ID
			reason = out.Reason
		}
		room.LastActive = now
		if err := s.persist(ctx, tx, room, orig, now); err != nil {
			return err
		}
		res = CommandResult{CommandID: req.CommandID, Operation: req.Operation,
			CommittedVersion: strconv.FormatInt(room.Version, 10), Result: out.Result}
		return s.putReceipt(ctx, tx, scope, a.PlayerID, req.CommandID, reqHash, req.Operation, room.Version, res, now)
	})
	if changedRoom != 0 && (err == nil || isDomain(err)) {
		s.notify(changedRoom, reason)
	}
	return res, err
}

func isDomain(err error) bool { _, ok := game.AsError(err); return ok }

// ReconcileResult is returned by N9.
type ReconcileResult struct {
	StateVersion string `json:"stateVersion"`
	Changed      bool   `json:"changed"`
}

// Reconcile materializes due deadlines and presence transitions (N9). It is
// idempotent and never alters questions on request.
func (s *Service) Reconcile(ctx context.Context, a Actor, code, epoch string) (ReconcileResult, error) {
	if err := ValidateRoomCode(code); err != nil {
		return ReconcileResult{}, err
	}
	if epoch == "" {
		return ReconcileResult{}, game.Errf(game.CodeInvalidInput, "epoch es obligatorio")
	}
	var res ReconcileResult
	var roomID int64
	err := s.store.InTx(ctx, func(tx *store.Tx) error {
		now := s.Now()
		room, err := s.lockRoom(ctx, tx, code, epoch, now)
		if err != nil {
			return err
		}
		if room.Member(a.PlayerID) == nil {
			return errNotMember
		}
		roomID = room.ID
		res, err = s.reconcileLocked(ctx, tx, room, now)
		return err
	})
	if err == nil && res.Changed {
		s.notify(roomID, "sync")
	}
	return res, err
}

func (s *Service) reconcileLocked(ctx context.Context, tx *store.Tx, room *game.Room, now time.Time) (ReconcileResult, error) {
	orig := snapshotMembers(room)
	if !s.advance(s.env(now, tx), room) {
		return ReconcileResult{StateVersion: strconv.FormatInt(room.Version, 10)}, nil
	}
	room.Version++
	if err := s.persist(ctx, tx, room, orig, now); err != nil {
		return ReconcileResult{}, err
	}
	return ReconcileResult{StateVersion: strconv.FormatInt(room.Version, 10), Changed: true}, nil
}

// ReconcileRoom is the internal reconciliation used by the realtime watcher.
func (s *Service) ReconcileRoom(ctx context.Context, roomID int64) (bool, error) {
	var changed bool
	err := s.store.InTx(ctx, func(tx *store.Tx) error {
		now := s.Now()
		room, err := tx.LoadRoom(ctx, "", roomID, true)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		res, err := s.reconcileLocked(ctx, tx, room, now)
		changed = res.Changed
		return err
	})
	if err == nil && changed {
		s.notify(roomID, "sync")
	}
	return changed, err
}

// --- presence -----------------------------------------------------------------

// ConnectionResult is returned when a lease is opened or renewed.
type ConnectionResult struct {
	ConnectionID string    `json:"connectionId"`
	LeaseUntil   time.Time `json:"leaseUntil"`
	ServerTime   time.Time `json:"serverTime"`
	Replayed     bool      `json:"replayed,omitempty"`
}

// MemberContext is what a realtime connection needs after authentication.
type MemberContext struct {
	RoomID       int64
	RoomCode     string
	Epoch        string
	StateVersion int64
}

// OpenConnection opens a presence lease (WS auth or N13). key is required for
// HTTP opens and empty for WebSocket opens (one per socket).
func (s *Service) OpenConnection(ctx context.Context, a Actor, code, epoch, kind, key string) (ConnectionResult, MemberContext, error) {
	if key != "" {
		if err := ValidateIdempotencyKey(key); err != nil {
			return ConnectionResult{}, MemberContext{}, err
		}
	}
	if err := ValidateRoomCode(code); err != nil {
		return ConnectionResult{}, MemberContext{}, err
	}
	if epoch == "" {
		return ConnectionResult{}, MemberContext{}, game.Errf(game.CodeInvalidInput, "epoch es obligatorio")
	}
	reqHash := hashParts("connections.open", code, epoch, kind)
	var (
		res     ConnectionResult
		mc      MemberContext
		changed bool
	)
	err := s.store.InTx(ctx, func(tx *store.Tx) error {
		changed = false
		now := s.Now()
		room, err := s.lockRoom(ctx, tx, code, epoch, now)
		if err != nil {
			return err
		}
		mc = MemberContext{RoomID: room.ID, RoomCode: room.Code, Epoch: room.Epoch, StateVersion: room.Version}
		if room.Member(a.PlayerID) == nil {
			return errNotMember
		}
		scope := roomScope(room)
		if key != "" {
			if replayed, err := s.replay(ctx, tx, scope, a.PlayerID, key, reqHash, now, &res); replayed || err != nil {
				res.Replayed = replayed
				return err
			}
		}
		orig := snapshotMembers(room)
		env := s.env(now, tx)
		advanced := s.advance(env, room)
		m := room.Member(a.PlayerID)
		var openErr error
		statusChanged := false
		if m == nil {
			openErr = errNotMember
		} else {
			lease := game.Lease{ID: game.NewID(), Kind: kind, Until: now.Add(s.cfg.LeaseTTL)}
			statusChanged, openErr = m.OpenLease(lease, now, s.cfg.MaxConnectionsPerPlayer)
			res = ConnectionResult{ConnectionID: lease.ID, LeaseUntil: lease.Until, ServerTime: now}
		}
		changed = statusChanged || advanced
		if changed {
			room.Version++
		}
		room.LastActive = now
		if openErr != nil {
			if !advanced {
				return openErr
			}
			if err := s.persist(ctx, tx, room, orig, now); err != nil {
				return err
			}
			return &store.CommitThenFail{Err: openErr}
		}
		if err := s.persist(ctx, tx, room, orig, now); err != nil {
			return err
		}
		mc.StateVersion = room.Version
		if key != "" {
			return s.putReceipt(ctx, tx, scope, a.PlayerID, key, reqHash, "connections.open", room.Version, res, now)
		}
		return nil
	})
	if changed && (err == nil || isDomain(err)) {
		s.notify(mc.RoomID, "presence.changed")
	}
	return res, mc, err
}

// RenewConnection extends a lease (heartbeat / N14) locking only the user row.
func (s *Service) RenewConnection(ctx context.Context, a Actor, roomID int64, connID string) (ConnectionResult, error) {
	now := s.Now()
	until := now.Add(s.cfg.LeaseTTL)
	var ok bool
	err := s.store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		ok, err = tx.RenewLease(ctx, a.PlayerID, roomID, connID, until, now)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return ConnectionResult{}, errNotMember
	}
	if err != nil {
		return ConnectionResult{}, err
	}
	if !ok {
		return ConnectionResult{}, game.Errf(game.CodeLeaseNotFound, "la conexión expiró; abre una nueva")
	}
	if err := s.store.TouchRoomActivity(ctx, roomID, now); err != nil {
		s.log.WarnContext(ctx, "room activity update failed", "room", roomID, "err", err)
	}
	return ConnectionResult{ConnectionID: connID, LeaseUntil: until, ServerTime: now}, nil
}

// ResolveRoomID returns the ID of an active room the actor belongs to.
func (s *Service) ResolveRoomID(ctx context.Context, a Actor, code, epoch string) (int64, error) {
	var id int64
	err := s.store.ReadTx(ctx, func(tx *store.Tx) error {
		r, err := tx.RoomByCode(ctx, code, false)
		if errors.Is(err, store.ErrNotFound) {
			return game.Errf(game.CodeRoomNotFound, "la sala no existe o ya cerró")
		}
		if err != nil {
			return err
		}
		if epoch != r.Epoch {
			return game.Errf(game.CodeStaleEpoch, "la sala con ese código pertenece a otra generación")
		}
		if a.RoomID == nil || *a.RoomID != r.ID {
			return errNotMember
		}
		id = r.ID
		return nil
	})
	return id, err
}

// CloseConnection removes a lease; idempotent (N15, socket close).
func (s *Service) CloseConnection(ctx context.Context, a Actor, roomID int64, connID string) error {
	changed := false
	err := s.store.InTx(ctx, func(tx *store.Tx) error {
		changed = false
		now := s.Now()
		room, err := tx.LoadRoom(ctx, "", roomID, true)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		m := room.Member(a.PlayerID)
		if m == nil {
			return nil
		}
		orig := snapshotMembers(room)
		found, statusChanged := m.CloseLease(connID, now)
		if !found {
			return nil
		}
		if statusChanged {
			room.Version++
			changed = true
		}
		return s.persist(ctx, tx, room, orig, now)
	})
	if err == nil && changed {
		s.notify(roomID, "presence.changed")
	}
	return err
}

// --- realtime support ------------------------------------------------------------

// RoomSnapshot is a consistent public view plus the authorized member IDs.
type RoomSnapshot struct {
	View    game.View
	Version int64
	Epoch   string
	Members map[int64]bool
}

// Snapshot loads the public view of a room by ID (watcher fan-out).
func (s *Service) Snapshot(ctx context.Context, roomID int64) (*RoomSnapshot, error) {
	var snap *RoomSnapshot
	err := s.store.ReadTx(ctx, func(tx *store.Tx) error {
		room, err := tx.LoadRoom(ctx, "", roomID, false)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		members := map[int64]bool{}
		for _, m := range room.Members {
			members[m.ID] = true
		}
		snap = &RoomSnapshot{View: s.view(room), Version: room.Version, Epoch: room.Epoch, Members: members}
		return nil
	})
	return snap, err
}

// RoomHeads exposes the batched watcher query.
func (s *Service) RoomHeads(ctx context.Context, ids []int64) (map[int64]*store.RoomHead, error) {
	return s.store.RoomHeads(ctx, ids)
}

// Due reports whether a room head requires a server-side reconciliation.
func (s *Service) Due(h *store.RoomHead) bool {
	now := s.Now()
	if h.Deadline != nil && !now.Before(*h.Deadline) {
		return true
	}
	for _, m := range h.Members {
		if m.Presence(now, s.cfg.DisconnectGrace) != game.PresenceNone {
			return true
		}
	}
	return false
}

// --- internals ---------------------------------------------------------------------

// advance applies due presence transitions and game deadlines (AdvanceDue).
func (s *Service) advance(env game.Env, room *game.Room) bool {
	changed := false
	for _, m := range append([]*game.Member(nil), room.Members...) {
		switch m.Presence(env.Now, s.cfg.DisconnectGrace) {
		case game.PresenceMarkReconnecting:
			m.MarkReconnecting(env.Now)
			changed = true
		case game.PresenceFinalize:
			game.RemoveMember(env, room, m.ID, game.RemovalDisconnect)
			changed = true
		case game.PresenceNone:
		}
	}
	if game.AdvanceDue(env, room) {
		changed = true
	}
	return changed
}

func snapshotMembers(room *game.Room) map[int64]*game.Member {
	orig := make(map[int64]*game.Member, len(room.Members))
	c, err := room.Clone()
	if err != nil {
		return orig
	}
	for _, m := range c.Members {
		orig[m.ID] = m
	}
	return orig
}

// persist writes the room and the members whose columns changed, deletes the
// users removed from the room (Java parity) and closes an empty room.
func (s *Service) persist(ctx context.Context, tx *store.Tx, room *game.Room, orig map[int64]*game.Member, now time.Time) error {
	current := make(map[int64]bool, len(room.Members))
	for _, m := range room.Members {
		current[m.ID] = true
		if o, ok := orig[m.ID]; !ok || !game.SameMemberState(o, m) {
			if err := tx.SaveMember(ctx, m); err != nil {
				return err
			}
		}
	}
	if len(room.Members) == 0 && room.ClosedAt == nil {
		room.ClosedAt = &now
	}
	if err := tx.SaveRoom(ctx, room); err != nil {
		return err
	}
	for id := range orig {
		if !current[id] {
			if err := tx.DeleteUser(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func roomScope(room *game.Room) string {
	return "room:" + strconv.FormatInt(room.ID, 10) + ":" + room.Epoch
}

func (s *Service) replay(ctx context.Context, tx *store.Tx, scope string, actorID int64, key string, reqHash []byte, now time.Time, dst any) (bool, error) {
	rec, err := tx.GetReceipt(ctx, scope, actorID, auth.HashKey(key), now)
	if err != nil || rec == nil {
		return false, err
	}
	if !bytes.Equal(rec.RequestHash, reqHash) {
		return false, game.Errf(game.CodeIdempotencyKeyReused, "la clave de idempotencia ya se usó con otra solicitud")
	}
	if err := json.Unmarshal(rec.Result, dst); err != nil {
		return false, fmt.Errorf("decoding receipt: %w", err)
	}
	if cr, ok := dst.(*CommandResult); ok {
		cr.Replayed = true
	}
	return true, nil
}

func (s *Service) putReceipt(ctx context.Context, tx *store.Tx, scope string, actorID int64, key string, reqHash []byte, op string, version int64, result any, now time.Time) error {
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encoding receipt: %w", err)
	}
	v := version
	return tx.PutReceipt(ctx, scope, actorID, auth.HashKey(key),
		store.Receipt{RequestHash: reqHash, Operation: op, CommittedVersion: &v, Result: b}, now, now.Add(s.cfg.ReceiptRetention))
}

func hashParts(parts ...string) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

func commandHash(req CommandRequest) ([]byte, error) {
	var payload bytes.Buffer
	if len(bytes.TrimSpace(req.Payload)) > 0 {
		if err := json.Compact(&payload, req.Payload); err != nil {
			return nil, game.Errf(game.CodeInvalidInput, "payload no es JSON válido")
		}
	}
	ev := ""
	if req.ExpectedVersion != nil {
		ev = *req.ExpectedVersion
	}
	return hashParts("command", req.Operation, req.Epoch, req.GameRunID, req.RoundID, ev, payload.String()), nil
}

func validateCommand(code string, req CommandRequest) (*int64, error) {
	if err := ValidateRoomCode(code); err != nil {
		return nil, err
	}
	if err := ValidateIdempotencyKey(req.CommandID); err != nil {
		return nil, err
	}
	if req.Epoch == "" {
		return nil, game.Errf(game.CodeInvalidInput, "epoch es obligatorio")
	}
	if !game.KnownOperation(req.Operation) {
		return nil, game.Errf(game.CodeInvalidInput, "operación desconocida")
	}
	if len(req.GameRunID) > 64 || len(req.RoundID) > 64 || len(req.Epoch) > 64 {
		return nil, game.Errf(game.CodeInvalidInput, "identificadores demasiado largos")
	}
	if req.ExpectedVersion == nil {
		return nil, nil //nolint:nilnil // optional field
	}
	v, err := strconv.ParseInt(*req.ExpectedVersion, 10, 64)
	if err != nil || v < 0 {
		return nil, game.Errf(game.CodeInvalidInput, "expectedVersion no es válido")
	}
	return &v, nil
}

// ValidateIdempotencyKey requires 16–128 URL-safe characters (a random UUID
// or 128-bit value is recommended).
func ValidateIdempotencyKey(key string) error {
	if len(key) < 16 || len(key) > 128 {
		return game.Errf(game.CodeInvalidInput, "Idempotency-Key/commandId debe tener entre 16 y 128 caracteres")
	}
	for _, c := range key {
		if !(c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return game.Errf(game.CodeInvalidInput, "Idempotency-Key/commandId contiene caracteres no permitidos")
		}
	}
	return nil
}

// ValidateRoomCode accepts the four-digit public codes (1000–9999).
func ValidateRoomCode(code string) error {
	if len(code) != 4 {
		return game.Errf(game.CodeRoomNotFound, "la sala no existe o ya cerró")
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return game.Errf(game.CodeRoomNotFound, "la sala no existe o ya cerró")
		}
	}
	return nil
}
