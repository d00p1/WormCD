// Package api exposes the WormCD HTTP API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler { return s.mux }

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
	t, err := s.records.Target(r.Context(), ref.Application, ref.Target)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, NewTargetView(t))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ref := refOf(r)
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
	number, err := s.ops.Sync(r.Context(), ref)
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	if req.Revision < 1 {
		writeJSONError(w, http.StatusBadRequest, "revision must be a positive number")
		return
	}
	number, err := s.ops.Rollback(r.Context(), ref, req.Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, RunView{Application: ref.Application, Target: ref.Target, Revision: number})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	ref := refOf(r)
	revs, err := s.records.Revisions(r.Context(), ref.Application, ref.Target)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, NewHistoryView(ref.Application, ref.Target, revs))
}

func refOf(r *http.Request) reconcile.Ref {
	return reconcile.Ref{Application: r.PathValue("app"), Target: r.PathValue("target")}
}

// writeError maps a failure to a status code: an unknown target or revision is
// 404, anything else is 500 with the error text.
func writeError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSONError(w, http.StatusInternalServerError, err.Error())
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "error", err)
	}
}
