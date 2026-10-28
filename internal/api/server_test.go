package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/reconcile"
	"github.com/d00p1/wormcd/internal/store"
)

func TestHealthz(t *testing.T) {
	req := localRequest(http.MethodGet, "/healthz", "")
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
	// ctxErr is the error of the context the last run received.
	ctxErr error
}

func (f *fakeOps) Sync(ctx context.Context, r reconcile.Ref) (int64, error) {
	f.synced = append(f.synced, r)
	f.ctxErr = ctx.Err()
	return f.syncRev, f.syncErr
}

func (f *fakeOps) Rollback(ctx context.Context, _ reconcile.Ref, number int64) (int64, error) {
	f.rolledBack = append(f.rolledBack, number)
	f.ctxErr = ctx.Err()
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

// localRequest builds a request the way a local client sends it: a loopback
// Host and a JSON content type.
func localRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	return req
}

func serve(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(method, path, body))
	return rec
}

func decodeError(t *testing.T, body []byte) errorBody {
	t.Helper()
	var got errorBody
	decode(t, body, &got)
	return got
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
	ops := &fakeOps{syncErr: &reconcile.RunError{Stage: "apply", Err: errors.New("apply failed")}}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	got := decodeError(t, rec.Body.Bytes())
	if got.Reason != "sync_failed" || !strings.Contains(got.Error, "apply failed") || got.Revision != 0 {
		t.Fatalf("body = %+v, want reason sync_failed with the error text and no revision", got)
	}
}

func TestSyncFailureReportsRecordedRevision(t *testing.T) {
	ops := &fakeOps{syncErr: &reconcile.RunError{Stage: "status", Revision: 4, Err: errors.New("status not updated")}}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := decodeError(t, rec.Body.Bytes()); got.Revision != 4 {
		t.Fatalf("body = %+v, want revision 4: the revision exists", got)
	}
}

func TestSyncUnknownTargetIsNotFound(t *testing.T) {
	ops := &fakeOps{syncErr: fmt.Errorf("sync web/x: %w", reconcile.ErrUnknownTarget)}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/x/sync", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body)
	}
	if got := decodeError(t, rec.Body.Bytes()); got.Reason != "not_found" {
		t.Fatalf("reason = %q, want not_found", got.Reason)
	}
}

func TestSyncInvalidManifestIsUnprocessable(t *testing.T) {
	ops := &fakeOps{syncErr: &reconcile.RunError{Stage: reconcile.StageRender, Err: fmt.Errorf("%w: compose: bad field", reconcile.ErrInvalidManifest)}}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body)
	}
	if got := decodeError(t, rec.Body.Bytes()); got.Reason != "invalid_manifest" || !strings.Contains(got.Error, "bad field") {
		t.Fatalf("body = %+v, want reason invalid_manifest", got)
	}
}

func TestStatusInvalidManifestIsUnprocessable(t *testing.T) {
	ops := &fakeOps{statusErr: &reconcile.RunError{Stage: reconcile.StageRender, Err: fmt.Errorf("%w: bad manifest", reconcile.ErrInvalidManifest)}}
	records := &fakeRecords{target: store.Target{Application: "web", Target: "prod"}}
	rec := serve(t, New(ops, records).Handler(), http.MethodGet, "/api/v1/apps/web/targets/prod/status", "")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body)
	}
}

func TestRenderErrorWithoutManifestCauseIsServerError(t *testing.T) {
	ops := &fakeOps{syncErr: &reconcile.RunError{Stage: reconcile.StageRender, Err: errors.New("read checkout: permission denied")}}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
	if got := decodeError(t, rec.Body.Bytes()); got.Reason != "sync_failed" {
		t.Fatalf("reason = %q, want sync_failed", got.Reason)
	}
}

// realOps returns the reconcile service of a new store, with the checkout at
// root. Its runtime factory is nil: the tests using it fail before a runtime is
// needed.
func realOps(t *testing.T, root string) *reconcile.Service {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "wormcd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return reconcile.New(st, nil, reconcile.Config{Root: root})
}

func TestSyncMissingRootIsServerError(t *testing.T) {
	ops := realOps(t, filepath.Join(t.TempDir(), "missing"))
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
	if got := decodeError(t, rec.Body.Bytes()); got.Reason != "sync_failed" {
		t.Fatalf("reason = %q, want sync_failed (body %s)", got.Reason, rec.Body)
	}
}

func TestInvalidRefIsBadRequest(t *testing.T) {
	h := New(realOps(t, t.TempDir()), &fakeRecords{}).Handler()
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/apps/web/targets/pr%5Cod/sync", ""},
		{http.MethodPost, "/api/v1/apps/we%5Cb/targets/prod/rollback", `{"revision":1}`},
		{http.MethodGet, "/api/v1/apps/web/targets/pr%5Cod/status", ""},
	} {
		rec := serve(t, h, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, want 400 (body %s)", tc.method, tc.path, rec.Code, rec.Body)
			continue
		}
		if got := decodeError(t, rec.Body.Bytes()); got.Reason != "invalid_ref" {
			t.Errorf("%s %s: reason = %q, want invalid_ref", tc.method, tc.path, got.Reason)
		}
	}
}

func TestStatusRunErrorIsServerError(t *testing.T) {
	ops := &fakeOps{statusErr: &reconcile.RunError{Stage: "runtime", Err: errors.New("connection refused")}}
	records := &fakeRecords{target: store.Target{Application: "web", Target: "prod"}}
	rec := serve(t, New(ops, records).Handler(), http.MethodGet, "/api/v1/apps/web/targets/prod/status", "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
	if got := decodeError(t, rec.Body.Bytes()); got.Reason != "sync_failed" {
		t.Fatalf("reason = %q, want sync_failed", got.Reason)
	}
}

func TestSyncRejectsUnknownField(t *testing.T) {
	ops := &fakeOps{syncRev: 1}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/sync", `{"force":true}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if len(ops.synced) != 0 {
		t.Fatal("sync ran for a body with an unknown field")
	}
	rec = serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty object: status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

func TestStatusUnknownTargetIsNotFound(t *testing.T) {
	ops := &fakeOps{statusErr: &reconcile.RunError{Stage: reconcile.StageRender, Err: fmt.Errorf("%w: missing", reconcile.ErrUnknownTarget)}}
	records := &fakeRecords{target: store.Target{Application: "web", Target: "prod"}}
	rec := serve(t, New(ops, records).Handler(), http.MethodGet, "/api/v1/apps/web/targets/prod/status", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body)
	}
}

func TestRunSurvivesClientCancel(t *testing.T) {
	ops := &fakeOps{syncRev: 2}
	req := localRequest(http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	New(ops, &fakeRecords{}).Handler().ServeHTTP(rec, req.WithContext(canceled))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if ops.ctxErr != nil {
		t.Fatalf("run context error = %v, want none: a client that disconnects must not stop the run", ops.ctxErr)
	}
}

func TestLoopbackGuardHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.1:8080", "localhost", "LocalHost:8080", "[::1]", "[::1]:8080", "::1"} {
		req := localRequest(http.MethodGet, "/healthz", "")
		req.Host = host
		rec := httptest.NewRecorder()
		New(&fakeOps{}, &fakeRecords{}).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: status = %d, want 200", host, rec.Code)
		}
	}
	for _, host := range []string{"evil.example", "evil.example:8080", "127.0.0.2", "0.0.0.0:8080", "", "localhost.evil.example"} {
		ops := &fakeOps{syncRev: 1}
		req := localRequest(http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")
		req.Host = host
		rec := httptest.NewRecorder()
		New(ops, &fakeRecords{}).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: status = %d, want 403", host, rec.Code)
		}
		if len(ops.synced) != 0 {
			t.Errorf("Host %q: sync ran", host)
		}
	}
}

func TestLoopbackGuardOrigin(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:8080", "http://localhost", "http://[::1]:8080", "https://localhost:8443", "http://127.0.0.1/"} {
		req := localRequest(http.MethodGet, "/healthz", "")
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		New(&fakeOps{}, &fakeRecords{}).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Origin %q: status = %d, want 200", origin, rec.Code)
		}
	}
	for _, origin := range []string{"https://evil.example", "http://evil.example:8080", "null", "http://127.0.0.1.evil.example", "http://evil.example@127.0.0.1", "http://127.0.0.1/evil", "ftp://127.0.0.1"} {
		ops := &fakeOps{syncRev: 1}
		req := localRequest(http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		New(ops, &fakeRecords{}).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Origin %q: status = %d, want 403", origin, rec.Code)
		}
		if len(ops.synced) != 0 {
			t.Errorf("Origin %q: sync ran", origin)
		}
	}
}

func TestLoopbackGuardRequiresJSONForPost(t *testing.T) {
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		ops := &fakeOps{syncRev: 1}
		req := localRequest(http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		New(ops, &fakeRecords{}).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q: status = %d, want 415", contentType, rec.Code)
		}
		if len(ops.synced) != 0 {
			t.Errorf("Content-Type %q: sync ran", contentType)
		}
	}

	req := localRequest(http.MethodPost, "/api/v1/apps/web/targets/prod/sync", "")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	New(&fakeOps{syncRev: 1}, &fakeRecords{}).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("application/json with charset: status = %d, want 200", rec.Code)
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
	for _, body := range []string{``, `{`, `{"revision":0}`, `{"revision":-1}`, `{"revision":"2"}`, `{"revision":1}{"revision":2}`, `{"revision":1} x`, `{"revision":1,"force":true}`} {
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

func TestRollbackAcceptsTrailingWhitespace(t *testing.T) {
	ops := &fakeOps{syncRev: 3}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/rollback", "{\"revision\":2}\n\n")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

func TestRollbackMissingRevisionIsNotFound(t *testing.T) {
	ops := &fakeOps{rollbackErr: fmt.Errorf("rollback: %w", store.ErrNotFound)}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/rollback", `{"revision":9}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRollbackCorruptRevisionIsUnprocessable(t *testing.T) {
	ops := &fakeOps{rollbackErr: fmt.Errorf("rollback: %w", reconcile.ErrCorruptRevision)}
	rec := serve(t, New(ops, &fakeRecords{}).Handler(), http.MethodPost, "/api/v1/apps/web/targets/prod/rollback", `{"revision":1}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body)
	}
	if got := decodeError(t, rec.Body.Bytes()); got.Reason != "corrupt_revision" {
		t.Fatalf("reason = %q, want corrupt_revision", got.Reason)
	}
}

func TestHistoryUnknownTargetIsNotFound(t *testing.T) {
	records := &fakeRecords{targetErr: store.ErrNotFound}
	rec := serve(t, New(&fakeOps{}, records).Handler(), http.MethodGet, "/api/v1/apps/web/targets/nope/history", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body)
	}
}

func TestHistoryEmptyKnownTargetIsOK(t *testing.T) {
	records := &fakeRecords{target: store.Target{Application: "web", Target: "prod"}}
	rec := serve(t, New(&fakeOps{}, records).Handler(), http.MethodGet, "/api/v1/apps/web/targets/prod/history", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
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
