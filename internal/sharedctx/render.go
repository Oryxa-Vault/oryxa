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
	var lines []string
	if elided > 0 {
		lines = append(lines, fmt.Sprintf("- (%d earlier items not shown)", elided))
	}
	for _, it := range items {
		lines = append(lines, "- "+it.Text)
	}
	return strings.Join(lines, "\n")
}

// Elided reports how many items Render leaves out, so a caller can record that
// the prompt was trimmed rather than discover it from a model's behaviour.
func Elided(entries []Entry) int {
	n := 0
	for _, e := range entries {
		if e.Kind != KindValue {
			_, dropped := tail(e.Items)
			n += dropped
		}
	}
	return n
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
