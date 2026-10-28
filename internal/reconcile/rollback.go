package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/store"
)

// Rollback applies the rendered spec of revision number of the target and
// records it as a new revision. Revision numbers only grow, so the history
// keeps the rolled-back-from state. It returns the number of the new revision.
// The run holds the same per-target lock as Sync.
func (s *Service) Rollback(ctx context.Context, r Ref, number int64) (int64, error) {
	if err := validateRef(r); err != nil {
		return 0, err
	}
	// Revisions are immutable, so the spec is read before the lock. A missing or
	// unreadable revision is reported without touching the target's status.
	rev, err := s.store.Revision(ctx, r.Application, r.Target, number)
	if err != nil {
		return 0, fmt.Errorf("rollback %s to revision %d: %w", r, number, err)
	}
	var state desired.Desired
	if err := json.Unmarshal(rev.RenderedSpec, &state); err != nil {
		return 0, fmt.Errorf("rollback %s: revision %d is corrupted: decode rendered spec: %w", r, number, err)
	}
	if state.Application != r.Application || state.Target != r.Target {
		return 0, fmt.Errorf("rollback %s to revision %d: revision belongs to %s/%s",
			r, number, state.Application, state.Target)
	}
	// Encoding the decoded state must give back the stored bytes. If it does
	// not, the stored spec lost or changed a field, and applying it would not
	// apply what was rendered.
	canonical, err := state.Canonical()
	if err != nil || !bytes.Equal(canonical, rev.RenderedSpec) {
		return 0, fmt.Errorf("rollback %s: revision %d is corrupted: rendered spec does not round-trip", r, number)
	}
	// A spec without services would make Prune remove every object of the target.
	if len(state.Services) == 0 {
		return 0, fmt.Errorf("rollback %s: revision %d has no services, refusing rollback", r, number)
	}

	op := operation{
		kind:      kindRollback,
		started:   EventRollbackStarted,
		succeeded: EventRollbackSucceeded,
		failed:    EventRollbackFailed,
		message:   fmt.Sprintf("rollback to revision %d started", number),
		revision:  number,
	}
	return s.execute(ctx, r, op, func(_ context.Context, target store.Target) (*desired.Desired, string, error) {
		return &state, target.Cluster, nil
	})
}
