package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/reconcile"
	"github.com/d00p1/wormcd/internal/store"
)

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	New(&fakeOps{}, &fakeRecords{}).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got, want := rec.Body.String(), "{\"status\":\"ok\"}\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

type fakeOps struct {
	syncRev     int64
	syncErr     error
	rollbackErr error
	status      store.Status
	statusErr   error

	synced     []reconcile.Ref
	rolledBack []int64
}

func (f *fakeOps) Sync(_ context.Context, r reconcile.Ref) (int64, error) {
	f.synced = append(f.synced, r)
	return f.syncRev, f.syncErr
}

func (f *fakeOps) Rollback(_ context.Context, _ reconcile.Ref, number int64) (int64, error) {
	f.rolledBack = append(f.rolledBack, number)
	if f.rollbackErr != nil {
		return 0, f.rollbackErr
	}
	return f.syncRev, nil
}

func (f *fakeOps) Status(context.Context, reconcile.Ref) (store.Status, error) {
	return f.status, f.statusErr
}

type fakeRecords struct {
	target    store.Target
	targetErr error
	revs      []store.Revision
	revsErr   error
}

func (f *fakeRecords) Target(_ context.Context, app, target string) (store.Target, error) {
	if f.targetErr != nil {
		return store.Target{}, f.targetErr
	}
	return f.target, nil
}

func (f *fakeRecords) Revisions(context.Context, string, string) ([]store.Revision, error) {
	return f.revs, f.revsErr
}

func serve(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGetTarget(t *testing.T) {
	records := &fakeRecords{target: store.Target{
		Application:  "web",
		Target:       "prod",
		Cluster:      "local",
		Status:       store.StatusSynced,
		LastRevision: 3,
	}}
	rec := serve(t, New(&fakeOps{}, records).Handler(), http.MethodGet, "/api/v1/apps/web/targets/prod", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var got TargetView
	decode(t, rec.Body.Bytes(), &got)
	want := TargetView{Application: "web", Target: "prod", Cluster: "local", Status: store.StatusSynced, LastRevision: 3}
	if got != want {
		t.Fatalf("view = %+v, want %+v", got, want)
	}
}

func TestGetTargetNotFound(t *testing.T) {
	records := &fakeRecords{targetErr: store.ErrNotFound}
	rec := serve(t, New(&fakeOps{}, records).Handler(), http.MethodGet, "/api/v1/apps/web/targets/prod", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGetStatusReturnsStoredAndLive(t *testing.T) {
	records := &fakeRecords{target: store.Target{Application: "web", Target: "prod", Status: store.StatusSynced, LastRevision: 2}}
	ops := &fakeOps{status: store.StatusOutOfSync}
	rec := serve(t, New(ops, records).Handler(), http.MethodGet, "/api/v1/apps/web/targets/prod/status", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var got StatusView
	decode(t, rec.Body.Bytes(), &got)
	if got.Live != store.StatusOutOfSync || got.Target.Status != store.StatusSynced || got.Target.LastRevision != 2 {
		t.Fatalf("view = %+v, want stored Synced revision 2 and live OutOfSync", got)
	}
}

func TestSyncReturnsRevision(t *testing.T) {
	ops := &fakeOps{syncRev: 4}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/staging/sync", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var got RunView
	decode(t, rec.Body.Bytes(), &got)
	if got != (RunView{Application: "web", Target: "staging", Revision: 4}) {
		t.Fatalf("view = %+v", got)
	}
	if len(ops.synced) != 1 || ops.synced[0] != (reconcile.Ref{Application: "web", Target: "staging"}) {
		t.Fatalf("synced = %v", ops.synced)
	}
}

func TestSyncErrorIsServerError(t *testing.T) {
	ops := &fakeOps{syncErr: errors.New("apply failed")}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "apply failed") {
		t.Fatalf("body = %s, want the error text", rec.Body)
	}
}

func TestRollbackPassesRevision(t *testing.T) {
	ops := &fakeOps{syncRev: 5}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/rollback", `{"revision":2}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if len(ops.rolledBack) != 1 || ops.rolledBack[0] != 2 {
		t.Fatalf("rolled back to %v, want [2]", ops.rolledBack)
	}
	var got RunView
	decode(t, rec.Body.Bytes(), &got)
	if got.Revision != 5 {
		t.Fatalf("revision = %d, want 5", got.Revision)
	}
}

func TestRollbackRejectsBadBody(t *testing.T) {
	for _, body := range []string{``, `{`, `{"revision":0}`, `{"revision":-1}`, `{"revision":"2"}`} {
		ops := &fakeOps{}
		rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/rollback", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
		if len(ops.rolledBack) != 0 {
			t.Errorf("body %q: rollback ran", body)
		}
	}
}

func TestRollbackMissingRevisionIsNotFound(t *testing.T) {
	ops := &fakeOps{rollbackErr: fmt.Errorf("rollback: %w", store.ErrNotFound)}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/rollback", `{"revision":9}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHistoryListsRevisions(t *testing.T) {
	created := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	records := &fakeRecords{revs: []store.Revision{
		{Number: 2, Commit: "abc123", CreatedAt: created},
		{Number: 1, CreatedAt: created.Add(-time.Hour)},
	}}
	rec := serve(t, New(&fakeOps{}, records).Handler(), http.MethodGet, "/api/v1/apps/web/targets/prod/history", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var got HistoryView
	decode(t, rec.Body.Bytes(), &got)
	if len(got.Revisions) != 2 {
		t.Fatalf("revisions = %d, want 2", len(got.Revisions))
	}
	first := got.Revisions[0]
	if first.Number != 2 || first.Commit != "abc123" || !first.CreatedAt.Equal(created) || first.Status != store.StatusSynced {
		t.Fatalf("first revision = %+v", first)
	}
	if strings.Contains(rec.Body.String(), `"commit":""`) {
		t.Fatalf("empty commit must be omitted: %s", rec.Body)
	}
}

func decode(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}
