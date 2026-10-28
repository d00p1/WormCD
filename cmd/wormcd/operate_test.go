package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/d00p1/wormcd/internal/api"
	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/reconcile"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/d00p1/wormcd/internal/store"
)

func TestParseRef(t *testing.T) {
	ref, err := parseRef("sync", "web/staging")
	if err != nil {
		t.Fatalf("parseRef: %v", err)
	}
	if ref != (reconcile.Ref{Application: "web", Target: "staging"}) {
		t.Fatalf("ref = %+v", ref)
	}
	for _, bad := range []string{"web", "/prod", "web/", "web/prod/x", ""} {
		if _, err := parseRef("sync", bad); err == nil {
			t.Errorf("parseRef(%q) returned no error", bad)
		}
	}
}

func TestParseOperationTargetAcceptsFlagsAroundTarget(t *testing.T) {
	for _, args := range [][]string{
		{"web/prod", "--data", "d.db", "--root", "r"},
		{"--data", "d.db", "web/prod", "--root", "r"},
		{"--data", "d.db", "--root", "r", "web/prod"},
	} {
		fs := flag.NewFlagSet("sync", flag.ContinueOnError)
		data, root, output := bindOperationFlags(fs)
		ref, err := parseOperationTarget(fs, "sync", args, output)
		if err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
		if ref.String() != "web/prod" || *data != "d.db" || *root != "r" {
			t.Fatalf("args %v: ref %s data %q root %q", args, ref, *data, *root)
		}
	}
}

func TestParseOperationTargetRejectsBadArguments(t *testing.T) {
	cases := map[string][]string{
		"no target":      {"--data", "d.db"},
		"two targets":    {"web/prod", "web/staging"},
		"bad target":     {"web"},
		"unknown output": {"web/prod", "--output", "yaml"},
		"unknown flag":   {"web/prod", "--nope"},
	}
	for name, args := range cases {
		fs := flag.NewFlagSet("sync", flag.ContinueOnError)
		_, _, output := bindOperationFlags(fs)
		if _, err := parseOperationTarget(fs, "sync", args, output); err == nil {
			t.Errorf("%s: no error for %v", name, args)
		}
	}
}

func TestRollbackRequiresPositiveRevision(t *testing.T) {
	for _, args := range [][]string{
		{"web/prod", "--data", "d.db"},
		{"web/prod", "--data", "d.db", "--revision", "0"},
	} {
		err := runRollbackWith(args, &bytes.Buffer{}, nil)
		if err == nil || !strings.Contains(err.Error(), "--revision") {
			t.Fatalf("args %v: error = %v, want --revision error", args, err)
		}
	}
}

func TestOperationsRequireData(t *testing.T) {
	err := runSyncWith([]string{"web/prod"}, &bytes.Buffer{}, testRuntimes(newMemRuntime()))
	if err == nil || !strings.Contains(err.Error(), "--data is required") {
		t.Fatalf("error = %v, want --data is required", err)
	}
}

// memRuntime keeps the observed state of the cluster in memory. A successful
// Apply makes the observed state equal the desired state.
type memRuntime struct {
	mu       sync.Mutex
	observed runtime.Observed
}

func newMemRuntime() *memRuntime { return &memRuntime{} }

func (m *memRuntime) Kind() runtime.Kind { return runtime.KindSwarm }

func (m *memRuntime) Inspect(_ context.Context, scope runtime.Scope) (runtime.Observed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obs := m.observed
	obs.Application = scope.Application
	obs.Target = scope.Target
	obs.Namespace = desired.TargetNamespace(scope.Application, scope.Target)
	return obs, nil
}

func (m *memRuntime) Apply(_ context.Context, plan runtime.Plan) (runtime.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := plan.Desired
	m.observed = runtime.Observed{
		Services: append([]desired.Service(nil), state.Services...),
		Networks: append([]desired.Network(nil), state.Networks...),
		Configs:  append([]desired.Config(nil), state.Configs...),
		Secrets:  append([]desired.Secret(nil), state.Secrets...),
		Volumes:  append([]desired.Volume(nil), state.Volumes...),
	}
	return runtime.Result{}, nil
}

func (m *memRuntime) Prune(context.Context, runtime.Scope, runtime.Set) (runtime.PruneResult, error) {
	return runtime.PruneResult{}, nil
}

func (m *memRuntime) Health(context.Context, runtime.Scope) (runtime.Health, error) {
	return runtime.Health{Healthy: true}, nil
}

// setImage changes the image of the only service in the cluster, as a manual
// change would.
func (m *memRuntime) setImage(image string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observed.Services[0].Image = image
}

func (m *memRuntime) serviceImage() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.observed.Services[0].Image
}

func testRuntimes(rt *memRuntime) runtimeProvider {
	return func() (reconcile.RuntimeFactory, func()) {
		factory := func(context.Context, string) (runtime.Runtime, error) { return rt, nil }
		return factory, func() {}
	}
}

func writeCheckout(t *testing.T, root, image string) {
	t.Helper()
	const wormcdYAML = `apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
spec:
  compose: compose.yaml
  targets:
    - name: prod
      cluster: local
`
	files := map[string]string{
		"wormcd.yaml":  wormcdYAML,
		"compose.yaml": "services:\n  web:\n    image: " + image + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOperationsEndToEnd runs sync, status, history and rollback through the CLI
// on a fake runtime, then reads the same history through the API.
func TestOperationsEndToEnd(t *testing.T) {
	const (
		imageV1 = "ghcr.io/d00p1/web:v1"
		imageV2 = "ghcr.io/d00p1/web:v2"
	)
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "wormcd.db")
	rt := newMemRuntime()
	provide := testRuntimes(rt)

	writeCheckout(t, root, imageV1)
	var out bytes.Buffer
	if err := runSyncWith([]string{"web/prod", "--root", root, "--data", data}, &out, provide); err != nil {
		t.Fatalf("sync v1: %v", err)
	}
	if got := out.String(); got != "synced web/prod as revision 1\n" {
		t.Fatalf("sync output = %q", got)
	}

	writeCheckout(t, root, imageV2)
	out.Reset()
	if err := runSyncWith([]string{"--data", data, "--root", root, "web/prod"}, &out, provide); err != nil {
		t.Fatalf("sync v2: %v", err)
	}
	if got := out.String(); got != "synced web/prod as revision 2\n" {
		t.Fatalf("sync output = %q", got)
	}
	if rt.serviceImage() != imageV2 {
		t.Fatalf("cluster image = %s, want %s", rt.serviceImage(), imageV2)
	}

	out.Reset()
	if err := runStatusWith([]string{"web/prod", "--root", root, "--data", data}, &out, provide); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out.String(), "live status: Synced") || !strings.Contains(out.String(), "last revision: 2") {
		t.Fatalf("status output = %q", out.String())
	}

	rt.setImage("ghcr.io/d00p1/web:manual")
	out.Reset()
	if err := runStatusWith([]string{"web/prod", "--root", root, "--data", data}, &out, provide); err != nil {
		t.Fatalf("status after drift: %v", err)
	}
	if !strings.Contains(out.String(), "live status: OutOfSync") {
		t.Fatalf("status after drift = %q", out.String())
	}

	out.Reset()
	if err := runHistoryWith([]string{"web/prod", "--data", data, "--root", root, "--output", "json"}, &out, provide); err != nil {
		t.Fatalf("history: %v", err)
	}
	var history api.HistoryView
	if err := json.Unmarshal(out.Bytes(), &history); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(history.Revisions) != 2 || history.Revisions[0].Number != 2 || history.Revisions[1].Number != 1 {
		t.Fatalf("history = %+v, want revisions 2 and 1, newest first", history.Revisions)
	}
	if history.Revisions[0].CreatedAt.IsZero() || history.Revisions[0].Status != store.StatusSynced {
		t.Fatalf("revision 2 = %+v", history.Revisions[0])
	}

	out.Reset()
	if err := runRollbackWith([]string{"web/prod", "--revision", "1", "--data", data, "--root", root}, &out, provide); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := out.String(); got != "rolled back web/prod to revision 1 as revision 3\n" {
		t.Fatalf("rollback output = %q", got)
	}
	if rt.serviceImage() != imageV1 {
		t.Fatalf("cluster image after rollback = %s, want %s", rt.serviceImage(), imageV1)
	}

	out.Reset()
	if err := runHistoryWith([]string{"web/prod", "--data", data, "--root", root}, &out, provide); err != nil {
		t.Fatalf("history table: %v", err)
	}
	if !strings.Contains(out.String(), "REVISION") || strings.Count(out.String(), "Synced") != 3 {
		t.Fatalf("history table = %q, want header and 3 Synced rows", out.String())
	}

	assertAPIMatchesCLI(t, data, root, rt)
}

// assertAPIMatchesCLI serves the same store and runtime through the API and
// checks the target and history it returns.
func assertAPIMatchesCLI(t *testing.T, data, root string, rt *memRuntime) {
	t.Helper()
	cp, closeCP, err := openControlPlane(context.Background(), data, root, testRuntimes(rt))
	if err != nil {
		t.Fatalf("openControlPlane: %v", err)
	}
	defer closeCP()
	h := api.New(cp.svc, cp.store).Handler()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1:8080"
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/api/v1/apps/web/targets/prod")
	var target api.TargetView
	if err := json.Unmarshal(rec.Body.Bytes(), &target); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET target: %d %s", rec.Code, rec.Body)
	}
	if target.LastRevision != 3 || target.Status != store.StatusSynced {
		t.Fatalf("API target = %+v, want revision 3 Synced", target)
	}

	rec = get("/api/v1/apps/web/targets/prod/history")
	var history api.HistoryView
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET history: %d %s", rec.Code, rec.Body)
	}
	if len(history.Revisions) != 3 || history.Revisions[0].Number != 3 {
		t.Fatalf("API history = %+v, want 3 revisions newest first", history.Revisions)
	}

	rec = get("/api/v1/apps/web/targets/missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown target: %d, want 404", rec.Code)
	}
}

func TestWithRecordedNamesRevision(t *testing.T) {
	base := errors.New("sync web/prod: status: write failed")
	if got := withRecorded(base, 0); got != base {
		t.Fatalf("no revision: err = %v, want the error unchanged", got)
	}
	got := withRecorded(base, 4)
	if !errors.Is(got, base) || !strings.Contains(got.Error(), "revision 4 was recorded") {
		t.Fatalf("err = %v, want the cause and revision 4", got)
	}
}

func TestSyncUnknownTargetLeavesNoRow(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "wormcd.db")
	writeCheckout(t, root, "ghcr.io/d00p1/web:v1")

	err := runSyncWith([]string{"web/typo", "--root", root, "--data", data}, &bytes.Buffer{}, testRuntimes(newMemRuntime()))
	if err == nil || !strings.Contains(err.Error(), "not declared in the manifests") {
		t.Fatalf("error = %v, want the undeclared target error", err)
	}
	cp, closeCP, err := openControlPlane(context.Background(), data, root, testRuntimes(newMemRuntime()))
	if err != nil {
		t.Fatalf("openControlPlane: %v", err)
	}
	defer closeCP()
	if _, err := cp.store.Target(context.Background(), "web", "typo"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Target err = %v, want no row for the typo", err)
	}
}

func TestRequireLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		if err := requireLoopback(addr); err != nil {
			t.Errorf("requireLoopback(%q): %v", addr, err)
		}
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "10.0.0.5:8080", "[::]:8080"} {
		if err := requireLoopback(addr); err == nil {
			t.Errorf("requireLoopback(%q) returned no error", addr)
		}
	}
}
