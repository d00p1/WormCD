package reconcile

import (
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
		return 0, fmt.Errorf("rollback %s to revision %d: decode rendered spec: %w", r, number, err)
	}
	if state.Application != r.Application || state.Target != r.Target {
		return 0, fmt.Errorf("rollback %s to revision %d: revision belongs to %s/%s",
			r, number, state.Application, state.Target)
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
