// Package api exposes the WormCD HTTP API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/d00p1/wormcd/internal/reconcile"
	"github.com/d00p1/wormcd/internal/store"
)

// Operations is the part of the reconcile core the API runs. The CLI runs the
// same operations, so both report the same results.
type Operations interface {
	Sync(ctx context.Context, r reconcile.Ref) (int64, error)
	Rollback(ctx context.Context, r reconcile.Ref, number int64) (int64, error)
	Status(ctx context.Context, r reconcile.Ref) (store.Status, error)
}

// Records is the read side of the store.
type Records interface {
	Target(ctx context.Context, application, target string) (store.Target, error)
	Revisions(ctx context.Context, application, target string) ([]store.Revision, error)
}

// Server is the WormCD HTTP API.
type Server struct {
	mux     *http.ServeMux
	ops     Operations
	records Records
}

// New returns a Server with all routes registered.
func New(ops Operations, records Records) *Server {
	s := &Server{mux: http.NewServeMux(), ops: ops, records: records}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /api/v1/apps/{app}/targets/{target}", s.handleTarget)
	s.mux.HandleFunc("GET /api/v1/apps/{app}/targets/{target}/status", s.handleStatus)
	s.mux.HandleFunc("POST /api/v1/apps/{app}/targets/{target}/sync", s.handleSync)
	s.mux.HandleFunc("POST /api/v1/apps/{app}/targets/{target}/rollback", s.handleRollback)
	s.mux.HandleFunc("GET /api/v1/apps/{app}/targets/{target}/history", s.handleHistory)
	return s
}

// Handler returns the root http.Handler. Every request first passes the
// loopback guard, so a page from another site cannot reach the API through a
// cross-site form or a DNS-rebound name.
func (s *Server) Handler() http.Handler { return loopbackOnly(s.mux) }

// TargetView is a target as stored: the last sync result, without contacting
// the cluster.
type TargetView struct {
	Application  string       `json:"application"`
	Target       string       `json:"target"`
	Cluster      string       `json:"cluster,omitempty"`
	Status       store.Status `json:"status"`
	Message      string       `json:"message,omitempty"`
	LastRevision int64        `json:"lastRevision"`
}

// StatusView is the stored target together with the live drift check.
type StatusView struct {
	Target TargetView   `json:"target"`
	Live   store.Status `json:"live"`
}

// RunView is the result of a sync or rollback: the revision it recorded.
type RunView struct {
	Application string `json:"application"`
	Target      string `json:"target"`
	Revision    int64  `json:"revision"`
}

// RevisionView is one entry of a target's history. Only successful runs store a
// revision, so every entry is Synced.
type RevisionView struct {
	Number    int64        `json:"number"`
	CreatedAt time.Time    `json:"createdAt"`
	Commit    string       `json:"commit,omitempty"`
	Status    store.Status `json:"status"`
}

// HistoryView lists the revisions of a target, newest first.
type HistoryView struct {
	Application string         `json:"application"`
	Target      string         `json:"target"`
	Revisions   []RevisionView `json:"revisions"`
}

// NewTargetView converts a stored target to its view.
func NewTargetView(t store.Target) TargetView {
	return TargetView{
		Application:  t.Application,
		Target:       t.Target,
		Cluster:      t.Cluster,
		Status:       t.Status,
		Message:      t.Message,
		LastRevision: t.LastRevision,
	}
}

// NewHistoryView converts stored revisions of application/target to a view.
func NewHistoryView(application, target string, revs []store.Revision) HistoryView {
	view := HistoryView{Application: application, Target: target, Revisions: make([]RevisionView, 0, len(revs))}
	for _, rev := range revs {
		view.Revisions = append(view.Revisions, RevisionView{
			Number:    rev.Number,
			CreatedAt: rev.CreatedAt,
			Commit:    rev.Commit,
			Status:    store.StatusSynced,
		})
	}
	return view
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleTarget(w http.ResponseWriter, r *http.Request) {
	ref := refOf(r)
	if err := reconcile.ValidateRef(ref); err != nil {
		writeError(w, err)
		return
	}
	t, err := s.records.Target(r.Context(), ref.Application, ref.Target)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, NewTargetView(t))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ref := refOf(r)
	if err := reconcile.ValidateRef(ref); err != nil {
		writeError(w, err)
		return
	}
	t, err := s.records.Target(r.Context(), ref.Application, ref.Target)
	if err != nil {
		writeError(w, err)
		return
	}
	live, err := s.ops.Status(r.Context(), ref)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, StatusView{Target: NewTargetView(t), Live: live})
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	ref := refOf(r)
	// The body is optional, but when it is sent it must be an empty JSON object.
	var req struct{}
	if err := decodeBody(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A run must finish even if the client goes away, or it would stop halfway through an apply.
	// Waiting for another run on the same target has no deadline: a known limitation of M1.
	number, err := s.ops.Sync(context.WithoutCancel(r.Context()), ref)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, RunView{Application: ref.Application, Target: ref.Target, Revision: number})
}

type rollbackRequest struct {
	Revision int64 `json:"revision"`
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	ref := refOf(r)
	var req rollbackRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Revision < 1 {
		writeJSONError(w, http.StatusBadRequest, "revision must be a positive number")
		return
	}
	// A run must finish even if the client goes away, or it would stop halfway through an apply.
	number, err := s.ops.Rollback(context.WithoutCancel(r.Context()), ref, req.Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, RunView{Application: ref.Application, Target: ref.Target, Revision: number})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	ref := refOf(r)
	if err := reconcile.ValidateRef(ref); err != nil {
		writeError(w, err)
		return
	}
	revs, err := s.records.Revisions(r.Context(), ref.Application, ref.Target)
	if err != nil {
		writeError(w, err)
		return
	}
	// An empty history is the answer for a known target only: an unknown one is 404, as in the CLI.
	if len(revs) == 0 {
		if _, err := s.records.Target(r.Context(), ref.Application, ref.Target); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, NewHistoryView(ref.Application, ref.Target, revs))
}

// decodeBody reads one JSON object from the body of r into v. Unknown fields,
// more than one value and a body that is not JSON are errors. An empty body
// returns an error that wraps io.EOF.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("body must hold one JSON object")
	}
	return nil
}

func refOf(r *http.Request) reconcile.Ref {
	return reconcile.Ref{Application: r.PathValue("app"), Target: r.PathValue("target")}
}

// errorBody is the body of every failed request. Reason is a stable word for
// the client; Revision is set when a failed run had recorded a revision.
type errorBody struct {
	Error    string `json:"error"`
	Reason   string `json:"reason,omitempty"`
	Revision int64  `json:"revision,omitempty"`
}

// writeError maps a failure to a status code and reason:
//   - an unknown target or revision: 404 not_found
//   - a name that is not a target name: 400 invalid_ref
//   - a manifest that does not parse, validate or render: 422 invalid_manifest
//   - a corrupted stored revision: 422 corrupt_revision
//   - a run that failed on the cluster, the store or the checkout: 500 sync_failed
//   - anything else: 500 internal
func writeError(w http.ResponseWriter, err error) {
	var run *reconcile.RunError
	isRun := errors.As(err, &run)
	body := errorBody{Error: err.Error()}
	if isRun {
		body.Revision = run.Revision
	}
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, reconcile.ErrUnknownTarget):
		status, body.Reason = http.StatusNotFound, "not_found"
	case errors.Is(err, reconcile.ErrInvalidRef):
		status, body.Reason = http.StatusBadRequest, "invalid_ref"
	case errors.Is(err, reconcile.ErrCorruptRevision):
		status, body.Reason = http.StatusUnprocessableEntity, "corrupt_revision"
	case errors.Is(err, reconcile.ErrInvalidManifest):
		status, body.Reason = http.StatusUnprocessableEntity, "invalid_manifest"
	case isRun:
		body.Reason = "sync_failed"
	default:
		body.Reason = "internal"
	}
	writeJSON(w, status, body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// loopbackOnly refuses requests that a local client would not send. The API has
// no authentication in M1, so it relies on the address it listens on. A browser
// page can still reach that address: through a DNS name that rebinds to
// 127.0.0.1 (the Host header then names that site) or through a cross-site
// form (the Origin header names that site, and a form posts without JSON). The
// guard rejects a foreign Host or Origin, and requires JSON for POST.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			writeJSONError(w, http.StatusForbidden, "host is not a loopback address")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
			writeJSONError(w, http.StatusForbidden, "origin is not a loopback address")
			return
		}
		if r.Method == http.MethodPost && !isJSON(r.Header.Get("Content-Type")) {
			writeJSONError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether hostport, a Host header or the host of an
// Origin, names localhost, 127.0.0.1 or [::1], with or without a port.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}

// isLoopbackOrigin reports whether origin is an http or https URL of a loopback
// host, with no user info and no path. Without the user-info check,
// "http://evil.example@127.0.0.1" would pass as 127.0.0.1.
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if u.Path != "" && u.Path != "/" {
		return false
	}
	return isLoopbackHost(u.Host)
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "error", err)
	}
}
