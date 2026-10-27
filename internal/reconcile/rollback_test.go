package reconcile

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/d00p1/wormcd/internal/store"
)

func TestRollbackAppliesRevisionAsNewRevision(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.Sync(ctx, prod); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	e.setImage(t, imageV2)
	if _, err := e.svc.Sync(ctx, prod); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	number, err := e.svc.Rollback(ctx, prod, 1)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if number != 3 {
		t.Fatalf("revision = %d, want new revision 3", number)
	}
	for name, image := range e.fake.images("prod") {
		if image != imageV1 {
			t.Fatalf("%s image after rollback = %s, want %s", name, image, imageV1)
		}
	}
	e.assertState(t, "prod", store.StatusSynced, 3)

	r1, err := e.st.Revision(ctx, "web", "prod", 1)
	if err != nil {
		t.Fatalf("Revision 1: %v", err)
	}
	r3, err := e.st.Revision(ctx, "web", "prod", 3)
	if err != nil {
		t.Fatalf("Revision 3: %v", err)
	}
	if !bytes.Equal(r1.RenderedSpec, r3.RenderedSpec) {
		t.Fatalf("revision 3 spec differs from revision 1:\n%s\n%s", r3.RenderedSpec, r1.RenderedSpec)
	}

	events := e.events(t, "prod")
	tail := events[len(events)-2:]
	if tail[0].Type != EventRollbackStarted || tail[0].Revision != 1 {
		t.Fatalf("started = %+v, want rollback.started for revision 1", tail[0])
	}
	if tail[1].Type != EventRollbackSucceeded || tail[1].Revision != 3 ||
		!strings.Contains(tail[1].Payload, "rollback to revision 1") {
		t.Fatalf("succeeded = %+v, want rollback.succeeded to revision 1 as 3", tail[1])
	}
}

func TestRollbackMissingRevisionLeavesStatus(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.Sync(ctx, prod); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	eventsBefore := len(e.events(t, "prod"))

	if _, err := e.svc.Rollback(ctx, prod, 9); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Rollback err = %v, want ErrNotFound", err)
	}
	e.assertState(t, "prod", store.StatusSynced, 1)
	if n := len(e.events(t, "prod")); n != eventsBefore {
		t.Fatalf("events = %d, want %d: a missing revision writes nothing", n, eventsBefore)
	}
}

func TestRollbackApplyErrorDegrades(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.Sync(ctx, prod); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	e.setImage(t, imageV2)
	if _, err := e.svc.Sync(ctx, prod); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	e.fake.applyErr = errors.New("update rejected")
	if _, err := e.svc.Rollback(ctx, prod, 1); err == nil {
		t.Fatal("Rollback: want apply error")
	}
	e.assertState(t, "prod", store.StatusDegraded, 2)
	events := e.events(t, "prod")
	if last := events[len(events)-1]; last.Type != EventRollbackFailed || last.Revision != 1 {
		t.Fatalf("last event = %+v, want rollback.failed for revision 1", last)
	}
}
