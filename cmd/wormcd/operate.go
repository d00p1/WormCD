package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/d00p1/wormcd/internal/api"
	"github.com/d00p1/wormcd/internal/cluster"
	"github.com/d00p1/wormcd/internal/reconcile"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/d00p1/wormcd/internal/runtime/swarm"
	"github.com/d00p1/wormcd/internal/store"
)

// localDockerEndpoint is the only runtime endpoint until the cluster registry
// arrives: the Swarm manager this process runs on.
const localDockerEndpoint = "unix:///var/run/docker.sock"

// runtimeProvider returns the runtime factory a command uses and a function that
// releases the clients the factory created.
type runtimeProvider func() (reconcile.RuntimeFactory, func())

// localRuntimes creates one Swarm client per cluster name on first use and
// keeps it until the returned function is called.
func localRuntimes() (reconcile.RuntimeFactory, func()) {
	var (
		mu       sync.Mutex
		runtimes = make(map[string]runtime.Runtime)
		closers  []func()
	)
	factory := func(_ context.Context, name string) (runtime.Runtime, error) {
		mu.Lock()
		defer mu.Unlock()
		if rt, ok := runtimes[name]; ok {
			return rt, nil
		}
		engine, err := cluster.New(cluster.Config{Name: name, Endpoint: localDockerEndpoint})
		if err != nil {
			return nil, err
		}
		closers = append(closers, func() { _ = engine.Close() })
		rt := swarm.New(engine)
		runtimes[name] = rt
		return rt, nil
	}
	closeAll := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, closeClient := range closers {
			closeClient()
		}
	}
	return factory, closeAll
}

// controlPlane is what sync, status, history and rollback share with the API.
type controlPlane struct {
	svc   *reconcile.Service
	store *store.Store
}

// openControlPlane opens the store at data and builds the reconcile service on
// top of it. lockWait is the longest a run waits for the lock of its target.
// The returned function releases the runtime clients and the store.
func openControlPlane(ctx context.Context, data, root string, lockWait time.Duration, runtimes runtimeProvider) (*controlPlane, func(), error) {
	if data == "" {
		return nil, nil, errors.New("--data is required")
	}
	factory, closeRuntimes := runtimes()
	st, err := store.Open(ctx, data)
	if err != nil {
		closeRuntimes()
		return nil, nil, err
	}
	cp := &controlPlane{
		svc: reconcile.New(st, factory, reconcile.Config{
			Root:     root,
			LockWait: lockWait,
			DataDir:  filepath.Dir(data),
		}),
		store: st,
	}
	closeAll := func() {
		closeRuntimes()
		if err := st.Close(); err != nil {
			slog.Error("close store", "path", data, "err", err)
		}
	}
	return cp, closeAll, nil
}

func runSync(args []string, stdout io.Writer) error {
	return runSyncWith(args, stdout, localRuntimes)
}

func runSyncWith(args []string, stdout io.Writer, runtimes runtimeProvider) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	data, root, output := bindOperationFlags(fs)
	lockWait := bindLockFlag(fs)
	ref, err := parseOperationTarget(fs, "sync", args, output)
	if err != nil {
		return err
	}
	if err := checkLockWait("sync", *lockWait); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cp, closeCP, err := openControlPlane(ctx, *data, *root, *lockWait, runtimes)
	if err != nil {
		return err
	}
	defer closeCP()

	number, err := cp.svc.Sync(ctx, ref)
	if err != nil {
		return withRecorded(err, number)
	}
	view := api.RunView{Application: ref.Application, Target: ref.Target, Revision: number}
	return writeResult(stdout, *output, view, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "synced %s as revision %d\n", ref, number)
	})
}

func runStatus(args []string, stdout io.Writer) error {
	return runStatusWith(args, stdout, localRuntimes)
}

func runStatusWith(args []string, stdout io.Writer, runtimes runtimeProvider) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	data, root, output := bindOperationFlags(fs)
	ref, err := parseOperationTarget(fs, "status", args, output)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cp, closeCP, err := openControlPlane(ctx, *data, *root, reconcile.DefaultLockWait, runtimes)
	if err != nil {
		return err
	}
	defer closeCP()

	t, err := cp.store.Target(ctx, ref.Application, ref.Target)
	if err != nil {
		return fmt.Errorf("status %s: %w", ref, err)
	}
	live, err := cp.svc.Status(ctx, ref)
	if err != nil {
		return err
	}
	view := api.StatusView{Target: api.NewTargetView(t), Live: live}
	return writeResult(stdout, *output, view, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "target %s (cluster %s)\n", ref, t.Cluster)
		_, _ = fmt.Fprintf(w, "last revision: %d\n", t.LastRevision)
		_, _ = fmt.Fprintf(w, "stored status: %s\n", t.Status)
		if t.Message != "" {
			_, _ = fmt.Fprintf(w, "message: %s\n", t.Message)
		}
		_, _ = fmt.Fprintf(w, "live status: %s\n", live)
	})
}

func runHistory(args []string, stdout io.Writer) error {
	return runHistoryWith(args, stdout, localRuntimes)
}

func runHistoryWith(args []string, stdout io.Writer, runtimes runtimeProvider) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	data, root, output := bindOperationFlags(fs)
	ref, err := parseOperationTarget(fs, "history", args, output)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cp, closeCP, err := openControlPlane(ctx, *data, *root, reconcile.DefaultLockWait, runtimes)
	if err != nil {
		return err
	}
	defer closeCP()

	revs, err := cp.store.Revisions(ctx, ref.Application, ref.Target)
	if err != nil {
		return err
	}
	if len(revs) == 0 {
		if _, err := cp.store.Target(ctx, ref.Application, ref.Target); err != nil {
			return fmt.Errorf("history %s: %w", ref, err)
		}
	}
	view := api.NewHistoryView(ref.Application, ref.Target, revs)
	return writeResult(stdout, *output, view, func(w io.Writer) {
		if len(view.Revisions) == 0 {
			_, _ = fmt.Fprintf(w, "%s has no revisions\n", ref)
			return
		}
		_, _ = fmt.Fprintf(w, "%-8s  %-20s  %-8s  %s\n", "REVISION", "CREATED", "COMMIT", "STATUS")
		for _, rev := range view.Revisions {
			commit := rev.Commit
			if commit == "" {
				commit = "-"
			}
			_, _ = fmt.Fprintf(w, "%-8d  %-20s  %-8s  %s\n",
				rev.Number, rev.CreatedAt.UTC().Format(time.RFC3339), commit, rev.Status)
		}
	})
}

func runRollback(args []string, stdout io.Writer) error {
	return runRollbackWith(args, stdout, localRuntimes)
}

func runRollbackWith(args []string, stdout io.Writer, runtimes runtimeProvider) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	data, root, output := bindOperationFlags(fs)
	revision := fs.Int64("revision", 0, "revision number to roll back to")
	lockWait := bindLockFlag(fs)
	ref, err := parseOperationTarget(fs, "rollback", args, output)
	if err != nil {
		return err
	}
	if *revision < 1 {
		return errors.New("rollback: --revision must be a positive revision number")
	}
	if err := checkLockWait("rollback", *lockWait); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cp, closeCP, err := openControlPlane(ctx, *data, *root, *lockWait, runtimes)
	if err != nil {
		return err
	}
	defer closeCP()

	number, err := cp.svc.Rollback(ctx, ref, *revision)
	if err != nil {
		return withRecorded(err, number)
	}
	view := api.RunView{Application: ref.Application, Target: ref.Target, Revision: number}
	return writeResult(stdout, *output, view, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "rolled back %s to revision %d as revision %d\n", ref, *revision, number)
	})
}

// bindOperationFlags registers the flags every operation command takes.
func bindOperationFlags(fs *flag.FlagSet) (data, root, output *string) {
	data = fs.String("data", "", "path to the SQLite database (required)")
	root = fs.String("root", ".", "deploy repository root")
	output = fs.String("output", "human", "output format: human or json")
	return data, root, output
}

// bindLockFlag registers --lock-timeout, the longest a run waits for another
// run on the same target. Only the commands that take a target lock have it.
func bindLockFlag(fs *flag.FlagSet) *time.Duration {
	return fs.Duration("lock-timeout", reconcile.DefaultLockWait, "longest wait for another run on the same target; must be positive")
}

// checkLockWait rejects a lock timeout that cannot wait for anything. A zero
// timeout would make every busy target fail at once, so it is an error too.
func checkLockWait(name string, d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("%s: --lock-timeout must be positive, got %s", name, d)
	}
	return nil
}

// parseOperationTarget reads the app/target argument and the flags of an
// operation command. The argument may come before, between or after the flags.
func parseOperationTarget(fs *flag.FlagSet, name string, args []string, output *string) (reconcile.Ref, error) {
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return reconcile.Ref{}, err
	}
	if len(positional) != 1 {
		return reconcile.Ref{}, fmt.Errorf("%s: expected one app/target argument, got %d", name, len(positional))
	}
	if *output != "human" && *output != "json" {
		return reconcile.Ref{}, fmt.Errorf("%s: unsupported output format %q", name, *output)
	}
	ref, err := parseRef(name, positional[0])
	if err != nil {
		return reconcile.Ref{}, err
	}
	return ref, nil
}

// parseInterleaved parses fs from args and returns the positional arguments in
// order. flag.FlagSet stops at the first positional argument, so the rest is
// parsed again.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// parseRef reads an app/target argument. It applies the rules of the API too.
func parseRef(name, s string) (reconcile.Ref, error) {
	app, target, ok := strings.Cut(s, "/")
	if !ok {
		return reconcile.Ref{}, fmt.Errorf("%s: target %q must be in app/target form", name, s)
	}
	ref := reconcile.Ref{Application: app, Target: target}
	if err := reconcile.ValidateRef(ref); err != nil {
		return reconcile.Ref{}, fmt.Errorf("%s: %w", name, err)
	}
	return ref, nil
}

// withRecorded tells the operator about a revision that a failed run recorded
// before it failed. Such a revision exists, and the target keeps it as its last.
func withRecorded(err error, number int64) error {
	if number == 0 {
		return err
	}
	return fmt.Errorf("%w; revision %d was recorded", err, number)
}

// writeResult prints v as JSON for output "json" and calls human otherwise.
func writeResult(w io.Writer, output string, v any, human func(io.Writer)) error {
	if output == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	human(w)
	return nil
}
