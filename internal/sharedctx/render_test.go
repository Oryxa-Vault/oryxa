package sharedctx

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func store(t *testing.T) *Store {
	t.Helper()
	return New()
}

func TestRenderEmptyIsEmpty(t *testing.T) {
	// Not "(none)", not "{}". A room that has said nothing must add nothing to
	// the prompt, or every agent's first turn begins by explaining its own
	// emptiness.
	if got := Render(store(t).Snapshot()); got != "" {
		t.Fatalf("empty room rendered %q, want empty", got)
	}
}

func TestRenderValueOnOneLine(t *testing.T) {
	s := store(t)
	s.Set("plan", "alice", "ship v1 by friday", 0, 1, time.Now())

	want := "plan: ship v1 by friday"
	if got := Render(s.Snapshot()); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderMultilineValueStartsOnItsOwnLine(t *testing.T) {
	s := store(t)
	s.Set("plan", "alice", "step one\nstep two", 0, 1, time.Now())

	want := "plan:\nstep one\nstep two"
	if got := Render(s.Snapshot()); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderAppendAsList(t *testing.T) {
	s := store(t)
	s.Append("findings", "alice", "api is rate limited", 1, time.Now())
	s.Append("findings", "bob", "cache hit rate is 40%", 2, time.Now())

	want := "findings:\n- api is rate limited\n- cache hit rate is 40%"
	if got := Render(s.Snapshot()); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderSeparatesEntriesWithBlankLine(t *testing.T) {
	s := store(t)
	s.Set("plan", "alice", "ship", 0, 1, time.Now())
	s.Append("findings", "bob", "one", 2, time.Now())

	got := Render(s.Snapshot())
	if !strings.Contains(got, "\n\n") {
		t.Fatalf("entries not separated by a blank line:\n%s", got)
	}
}

// The order must come from the data, never from map iteration. An unstable
// rendering changes the prompt while the room stands still, which breaks
// reproducibility and misses every downstream prompt cache.
func TestRenderIsDeterministic(t *testing.T) {
	s := store(t)
	for i, k := range []string{"zebra", "alpha", "middle", "beta", "omega"} {
		s.Set(k, "someone", "v", 0, int64(i+1), time.Now())
	}

	first := Render(s.Snapshot())
	for i := 0; i < 100; i++ {
		if got := Render(s.Snapshot()); got != first {
			t.Fatalf("render %d differed:\n%s\n---\n%s", i, first, got)
		}
	}
	if !strings.HasPrefix(first, "alpha:") {
		t.Fatalf("not sorted by key:\n%s", first)
	}
}

func TestRenderEntrySkipsTheKey(t *testing.T) {
	s := store(t)
	s.Set("plan", "alice", "ship v1", 0, 1, time.Now())
	e, _ := s.Get("plan")

	if got := RenderEntry(e); got != "ship v1" {
		t.Fatalf("got %q, want the bare value", got)
	}
}

func TestRenderEntryListKeepsBullets(t *testing.T) {
	s := store(t)
	s.Append("notes", "alice", "one", 1, time.Now())
	s.Append("notes", "alice", "two", 2, time.Now())
	e, _ := s.Get("notes")

	if got := RenderEntry(e); got != "- one\n- two" {
		t.Fatalf("got %q", got)
	}
}

// An entry can exist with nothing in it — created and then never written, or an
// empty value. It must not leave a dangling "key:" line in the prompt.
func TestRenderSkipsEmptyEntries(t *testing.T) {
	s := store(t)
	s.Set("empty", "alice", "", 0, 1, time.Now())
	s.Set("real", "alice", "here", 0, 2, time.Now())

	got := Render(s.Snapshot())
	if strings.Contains(got, "empty") {
		t.Fatalf("empty entry leaked into the prompt: %q", got)
	}
	if got != "real: here" {
		t.Fatalf("got %q", got)
	}
}

// ---- bounding what reaches a prompt ----

func manyItems(n int) Entry {
	e := Entry{Key: "findings", Kind: KindAppend}
	for i := 1; i <= n; i++ {
		e.Items = append(e.Items, Item{By: "a", Text: fmt.Sprintf("finding %d", i)})
	}
	return e
}

func TestLongEntryKeepsTheNewestItems(t *testing.T) {
	got := RenderEntry(manyItems(MaxItemsPerEntry + 5))

	if strings.Contains(got, "finding 5\n") || strings.Contains(got, "- finding 5") {
		t.Fatalf("oldest items survived the bound:\n%s", got)
	}
	for _, want := range []string{
		fmt.Sprintf("- finding %d", MaxItemsPerEntry+5),
		fmt.Sprintf("- finding %d", MaxItemsPerEntry+1),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("newest items were dropped, missing %q:\n%s", want, got)
		}
	}
}

// A model handed the last twenty findings with no marker will answer as though
// those were all of them.
func TestTruncationSaysSo(t *testing.T) {
	got := RenderEntry(manyItems(MaxItemsPerEntry + 5))
	if !strings.Contains(got, "(5 earlier items not shown)") {
		t.Fatalf("truncation was silent:\n%s", got)
	}
}

func TestShortEntryIsUntouched(t *testing.T) {
	got := RenderEntry(manyItems(3))
	if strings.Contains(got, "not shown") {
		t.Fatalf("a room inside the bound was marked as trimmed:\n%s", got)
	}
	if lines := strings.Count(got, "\n") + 1; lines != 3 {
		t.Fatalf("rendered %d lines, want 3:\n%s", lines, got)
	}
}

// A value entry is the shape that does not grow, so nothing here applies to it.
func TestValueEntryIsNeverTrimmed(t *testing.T) {
	e := Entry{Key: "plan", Kind: KindValue, Value: strings.Repeat("x", 5000)}
	if got := RenderEntry(e); got != e.Value {
		t.Fatalf("value entry was altered: %d chars in, %d out", len(e.Value), len(got))
	}
	if n := Elided([]Entry{e}); n != 0 {
		t.Fatalf("Elided counted %d for a value entry", n)
	}
}

func TestElidedCountsWhatThePromptLeftOut(t *testing.T) {
	entries := []Entry{manyItems(MaxItemsPerEntry + 7), {Key: "plan", Kind: KindValue, Value: "ship it"}}
	if n := Elided(entries); n != 7 {
		t.Fatalf("Elided = %d, want 7", n)
	}
	if n := Elided([]Entry{manyItems(2)}); n != 0 {
		t.Fatalf("Elided = %d for a short room, want 0", n)
	}
}
