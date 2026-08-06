package sharedctx

import (
	"errors"
	"testing"
	"time"
)

func now() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) }

// Append is the default because it cannot conflict. If two people write at once
// both entries survive — there is no merge to get wrong.
func TestAppendNeverConflicts(t *testing.T) {
	s := New()
	if _, err := s.Append("findings", "alice", "postgres is fine", 1, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("findings", "bob", "sqlite is simpler", 2, now()); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get("findings")
	if !ok || len(e.Items) != 2 {
		t.Fatalf("got %d items, want both", len(e.Items))
	}
	if e.Items[0].By != "alice" || e.Items[1].By != "bob" {
		t.Fatalf("attribution lost: %+v", e.Items)
	}
}

// The whole point of a value entry: a stale write is refused loudly rather than
// silently discarding whatever the other writer knew.
func TestStaleValueWriteIsRefused(t *testing.T) {
	s := New()
	first, err := s.Set("plan", "alice", "use postgres", 0, 1, now())
	if err != nil {
		t.Fatal(err)
	}

	// Bob writes based on the version he saw — fine.
	if _, err := s.Set("plan", "bob", "use postgres, WAL on", first.Version, 2, now()); err != nil {
		t.Fatalf("write at current version should succeed: %v", err)
	}

	// Carol still holds the first version. Her write must not land.
	_, err = s.Set("plan", "carol", "use sqlite", first.Version, 3, now())
	var c *Conflict
	if !errors.As(err, &c) {
		t.Fatalf("stale write returned %v, want a conflict", err)
	}
	if c.Current != "use postgres, WAL on" || c.Version != 2 {
		t.Fatalf("conflict must carry what is current, got %+v", c)
	}

	// And the store still holds Bob's write, not Carol's.
	e, _ := s.Get("plan")
	if e.Value != "use postgres, WAL on" || e.By != "bob" {
		t.Fatalf("a refused write changed the entry: %+v", e)
	}
}

func TestFirstWriteAndForcedOverwrite(t *testing.T) {
	s := New()
	if _, err := s.Set("k", "alice", "v1", 0, 1, now()); err != nil {
		t.Fatalf("first write with ifMatch=0 should succeed: %v", err)
	}
	// ifMatch > 0 on a key that does not exist is a conflict, not a create.
	if _, err := s.Set("missing", "alice", "v", 5, 2, now()); err == nil {
		t.Fatal("expected a conflict writing a version to a nonexistent key")
	}
	// -1 means "I know, overwrite anyway".
	if _, err := s.Set("k", "bob", "v2", -1, 3, now()); err != nil {
		t.Fatalf("forced overwrite failed: %v", err)
	}
	if e, _ := s.Get("k"); e.Value != "v2" {
		t.Fatalf("value = %q, want v2", e.Value)
	}
}

// Mixing kinds on one key is a mistake worth refusing rather than guessing at.
func TestKindsDoNotMix(t *testing.T) {
	s := New()
	s.Append("notes", "alice", "one", 1, now())
	if _, err := s.Set("notes", "bob", "clobber", -1, 2, now()); !errors.Is(err, ErrWrongKind) {
		t.Fatalf("setting an append list returned %v, want ErrWrongKind", err)
	}

	s.Set("plan", "alice", "v", 0, 3, now())
	if _, err := s.Append("plan", "bob", "x", 4, now()); !errors.Is(err, ErrWrongKind) {
		t.Fatalf("appending to a value returned %v, want ErrWrongKind", err)
	}
}

func TestPinnedIsTheCuratedSubset(t *testing.T) {
	s := New()
	s.Append("notes", "alice", "a", 1, now())
	s.Set("goal", "alice", "ship it", 0, 2, now())
	if _, err := s.Pin("goal", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pin("nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pinning a missing key returned %v", err)
	}
	p := s.Pinned()
	if len(p) != 1 || p[0].Key != "goal" {
		t.Fatalf("pinned = %+v, want just goal", p)
	}
}

// Snapshots must be copies: a reader encoding one while a writer appends would
// otherwise race, which is exactly the bug this project already hit once.
func TestSnapshotIsACopy(t *testing.T) {
	s := New()
	s.Append("notes", "alice", "one", 1, now())
	snap := s.Snapshot()
	s.Append("notes", "bob", "two", 2, now())

	if len(snap[0].Items) != 1 {
		t.Fatal("snapshot changed under the caller")
	}
	snap[0].Items[0].Text = "mutated"
	if e, _ := s.Get("notes"); e.Items[0].Text != "one" {
		t.Fatal("mutating a snapshot reached the store")
	}
}
