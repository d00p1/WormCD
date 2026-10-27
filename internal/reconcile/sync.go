package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/manifest"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/d00p1/wormcd/internal/store"
)

// RuntimeFactory returns the runtime for a cluster. Reconcile does not choose
// the implementation: the composition root injects it (ADR-0003).
type RuntimeFactory func(ctx context.Context, cluster string) (runtime.Runtime, error)

// reconcileStore is the part of *store.Store that reconcile uses. Tests wrap it
// to make individual writes fail.
type reconcileStore interface {
	EnsureTarget(ctx context.Context, application, target, cluster string) (store.Target, error)
	SetTargetState(ctx context.Context, application, target string, status store.Status, message string, lastRevision int64) error
	AddEvent(ctx context.Context, e store.Event) error
	AppendRevision(ctx context.Context, application, target string, rev store.NewRevision) (int64, error)
	Revision(ctx context.Context, application, target string, number int64) (store.Revision, error)
}

// DefaultSyncTimeout is the deadline of one sync or rollback when Config does
// not set one. It covers the whole run, not one cluster call: a service
// convergence waits per service, so N services need more than one wait.
const DefaultSyncTimeout = 30 * time.Minute

// Config sets up a Service. Root is the deploy repository checkout. SyncTimeout
// is the deadline of one sync or rollback; zero means DefaultSyncTimeout.
type Config struct {
	Root        string
	SyncTimeout time.Duration
}

// finishTimeout bounds the writes that record a failed run. They ignore
// cancellation of the run, which is often the reason for the failure, but must
// not hang on a stuck database.
const finishTimeout = 10 * time.Second

// errSyncDeadline is the cause of the run context when SyncTimeout expires. It
// tells that deadline apart from a cancellation of the caller's context.
var errSyncDeadline = errors.New("sync deadline")

// Service runs sync, rollback and status for targets. Operations on one target
// are serialized in this process (ADR-0007); different targets run in parallel.
type Service struct {
	store       reconcileStore
	runtimes    RuntimeFactory
	root        string
	syncTimeout time.Duration
	locks       *locks
}

// New returns a Service. Sync renders manifests from cfg.Root.
func New(st *store.Store, runtimes RuntimeFactory, cfg Config) *Service {
	timeout := cfg.SyncTimeout
	if timeout == 0 {
		timeout = DefaultSyncTimeout
	}
	return &Service{
		store:       st,
		runtimes:    runtimes,
		root:        cfg.Root,
		syncTimeout: timeout,
		locks:       newLocks(),
	}
}

// opKind is the kind of a run: a sync from the checkout or a rollback to a
// stored revision.
type opKind string

const (
	kindSync     opKind = "sync"
	kindRollback opKind = "rollback"
)

// operation describes one run for events, status messages and errors.
type operation struct {
	kind      opKind
	started   EventType
	succeeded EventType
	failed    EventType
	// message describes the start of the run.
	message string
	// revision is the revision a rollback restores; 0 for sync.
	revision int64
}

// source returns the desired state of the run and the cluster it targets. It
// runs after the target is marked Progressing.
type source func(ctx context.Context, target store.Target) (*desired.Desired, string, error)

// Sync renders the target from the checkout, applies it to the cluster, prunes
// and checks health, then records a revision. It returns the number of the new
// revision. On failure the target's status is set to Blocked or Degraded and the
// last successful revision stays the last one.
func (s *Service) Sync(ctx context.Context, r Ref) (int64, error) {
	op := operation{
		kind:      kindSync,
		started:   EventSyncStarted,
		succeeded: EventSyncSucceeded,
		failed:    EventSyncFailed,
		message:   "sync started",
	}
	return s.execute(ctx, r, op, func(ctx context.Context, _ store.Target) (*desired.Desired, string, error) {
		return s.render(ctx, r)
	})
}

// Status reports whether the cluster matches the desired state of the target.
// It reads the cluster and the checkout only: it never applies, prunes or
// writes to the store. It does not wait for a running sync, so during a sync it
// can report OutOfSync for an intermediate state.
func (s *Service) Status(ctx context.Context, r Ref) (store.Status, error) {
	if err := validateRef(r); err != nil {
		return "", err
	}
	state, cluster, err := s.render(ctx, r)
	if err != nil {
		return "", fmt.Errorf("status %s: render: %w", r, err)
	}
	rt, err := s.runtimes(ctx, cluster)
	if err != nil {
		return "", fmt.Errorf("status %s: runtime: %w", r, err)
	}
	plan, err := observe(ctx, r, state, rt)
	if err != nil {
		return "", fmt.Errorf("status %s: inspect: %w", r, err)
	}
	if plan.HasChanges() {
		return store.StatusOutOfSync, nil
	}
	return store.StatusSynced, nil
}

// execute is the run shared by Sync and Rollback: validate, take the target
// lock, mark the target Progressing, get the desired state and converge.
// The deadline starts once the lock is held, so waiting for another run on the
// same target does not use up this run's time. Waiting itself honors the
// caller's ctx.
func (s *Service) execute(ctx context.Context, r Ref, op operation, src source) (int64, error) {
	if err := validateRef(r); err != nil {
		return 0, err
	}
	release, err := s.locks.acquire(ctx, r)
	if err != nil {
		return 0, runErr(op, r, err)
	}
	defer release()
	ctx, cancel := context.WithTimeoutCause(ctx, s.syncTimeout, errSyncDeadline)
	defer cancel()

	// Without the target row there is nothing to record a failure on, so this
	// error is returned as it is.
	target, err := s.store.EnsureTarget(ctx, r.Application, r.Target, "")
	if err != nil {
		return 0, runErr(op, r, err)
	}
	if err := s.store.SetTargetState(ctx, r.Application, r.Target, store.StatusProgressing, op.message, target.LastRevision); err != nil {
		return 0, s.fail(ctx, r, op, store.StatusDegraded, stageProgress, target.LastRevision, err)
	}
	if err := s.event(ctx, r, op.revision, op.started, op.message, "", ""); err != nil {
		return 0, s.fail(ctx, r, op, store.StatusDegraded, stageStart, target.LastRevision, err)
	}

	state, cluster, err := src(ctx, target)
	if err != nil {
		return 0, s.fail(ctx, r, op, store.StatusBlocked, stageRender, target.LastRevision, err)
	}
	// The row already exists, so the cluster is written only when it changed.
	// The cluster is known only after the manifest is rendered, so a first sync
	// writes it here.
	if cluster != target.Cluster {
		updated, err := s.store.EnsureTarget(ctx, r.Application, r.Target, cluster)
		if err != nil {
			return 0, s.fail(ctx, r, op, store.StatusDegraded, stageTarget, target.LastRevision, err)
		}
		target = updated
	}

	rt, err := s.runtimes(ctx, cluster)
	if err != nil {
		return 0, s.fail(ctx, r, op, store.StatusBlocked, stageRuntime, target.LastRevision, err)
	}
	return s.converge(ctx, r, op, state, rt, target.LastRevision)
}

// converge brings the cluster to state, prunes, checks health and records
// state as the next revision. Each step stops the run at its first error.
func (s *Service) converge(ctx context.Context, r Ref, op operation, state *desired.Desired, rt runtime.Runtime, last int64) (int64, error) {
	plan, err := observe(ctx, r, state, rt)
	if err != nil {
		return 0, s.fail(ctx, r, op, store.StatusBlocked, stageInspect, last, err)
	}
	if plan.HasChanges() {
		if _, err := rt.Apply(ctx, plan); err != nil {
			return 0, s.fail(ctx, r, op, store.StatusDegraded, stageApply, last, err)
		}
	}
	if _, err := rt.Prune(ctx, scopeOf(r), keepSet(state)); err != nil {
		return 0, s.fail(ctx, r, op, store.StatusDegraded, stagePrune, last, err)
	}

	health, err := rt.Health(ctx, scopeOf(r))
	if err != nil {
		return 0, s.fail(ctx, r, op, store.StatusDegraded, stageHealth, last, err)
	}
	if !health.Healthy {
		cause := "not healthy"
		if len(health.Problems) > 0 {
			cause += ": " + strings.Join(health.Problems, "; ")
		}
		return 0, s.fail(ctx, r, op, store.StatusDegraded, stageHealth, last, errors.New(cause))
	}

	number, err := s.store.AppendRevision(ctx, r.Application, r.Target, store.NewRevision{
		RenderedSpec: state,
		ImageDigests: imageDigests(state),
	})
	if err != nil {
		return 0, s.fail(ctx, r, op, store.StatusDegraded, stageRevision, last, err)
	}
	if err := s.store.SetTargetState(ctx, r.Application, r.Target, store.StatusSynced, "", number); err != nil {
		// The revision is recorded and stays. The status says so, because it
		// could not be updated to Synced.
		cause := fmt.Errorf("revision %d recorded but status not updated: %w", number, err)
		return 0, s.fail(ctx, r, op, store.StatusDegraded, stageStatus, number, cause)
	}
	// The revision and the Synced status are already stored, so the number is
	// returned with the error: the caller learns that the revision exists.
	if err := s.event(ctx, r, number, op.succeeded, op.successMessage(number), "", ""); err != nil {
		return number, runErr(op, r, err)
	}
	return number, nil
}

// observe reads the cluster for r and compares it with state. Sync and Status
// both use it, so they report the same plan.
func observe(ctx context.Context, r Ref, state *desired.Desired, rt runtime.Runtime) (runtime.Plan, error) {
	observed, err := rt.Inspect(ctx, scopeOf(r))
	if err != nil {
		return runtime.Plan{}, err
	}
	return Diff(state, observed), nil
}

// render loads the desired state of r from the checkout and returns the
// cluster the target declares.
func (s *Service) render(ctx context.Context, r Ref) (*desired.Desired, string, error) {
	return manifest.LoadWithCluster(ctx, manifest.Options{App: r.String(), Root: s.root})
}

// fail records a failed run: target status, message and failure event. The
// returned error carries the stage and the cause. Store errors are joined to it
// so that a failed record is not lost.
func (s *Service) fail(ctx context.Context, r Ref, op operation, status store.Status, stage Stage, last int64, cause error) error {
	err := runErr(op, r, fmt.Errorf("%s: %w", stage, s.interrupted(ctx, op, cause)))
	// The run may have failed because ctx was canceled, so the failure is
	// written with a context that ignores cancellation.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	setErr := s.store.SetTargetState(fctx, r.Application, r.Target, status, err.Error(), last)
	evErr := s.event(fctx, r, op.revision, op.failed, fmt.Sprintf("%s failed at %s", op.kind, stage), stage, cause.Error())
	return errors.Join(err, setErr, evErr)
}

// interrupted names why the run's context ended, when it did. The run's own
// deadline and a cancellation by the caller are different outcomes.
func (s *Service) interrupted(ctx context.Context, op operation, cause error) error {
	if ctx.Err() == nil {
		return cause
	}
	if errors.Is(context.Cause(ctx), errSyncDeadline) {
		return fmt.Errorf("%s deadline exceeded after %s: %w", op.kind, s.syncTimeout, cause)
	}
	return fmt.Errorf("%s canceled by caller: %w", op.kind, cause)
}

// runErr prefixes err with the run's kind and target.
func runErr(op operation, r Ref, err error) error {
	return fmt.Errorf("%s %s: %w", op.kind, r, err)
}

func (s *Service) event(ctx context.Context, r Ref, revision int64, typ EventType, message string, stage Stage, cause string) error {
	return s.store.AddEvent(ctx, store.Event{
		Application: r.Application,
		Target:      r.Target,
		Revision:    revision,
		Type:        string(typ),
		Payload:     eventPayload{Message: message, Stage: stage, Error: cause},
	})
}

func (op operation) successMessage(number int64) string {
	if op.kind == kindRollback {
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
