package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/manifest"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/d00p1/wormcd/internal/store"
)

// RuntimeFactory returns the runtime for a cluster. Reconcile does not choose
// the implementation: the composition root injects it (ADR-0003).
type RuntimeFactory func(ctx context.Context, cluster string) (runtime.Runtime, error)

// Service runs sync, rollback and status for targets. Operations on one target
// are serialized in this process (ADR-0007); different targets run in parallel.
type Service struct {
	store    *store.Store
	runtimes RuntimeFactory
	root     string
	locks    *locks
}

// New returns a Service. root is the deploy repository checkout that sync
// renders manifests from.
func New(st *store.Store, runtimes RuntimeFactory, root string) *Service {
	return &Service{
		store:    st,
		runtimes: runtimes,
		root:     root,
		locks:    newLocks(),
	}
}

// operation describes one sync or rollback for events and status messages.
type operation struct {
	name      string
	started   string
	succeeded string
	failed    string
	// revision is the revision a rollback restores, 0 for sync.
	revision int64
}

// Sync renders the target from the checkout, applies it to the cluster, prunes
// and checks health, then records a revision. It returns the number of the new
// revision. On failure the target's status is set to Blocked or Degraded and the
// last successful revision stays the last one.
func (s *Service) Sync(ctx context.Context, r Ref) (int64, error) {
	if err := validateRef(r); err != nil {
		return 0, err
	}
	release, err := s.locks.acquire(ctx, r)
	if err != nil {
		return 0, fmt.Errorf("sync %s: %w", r, err)
	}
	defer release()

	op := operation{
		name:      "sync",
		started:   EventSyncStarted,
		succeeded: EventSyncSucceeded,
		failed:    EventSyncFailed,
	}
	target, err := s.begin(ctx, r, op, "sync started")
	if err != nil {
		return 0, err
	}

	state, cluster, err := manifest.LoadWithCluster(ctx, manifest.Options{App: r.String(), Root: s.root})
	if err != nil {
		return 0, s.fail(ctx, r, op, store.StatusBlocked, "render", target.LastRevision, err)
	}
	target, err = s.store.EnsureTarget(ctx, r.Application, r.Target, cluster)
	if err != nil {
		return 0, fmt.Errorf("sync %s: %w", r, err)
	}

	rt, err := s.runtimes(ctx, cluster)
	if err != nil {
		return 0, s.fail(ctx, r, op, store.StatusBlocked, "runtime", target.LastRevision, err)
	}
	return s.converge(ctx, r, op, state, rt, target.LastRevision)
}

// Status reports whether the cluster matches the desired state of the target.
// It reads the cluster and the checkout only: it never applies, prunes or
// writes to the store. It does not wait for a running sync, so during a sync it
// can report OutOfSync for an intermediate state.
func (s *Service) Status(ctx context.Context, r Ref) (store.Status, error) {
	if err := validateRef(r); err != nil {
		return "", err
	}
	state, cluster, err := manifest.LoadWithCluster(ctx, manifest.Options{App: r.String(), Root: s.root})
	if err != nil {
		return "", fmt.Errorf("status %s: render: %w", r, err)
	}
	rt, err := s.runtimes(ctx, cluster)
	if err != nil {
		return "", fmt.Errorf("status %s: runtime: %w", r, err)
	}
	observed, err := rt.Inspect(ctx, scopeOf(r))
	if err != nil {
		return "", fmt.Errorf("status %s: inspect: %w", r, err)
	}
	if Diff(state, observed).HasChanges() {
		return store.StatusOutOfSync, nil
	}
	return store.StatusSynced, nil
}

// begin marks the target Progressing, records the start event and returns the
// target row. The row is created on first use with an empty cluster.
func (s *Service) begin(ctx context.Context, r Ref, op operation, message string) (store.Target, error) {
	target, err := s.store.EnsureTarget(ctx, r.Application, r.Target, "")
	if err != nil {
		return store.Target{}, fmt.Errorf("%s %s: %w", op.name, r, err)
	}
	if err := s.store.SetTargetState(ctx, r.Application, r.Target, store.StatusProgressing, message, target.LastRevision); err != nil {
		return store.Target{}, fmt.Errorf("%s %s: %w", op.name, r, err)
	}
	if err := s.event(ctx, r, op.revision, op.started, message, "", ""); err != nil {
		return store.Target{}, fmt.Errorf("%s %s: %w", op.name, r, err)
	}
	return target, nil
}

// converge brings the cluster to state, prunes, checks health and records
// state as the next revision. Each step stops the run at its first error.
func (s *Service) converge(ctx context.Context, r Ref, op operation, state *desired.Desired, rt runtime.Runtime, last int64) (int64, error) {
	scope := scopeOf(r)

	observed, err := rt.Inspect(ctx, scope)
	if err != nil {
		return 0, s.fail(ctx, r, op, store.StatusBlocked, "inspect", last, err)
	}
	plan := Diff(state, observed)
	if plan.HasChanges() {
		if _, err := rt.Apply(ctx, plan); err != nil {
			return 0, s.fail(ctx, r, op, store.StatusDegraded, "apply", last, err)
		}
	}
	if _, err := rt.Prune(ctx, scope, keepSet(state)); err != nil {
		return 0, s.fail(ctx, r, op, store.StatusDegraded, "prune", last, err)
	}

	health, err := rt.Health(ctx, scope)
	if err != nil {
		return 0, s.fail(ctx, r, op, store.StatusDegraded, "health", last, err)
	}
	if !health.Healthy {
		cause := "not healthy"
		if len(health.Problems) > 0 {
			cause += ": " + strings.Join(health.Problems, "; ")
		}
		return 0, s.fail(ctx, r, op, store.StatusDegraded, "health", last, errors.New(cause))
	}

	number, err := s.store.AppendRevision(ctx, r.Application, r.Target, store.NewRevision{
		RenderedSpec: state,
		ImageDigests: imageDigests(state),
	})
	if err != nil {
		return 0, s.fail(ctx, r, op, store.StatusDegraded, "revision", last, err)
	}
	if err := s.store.SetTargetState(ctx, r.Application, r.Target, store.StatusSynced, "", number); err != nil {
		return 0, fmt.Errorf("%s %s: %w", op.name, r, err)
	}
	if err := s.event(ctx, r, number, op.succeeded, op.successMessage(number), "", ""); err != nil {
		return 0, fmt.Errorf("%s %s: %w", op.name, r, err)
	}
	return number, nil
}

// fail records a failed run: target status, message and failure event. The
// returned error carries the stage and the cause. Store errors are joined to it
// so that a failed record is not lost.
func (s *Service) fail(ctx context.Context, r Ref, op operation, status store.Status, stage string, last int64, cause error) error {
	err := fmt.Errorf("%s %s: %s: %w", op.name, r, stage, cause)
	setErr := s.store.SetTargetState(ctx, r.Application, r.Target, status, err.Error(), last)
	evErr := s.event(ctx, r, op.revision, op.failed, fmt.Sprintf("%s failed at %s", op.name, stage), stage, cause.Error())
	return errors.Join(err, setErr, evErr)
}

func (s *Service) event(ctx context.Context, r Ref, revision int64, typ, message, stage, cause string) error {
	return s.store.AddEvent(ctx, store.Event{
		Application: r.Application,
		Target:      r.Target,
		Revision:    revision,
		Type:        typ,
		Payload:     eventPayload{Message: message, Stage: stage, Error: cause},
	})
}

func (op operation) successMessage(number int64) string {
	if op.revision != 0 {
		return fmt.Sprintf("rollback to revision %d succeeded as revision %d", op.revision, number)
	}
	return fmt.Sprintf("sync succeeded as revision %d", number)
}

func validateRef(r Ref) error {
	if r.Application == "" || r.Target == "" {
		return fmt.Errorf("target %q must be in app/target form", r)
	}
	return nil
}

func scopeOf(r Ref) runtime.Scope {
	return runtime.Scope{Application: r.Application, Target: r.Target}
}

// keepSet lists every object the desired state declares, so Prune removes only
// what is not part of it.
func keepSet(state *desired.Desired) runtime.Set {
	keep := runtime.Set{}
	for _, s := range state.Services {
		keep[desired.Key{Kind: desired.KindService, Name: s.Name}] = struct{}{}
	}
	for _, n := range state.Networks {
		keep[desired.Key{Kind: desired.KindNetwork, Name: n.Name}] = struct{}{}
	}
	for _, c := range state.Configs {
		keep[desired.Key{Kind: desired.KindConfig, Name: c.Name}] = struct{}{}
	}
	for _, s := range state.Secrets {
		keep[desired.Key{Kind: desired.KindSecret, Name: s.Name}] = struct{}{}
	}
	for _, v := range state.Volumes {
		keep[desired.Key{Kind: desired.KindVolume, Name: v.Name}] = struct{}{}
	}
	return keep
}

// imageDigests maps each service to the image its desired state declares.
// Resolving the image to a registry digest is not part of M1.
func imageDigests(state *desired.Desired) map[string]string {
	images := make(map[string]string, len(state.Services))
	for _, s := range state.Services {
		images[s.Name] = s.Image
	}
	return images
}
