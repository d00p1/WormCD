package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// NewRevision is the input for AppendRevision. RenderedSpec is the JSON
// document of the fully rendered desired state. Callers sort it before
// marshaling so that identical renders store identical bytes.
type NewRevision struct {
	Commit       string
	RenderedSpec json.RawMessage
	ImageDigests map[string]string
}

// Revision is a stored, fully rendered desired state of a target.
type Revision struct {
	Number       int64
	Commit       string
	RenderedSpec json.RawMessage
	ImageDigests map[string]string
	CreatedAt    time.Time
}

// AppendRevision stores rev as the next revision of the target and returns its
// number. Numbers are sequential per target starting at 1.
func (s *Store) AppendRevision(ctx context.Context, application, target string, rev NewRevision) (int64, error) {
	if !json.Valid(rev.RenderedSpec) {
		return 0, errors.New("store: rendered spec is not valid JSON")
	}
	digests := rev.ImageDigests
	if digests == nil {
		digests = map[string]string{}
	}
	digestsJSON, err := json.Marshal(digests)
	if err != nil {
		return 0, fmt.Errorf("store: encode image digests: %w", err)
	}

	var number int64
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		t, err := findTarget(ctx, tx, application, target)
		if err != nil {
			return err
		}

		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(number), 0) + 1 FROM revisions WHERE target_id = ?`, t.ID).Scan(&number); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO revisions (target_id, number, "commit", rendered_spec, image_digests, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			t.ID, number, rev.Commit, string(rev.RenderedSpec), string(digestsJSON), now()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE targets SET last_revision = ? WHERE id = ?`, number, t.ID)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: append revision for %s/%s: %w", application, target, err)
	}
	return number, nil
}

// revisionColumns lists the columns scanRevision reads, in order.
const revisionColumns = `r.number, r."commit", r.rendered_spec, r.image_digests, r.created_at`

// Revision returns revision number of the target without contacting git.
func (s *Store) Revision(ctx context.Context, application, target string, number int64) (Revision, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+revisionColumns+`
		FROM revisions r JOIN targets t ON t.id = r.target_id
		WHERE t.app = ? AND t.target = ? AND r.number = ?`,
		application, target, number)
	rev, err := scanRevision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	if err != nil {
		return Revision{}, fmt.Errorf("store: get revision %d of %s/%s: %w", number, application, target, err)
	}
	return rev, nil
}

// Revisions returns all revisions of the target, newest first.
func (s *Store) Revisions(ctx context.Context, application, target string) ([]Revision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+revisionColumns+`
		FROM revisions r JOIN targets t ON t.id = r.target_id
		WHERE t.app = ? AND t.target = ?
		ORDER BY r.number DESC`,
		application, target)
	if err != nil {
		return nil, fmt.Errorf("store: list revisions of %s/%s: %w", application, target, err)
	}
	defer func() { _ = rows.Close() }()

	var revisions []Revision
	for rows.Next() {
		rev, err := scanRevision(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list revisions of %s/%s: %w", application, target, err)
		}
		revisions = append(revisions, rev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list revisions of %s/%s: %w", application, target, err)
	}
	return revisions, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRevision(row rowScanner) (Revision, error) {
	var (
		rev                 Revision
		spec, digests, when string
	)
	if err := row.Scan(&rev.Number, &rev.Commit, &spec, &digests, &when); err != nil {
		return Revision{}, err
	}
	rev.RenderedSpec = json.RawMessage(spec)
	if err := json.Unmarshal([]byte(digests), &rev.ImageDigests); err != nil {
		return Revision{}, fmt.Errorf("decode image digests: %w", err)
	}
	created, err := time.Parse(time.RFC3339Nano, when)
	if err != nil {
		return Revision{}, fmt.Errorf("decode created_at: %w", err)
	}
	rev.CreatedAt = created
	return rev, nil
}
