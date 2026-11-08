package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
)

// Player is an authenticated users row.
type Player struct {
	ID             int64
	Username       string
	RoomID         *int64
	TokenExpiresAt time.Time
}

// ErrNameTaken means the global unique username is in use.
var ErrNameTaken = errors.New("username taken")

// CreatePlayer registers a player with a hashed bearer. A Go-registered row
// that is not in an active room and whose token expired (or that was created
// before orphanBefore and never joined a room) is reclaimed: its name is
// freed. Legacy Java rows (no token_hash) are never touched.
func (t *Tx) CreatePlayer(ctx context.Context, username string, tokenHash []byte, expires, now, orphanBefore time.Time) (int64, error) {
	var (
		existingID  int64
		roomActive  bool
		goOwned     bool
		tokenExpiry *time.Time
		createdAt   *time.Time
	)
	err := t.tx.QueryRow(ctx, `SELECT u.id,
			EXISTS (SELECT 1 FROM game_session g WHERE g.id = u.game_session_id AND g.epoch IS NOT NULL AND g.closed_at IS NULL),
			u.token_hash IS NOT NULL, u.token_expires_at, u.created_at
		FROM users u WHERE u.username = $1 FOR UPDATE`, username,
	).Scan(&existingID, &roomActive, &goOwned, &tokenExpiry, &createdAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return 0, fmt.Errorf("checking username: %w", err)
	default:
		expired := tokenExpiry != nil && !tokenExpiry.After(now)
		orphan := createdAt != nil && createdAt.Before(orphanBefore)
		if !goOwned || roomActive || !(expired || orphan) {
			return 0, ErrNameTaken
		}
		if err := t.DeleteUser(ctx, existingID); err != nil {
			return 0, err
		}
	}
	var id int64
	err = t.tx.QueryRow(ctx, `INSERT INTO users (username, ready, token_hash, token_expires_at, created_at, connection_leases)
		VALUES ($1, false, $2, $3, $4, '[]'::jsonb) RETURNING id`, username, tokenHash, expires, now).Scan(&id)
	if IsUniqueViolation(err) {
		return 0, ErrNameTaken
	}
	if err != nil {
		return 0, fmt.Errorf("inserting player: %w", err)
	}
	return id, nil
}

// PlayerByTokenHash resolves a bearer hash.
func (s *Store) PlayerByTokenHash(ctx context.Context, hash []byte) (*Player, error) {
	var p Player
	var exp *time.Time
	err := s.pool.QueryRow(ctx, `SELECT id, username, game_session_id, token_expires_at
		FROM users WHERE token_hash = $1`, hash).Scan(&p.ID, &p.Username, &p.RoomID, &exp)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: resolving token: %w", ErrUnavailable, err)
	}
	if exp != nil {
		p.TokenExpiresAt = *exp
	}
	return &p, nil
}

// LockPlayer locks the users row of the actor.
func (t *Tx) LockPlayer(ctx context.Context, id int64) (*Player, error) {
	var p Player
	err := t.tx.QueryRow(ctx, `SELECT id, username, game_session_id FROM users WHERE id = $1 FOR UPDATE`, id).
		Scan(&p.ID, &p.Username, &p.RoomID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("locking player: %w", err)
	}
	return &p, nil
}

// PlayerRoom reads the player's room without locking.
func (t *Tx) PlayerRoom(ctx context.Context, id int64) (*int64, error) {
	var room *int64
	err := t.tx.QueryRow(ctx, `SELECT game_session_id FROM users WHERE id = $1`, id).Scan(&room)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading player room: %w", err)
	}
	return room, nil
}

// RoomIsActive reports whether the room ID is an open Go-managed room and
// returns its code and epoch.
func (t *Tx) RoomIsActive(ctx context.Context, id int64) (bool, string, string, error) {
	var code, epoch string
	err := t.tx.QueryRow(ctx, `SELECT session_code, epoch::text FROM game_session WHERE id = $1 AND `+activeRoom, id).Scan(&code, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", "", nil
	}
	if err != nil {
		return false, "", "", fmt.Errorf("checking room: %w", err)
	}
	return true, code, epoch, nil
}

// RenewLease extends one lease of the actor locking only its users row
// (no room lock: preserves the room→user order, plan 7.4).
func (t *Tx) RenewLease(ctx context.Context, userID, roomID int64, leaseID string, until, now time.Time) (bool, error) {
	var (
		m      game.Member
		leases []byte
		room   *int64
	)
	err := t.tx.QueryRow(ctx, `SELECT id, game_session_id, connection_leases, disconnected_at FROM users WHERE id = $1 FOR UPDATE`, userID).
		Scan(&m.ID, &room, &leases, &m.DisconnectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("locking lease owner: %w", err)
	}
	if room == nil || *room != roomID {
		return false, ErrNotFound
	}
	if len(leases) > 0 {
		if err := json.Unmarshal(leases, &m.Leases); err != nil {
			return false, fmt.Errorf("decoding leases: %w", err)
		}
	}
	if !m.RenewLease(leaseID, until, now) {
		return false, nil
	}
	b, err := json.Marshal(m.Leases)
	if err != nil {
		return false, fmt.Errorf("encoding leases: %w", err)
	}
	if _, err := t.tx.Exec(ctx, `UPDATE users SET connection_leases = $2 WHERE id = $1`, userID, b); err != nil {
		return false, fmt.Errorf("renewing lease: %w", err)
	}
	return true, nil
}

// TouchRoomActivity coalesces activity updates (one per minute) in its own
// statement, after the user lock was released.
func (s *Store) TouchRoomActivity(ctx context.Context, roomID int64, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE game_session SET last_activity_at = $2
		WHERE id = $1 AND `+activeRoom+` AND (last_activity_at IS NULL OR last_activity_at < $2 - interval '60 seconds')`, roomID, now)
	if err != nil {
		return fmt.Errorf("touching room activity: %w", err)
	}
	return nil
}
