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

// ---- rollup ----

func rolled(n, covers int, through int64) Entry {
	e := manyItems(n)
	for i := range e.Items {
		e.Items[i].Seq = int64(i + 1)
	}
	e.Rollup = &Rollup{Text: "the api was rate limited and the pool was capped", Covers: covers, Through: through}
	return e
}

func TestRollupReplacesTheNotShownMarker(t *testing.T) {
	// 25 items, 20 shown, 5 trimmed — and the rollup covers all 5.
	got := RenderEntry(rolled(MaxItemsPerEntry+5, 5, 5))
	if strings.Contains(got, "not shown") {
		t.Fatalf("a covered tail still reported items as missing:\n%s", got)
	}
	if !strings.Contains(got, "(5 earlier items, summarised) the api was rate limited") {
		t.Fatalf("summary missing:\n%s", got)
	}
}

// The summary stands where the items it replaced were, so the entry still reads
// oldest to newest.
func TestRollupComesBeforeTheItemsItPrecedes(t *testing.T) {
	got := RenderEntry(rolled(MaxItemsPerEntry+5, 5, 5))
	sum := strings.Index(got, "summarised")
	first := strings.Index(got, "- finding 6")
	if sum < 0 || first < 0 || sum > first {
		t.Fatalf("summary is not first:\n%s", got)
	}
}

// Items added after a rollup are not represented by it. Letting them vanish
// behind a summary that never saw them is the failure worth avoiding.
func TestItemsPastTheRollupAreStillReportedMissing(t *testing.T) {
	// 30 items, 10 trimmed, rollup only covers the first 4.
	got := RenderEntry(rolled(MaxItemsPerEntry+10, 4, 4))
	if !strings.Contains(got, "(4 earlier items, summarised)") {
		t.Fatalf("covered items lost their summary:\n%s", got)
	}
	if !strings.Contains(got, "(6 earlier items not shown)") {
		t.Fatalf("uncovered items were not reported:\n%s", got)
	}
}

// Elided is the "you are answering from a partial room" warning. Items a rollup
// speaks for are represented, not missing — counting them would leave the
// warning permanently on in any room long enough to need one.
func TestElidedIgnoresWhatARollupCovers(t *testing.T) {
	if n := Elided([]Entry{rolled(MaxItemsPerEntry+5, 5, 5)}); n != 0 {
		t.Fatalf("Elided = %d for a fully covered tail, want 0", n)
	}
	if n := Elided([]Entry{rolled(MaxItemsPerEntry+10, 4, 4)}); n != 6 {
		t.Fatalf("Elided = %d, want the 6 nothing speaks for", n)
	}
}

func TestNeedsRollupOnlyWhenTheTailIsWorthIt(t *testing.T) {
	s := store(t)
	for i := 1; i <= MaxItemsPerEntry+3; i++ {
		s.Append("findings", "a", fmt.Sprintf("f%d", i), int64(i), time.Now())
	}
	if _, _, ok := s.NeedsRollup("findings", 10); ok {
		t.Fatal("3 trimmed items triggered a rollup at a threshold of 10")
	}
	items, through, ok := s.NeedsRollup("findings", 3)
	if !ok {
		t.Fatal("3 trimmed items did not meet a threshold of 3")
	}
	if len(items) != 3 || through != 3 {
		t.Fatalf("got %d items through seq %d, want 3 through 3", len(items), through)
	}
	if items[0].Text != "f1" {
		t.Fatalf("rollup was handed %q first; it must summarise from the oldest", items[0].Text)
	}
}

// Summarising a summary loses a little each pass. A second rollup is built from
// the original items, which the store still has.
func TestSecondRollupStartsFromTheOriginalItems(t *testing.T) {
	s := store(t)
	for i := 1; i <= MaxItemsPerEntry+5; i++ {
		s.Append("findings", "a", fmt.Sprintf("f%d", i), int64(i), time.Now())
	}
	if _, err := s.SetRollup("findings", "first summary", "roller", 5, 5); err != nil {
		t.Fatal(err)
	}
	for i := MaxItemsPerEntry + 6; i <= MaxItemsPerEntry+12; i++ {
		s.Append("findings", "a", fmt.Sprintf("f%d", i), int64(i), time.Now())
	}
	items, _, ok := s.NeedsRollup("findings", 1)
	if !ok {
		t.Fatal("a grown tail did not ask for a rollup")
	}
	if items[0].Text != "f1" {
		t.Fatalf("re-roll started at %q, not the oldest original item", items[0].Text)
	}
}

func TestRollupIsRefusedOnAValueEntry(t *testing.T) {
	s := store(t)
	s.Set("plan", "a", "ship", 0, 1, time.Now())
	if _, err := s.SetRollup("plan", "x", "roller", 1, 1); err == nil {
		t.Fatal("a value entry accepted a rollup")
	}
}

// A reader must never share a Rollup with the store.
func TestSnapshotDoesNotShareTheRollup(t *testing.T) {
	s := store(t)
	s.Append("findings", "a", "one", 1, time.Now())
	s.SetRollup("findings", "original", "roller", 1, 1)

	got, _ := s.Get("findings")
	got.Rollup.Text = "mutated by a reader"

	again, _ := s.Get("findings")
	if again.Rollup.Text != "original" {
		t.Fatalf("a reader mutated the store: %q", again.Rollup.Text)
	}
}
