package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/d00p1/wormcd/internal/store"
	_ "modernc.org/sqlite"
)

const wormcdYAML = `apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
spec:
  compose: compose.yaml
  targets:
    - name: prod
      cluster: prod-1
    - name: staging
      cluster: staging-1
`

var (
	prod    = Ref{Application: "web", Target: "prod"}
	staging = Ref{Application: "web", Target: "staging"}
)

const (
	imageV1 = "ghcr.io/d00p1/web:v1"
	imageV2 = "ghcr.io/d00p1/web:v2"
)

// env is a store in a temp directory, a deploy checkout with the web app and
// a fake runtime. The test also reads the events table through its own handle,
// because the store has no reader for events.
type env struct {
	svc  *Service
	st   *store.Store
	db   *sql.DB
	root string
	fake *fakeRuntime
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "wormcd.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	root := filepath.Join(dir, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, db: db, root: root, fake: newFakeRuntime()}
	writeFile(t, filepath.Join(root, "wormcd.yaml"), wormcdYAML)
	e.setImage(t, imageV1)
	e.svc = New(st, func(_ context.Context, _ string) (runtime.Runtime, error) {
		return e.fake, nil
	}, root)
	return e
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setImage changes the image the checkout declares, as a git commit would.
func (e *env) setImage(t *testing.T, image string) {
	t.Helper()
	writeFile(t, filepath.Join(e.root, "compose.yaml"), fmt.Sprintf("services:\n  web:\n    image: %s\n", image))
}

func (e *env) target(t *testing.T, name string) store.Target {
	t.Helper()
	got, err := e.st.Target(context.Background(), "web", name)
	if err != nil {
		t.Fatalf("Target %s: %v", name, err)
	}
	return got
}

type eventRow struct {
	Type     EventType
	Revision int64
	Payload  string
}

func (e *env) events(t *testing.T, target string) []eventRow {
	t.Helper()
	rows, err := e.db.Query(`
		SELECT ev.type, COALESCE(r.number, 0), COALESCE(ev.payload, '')
		FROM events ev
		JOIN targets t ON t.id = ev.target_id
		LEFT JOIN revisions r ON r.id = ev.revision_id
		WHERE t.app = 'web' AND t.target = ?
		ORDER BY ev.id`, target)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []eventRow
	for rows.Next() {
		var row eventRow
		if err := rows.Scan(&row.Type, &row.Revision, &row.Payload); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("events: %v", err)
	}
	return out
}

func eventTypes(events []eventRow) []EventType {
	types := make([]EventType, 0, len(events))
	for _, e := range events {
		types = append(types, e.Type)
	}
	return types
}

func (e *env) revisionCount(t *testing.T, target string) int {
	t.Helper()
	revs, err := e.st.Revisions(context.Background(), "web", target)
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	return len(revs)
}

func (e *env) assertState(t *testing.T, target string, status store.Status, last int64) store.Target {
	t.Helper()
	got := e.target(t, target)
	if got.Status != status || got.LastRevision != last {
		t.Fatalf("%s = status %s last revision %d, want %s and %d (message %q)",
			target, got.Status, got.LastRevision, status, last, got.Message)
	}
	return got
}

func TestSyncCreatesServicesAndRevision(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	number, err := e.svc.Sync(ctx, prod)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if number != 1 {
		t.Fatalf("revision = %d, want 1", number)
	}

	got := e.assertState(t, "prod", store.StatusSynced, 1)
	if got.Cluster != "prod-1" {
		t.Errorf("cluster = %q, want prod-1 from the manifest", got.Cluster)
	}
	images := e.fake.images("prod")
	if len(images) != 1 {
		t.Fatalf("observed services = %v, want one", images)
	}
	for name, image := range images {
		if image != imageV1 {
			t.Errorf("%s image = %s, want %s", name, image, imageV1)
		}
	}

	rev, err := e.st.Revision(ctx, "web", "prod", 1)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if rev.Commit != "" {
		t.Errorf("commit = %q, want empty (no git in M1)", rev.Commit)
	}
	for _, image := range rev.ImageDigests {
		if image != imageV1 {
			t.Errorf("image digest entry = %s, want %s", image, imageV1)
		}
	}

	want := []EventType{EventSyncStarted, EventSyncSucceeded}
	if types := eventTypes(e.events(t, "prod")); !slices.Equal(types, want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
}

func TestSyncRepairsManualDrift(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.Sync(ctx, prod); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	name := e.fake.serviceName("prod")
	e.fake.setImage("prod", name, "ghcr.io/someone/manual:hotfix")
	if status, err := e.svc.Status(ctx, prod); err != nil || status != store.StatusOutOfSync {
		t.Fatalf("Status after drift = %v, %v; want OutOfSync", status, err)
	}

	number, err := e.svc.Sync(ctx, prod)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if number != 2 {
		t.Fatalf("revision = %d, want 2", number)
	}
	if image := e.fake.images("prod")[name]; image != imageV1 {
		t.Fatalf("image after sync = %s, want %s", image, imageV1)
	}
	e.assertState(t, "prod", store.StatusSynced, 2)
	if status, err := e.svc.Status(ctx, prod); err != nil || status != store.StatusSynced {
		t.Fatalf("Status after repair = %v, %v; want Synced", status, err)
	}
}

func TestSyncApplyErrorDegradesAndKeepsLastRevision(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.Sync(ctx, prod); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	e.setImage(t, imageV2)
	e.fake.applyErr = errors.New("service update rejected")
	_, prunesBefore := mutations(e.fake)

	_, err := e.svc.Sync(ctx, prod)
	if err == nil || !strings.Contains(err.Error(), "apply") {
		t.Fatalf("Sync err = %v, want apply error", err)
	}

	got := e.assertState(t, "prod", store.StatusDegraded, 1)
	if !strings.Contains(got.Message, "apply") {
		t.Errorf("message = %q, want it to name the apply stage", got.Message)
	}
	if _, prunes := mutations(e.fake); prunes != prunesBefore {
		t.Fatalf("Prune called after failed Apply")
	}
	if n := e.revisionCount(t, "prod"); n != 1 {
		t.Fatalf("revisions = %d, want 1", n)
	}
	events := e.events(t, "prod")
	last := events[len(events)-1]
	if last.Type != EventSyncFailed || !strings.Contains(last.Payload, `"stage":"apply"`) {
		t.Fatalf("last event = %+v, want sync.failed at apply", last)
	}
}

func TestSyncUnhealthyDegradesWithoutRevision(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.Sync(ctx, prod); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	e.setImage(t, imageV2)
	e.fake.healthy = false
	e.fake.problems = []string{"service web: 0/1 tasks running"}

	_, err := e.svc.Sync(ctx, prod)
	if err == nil {
		t.Fatal("Sync: want health error")
	}
	got := e.assertState(t, "prod", store.StatusDegraded, 1)
	if !strings.Contains(got.Message, "not healthy") || !strings.Contains(got.Message, "0/1 tasks running") {
		t.Errorf("message = %q, want health problems", got.Message)
	}
	if n := e.revisionCount(t, "prod"); n != 1 {
		t.Fatalf("revisions = %d, want 1: an unhealthy sync must not record a revision", n)
	}
}

func TestSyncInvalidManifestIsBlocked(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	writeFile(t, filepath.Join(e.root, "wormcd.yaml"), "kind: [unterminated\n")

	if _, err := e.svc.Sync(ctx, prod); err == nil {
		t.Fatal("Sync: want render error")
	}
	e.assertState(t, "prod", store.StatusBlocked, 0)
	if inspects, applies, _, _ := e.fake.calls(); inspects != 0 || applies != 0 {
		t.Fatalf("runtime touched: inspects=%d applies=%d", inspects, applies)
	}
	events := e.events(t, "prod")
	if last := events[len(events)-1]; last.Type != EventSyncFailed || !strings.Contains(last.Payload, `"stage":"render"`) {
		t.Fatalf("last event = %+v, want sync.failed at render", last)
	}
}

func TestSyncUnavailableClusterIsBlocked(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "wormcd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "wormcd.yaml"), wormcdYAML)
	writeFile(t, filepath.Join(root, "compose.yaml"), fmt.Sprintf("services:\n  web:\n    image: %s\n", imageV1))

	fake := newFakeRuntime()
	svc := New(st, func(_ context.Context, cluster string) (runtime.Runtime, error) {
		return nil, fmt.Errorf("cluster %s: connection refused", cluster)
	}, root)

	if _, err := svc.Sync(ctx, prod); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Sync err = %v, want connection error", err)
	}
	got, err := st.Target(ctx, "web", "prod")
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if got.Status != store.StatusBlocked || got.Cluster != "prod-1" {
		t.Fatalf("target = %+v, want Blocked on prod-1", got)
	}
	if inspects, _, _, _ := fake.calls(); inspects != 0 {
		t.Fatalf("runtime touched: inspects=%d", inspects)
	}
}

func TestSyncSameTargetIsSerialized(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	gate := make(chan struct{})
	e.fake.gates["prod"] = gate
	e.fake.entered = make(chan string, 2)

	first := syncAsync(e.svc, ctx, prod)
	<-e.fake.entered // the first sync holds the target and is blocked in Apply

	second := syncAsync(e.svc, ctx, prod)
	close(gate)

	for _, res := range []syncResult{<-first, <-second} {
		if res.err != nil {
			t.Fatalf("Sync: %v", res.err)
		}
	}
	if n := e.revisionCount(t, "prod"); n != 2 {
		t.Fatalf("revisions = %d, want 2", n)
	}
	if max := e.fake.maxInFlight["prod"]; max != 1 {
		t.Fatalf("max concurrent syncs of prod = %d, want 1", max)
	}
}

func TestSyncWaitsForTargetLock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	gate := make(chan struct{})
	e.fake.gates["prod"] = gate
	e.fake.entered = make(chan string, 1)

	first := syncAsync(e.svc, ctx, prod)
	<-e.fake.entered

	// The lock is held and cannot be taken, so a canceled context must make the
	// second sync give up before it reaches the runtime.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.svc.Sync(canceled, prod); !errors.Is(err, context.Canceled) {
		t.Fatalf("second Sync err = %v, want context.Canceled while the lock is held", err)
	}
	if inspects, _, _, _ := e.fake.calls(); inspects != 1 {
		t.Fatalf("inspects = %d, want 1: the waiting sync must not reach the runtime", inspects)
	}

	close(gate)
	if res := <-first; res.err != nil {
		t.Fatalf("first Sync: %v", res.err)
	}
}

func TestSyncDifferentTargetsRunInParallel(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	gate := make(chan struct{})
	e.fake.gates["prod"] = gate
	e.fake.entered = make(chan string, 1)

	first := syncAsync(e.svc, ctx, prod)
	<-e.fake.entered

	// prod is blocked in Apply. staging must finish without waiting for it.
	if _, err := e.svc.Sync(ctx, staging); err != nil {
		t.Fatalf("staging Sync while prod is busy: %v", err)
	}
	close(gate)
	if res := <-first; res.err != nil {
		t.Fatalf("prod Sync: %v", res.err)
	}
	e.assertState(t, "staging", store.StatusSynced, 1)
	e.assertState(t, "prod", store.StatusSynced, 1)
}

func TestStatusDoesNotMutate(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.Sync(ctx, prod); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	inspects, applies, prunes, _ := e.fake.calls()
	eventCount := len(e.events(t, "prod"))

	if status, err := e.svc.Status(ctx, prod); err != nil || status != store.StatusSynced {
		t.Fatalf("Status = %v, %v; want Synced", status, err)
	}
	e.fake.setImage("prod", e.fake.serviceName("prod"), "ghcr.io/someone/manual:hotfix")
	if status, err := e.svc.Status(ctx, prod); err != nil || status != store.StatusOutOfSync {
		t.Fatalf("Status after drift = %v, %v; want OutOfSync", status, err)
	}

	gotInspects, gotApplies, gotPrunes, _ := e.fake.calls()
	if gotApplies != applies || gotPrunes != prunes {
		t.Fatalf("Status mutated the cluster: applies %d->%d, prunes %d->%d", applies, gotApplies, prunes, gotPrunes)
	}
	if gotInspects != inspects+2 {
		t.Fatalf("inspects = %d, want %d", gotInspects, inspects+2)
	}
	if n := len(e.events(t, "prod")); n != eventCount {
		t.Fatalf("events = %d, want %d: Status writes no events", n, eventCount)
	}
	e.assertState(t, "prod", store.StatusSynced, 1)
}

type syncResult struct {
	revision int64
	err      error
}

func syncAsync(svc *Service, ctx context.Context, r Ref) <-chan syncResult {
	out := make(chan syncResult, 1)
	go func() {
		revision, err := svc.Sync(ctx, r)
		out <- syncResult{revision: revision, err: err}
	}()
	return out
}

func mutations(f *fakeRuntime) (applies, prunes int) {
	_, applies, prunes, _ = f.calls()
	return applies, prunes
}

var _ runtime.Runtime = (*fakeRuntime)(nil)
