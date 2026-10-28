package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Ref identifies a target by application and target name ("app/target"). See
// ADR-0007.
type Ref struct {
	Application string
	Target      string
}

func (r Ref) String() string { return r.Application + "/" + r.Target }

// lockPollInterval is how often a run waits for a file lock held by another
// process. flock cannot be waited on with a context, so the wait polls.
const lockPollInterval = 100 * time.Millisecond

// locks serializes operations on the same target within this process. Each
// target has its own one-slot semaphore, so different targets never wait for
// each other. Waiting honors ctx.
//
// acquire creates the slot for any valid app/target, before render checks that
// the target exists in the manifests. The map therefore grows with the number of
// distinct app/target pairs that ran a sync or rollback in this process. Entries
// are never removed.
type locks struct {
	mu    sync.Mutex
	slots map[Ref]chan struct{}
	// onWait, if set, is called once when acquire starts to block. Tests use it
	// to know that a caller is waiting for the lock.
	onWait func()
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
	if l.onWait != nil {
		l.onWait()
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

// fileLocks serializes operations on the same target across processes: the CLI
// and the server are separate processes, so their in-process locks cannot see
// each other. Each target has a flock on <dir>/<lockFileName>. An empty dir
// disables the file locks, which leaves only the in-process locks.
type fileLocks struct {
	dir string
	// onWait, if set, is called once when acquire starts to wait for another
	// process. Tests use it to know that a caller is waiting.
	onWait func()
}

// acquire takes the file lock of r and returns the function that releases it.
// If ctx ends while another process holds the lock, it returns ctx.Err() and
// holds nothing.
func (l *fileLocks) acquire(ctx context.Context, r Ref) (func(), error) {
	if l.dir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return nil, fmt.Errorf("lock %s: %w", r, err)
	}
	f, err := os.OpenFile(filepath.Join(l.dir, lockFileName(r)), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", r, err)
	}
	fd := int(f.Fd())
	waited := false
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", r, err)
		}
		if !waited && l.onWait != nil {
			l.onWait()
		}
		waited = true
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

// lockFileName is the file name of the lock of r: the hex SHA-256 of "app/target".
// A plain "app-target" name collides: app "a-b" with target "c" and app "a" with
// target "b-c" give the same name. The hash is unambiguous because ValidateRef
// keeps "/" out of the names, and it keeps the name inside the lock directory.
func lockFileName(r Ref) string {
	sum := sha256.Sum256([]byte(r.String()))
	return hex.EncodeToString(sum[:]) + ".lock"
}
