package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/d00p1/wormcd/internal/api"
	"github.com/d00p1/wormcd/internal/cluster"
	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/manifest"
	"github.com/d00p1/wormcd/internal/reconcile"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/d00p1/wormcd/internal/runtime/swarm"
	"github.com/d00p1/wormcd/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var exit *exitError
		if errors.As(err, &exit) {
			os.Exit(exit.code)
		}
		_, _ = fmt.Fprintf(os.Stderr, "wormcd: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("command is required")
	}

	switch args[0] {
	case "version":
		_, err := fmt.Println(version.String())
		return err
	case "server":
		return runServer(args[1:])
	case "render":
		return runRender(args[1:])
	case "diff":
		return runDiff(args[1:])
	case "sync":
		return runSync(args[1:], os.Stdout)
	case "status":
		return runStatus(args[1:], os.Stdout)
	case "history":
		return runHistory(args[1:], os.Stdout)
	case "rollback":
		return runRollback(args[1:], os.Stdout)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	_, _ = fmt.Fprint(os.Stderr, `WormCD - declarative CD for Docker Swarm

Usage:
  wormcd <command> [flags]

Commands:
  server    run the control plane
  render    render compose + overlay into the normalized desired state
  diff      show the plan against a cluster (read-only)
  sync      apply the desired state of app/target and record a revision
  status    show the stored target and whether the cluster matches it
  history   list the revisions of app/target
  rollback  apply a stored revision of app/target as a new revision
  version   print version information
`)
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "HTTP listen address; must be a loopback address in M1")
	data := fs.String("data", "", "path to the SQLite database (required)")
	root := fs.String("root", ".", "deploy repository root used by sync and rollback")
	lockWait := bindLockFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireLoopback(*listen); err != nil {
		return err
	}
	if err := checkLockWait("server", *lockWait); err != nil {
		return err
	}

	cp, closeCP, err := openControlPlane(context.Background(), *data, *root, *lockWait, localRuntimes)
	if err != nil {
		return err
	}
	defer closeCP()
	slog.Info("store opened", "path", *data)

	// WriteTimeout stays unset: a sync answers only after it has converged,
	// which can take longer than any fixed write deadline.
	srv := &http.Server{
		Addr:              *listen,
		Handler:           api.New(cp.svc, cp.store).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("control plane listening", "addr", *listen)
		errCh <- srv.ListenAndServe()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// requireLoopback refuses a listen address reachable from other hosts. The API
// has no authentication in M1; ADR-0007 allows another address only with a
// bearer token, which is a separate issue.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("server: listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("server: listen address %q is not loopback; the API has no authentication in M1, use 127.0.0.1", addr)
}

func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	file := fs.String("file", "", "path to wormcd.yaml")
	app := fs.String("app", "", "application as name/target, discovered under --root")
	root := fs.String("root", ".", "deploy repository root used by --app")
	target := fs.String("target", "", "target name (optional when the app has one target)")
	output := fs.String("output", "human", "output format: human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *output != "human" && *output != "json" {
		return fmt.Errorf("render: unsupported output format %q", *output)
	}

	state, err := manifest.Load(context.Background(), manifest.Options{
		File:   *file,
		App:    *app,
		Root:   *root,
		Target: *target,
	})
	if err != nil {
		return err
	}

	if *output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(state)
	}

	printDesired(state)
	return nil
}

func printDesired(state *desired.Desired) {
	_, _ = fmt.Printf("application: %s\ntarget: %s\nnamespace: %s\n", state.Application, state.Target, state.Namespace)

	_, _ = fmt.Printf("services (%d):\n", len(state.Services))
	for _, service := range state.Services {
		replicas := fmt.Sprintf("x%d", service.Replicas)
		if service.Mode == desired.ModeGlobal {
			replicas = "global"
		}
		_, _ = fmt.Printf("  %s  %s  %s\n", service.Name, service.Image, replicas)
	}

	_, _ = fmt.Printf("networks (%d):\n", len(state.Networks))
	for _, network := range state.Networks {
		kind := network.Driver
		if network.External {
			kind = "external"
		}
		_, _ = fmt.Printf("  %s  %s\n", network.Name, kind)
	}

	_, _ = fmt.Printf("volumes (%d):\n", len(state.Volumes))
	for _, volume := range state.Volumes {
		kind := volume.Driver
		if volume.External {
			kind = "external"
		}
		_, _ = fmt.Printf("  %s  %s\n", volume.Name, kind)
	}

	_, _ = fmt.Printf("configs (%d):\n", len(state.Configs))
	for _, config := range state.Configs {
		_, _ = fmt.Printf("  %s\n", config.Name)
	}

	_, _ = fmt.Printf("secrets (%d):\n", len(state.Secrets))
	for _, secret := range state.Secrets {
		_, _ = fmt.Printf("  %s\n", secret.Name)
	}
}

func runDiff(args []string) error {
	return runDiffTo(args, os.Stdout)
}

// diffInspector is the only part of a runtime the diff command may use.
// Keeping it this narrow makes mutating Docker calls impossible in the diff
// path by construction.
type diffInspector interface {
	Inspect(context.Context, runtime.Scope) (runtime.Observed, error)
}

func connectInspector(cfg cluster.Config) (diffInspector, func(), error) {
	engine, err := cluster.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	return swarm.New(engine), func() { _ = engine.Close() }, nil
}

// runDiffTo builds a read-only plan. Exit code 2 means "changes detected";
// errors still exit with 1.
func runDiffTo(args []string, stdout io.Writer) error {
	return runDiffWith(args, stdout, connectInspector)
}

func runDiffWith(
	args []string,
	stdout io.Writer,
	connect func(cluster.Config) (diffInspector, func(), error),
) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	app := fs.String("app", "", "application target, e.g. web/prod")
	root := fs.String("root", ".", "deploy repository root used by --app")
	output := fs.String("output", "human", "output format: human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *app == "" {
		return errors.New("diff: --app is required")
	}
	if *output != "human" && *output != "json" {
		return fmt.Errorf("diff: unsupported output format %q", *output)
	}

	ctx := context.Background()
	state, clusterName, err := manifest.LoadWithCluster(ctx, manifest.Options{App: *app, Root: *root})
	if err != nil {
		return err
	}

	// M0 has no cluster registry yet: the local Swarm is the only supported
	// target. The cluster name is still resolved so errors name the right
	// cluster; the endpoint becomes configurable when the registry arrives.
	inspector, closeInspector, err := connect(cluster.Config{
		Name:     clusterName,
		Endpoint: "unix:///var/run/docker.sock",
	})
	if err != nil {
		return err
	}
	defer closeInspector()

	observed, err := inspector.Inspect(ctx, runtime.Scope{
		Application: state.Application,
		Target:      state.Target,
	})
	if err != nil {
		return err
	}

	plan := reconcile.Diff(state, observed)
	if *output == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(plan); err != nil {
			return err
		}
	} else {
		printPlan(stdout, plan)
	}

	if plan.HasChanges() {
		return &exitError{code: 2}
	}
	return nil
}

func printPlan(w io.Writer, plan runtime.Plan) {
	_, _ = fmt.Fprintf(w, "plan for %s/%s (%s)\n", plan.Application, plan.Target, plan.Namespace)
	if len(plan.Entries) == 0 {
		_, _ = fmt.Fprintln(w, "  no managed objects")
	}
	for _, entry := range plan.Entries {
		line := fmt.Sprintf("  %-6s  %s", entry.Action, entry.Key)
		if len(entry.Changes) > 0 {
			line += " (" + strings.Join(entry.Changes, ", ") + ")"
		}
		_, _ = fmt.Fprintln(w, line)
	}
	_, _ = fmt.Fprintf(w, "%d create, %d update, %d delete, %d noop\n",
		plan.Summary.Create, plan.Summary.Update, plan.Summary.Delete, plan.Summary.Noop)
	if !plan.HasChanges() {
		_, _ = fmt.Fprintln(w, "in sync")
	}
}

// exitError carries a process exit code without printing an extra message.
type exitError struct {
	code int
}

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
