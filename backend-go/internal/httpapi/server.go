// Package httpapi exposes the REST v1 contract (MIGRATION_PLAN 6) over
// net/http.ServeMux: JSON UTF-8, string IDs, RFC 3339 UTC times, uniform
// errors {error:{code,message,retryable,details?},requestId}, GET never
// mutates, critical mutations carry an Idempotency-Key.
package httpapi

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/apierror"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/auth"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/coordinator"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
)

// Options configures the API.
type Options struct {
	AllowedOrigins []string
	BodyLimit      int64
	RequestTimeout time.Duration
	OpsHealthToken string
	TrustProxy     bool
	BuildID        string
	// Realtime is mounted at GET /api/v1/realtime when non-nil.
	Realtime http.Handler
	// Limiters (nil disables).
	RegisterLimiter *Limiter
	CommandLimiter  *Limiter
}

type api struct {
	svc  *coordinator.Service
	log  *slog.Logger
	opts Options
}

// New returns the root handler.
func New(svc *coordinator.Service, log *slog.Logger, opts Options) http.Handler {
	a := &api{svc: svc, log: log, opts: opts}
	allowed := map[string]bool{}
	for _, o := range opts.AllowedOrigins {
		allowed[o] = true
	}
	rest := http.NewServeMux()
	for _, rt := range a.restRoutes() {
		rest.HandleFunc(rt.pattern, rt.handler)
	}
	rest.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, game.Errf(game.CodeNotFound, "ruta no encontrada"))
	})

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "build": opts.BuildID})
	})
	if opts.Realtime != nil {
		root.Handle("GET /api/v1/realtime", opts.Realtime)
	}
	root.Handle("/", withRESTLimits(opts.BodyLimit, opts.RequestTimeout, rest))
	return withObservability(log, withSecurityHeaders(withCORS(allowed, root)))
}

type route struct {
	pattern string
	handler http.HandlerFunc
}

// RootRoutes are served outside the REST limits middleware.
var RootRoutes = []string{"GET /healthz", "GET /api/v1/realtime"}

// restRoutes is the single table of REST routes. docs/swagger.yml must
// document exactly these plus RootRoutes (TestOpenAPIContractCoversRoutes).
func (a *api) restRoutes() []route {
	return []route{
		{"POST /api/v1/players", a.register},
		{"GET /api/v1/players/me", a.authed(a.me)},
		{"DELETE /api/v1/players/me", a.logout},
		{"POST /api/v1/rooms", a.authed(a.createRoom)},
		{"POST /api/v1/rooms/{code}/members", a.authed(a.joinRoom)},
		{"GET /api/v1/rooms/{code}/state", a.authed(a.state)},
		{"POST /api/v1/rooms/{code}/commands", a.authed(a.command)},
		{"GET /api/v1/rooms/{code}/games/current/role", a.authed(a.role)},
		{"POST /api/v1/rooms/{code}/reconcile", a.authed(a.reconcile)},
		{"POST /api/v1/rooms/{code}/connections", a.authed(a.openConnection)},
		{"PUT /api/v1/rooms/{code}/connections/{id}", a.authed(a.renewConnection)},
		{"DELETE /api/v1/rooms/{code}/connections/{id}", a.authed(a.closeConnection)},
		{"GET /readyz", a.readyz},
	}
}

var (
	errInternal     = game.Errf(game.CodeInternal, "error interno")
	errOriginDenied = game.Errf(game.CodeForbidden, "origen no permitido")
)

// --- plumbing -------------------------------------------------------------------

type errorEnvelope struct {
	Error     apierror.Body `json:"error"`
	RequestID string        `json:"requestId"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client may have gone; nothing useful to do
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, body, _ := apierror.Classify(err)
	if body.RetryAfterMs > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt((body.RetryAfterMs+999)/1000, 10))
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="bbtronic"`)
	}
	writeJSON(w, status, errorEnvelope{Error: body, RequestID: RequestID(r.Context())})
}

func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	if _, _, internal := apierror.Classify(err); internal {
		a.log.ErrorContext(r.Context(), "request failed", "requestId", RequestID(r.Context()), "route", r.Pattern, "err", err)
	}
	writeError(w, r, err)
}

// decodeJSON strictly decodes a JSON object body; an empty body decodes as {}.
func decodeJSON(r *http.Request, dst any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return game.Errf(game.CodePayloadTooLarge, "el cuerpo supera el límite permitido")
		}
		return game.Errf(game.CodeInvalidInput, "no se pudo leer el cuerpo")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return game.Errf(game.CodeInvalidInput, "Content-Type debe ser application/json")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return game.Errf(game.CodeInvalidInput, "JSON inválido o con campos desconocidos")
	}
	if dec.More() {
		return game.Errf(game.CodeInvalidInput, "se esperaba un único objeto JSON")
	}
	return nil
}

type authedHandler func(w http.ResponseWriter, r *http.Request, actor coordinator.Actor)

func (a *api) authed(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := auth.BearerFromRequest(r)
		if !ok {
			writeError(w, r, game.Errf(game.CodeUnauthenticated, "falta la credencial Bearer"))
			return
		}
		actor, err := a.svc.Authenticate(r.Context(), token)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		h(w, r, actor)
	}
}

func (a *api) limited(l *Limiter, key string, w http.ResponseWriter, r *http.Request) bool {
	if l == nil {
		return false
	}
	if ok, wait := l.Allow(key); !ok {
		writeError(w, r, &game.Error{Code: game.CodeRateLimited, Message: "demasiadas solicitudes", RetryAfter: wait})
		return true
	}
	return false
}

// --- players ----------------------------------------------------------------------

type playerView struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

func (a *api) register(w http.ResponseWriter, r *http.Request) {
	if a.limited(a.opts.RegisterLimiter, "ip:"+ClientIP(r, a.opts.TrustProxy), w, r) {
		return
	}
	var req struct {
		Username *string `json:"username"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if req.Username == nil {
		a.fail(w, r, game.Errf(game.CodeInvalidInput, "username es obligatorio"))
		return
	}
	res, err := a.svc.Register(r.Context(), *req.Username)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"player":      playerView{ID: game.FormatID(res.PlayerID), Username: res.Username},
		"accessToken": res.AccessToken,
		"expiresAt":   res.ExpiresAt,
	})
}

func (a *api) me(w http.ResponseWriter, r *http.Request, actor coordinator.Actor) {
	room, err := a.svc.Me(r.Context(), actor)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"player":    playerView{ID: game.FormatID(actor.PlayerID), Username: actor.Username},
		"room":      room,
		"expiresAt": actor.TokenExpiresAt,
	})
}

// logout is idempotent and does not reveal whether a token was valid.
func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	token, ok := auth.BearerFromRequest(r)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	actor, err := a.svc.Authenticate(r.Context(), token)
	if err != nil {
		if e, ok := game.AsError(err); ok && e.Code == game.CodeUnauthenticated {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		a.fail(w, r, err)
		return
	}
	if err := a.svc.Logout(r.Context(), actor); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- rooms -----------------------------------------------------------------------

func idempotencyKey(r *http.Request) (string, error) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return "", game.Errf(game.CodeInvalidInput, "falta la cabecera Idempotency-Key")
	}
	return key, coordinator.ValidateIdempotencyKey(key)
}

func (a *api) createRoom(w http.ResponseWriter, r *http.Request, actor coordinator.Actor) {
	key, err := idempotencyKey(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req struct{}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.svc.CreateRoom(r.Context(), actor, key)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (a *api) joinRoom(w http.ResponseWriter, r *http.Request, actor coordinator.Actor) {
	key, err := idempotencyKey(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req struct {
		Epoch string `json:"epoch"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.svc.JoinRoom(r.Context(), actor, r.PathValue("code"), req.Epoch, key)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *api) state(w http.ResponseWriter, r *http.Request, actor coordinator.Actor) {
	v, err := a.svc.State(r.Context(), actor, r.PathValue("code"), r.URL.Query().Get("epoch"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) command(w http.ResponseWriter, r *http.Request, actor coordinator.Actor) {
	if a.limited(a.opts.CommandLimiter, "player:"+game.FormatID(actor.PlayerID), w, r) {
		return
	}
	key, err := idempotencyKey(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req coordinator.CommandRequest
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(req.CommandID)) != 1 {
		a.fail(w, r, game.Errf(game.CodeInvalidInput, "commandId debe coincidir con Idempotency-Key"))
		return
	}
	res, err := a.svc.Execute(r.Context(), actor, r.PathValue("code"), req)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *api) role(w http.ResponseWriter, r *http.Request, actor coordinator.Actor) {
	q := r.URL.Query()
	role, err := a.svc.Role(r.Context(), actor, r.PathValue("code"), q.Get("epoch"), q.Get("gameRunId"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, role)
}

func (a *api) reconcile(w http.ResponseWriter, r *http.Request, actor coordinator.Actor) {
	var req struct {
		Epoch     string `json:"epoch"`
		GameRunID string `json:"gameRunId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.svc.Reconcile(r.Context(), actor, r.PathValue("code"), req.Epoch)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- HTTP-fallback presence leases (N13–N15) -------------------------------------------

func (a *api) openConnection(w http.ResponseWriter, r *http.Request, actor coordinator.Actor) {
	key, err := idempotencyKey(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req struct {
		Epoch string `json:"epoch"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	res, _, err := a.svc.OpenConnection(r.Context(), actor, r.PathValue("code"), req.Epoch, game.LeaseKindHTTP, key)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (a *api) renewConnection(w http.ResponseWriter, r *http.Request, actor coordinator.Actor) {
	var req struct {
		Epoch string `json:"epoch"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	roomID, err := a.svc.ResolveRoomID(r.Context(), actor, r.PathValue("code"), req.Epoch)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.svc.RenewConnection(r.Context(), actor, roomID, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *api) closeConnection(w http.ResponseWriter, r *http.Request, actor coordinator.Actor) {
	roomID, err := a.svc.ResolveRoomID(r.Context(), actor, r.PathValue("code"), r.URL.Query().Get("epoch"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.svc.CloseConnection(r.Context(), actor, roomID, r.PathValue("id")); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- readiness ----------------------------------------------------------------------

// readyz is disabled (404) without OPS_HEALTH_TOKEN and never leaks errors.
func (a *api) readyz(w http.ResponseWriter, r *http.Request) {
	if a.opts.OpsHealthToken == "" {
		writeError(w, r, game.Errf(game.CodeNotFound, "ruta no encontrada"))
		return
	}
	token, ok := auth.BearerFromRequest(r)
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(a.opts.OpsHealthToken)) != 1 {
		writeError(w, r, game.Errf(game.CodeUnauthenticated, "credencial inválida"))
		return
	}
	if err := a.svc.Ping(r.Context()); err != nil {
		a.log.WarnContext(r.Context(), "readiness check failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
