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

const roomColumns = `id, session_code, epoch::text, host_user_id, COALESCE(creator_name, ''), game_started,
	state_version, runtime_state, created_at, last_activity_at, closed_at`

// Only Go-managed (epoch NOT NULL), open rooms are visible to the API.
const activeRoom = `epoch IS NOT NULL AND closed_at IS NULL`

func scanRoom(row pgx.Row) (*game.Room, error) {
	var (
		r         game.Room
		state     []byte
		createdAt *time.Time
		lastAct   *time.Time
	)
	err := row.Scan(&r.ID, &r.Code, &r.Epoch, &r.HostID, &r.CreatorName, &r.Activated,
		&r.Version, &state, &createdAt, &lastAct, &r.ClosedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning room: %w", err)
	}
	if createdAt != nil {
		r.CreatedAt = createdAt.UTC()
	}
	if lastAct != nil {
		r.LastActive = *lastAct
	} else {
		r.LastActive = r.CreatedAt
	}
	if len(state) > 0 {
		if err := json.Unmarshal(state, &r.State); err != nil {
			return nil, fmt.Errorf("decoding runtime_state of room %d: %w", r.ID, err)
		}
	}
	if r.State.SchemaVersion > game.StateSchemaVersion {
		return nil, fmt.Errorf("room %d has runtime_state schema %d newer than supported %d",
			r.ID, r.State.SchemaVersion, game.StateSchemaVersion)
	}
	return &r, nil
}

// RoomByCode loads an active room; lock=true takes FOR UPDATE.
func (t *Tx) RoomByCode(ctx context.Context, code string, lock bool) (*game.Room, error) {
	q := `SELECT ` + roomColumns + ` FROM game_session WHERE session_code = $1 AND ` + activeRoom
	if lock {
		q += ` FOR UPDATE`
	}
	return scanRoom(t.tx.QueryRow(ctx, q, code))
}

// RoomByID loads an active room by ID.
func (t *Tx) RoomByID(ctx context.Context, id int64, lock bool) (*game.Room, error) {
	q := `SELECT ` + roomColumns + ` FROM game_session WHERE id = $1 AND ` + activeRoom
	if lock {
		q += ` FOR UPDATE`
	}
	return scanRoom(t.tx.QueryRow(ctx, q, id))
}

// LoadMembers loads the members of the room, sorted by ID; lock=true locks them.
func (t *Tx) LoadMembers(ctx context.Context, r *game.Room, lock bool) error {
	q := `SELECT id, username, ready, connection_leases, disconnected_at
		FROM users WHERE game_session_id = $1 ORDER BY id`
	if lock {
		q += ` FOR UPDATE`
	}
	rows, err := t.tx.Query(ctx, q, r.ID)
	if err != nil {
		return fmt.Errorf("querying members: %w", err)
	}
	defer rows.Close()
	r.Members = r.Members[:0]
	for rows.Next() {
		var (
			m      game.Member
			leases []byte
		)
		if err := rows.Scan(&m.ID, &m.Username, &m.Ready, &leases, &m.DisconnectedAt); err != nil {
			return fmt.Errorf("scanning member: %w", err)
		}
		if len(leases) > 0 {
			if err := json.Unmarshal(leases, &m.Leases); err != nil {
				return fmt.Errorf("decoding leases of user %d: %w", m.ID, err)
			}
		}
		r.Members = append(r.Members, &m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating members: %w", err)
	}
	return nil
}

// LoadDirectRound loads the current Preguntas Directas round when relevant.
func (t *Tx) LoadDirectRound(ctx context.Context, r *game.Room) error {
	g := r.State.Game
	if g == nil || g.Kind != game.KindDirect {
		r.Direct = nil
		return nil
	}
	rows, err := t.tx.Query(ctx, `SELECT id, COALESCE(from_user, ''), COALESCE(to_user, ''), COALESCE(question, ''), anonymous
		FROM question WHERE game_session_id = $1 AND round_id = $2 ORDER BY id`, r.ID, g.RoundID)
	if err != nil {
		return fmt.Errorf("querying round questions: %w", err)
	}
	dr := &game.DirectRound{Shown: map[int64]bool{}}
	qs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (game.DirectQuestion, error) {
		var q game.DirectQuestion
		err := row.Scan(&q.ID, &q.From, &q.To, &q.Text, &q.Anonymous)
		return q, err
	})
	if err != nil {
		return fmt.Errorf("scanning round questions: %w", err)
	}
	dr.Questions = qs
	ids := map[int64]bool{}
	for _, q := range qs {
		ids[q.ID] = true
	}
	rows, err = t.tx.Query(ctx, `SELECT question_id FROM shown_questions WHERE session_id = $1 AND question_id IS NOT NULL`, r.ID)
	if err != nil {
		return fmt.Errorf("querying shown questions: %w", err)
	}
	shown, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("scanning shown questions: %w", err)
	}
	for _, id := range shown {
		if ids[id] {
			dr.Shown[id] = true
		}
	}
	r.Direct = dr
	return nil
}

// LoadRoom loads an active room with members and round data.
func (t *Tx) LoadRoom(ctx context.Context, code string, id int64, lock bool) (*game.Room, error) {
	var (
		r   *game.Room
		err error
	)
	if code != "" {
		r, err = t.RoomByCode(ctx, code, lock)
	} else {
		r, err = t.RoomByID(ctx, id, lock)
	}
	if err != nil {
		return nil, err
	}
	if err := t.LoadMembers(ctx, r, lock); err != nil {
		return nil, err
	}
	if err := t.LoadDirectRound(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// InsertRoom creates a Go-managed room. It returns ErrNotFound-free false when
// the code collided with another active room (caller retries another code).
func (t *Tx) InsertRoom(ctx context.Context, r *game.Room) (bool, error) {
	state, err := json.Marshal(r.State)
	if err != nil {
		return false, fmt.Errorf("encoding state: %w", err)
	}
	err = t.tx.QueryRow(ctx, `INSERT INTO game_session
		(session_code, created_at, creator_name, game_started, current_question_index,
		 epoch, state_version, state_schema_version, runtime_state, last_activity_at, host_user_id)
		VALUES ($1, $2, $3, false, 0, $4::uuid, $5, $6, $7, $8, $9)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		r.Code, r.CreatedAt.UTC(), r.CreatorName, r.Epoch, r.Version, game.StateSchemaVersion, state, r.LastActive, r.HostID,
	).Scan(&r.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inserting room: %w", err)
	}
	return true, nil
}

// CloseIdleRoomWithCode logically closes an expired room holding code so the
// code can be reused (rooms are never deleted).
func (t *Tx) CloseIdleRoomWithCode(ctx context.Context, code string, idleBefore time.Time) error {
	// session_code is released (NULL) and kept in runtime_state.closedCode.
	_, err := t.tx.Exec(ctx, `UPDATE game_session SET closed_at = now(), session_code = NULL,
			runtime_state = jsonb_set(COALESCE(runtime_state, '{}'::jsonb), '{closedCode}', to_jsonb(session_code))
		WHERE session_code = $1 AND `+activeRoom+` AND COALESCE(last_activity_at, created_at) < $2`, code, idleBefore)
	if err != nil {
		return fmt.Errorf("closing idle room: %w", err)
	}
	return nil
}

// SaveRoom writes the room row. Legacy columns are mirrored from the runtime
// state (single source of truth) so the data stays readable.
func (t *Tx) SaveRoom(ctx context.Context, r *game.Room) error {
	code := &r.Code
	if r.ClosedAt != nil {
		// Release the public code (UNIQUE index in the real DB); keep it in state.
		r.State.ClosedCode = r.Code
		code = nil
	}
	state, err := json.Marshal(r.State)
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	var curGame, roundID, roundStatus *string
	if g := r.State.Game; g != nil {
		k := string(g.Kind)
		curGame = &k
		if g.RoundID != "" {
			roundID = &g.RoundID
		}
		if g.Kind != game.KindImpostor {
			roundStatus = &g.Phase
		}
	}
	_, err = t.tx.Exec(ctx, `UPDATE game_session SET
		host_user_id = $2, game_started = $3, state_version = $4, state_schema_version = $5,
		runtime_state = $6, current_game = $7, current_round_id = $8, round_status = $9,
		next_deadline_at = $10, last_activity_at = $11, closed_at = $12, session_code = $13
		WHERE id = $1`,
		r.ID, r.HostID, r.Activated, r.Version, game.StateSchemaVersion, state, curGame, roundID, roundStatus,
		r.NextDeadline(), r.LastActive, r.ClosedAt, code)
	if err != nil {
		return fmt.Errorf("saving room %d: %w", r.ID, err)
	}
	return nil
}

// SaveMember writes the mutable member columns.
func (t *Tx) SaveMember(ctx context.Context, m *game.Member) error {
	leases, err := json.Marshal(leasesOrEmpty(m.Leases))
	if err != nil {
		return fmt.Errorf("encoding leases: %w", err)
	}
	_, err = t.tx.Exec(ctx, `UPDATE users SET ready = $2, connection_leases = $3, disconnected_at = $4 WHERE id = $1`,
		m.ID, m.Ready, leases, m.DisconnectedAt)
	if err != nil {
		return fmt.Errorf("saving member %d: %w", m.ID, err)
	}
	return nil
}

func leasesOrEmpty(l []game.Lease) []game.Lease {
	if l == nil {
		return []game.Lease{}
	}
	return l
}

// AttachMember makes the user a member of the room with a clean state.
func (t *Tx) AttachMember(ctx context.Context, userID, roomID int64) error {
	_, err := t.tx.Exec(ctx, `UPDATE users SET game_session_id = $2, ready = false,
		connection_leases = '[]'::jsonb, disconnected_at = NULL WHERE id = $1`, userID, roomID)
	if err != nil {
		return fmt.Errorf("attaching member: %w", err)
	}
	return nil
}

// DeleteUser removes a player (Java logout/kick/final-disconnect semantics:
// the username is freed and the bearer stops working). Questions keep their
// text names; host_user_id is set NULL by its FK.
func (t *Tx) DeleteUser(ctx context.Context, userID int64) error {
	if _, err := t.tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("deleting user %d: %w", userID, err)
	}
	return nil
}

// RoomHead is the compact per-room state polled by the realtime watcher.
type RoomHead struct {
	ID       int64
	Epoch    string
	Version  int64
	Deadline *time.Time
	Members  []*game.Member
}

// RoomHeads fetches versions, deadlines and presence of many rooms in two
// batched queries (no per-socket SELECT of the whole game, plan 7.1).
func (s *Store) RoomHeads(ctx context.Context, ids []int64) (map[int64]*RoomHead, error) {
	heads := map[int64]*RoomHead{}
	if len(ids) == 0 {
		return heads, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, epoch::text, state_version, next_deadline_at
		FROM game_session WHERE id = ANY($1) AND `+activeRoom, ids)
	if err != nil {
		return nil, fmt.Errorf("%w: querying room heads: %w", ErrUnavailable, err)
	}
	for rows.Next() {
		h := &RoomHead{}
		if err := rows.Scan(&h.ID, &h.Epoch, &h.Version, &h.Deadline); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning room head: %w", err)
		}
		heads[h.ID] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating room heads: %w", err)
	}
	rows, err = s.pool.Query(ctx, `SELECT game_session_id, id, connection_leases, disconnected_at
		FROM users WHERE game_session_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("%w: querying presence: %w", ErrUnavailable, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			roomID int64
			m      game.Member
			leases []byte
		)
		if err := rows.Scan(&roomID, &m.ID, &leases, &m.DisconnectedAt); err != nil {
			return nil, fmt.Errorf("scanning presence: %w", err)
		}
		if len(leases) > 0 {
			if err := json.Unmarshal(leases, &m.Leases); err != nil {
				return nil, fmt.Errorf("decoding leases: %w", err)
			}
		}
		if h := heads[roomID]; h != nil {
			h.Members = append(h.Members, &m)
		}
	}
	return heads, rows.Err()
}
