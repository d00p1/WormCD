package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a target or revision does not exist.
var ErrNotFound = errors.New("store: not found")

// Status is the observed state of a target.
type Status string

const (
	// StatusUnknown is the starting status of every target, before it is observed.
	StatusUnknown Status = "Unknown"
	// StatusSynced means the last sync succeeded and the cluster matches it.
	StatusSynced Status = "Synced"
	// StatusOutOfSync means the cluster differs from the desired state.
	StatusOutOfSync Status = "OutOfSync"
	// StatusProgressing means a sync or rollback is running.
	StatusProgressing Status = "Progressing"
	// StatusDegraded means the last sync failed in apply, prune or health.
	StatusDegraded Status = "Degraded"
	// StatusBlocked means the target cannot be synced: the manifest is invalid,
	// or the cluster is unavailable.
	StatusBlocked Status = "Blocked"
)

// Store is a SQLite-backed store. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Target is one application target (app + target name) tracked by WormCD.
type Target struct {
	ID           int64
	Application  string
	Target       string
	Cluster      string
	LastRevision int64
	Status       Status
	Message      string
}

// Open opens (creating if needed) the SQLite database at path and applies
// pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: path is required")
	}
	dsn, err := dsnFor(path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One connection serializes access within this process. Every transaction
	// begins with BEGIN IMMEDIATE (_txlock=immediate), so it takes the write
	// lock up front instead of failing when it upgrades from a read. The busy
	// timeout makes a transaction that finds another process holding the lock
	// wait up to 5s instead of failing with SQLITE_BUSY.
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// EnsureTarget returns the target for application and target, creating it
// with status Unknown when it does not exist. A non-empty cluster updates the
// stored one, so a target that moves to another cluster is recorded; an empty
// cluster leaves the stored one unchanged.
func (s *Store) EnsureTarget(ctx context.Context, application, target, cluster string) (Target, error) {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO targets (app, target, cluster, last_revision, status, message)
		VALUES (?, ?, ?, 0, ?, '')
		ON CONFLICT (app, target) DO UPDATE SET
			cluster = CASE WHEN excluded.cluster = '' THEN targets.cluster ELSE excluded.cluster END`,
		application, target, cluster, StatusUnknown)
	if err != nil {
		return Target{}, fmt.Errorf("store: ensure target %s/%s: %w", application, target, err)
	}
	return s.Target(ctx, application, target)
}

// SetTargetState records the status, message and last successful revision of
// an existing target. It returns ErrNotFound if the target does not exist and
// an error for an unknown status.
func (s *Store) SetTargetState(ctx context.Context, application, target string, status Status, message string, lastRevision int64) error {
	switch status {
	case StatusUnknown, StatusSynced, StatusOutOfSync, StatusProgressing, StatusDegraded, StatusBlocked:
	default:
		return fmt.Errorf("store: unknown target status %q", status)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE targets SET status = ?, message = ?, last_revision = ?
		WHERE app = ? AND target = ?`,
		status, message, lastRevision, application, target)
	if err != nil {
		return fmt.Errorf("store: set state of %s/%s: %w", application, target, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set state of %s/%s: %w", application, target, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Target returns the target for application and target, or ErrNotFound.
func (s *Store) Target(ctx context.Context, application, target string) (Target, error) {
	t, err := findTarget(ctx, s.db, application, target)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Target{}, fmt.Errorf("store: get target %s/%s: %w", application, target, err)
	}
	return t, err
}

// queryer is implemented by *sql.DB and *sql.Tx. It covers the single-row
// lookups that take either a handle or a transaction.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const targetColumns = `id, app, target, cluster, last_revision, status, message`

// findTarget loads the target for application and target, or returns
// ErrNotFound.
func findTarget(ctx context.Context, q queryer, application, target string) (Target, error) {
	var t Target
	err := q.QueryRowContext(ctx,
		`SELECT `+targetColumns+` FROM targets WHERE app = ? AND target = ?`,
		application, target).Scan(&t.ID, &t.Application, &t.Target, &t.Cluster, &t.LastRevision, &t.Status, &t.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	if err != nil {
		return Target{}, err
	}
	return t, nil
}

// dsnFor builds a file: URI for path. The driver splits a plain file name at
// the first '?', so the path is escaped into the URI, where '?' and '#' are
// percent-encoded instead of being read as query or fragment delimiters.
func dsnFor(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	u := url.URL{
		Scheme: "file",
		Path:   abs,
		RawQuery: url.Values{
			"_pragma": {"foreign_keys(1)", "busy_timeout(5000)"},
			"_txlock": {"immediate"},
		}.Encode(),
	}
	return u.String(), nil
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
