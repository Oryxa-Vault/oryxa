package sharedctx

import (
	"fmt"
	"strings"
)

// MaxItemsPerEntry bounds how much of an append entry reaches a prompt.
//
// The bound is applied when rendering, never to the store: dropping items from
// the fold would need its own event to survive a restart, and a room whose
// history depends on when the server last restarted is not a history. Everything
// stays queryable over the API and in the log; this only decides how much of it
// is worth putting in front of a model.
//
// A room accumulating one finding per turn otherwise grows the prompt without
// limit, and the failure that follows is not an error — the model quietly
// returns less than it should, which looks like a bad agent rather than a full
// window.
const MaxItemsPerEntry = 20

// Render turns entries into the text an agent sees.
//
// The format is plain and boring on purpose. Whatever goes in front of a model
// is also what a human reads when a prompt produced something strange, and YAML
// or JSON framing would spend tokens on punctuation that neither audience needs:
//
//	plan: ship v1 by friday
//
//	findings:
//	- api is rate limited
//	- cache hit rate is 40%
//
// Entries arrive sorted by key from Snapshot, so the same state always renders
// the same text. That matters more than it looks: an unstable rendering would
// change the prompt without anything in the room changing, and every prompt
// cache downstream would miss.
func Render(entries []Entry) string {
	var blocks []string
	for _, e := range entries {
		if body := RenderEntry(e); body != "" {
			blocks = append(blocks, e.Key+":"+separator(body)+body)
		}
	}
	return strings.Join(blocks, "\n\n")
}

// RenderEntry is one entry's content without its key, which is what
// {{context.<key>}} substitutes: the caller named the key, so repeating it
// would just be noise inside their own prompt.
//
// A truncated list says so. Handing a model the last twenty findings with no
// marker invites it to answer as though those were all of them, which turns a
// budget into a wrong answer — the one failure worse than a long prompt.
func RenderEntry(e Entry) string {
	if e.Kind == KindValue {
		return e.Value
	}
	items, elided := tail(e.Items)
	covered, uncovered := coverage(e, elided)

	var lines []string
	// Order matters: the summary stands where the items it replaced would have
	// been, so the entry still reads oldest-to-newest.
	if covered > 0 {
		lines = append(lines, fmt.Sprintf("- (%d earlier items, summarised) %s", covered, e.Rollup.Text))
	}
	if uncovered > 0 {
		lines = append(lines, fmt.Sprintf("- (%d earlier items not shown)", uncovered))
	}
	for _, it := range items {
		lines = append(lines, "- "+it.Text)
	}
	return strings.Join(lines, "\n")
}

// coverage splits the trimmed items into those a rollup speaks for and those
// nothing speaks for.
func coverage(e Entry, elided int) (covered, uncovered int) {
	if elided == 0 {
		return 0, 0
	}
	if e.Rollup == nil {
		return 0, elided
	}
	for _, it := range e.Items[:elided] {
		if it.Seq <= e.Rollup.Through {
			covered++
		}
	}
	return covered, elided - covered
}

// Elided reports how many items reach the prompt neither in full nor through a
// summary — which is the number worth warning about.
//
// Items a rollup covers are deliberately not counted. They are represented
// rather than missing, and reporting them would leave the warning permanently on
// in any room long enough to need summarising, which is how a warning stops
// being read.
func Elided(entries []Entry) int {
	n := 0
	for _, e := range entries {
		if e.Kind == KindValue {
			continue
		}
		_, dropped := tail(e.Items)
		_, uncovered := coverage(e, dropped)
		n += uncovered
	}
	return n
}

// NeedsRollup reports entries whose unsummarised tail has grown past want, so a
// caller can decide whether to spend a model call. Returning the items rather
// than the entry is deliberate: a summary is built from the original items every
// time, never from the previous summary, because summarising a summary loses a
// little on each pass and a room that ran long enough would end up describing
// itself in generalities.
func (s *Store) NeedsRollup(key string, want int) (items []Item, through int64, ok bool) {
	e, found := s.Get(key)
	if !found || e.Kind == KindValue {
		return nil, 0, false
	}
	_, dropped := tail(e.Items)
	_, uncovered := coverage(e, dropped)
	if uncovered < want || dropped == 0 {
		return nil, 0, false
	}
	stale := e.Items[:dropped]
	return stale, stale[len(stale)-1].Seq, true
}

// SetRollup records a summary. It is applied from an event rather than computed
// here, so replay and the live path put the same text in front of an agent.
func (s *Store) SetRollup(key, text, by string, covers int, through int64) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	if e.Kind != KindAppend {
		return Entry{}, fmt.Errorf("%w: %q is a value", ErrWrongKind, key)
	}
	e.Rollup = &Rollup{Text: text, Covers: covers, Through: through, By: by}
	return copyEntry(e), nil
}

// tail keeps the newest items, because a room's recent state is what a turn is
// usually acting on. What the older ones held that still matters is what pinning
// and value entries are for — both survive this untouched.
func tail(items []Item) ([]Item, int) {
	if MaxItemsPerEntry <= 0 || len(items) <= MaxItemsPerEntry {
		return items, 0
	}
	return items[len(items)-MaxItemsPerEntry:], len(items) - MaxItemsPerEntry
}

// separator keeps a short value on the key's line and puts anything multi-line
// below it, so a list never begins halfway through a line.
func separator(body string) string {
	if strings.Contains(body, "\n") {
		return "\n"
	}
	return " "
}
