// Package apierror maps errors to the public error contract shared by REST
// and WebSocket: {code, message, retryable, details?}. Internal errors are
// sanitized: SQL, DSNs and stack traces never reach clients.
package apierror

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/store"
)

// Body is the public error object.
type Body struct {
	Code         game.Code      `json:"code"`
	Message      string         `json:"message"`
	Retryable    bool           `json:"retryable"`
	RetryAfterMs int64          `json:"retryAfterMs,omitempty"`
	Details      map[string]any `json:"details,omitempty"`
}

// Classify converts err into an HTTP status and public body. internal reports
// whether the error is unexpected and must be logged server-side.
func Classify(err error) (status int, body Body, internal bool) {
	if e, ok := game.AsError(err); ok {
		status = statusFor(e.Code)
		body = Body{Code: e.Code, Message: e.Message, Details: e.Details}
		if e.RetryAfter > 0 {
			body.RetryAfterMs = e.RetryAfter.Milliseconds()
			body.Retryable = true
		}
		if e.Code == game.CodeRateLimited || e.Code == game.CodeDatabaseUnavailable {
			body.Retryable = true
		}
		return status, body, false
	}
	if unavailable(err) {
		return http.StatusServiceUnavailable, Body{Code: game.CodeDatabaseUnavailable,
			Message: "servicio temporalmente no disponible", Retryable: true}, true
	}
	return http.StatusInternalServerError, Body{Code: game.CodeInternal, Message: "error interno"}, true
}

func unavailable(err error) bool {
	var netErr net.Error
	var connErr *pgconn.ConnectError
	return errors.Is(err, store.ErrUnavailable) || errors.Is(err, store.ErrCommitUnknown) ||
		errors.Is(err, context.DeadlineExceeded) || errors.As(err, &connErr) || errors.As(err, &netErr)
}

func statusFor(c game.Code) int {
	switch c {
	case game.CodeInvalidInput:
		return http.StatusBadRequest
	case game.CodeUnauthenticated:
		return http.StatusUnauthorized
	case game.CodeForbidden:
		return http.StatusForbidden
	case game.CodeNotFound, game.CodeRoomNotFound:
		return http.StatusNotFound
	case game.CodeRoomExpired:
		return http.StatusGone
	case game.CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case game.CodeRateLimited:
		return http.StatusTooManyRequests
	case game.CodeResetNotConfigured:
		return http.StatusNotImplemented
	case game.CodeDatabaseUnavailable:
		return http.StatusServiceUnavailable
	case game.CodeInternal:
		return http.StatusInternalServerError
	}
	return http.StatusConflict // STATE_CONFLICT, STALE_*, ALREADY_VOTED, NAME_TAKEN, ...
}
