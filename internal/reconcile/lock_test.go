package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/runtime"
)

// otherProcess returns a Service that shares the store, the checkout and the
// data directory of e, but not its in-process locks: it stands for a second
// process, such as the CLI next to the server.
func (e *env) otherProcess() *Service {
	return New(e.st, func(_ context.Context, _ string) (runtime.Runtime, error) {
		return e.fake, nil
	}, Config{Root: e.root, DataDir: e.dir})
}

func TestSyncWaitsForOtherProcessLock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	gate := make(chan struct{})
	e.fake.gates["prod"] = gate
	e.fake.entered = make(chan string, 2)

	first := syncAsync(e.svc, ctx, prod)
	<-e.fake.entered // the first run holds both locks and is blocked in Apply

	other := e.otherProcess()
	waiting := make(chan struct{}, 1)
	other.files.onWait = func() { waiting <- struct{}{} }
	second := syncAsync(other, ctx, prod)
	select {
	case <-waiting:
	case name := <-e.fake.entered:
		t.Fatalf("second run reached Apply for %s while another process holds the target", name)
	}
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
	if _, err := os.Stat(filepath.Join(e.dir, "locks", lockFileName(prod))); err != nil {
		t.Fatalf("lock file: %v", err)
	}
}

func TestLockFilesDoNotCollide(t *testing.T) {
	a := Ref{Application: "a-b", Target: "c"}
	b := Ref{Application: "a", Target: "b-c"}
	if lockFileName(a) == lockFileName(b) {
		t.Fatalf("lock file of %s and %s is the same: %s", a, b, lockFileName(a))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	files := &fileLocks{dir: t.TempDir(), onWait: func() { t.Errorf("a lock of another target was waited for") }}
	unlockA, err := files.acquire(ctx, a)
	if err != nil {
		t.Fatalf("acquire %s: %v", a, err)
	}
	defer unlockA()
	unlockB, err := files.acquire(ctx, b)
	if err != nil {
		t.Fatalf("acquire %s while %s is held: %v", b, a, err)
	}
	unlockB()
}

func TestSyncCanceledWhileWaitingForOtherProcessLock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	gate := make(chan struct{})
	e.fake.gates["prod"] = gate
	e.fake.entered = make(chan string, 1)

	first := syncAsync(e.svc, ctx, prod)
	<-e.fake.entered

	run, cancel := context.WithCancel(ctx)
	defer cancel()
	other := e.otherProcess()
	other.files.onWait = cancel
	if _, err := other.Sync(run, prod); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync err = %v, want context.Canceled while another process holds the target", err)
	}
	if inspects, _, _, _ := e.fake.calls(); inspects != 1 {
		t.Fatalf("inspects = %d, want 1: the waiting run must not reach the runtime", inspects)
	}

	close(gate)
	if res := <-first; res.err != nil {
		t.Fatalf("first Sync: %v", res.err)
	}
	if n := e.revisionCount(t, "prod"); n != 1 {
		t.Fatalf("revisions = %d, want 1", n)
	}
}
