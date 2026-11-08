package httpapi

import (
	"bufio"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/coordinator"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
)

// specOperations extracts "METHOD /path" pairs from docs/swagger.yml without a
// YAML dependency: paths are two-space keys, methods four-space keys.
func specOperations(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("../../docs/swagger.yml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pathRe := regexp.MustCompile(`^  (/[^:]*):\s*$`)
	methodRe := regexp.MustCompile(`^    (get|post|put|delete|patch):\s*$`)
	var ops []string
	current := ""
	inPaths := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && len(line) > 0 && line[0] != ' ' && line[0] != '#':
			inPaths = false
		case inPaths && pathRe.MatchString(line):
			current = pathRe.FindStringSubmatch(line)[1]
		case inPaths && current != "" && methodRe.MatchString(line):
			ops = append(ops, strings.ToUpper(methodRe.FindStringSubmatch(line)[1])+" "+current)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(ops)
	return ops
}

// The OpenAPI contract documents exactly the routes the server registers.
func TestOpenAPIContractCoversRoutes(t *testing.T) {
	t.Parallel()
	var routes []string
	for _, rt := range (&api{}).restRoutes() {
		routes = append(routes, rt.pattern)
	}
	routes = append(routes, RootRoutes...)
	slices.Sort(routes)
	if got := specOperations(t); !slices.Equal(got, routes) {
		t.Fatalf("docs/swagger.yml out of sync with the router\nspec:   %v\nrouter: %v", got, routes)
	}
}

// Every command operation and error code of the engine is listed in the spec.
func TestOpenAPIContractEnums(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../docs/swagger.yml")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(b)
	for _, op := range []string{game.OpRoomActivate, game.OpRoomReset, game.OpMemberReady, game.OpMembersResetReady,
		game.OpMemberKick, game.OpGameStart, game.OpGameEnd, game.OpRoundNew, game.OpRoundCheckReady,
		game.OpQuestionSubmit, game.OpQuestionNext, game.OpVoteSubmit, game.OpVoteCall, game.OpVotesClear} {
		if !game.KnownOperation(op) || !strings.Contains(spec, op) {
			t.Errorf("operation %s missing from the spec", op)
		}
	}
	for _, k := range game.Kinds {
		if !strings.Contains(spec, string(k)) {
			t.Errorf("game kind %s missing from the spec", k)
		}
	}
	for _, c := range []game.Code{game.CodeInvalidInput, game.CodeUnauthenticated, game.CodeForbidden, game.CodeNotFound,
		game.CodeRoomNotFound, game.CodeRoomExpired, game.CodeRoomFull, game.CodeNameTaken, game.CodeAlreadyInRoom,
		game.CodeIdempotencyKeyReused, game.CodeStateConflict, game.CodeStaleEpoch, game.CodeStaleGame, game.CodeStaleRound,
		game.CodeStaleQuestion, game.CodeWrongGame, game.CodeAlreadyVoted, game.CodeNoQuestions, game.CodeQuestionTypeExhausted,
		game.CodeCatalogEmpty, game.CodeRoleUnavailable, game.CodeCooldownActive, game.CodeLimitReached, game.CodeLeaseNotFound,
		game.CodeResetNotConfigured, game.CodePayloadTooLarge, game.CodeRateLimited, game.CodeDatabaseUnavailable, game.CodeInternal} {
		if !strings.Contains(spec, string(c)) {
			t.Errorf("error code %s missing from the spec", c)
		}
	}
	_ = coordinator.ValidateIdempotencyKey // the key pattern in the spec mirrors this validator
}
