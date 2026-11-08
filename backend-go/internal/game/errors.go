package game

import (
	"errors"
	"fmt"
	"time"
)

// Code is a stable, public error code shared by REST and WebSocket.
type Code string

// Public error codes. HTTP status mapping lives in the transport layer.
const (
	CodeInvalidInput          Code = "INVALID_INPUT"
	CodeForbidden             Code = "FORBIDDEN"
	CodeNotFound              Code = "NOT_FOUND"
	CodeStateConflict         Code = "STATE_CONFLICT"
	CodeStaleGame             Code = "STALE_GAME"
	CodeStaleRound            Code = "STALE_ROUND"
	CodeStaleQuestion         Code = "STALE_QUESTION"
	CodeWrongGame             Code = "WRONG_GAME"
	CodeAlreadyVoted          Code = "ALREADY_VOTED"
	CodeNoQuestions           Code = "NO_QUESTIONS"
	CodeQuestionTypeExhausted Code = "QUESTION_TYPE_EXHAUSTED"
	CodeCatalogEmpty          Code = "CATALOG_EMPTY"
	CodeRoleUnavailable       Code = "ROLE_UNAVAILABLE"
	CodeCooldownActive        Code = "COOLDOWN_ACTIVE"
	CodeLimitReached          Code = "LIMIT_REACHED"
	CodeResetNotConfigured    Code = "RESET_NOT_CONFIGURED"

	// Application-level codes (coordinator/transport).
	CodeUnauthenticated      Code = "UNAUTHENTICATED"
	CodeRoomNotFound         Code = "ROOM_NOT_FOUND"
	CodeRoomExpired          Code = "ROOM_EXPIRED"
	CodeRoomFull             Code = "ROOM_FULL"
	CodeNameTaken            Code = "NAME_TAKEN"
	CodeAlreadyInRoom        Code = "ALREADY_IN_ROOM"
	CodeIdempotencyKeyReused Code = "IDEMPOTENCY_KEY_REUSED"
	CodeStaleEpoch           Code = "STALE_EPOCH"
	CodeLeaseNotFound        Code = "LEASE_NOT_FOUND"
	CodeDatabaseUnavailable  Code = "DATABASE_UNAVAILABLE"
	CodeRateLimited          Code = "RATE_LIMITED"
	CodePayloadTooLarge      Code = "PAYLOAD_TOO_LARGE"
	CodeInternal             Code = "INTERNAL_ERROR"
)

// Error is a domain rule violation. Its Message is safe to show to clients.
type Error struct {
	Code       Code
	Message    string
	RetryAfter time.Duration
	Details    map[string]any
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Errf builds a domain error.
func Errf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// AsError extracts a domain error from err.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// IsCode reports whether err is a domain error with the given code.
func IsCode(err error, code Code) bool {
	e, ok := AsError(err)
	return ok && e.Code == code
}
