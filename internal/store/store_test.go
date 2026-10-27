package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wormcd.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// fakeSpec is a Canonicaler that returns its bytes unchanged.
type fakeSpec []byte

func (f fakeSpec) Canonical() ([]byte, error) { return f, nil }

// failingSpec is a Canonicaler whose encoding fails.
type failingSpec struct{}

func (failingSpec) Canonical() ([]byte, error) { return nil, errors.New("cannot encode") }

var sampleSpec = fakeSpec(`{"application":"web","target":"prod","services":[{"name":"web","replicas":3}]}`)

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wormcd.db")

	for range 2 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("user_version = %d, want %d", version, len(migrations))
	}

	for _, table := range []string{"targets", "revisions", "events"} {
		var name string
		if err := s.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	for _, table := range []string{"clusters", "sources"} {
		var name string
		err := s.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("table %s must not exist before M2", table)
		}
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wormcd.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA user_version = 99`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	_ = s.Close()

	if _, err := Open(ctx, path); err == nil {
		t.Fatal("Open succeeded on a newer schema version")
	}
}

func TestEnsureTarget(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	created, err := s.EnsureTarget(ctx, "web", "prod", "local")
	if err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}
	if created.Status != StatusUnknown || created.LastRevision != 0 {
		t.Fatalf("new target = %+v, want Unknown with no revisions", created)
	}

	again, err := s.EnsureTarget(ctx, "web", "prod", "local-2")
	if err != nil {
		t.Fatalf("EnsureTarget again: %v", err)
	}
	if again.ID != created.ID || again.Cluster != "local-2" {
		t.Fatalf("EnsureTarget again = %+v, want same ID and updated cluster", again)
	}

	if _, err := s.Target(ctx, "web", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Target on missing target: err = %v, want ErrNotFound", err)
	}
}

func TestEnsureTargetEmptyClusterKeepsExisting(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	if _, err := s.EnsureTarget(ctx, "web", "prod", "c1"); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}
	got, err := s.EnsureTarget(ctx, "web", "prod", "")
	if err != nil {
		t.Fatalf("EnsureTarget with empty cluster: %v", err)
	}
	if got.Cluster != "c1" {
		t.Fatalf("cluster = %q after empty EnsureTarget, want c1", got.Cluster)
	}

	got, err = s.EnsureTarget(ctx, "web", "prod", "c2")
	if err != nil {
		t.Fatalf("EnsureTarget with new cluster: %v", err)
	}
	if got.Cluster != "c2" {
		t.Fatalf("cluster = %q, want c2", got.Cluster)
	}
}

func TestConcurrentAppendRevisionAcrossStores(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wormcd.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	defer func() { _ = second.Close() }()
	if _, err := first.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}

	const perStore = 20
	var wg sync.WaitGroup
	errs := make(chan error, 2*perStore)
	numbers := make(chan int64, 2*perStore)
	for _, s := range []*Store{first, second} {
		for range perStore {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n, err := s.AppendRevision(ctx, "web", "prod", NewRevision{Commit: "c", RenderedSpec: sampleSpec})
				if err != nil {
					errs <- err
					return
				}
				numbers <- n
			}()
		}
	}
	wg.Wait()
	close(errs)
	close(numbers)
	for err := range errs {
		t.Errorf("AppendRevision: %v", err)
	}

	seen := make(map[int64]bool)
	for n := range numbers {
		if seen[n] {
			t.Errorf("revision number %d issued twice", n)
		}
		seen[n] = true
	}
	for n := int64(1); n <= 2*perStore; n++ {
		if !seen[n] {
			t.Errorf("revision number %d missing", n)
		}
	}
}

func TestOpenAppliesConnectionPragmas(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}
	var busyTimeout int
	if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}
}

func TestOpenPathWithQueryCharacters(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data?v=1#x")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	path := filepath.Join(dir, "wormcd#1?.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.EnsureTarget(ctx, "web", "prod", "local"); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}
	if _, err := s.AppendRevision(ctx, "web", "prod", NewRevision{Commit: "c1", RenderedSpec: sampleSpec}); err != nil {
		t.Fatalf("AppendRevision: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at %s: %v", path, err)
	}
}

func TestRevisionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, err := s.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}

	number, err := s.AppendRevision(ctx, "web", "prod", NewRevision{
		Commit:       "abc123",
		RenderedSpec: sampleSpec,
		ImageDigests: map[string]string{"web": "sha256:abc"},
	})
	if err != nil {
		t.Fatalf("AppendRevision: %v", err)
	}
	if number != 1 {
		t.Fatalf("number = %d, want 1", number)
	}

	got, err := s.Revision(ctx, "web", "prod", number)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if string(got.RenderedSpec) != string(sampleSpec) {
		t.Errorf("rendered spec = %s, want %s", got.RenderedSpec, sampleSpec)
	}
	if got.Commit != "abc123" || got.Number != 1 {
		t.Errorf("revision = %d %q, want 1 abc123", got.Number, got.Commit)
	}
	if !reflect.DeepEqual(got.ImageDigests, map[string]string{"web": "sha256:abc"}) {
		t.Errorf("image digests = %v", got.ImageDigests)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}

	target, err := s.Target(ctx, "web", "prod")
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if target.LastRevision != 1 {
		t.Errorf("last_revision = %d, want 1", target.LastRevision)
	}

	if _, err := s.Revision(ctx, "web", "prod", 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("Revision(2): err = %v, want ErrNotFound", err)
	}
}

func TestAppendRevisionRejectsInvalidSpec(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, err := s.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}
	_, err := s.AppendRevision(ctx, "web", "prod", NewRevision{RenderedSpec: fakeSpec(`{"services":`)})
	if err == nil {
		t.Fatal("AppendRevision accepted invalid JSON")
	}
}

func TestAppendRevisionRejectsEmptySpec(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, err := s.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}
	if _, err := s.AppendRevision(ctx, "web", "prod", NewRevision{RenderedSpec: fakeSpec(nil)}); err == nil {
		t.Fatal("AppendRevision accepted empty canonical bytes")
	}
}

func TestAppendRevisionStoresCanonicalBytes(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, err := s.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}
	want := []byte(`{"application":"web","services":[]}`)

	number, err := s.AppendRevision(ctx, "web", "prod", NewRevision{Commit: "c1", RenderedSpec: fakeSpec(want)})
	if err != nil {
		t.Fatalf("AppendRevision: %v", err)
	}
	got, err := s.Revision(ctx, "web", "prod", number)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if !bytes.Equal(got.RenderedSpec, want) {
		t.Errorf("stored spec = %s, want %s", got.RenderedSpec, want)
	}
}

func TestAppendRevisionCanonicalErrorWritesNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, err := s.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}

	_, err := s.AppendRevision(ctx, "web", "prod", NewRevision{Commit: "c1", RenderedSpec: failingSpec{}})
	if err == nil {
		t.Fatal("AppendRevision ignored the Canonical error")
	}
	history, err := s.Revisions(ctx, "web", "prod")
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("revisions = %d after failed canonicalization, want 0", len(history))
	}
	target, err := s.Target(ctx, "web", "prod")
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if target.LastRevision != 0 {
		t.Errorf("last_revision = %d after failed canonicalization, want 0", target.LastRevision)
	}
}

func TestAppendRevisionNilSpecIsError(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, err := s.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}
	if _, err := s.AppendRevision(ctx, "web", "prod", NewRevision{Commit: "c1"}); err == nil {
		t.Fatal("AppendRevision accepted a nil Canonicaler")
	}
}

// typedNilSpec mirrors desired.Desired: its Canonical method is safe on a nil
// receiver and reports an error instead of dereferencing it.
type typedNilSpec struct{}

func (s *typedNilSpec) Canonical() ([]byte, error) {
	if s == nil {
		return nil, errors.New("nil spec")
	}
	return sampleSpec, nil
}

func TestAppendRevisionTypedNilSpecWritesNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, err := s.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}

	var spec *typedNilSpec
	_, err := s.AppendRevision(ctx, "web", "prod", NewRevision{Commit: "c1", RenderedSpec: spec})
	if err == nil {
		t.Fatal("AppendRevision accepted a typed nil Canonicaler")
	}
	history, err := s.Revisions(ctx, "web", "prod")
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("revisions = %d after typed nil spec, want 0", len(history))
	}
}

func TestAppendRevisionRequiresTarget(t *testing.T) {
	s, _ := openTemp(t)
	_, err := s.AppendRevision(context.Background(), "web", "prod", NewRevision{RenderedSpec: sampleSpec})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestHistoryIsOrderedPerTarget(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	for _, name := range []string{"prod", "staging"} {
		if _, err := s.EnsureTarget(ctx, "web", name, ""); err != nil {
			t.Fatalf("EnsureTarget %s: %v", name, err)
		}
	}

	for _, commit := range []string{"c1", "c2", "c3"} {
		if _, err := s.AppendRevision(ctx, "web", "prod", NewRevision{Commit: commit, RenderedSpec: sampleSpec}); err != nil {
			t.Fatalf("AppendRevision %s: %v", commit, err)
		}
	}
	if _, err := s.AppendRevision(ctx, "web", "staging", NewRevision{Commit: "s1", RenderedSpec: sampleSpec}); err != nil {
		t.Fatalf("AppendRevision staging: %v", err)
	}

	history, err := s.Revisions(ctx, "web", "prod")
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	var got []string
	var numbers []int64
	for _, rev := range history {
		got = append(got, rev.Commit)
		numbers = append(numbers, rev.Number)
	}
	if !reflect.DeepEqual(got, []string{"c3", "c2", "c1"}) {
		t.Errorf("commits = %v, want newest first", got)
	}
	if !reflect.DeepEqual(numbers, []int64{3, 2, 1}) {
		t.Errorf("numbers = %v, want [3 2 1]", numbers)
	}

	staging, err := s.AppendRevision(ctx, "web", "staging", NewRevision{Commit: "s2", RenderedSpec: sampleSpec})
	if err != nil {
		t.Fatalf("AppendRevision staging: %v", err)
	}
	if staging != 2 {
		t.Errorf("staging revision = %d, want 2 (numbers are per target)", staging)
	}
}

func TestAddEvent(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, err := s.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}
	number, err := s.AppendRevision(ctx, "web", "prod", NewRevision{Commit: "c1", RenderedSpec: sampleSpec})
	if err != nil {
		t.Fatalf("AppendRevision: %v", err)
	}

	if err := s.AddEvent(ctx, Event{
		Application: "web",
		Target:      "prod",
		Revision:    number,
		Type:        "Synced",
		Payload:     map[string]int{"services": 1},
	}); err != nil {
		t.Fatalf("AddEvent: %v", err)
	}
	if err := s.AddEvent(ctx, Event{Application: "web", Target: "prod", Type: "Blocked"}); err != nil {
		t.Fatalf("AddEvent without revision: %v", err)
	}

	var (
		typ      string
		revision sql.NullInt64
		payload  sql.NullString
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT type, revision_id, payload FROM events WHERE type = 'Synced'`).Scan(&typ, &revision, &payload); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if !revision.Valid || payload.String != `{"services":1}` {
		t.Errorf("event = revision %v payload %q", revision, payload.String)
	}

	var nullRevision sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT revision_id FROM events WHERE type = 'Blocked'`).Scan(&nullRevision); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if nullRevision.Valid {
		t.Error("event without revision stored a revision_id")
	}

	if err := s.AddEvent(ctx, Event{Application: "web", Target: "prod", Revision: 9, Type: "Synced"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddEvent with unknown revision: err = %v, want ErrNotFound", err)
	}
}
