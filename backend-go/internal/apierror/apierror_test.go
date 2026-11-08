package apierror

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/store"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		status    int
		code      game.Code
		retryable bool
		internal  bool
	}{
		{"invalid", game.Errf(game.CodeInvalidInput, "x"), 400, game.CodeInvalidInput, false, false},
		{"unauth", game.Errf(game.CodeUnauthenticated, "x"), 401, game.CodeUnauthenticated, false, false},
		{"forbidden", game.Errf(game.CodeForbidden, "x"), 403, game.CodeForbidden, false, false},
		{"room 404", game.Errf(game.CodeRoomNotFound, "x"), 404, game.CodeRoomNotFound, false, false},
		{"expired 410", game.Errf(game.CodeRoomExpired, "x"), 410, game.CodeRoomExpired, false, false},
		{"conflict", game.Errf(game.CodeAlreadyVoted, "x"), 409, game.CodeAlreadyVoted, false, false},
		{"reset 501", game.Errf(game.CodeResetNotConfigured, "x"), 501, game.CodeResetNotConfigured, false, false},
		{"cooldown retry", &game.Error{Code: game.CodeCooldownActive, Message: "x", RetryAfter: 2 * time.Second}, 409, game.CodeCooldownActive, true, false},
		{"wrapped domain", fmt.Errorf("ctx: %w", game.Errf(game.CodeNameTaken, "x")), 409, game.CodeNameTaken, false, false},
		{"db down", fmt.Errorf("x: %w", store.ErrUnavailable), 503, game.CodeDatabaseUnavailable, true, true},
		{"timeout", context.DeadlineExceeded, 503, game.CodeDatabaseUnavailable, true, true},
		{"unknown", errors.New(`pq: relation "users" password=hunter2`), 500, game.CodeInternal, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, body, internal := Classify(tt.err)
			if status != tt.status || body.Code != tt.code || body.Retryable != tt.retryable || internal != tt.internal {
				t.Fatalf("got %d %+v internal=%t", status, body, internal)
			}
			if strings.Contains(body.Message, "hunter2") || strings.Contains(body.Message, "relation") {
				t.Fatal("internal details leaked")
			}
		})
	}
	if s, _, _ := Classify(game.Errf(game.CodeRateLimited, "x")); s != http.StatusTooManyRequests {
		t.Fatal("429")
	}
}
