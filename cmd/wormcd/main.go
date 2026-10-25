package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/d00p1/wormcd/internal/api"
	"github.com/d00p1/wormcd/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
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
	case "diff":
		return runDiff(args[1:])
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
  diff      show the plan against a cluster (read-only)
  version   print version information
`)
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           api.New().Handler(),
		ReadHeaderTimeout: 5 * time.Second,
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

func runDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	app := fs.String("app", "", "application target, e.g. web/prod")
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
	return fmt.Errorf("diff for %s: not implemented yet (issue #4)", *app)
}
