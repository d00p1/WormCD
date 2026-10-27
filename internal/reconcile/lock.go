package reconcile

import (
	"context"
	"sync"
)

// Ref identifies a target by application and target name ("app/target"). See
// ADR-0007.
type Ref struct {
	Application string
	Target      string
}

func (r Ref) String() string { return r.Application + "/" + r.Target }

// locks serializes operations on the same target within this process. Each
// target has its own one-slot semaphore, so different targets never wait for
// each other. Waiting honors ctx.
//
// The map gets one entry per target and is never pruned. The number of targets
// is bounded and checked up front (ADR-0007), so the registry cannot grow
// without limit.
type locks struct {
	mu    sync.Mutex
	slots map[Ref]chan struct{}
}

func newLocks() *locks {
	return &locks{slots: make(map[Ref]chan struct{})}
}

// acquire blocks until the target is free and returns the function that
// releases it. If ctx ends first, it returns ctx.Err() and holds nothing.
func (l *locks) acquire(ctx context.Context, r Ref) (release func(), err error) {
	slot := l.slot(r)
	// Take a free lock even when ctx is already done; the select below would
	// pick ctx.Done() at random in that case.
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	default:
	}
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *locks) slot(r Ref) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	slot, ok := l.slots[r]
	if !ok {
		slot = make(chan struct{}, 1)
		l.slots[r] = slot
	}
	return slot
}
