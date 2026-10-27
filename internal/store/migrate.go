package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations holds the schema in order. Entry i upgrades the schema from
// version i to i+1. Applied migrations are tracked in PRAGMA user_version, so
// reopening a database is a no-op and migrations are never re-run.
//
// clusters and sources are intentionally absent: they arrive with M2.
var migrations = []string{
	`CREATE TABLE targets (
		id            INTEGER PRIMARY KEY,
		app           TEXT NOT NULL,
		target        TEXT NOT NULL,
		cluster       TEXT NOT NULL DEFAULT '',
		last_revision INTEGER NOT NULL DEFAULT 0,
		status        TEXT NOT NULL,
		message       TEXT NOT NULL DEFAULT '',
		UNIQUE (app, target)
	);
	CREATE TABLE revisions (
		id             INTEGER PRIMARY KEY,
		target_id      INTEGER NOT NULL REFERENCES targets (id),
		number         INTEGER NOT NULL,
		"commit"       TEXT NOT NULL,
		rendered_spec  TEXT NOT NULL,
		image_digests  TEXT NOT NULL DEFAULT '{}',
		created_at     TEXT NOT NULL,
		UNIQUE (target_id, number)
	);
	CREATE TABLE events (
		id           INTEGER PRIMARY KEY,
		target_id    INTEGER NOT NULL REFERENCES targets (id),
		revision_id  INTEGER REFERENCES revisions (id),
		type         TEXT NOT NULL,
		payload      TEXT,
		created_at   TEXT NOT NULL
	);`,
}

// migrate reads the schema version and applies pending migrations in one
// transaction. Transactions begin with BEGIN IMMEDIATE, so the version is read
// under the write lock and a concurrent opener waits instead of re-running a
// migration another process has already applied.
func migrate(ctx context.Context, db *sql.DB) error {
	return withTx(ctx, db, func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("store: read schema version: %w", err)
		}
		if version > len(migrations) {
			return fmt.Errorf("store: schema version %d is newer than supported %d", version, len(migrations))
		}

		for i := version; i < len(migrations); i++ {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("store: migrate to version %d: %w", i+1, err)
			}
			// PRAGMA does not accept bound parameters; the value is a trusted int.
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
				return fmt.Errorf("store: migrate to version %d: %w", i+1, err)
			}
		}
		return nil
	})
}

func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
