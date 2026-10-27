package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a target or revision does not exist.
var ErrNotFound = errors.New("store: not found")

// Status is the observed state of a target.
type Status string

const (
	StatusUnknown     Status = "Unknown"
	StatusSynced      Status = "Synced"
	StatusOutOfSync   Status = "OutOfSync"
	StatusProgressing Status = "Progressing"
	StatusHealthy     Status = "Healthy"
	StatusDegraded    Status = "Degraded"
	StatusBlocked     Status = "Blocked"
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
	// A single connection serializes writes; the busy timeout covers other
	// processes sharing the file.
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
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
// with status Unknown when it does not exist. The cluster is updated on every
// call so a target that moves to another cluster is recorded.
func (s *Store) EnsureTarget(ctx context.Context, application, target, cluster string) (Target, error) {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO targets (app, target, cluster, last_revision, status, message)
		VALUES (?, ?, ?, 0, ?, '')
		ON CONFLICT (app, target) DO UPDATE SET cluster = excluded.cluster`,
		application, target, cluster, StatusUnknown)
	if err != nil {
		return Target{}, fmt.Errorf("store: ensure target %s/%s: %w", application, target, err)
	}
	return s.Target(ctx, application, target)
}

// Target returns the target for application and target, or ErrNotFound.
func (s *Store) Target(ctx context.Context, application, target string) (Target, error) {
	var t Target
	err := s.db.QueryRowContext(ctx, `
		SELECT id, app, target, cluster, last_revision, status, message
		FROM targets WHERE app = ? AND target = ?`,
		application, target).Scan(&t.ID, &t.Application, &t.Target, &t.Cluster, &t.LastRevision, &t.Status, &t.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	if err != nil {
		return Target{}, fmt.Errorf("store: get target %s/%s: %w", application, target, err)
	}
	return t, nil
}

// SetStatus records the observed status and a human-readable message for a
// target.
func (s *Store) SetStatus(ctx context.Context, application, target string, status Status, message string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE targets SET status = ?, message = ? WHERE app = ? AND target = ?`,
		status, message, application, target)
	if err != nil {
		return fmt.Errorf("store: set status %s/%s: %w", application, target, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set status %s/%s: %w", application, target, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
