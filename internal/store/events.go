package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Event is an entry in the history of a target. Revision is the number of the
// related revision, or 0 when the event is not tied to one. Type is free text
// for now; the event producers that define its values arrive with #20.
type Event struct {
	Application string
	Target      string
	Revision    int64
	Type        string
	Payload     any
}

// AddEvent records e. Payload is encoded as JSON; a nil payload is stored as
// NULL. Nothing in #19 calls AddEvent; the event producers arrive with #20.
func (s *Store) AddEvent(ctx context.Context, e Event) error {
	var payload sql.NullString
	if e.Payload != nil {
		raw, err := json.Marshal(e.Payload)
		if err != nil {
			return fmt.Errorf("store: encode event payload: %w", err)
		}
		payload = sql.NullString{String: string(raw), Valid: true}
	}

	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		t, err := findTarget(ctx, tx, e.Application, e.Target)
		if err != nil {
			return err
		}

		var revisionID sql.NullInt64
		if e.Revision != 0 {
			id, err := findRevisionID(ctx, tx, t.ID, e.Revision)
			if err != nil {
				return err
			}
			revisionID = sql.NullInt64{Int64: id, Valid: true}
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO events (target_id, revision_id, type, payload, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			t.ID, revisionID, e.Type, payload, now())
		return err
	})
	if err != nil {
		return fmt.Errorf("store: add event %s for %s/%s: %w", e.Type, e.Application, e.Target, err)
	}
	return nil
}
