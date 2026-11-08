// Package realtime implements the native JSON WebSocket protocol v1 (plan 7)
// with github.com/coder/websocket. There is no shared memory between Vercel
// instances: each process keeps a local hub of its own sockets, notifies them
// right after local commits, and a per-process watcher polls PostgreSQL
// revisions (batched) so changes committed by other instances converge.
// Snapshots replace client state; nothing here is an authority.
package realtime

import (
	"encoding/json"
	"strconv"

	"github.com/coder/websocket"
)

// ProtocolVersion of the JSON frames.
const ProtocolVersion = 1

// Message types.
const (
	TypeAuth            = "auth"
	TypeAuthOK          = "auth.ok"
	TypeStateRequest    = "state.request"
	TypeRoomSnapshot    = "room.snapshot"
	TypeHeartbeat       = "heartbeat"
	TypeHeartbeatAck    = "heartbeat.ack"
	TypeCommand         = "command"
	TypeCommandResult   = "command.result"
	TypeError           = "error"
	TypeServerReconnect = "server.reconnect"
	TypeAccessRevoked   = "access.revoked"
)

// Close codes. 1008 means "do not reconnect with the same credential".
const (
	CloseRevoked          = websocket.StatusPolicyViolation // 1008
	CloseRotate           = websocket.StatusServiceRestart  // 1012
	CloseTryAgain         = websocket.StatusTryAgainLater   // 1013
	CloseInternal         = websocket.StatusInternalError   // 1011
	CloseSlowConsumer     = websocket.StatusCode(4000)
	CloseHeartbeatTimeout = websocket.StatusCode(4001)
	CloseAuthTimeout      = websocket.StatusCode(4002)
)

// Envelope is every frame in both directions.
type Envelope struct {
	V            int             `json:"v"`
	Type         string          `json:"type"`
	RequestID    string          `json:"requestId,omitempty"`
	EventID      string          `json:"eventId,omitempty"`
	RoomCode     string          `json:"roomCode,omitempty"`
	Epoch        string          `json:"epoch,omitempty"`
	StateVersion string          `json:"stateVersion,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

// AuthPayload is the first client frame.
type AuthPayload struct {
	AccessToken string `json:"accessToken"`
	RoomCode    string `json:"roomCode"`
	Epoch       string `json:"epoch"`
}

// HeartbeatPayload renews the connection lease.
type HeartbeatPayload struct {
	ConnectionID string `json:"connectionId"`
	LastVersion  string `json:"lastVersion"`
}

func encode(typ string, e Envelope, payload any) []byte {
	e.V = ProtocolVersion
	e.Type = typ
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			b = []byte(`{}`)
		}
		e.Payload = b
	}
	out, err := json.Marshal(e)
	if err != nil {
		return []byte(`{"v":1,"type":"error"}`)
	}
	return out
}

func versionString(v int64) string { return strconv.FormatInt(v, 10) }
