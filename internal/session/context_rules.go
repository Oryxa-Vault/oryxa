package session

import (
	"encoding/json"
	"strings"

	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/sharedctx"
)

// This file is how shared context stops being a noticeboard only humans read.
//
// Two directions, both declarative, neither requiring anything of the agent:
//
//	in    contextSnapshot renders the room into {{context}}, which a connector
//	      splices into whatever field its framework calls the prompt
//	out   applyContextRules extracts part of a finished turn back into the room
//
// The agent never learns Oryxa exists. That is the point: a mechanism that
// needed agent cooperation would work on the frameworks with tool calling and
// leave the rest out, and "works everywhere" is the property being defended.

// contextDigest records which snapshot a turn was handed, precisely enough to
// reproduce it without copying it.
//
// Version is the sequence number of the newest context write the turn could
// see, so folding the log's context events up to it reproduces exactly this
// state. The rendered text is deliberately not recorded: it is already in the
// log as the events it was folded from, and because a room's context contains
// every earlier turn's output, storing it per turn would grow the log with the
// square of the room's length.
//
// Chars is what that context costs the prompt. It is the number worth watching:
// an agent that starts returning nothing is usually a room that outgrew the
// model's window, and without this the log gives no warning that it happened.
// Keys is what the room held. Reads is which of it this connector's template
// actually asked for, and Chars is what those reads cost the request — the two
// are separate because most connectors read none of the room, and reporting the
// room's size as their prompt size would make every one of them look like it was
// about to overrun a window it never touches.
//
// Elided is how many items were left out of what was rendered. Zero is the
// normal case; anything else means this turn answered from a partial room, which
// is worth knowing before trusting what it said.
type contextDigest struct {
	Version int64    `json:"version"`
	Keys    []string `json:"keys,omitempty"`
	Reads   []string `json:"reads,omitempty"`
	Chars   int      `json:"chars"`
	Elided  int      `json:"elided,omitempty"`
}

// contextSnapshot renders the room for one turn and describes what it rendered.
//
// Snapshotted once, at the moment the turn starts. A lane that finishes and
// writes while another lane is mid-flight does not retroactively change what
// that other agent was asked — its prompt is whatever the room held when it was
// handed the question, which is the only version of events it could act on.
//
// The digest comes from that same snapshot rather than a second read, so what
// the log says the agent saw and what the agent was actually given cannot drift.
// refs is what the connector's turn template references, which is what decides
// how much of the snapshot becomes a request.
func contextSnapshot(st *sharedctx.Store, refs []string) (connector.ContextView, contextDigest) {
	entries := st.Snapshot()
	keys := make(map[string]string, len(entries))
	byKey := make(map[string]sharedctx.Entry, len(entries))
	names := make([]string, 0, len(entries))
	var pinned []sharedctx.Entry
	var version int64
	for _, e := range entries {
		keys[e.Key] = sharedctx.RenderEntry(e)
		byKey[e.Key] = e
		names = append(names, e.Key)
		if e.Version > version {
			version = e.Version
		}
		if e.Pinned {
			pinned = append(pinned, e)
		}
	}
	view := connector.ContextView{
		All:    sharedctx.Render(entries),
		Pinned: sharedctx.Render(pinned),
		Keys:   keys,
	}

	// Cost is charged per reference, not per distinct binding: a template that
	// splices the room in twice pays for it twice, and the number is here to say
	// what the request weighs.
	d := contextDigest{Version: version, Keys: names, Reads: refs}
	for _, ref := range refs {
		switch {
		case ref == "context":
			d.Chars += len(view.All)
			d.Elided += sharedctx.Elided(entries)
		case ref == "context.pinned":
			d.Chars += len(view.Pinned)
			d.Elided += sharedctx.Elided(pinned)
		default:
			key := strings.TrimPrefix(ref, "context.")
			d.Chars += len(keys[key])
			if e, ok := byKey[key]; ok {
				d.Elided += sharedctx.Elided([]sharedctx.Entry{e})
			}
		}
	}
	return view, d
}

// applyContextRules writes what an agent contributed back into the room.
//
// Called only after a turn succeeds. A failed or cancelled turn leaves no trace
// in shared context: half an answer recorded as a finding is worse than no
// finding, because the next agent cannot tell the difference.
//
// Writes go through the same Manager methods humans use, so a rule-written
// entry is an ordinary event — attributed to the agent, replayed on restart,
// and serialised by the same lock. Nothing about it is a special case.
func (m *Manager) applyContextRules(id string, spec *connector.Spec, agent, out string, chunks []json.RawMessage) {
	for _, r := range spec.Context {
		values := ruleValues(r, out, chunks)
		if len(values) == 0 {
			continue
		}
		m.writeRule(id, r, agent, values)
	}
}

// ruleValues resolves one rule against a finished turn.
func ruleValues(r connector.ContextRule, out string, chunks []json.RawMessage) []string {
	if r.FromText() {
		if t := strings.TrimSpace(out); t != "" {
			return []string{t}
		}
		return nil
	}

	var values []string
	for _, raw := range chunks {
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			continue
		}
		if r.When != "" && !connector.Matches(decoded, r.When) {
			continue
		}
		for _, v := range connector.Select(decoded, r.From) {
			if v = strings.TrimSpace(v); v != "" {
				values = append(values, v)
			}
		}
	}
	// An agent that says several things in one turn says them in order, so what
	// it concluded is the last of them. Kept behind a flag rather than made the
	// default: a rule reading tool results or citations wants all of them, and
	// which of those an agent is doing is the connector author's to know.
	if r.Last && len(values) > 1 {
		values = values[len(values)-1:]
	}
	return values
}

func (m *Manager) writeRule(id string, r connector.ContextRule, agent string, values []string) {
	var entry sharedctx.Entry
	var err error

	if r.Kind == "value" {
		// The last match wins. A streaming agent emits its answer as a growing
		// prefix, so earlier matches are strictly less complete than later ones.
		//
		// ifMatch is -1: the agent holds no version and never saw one. Failing
		// the write because a human edited the key mid-turn would discard a real
		// answer to report a conflict nobody can act on — and the log keeps both
		// writes either way, so nothing is actually lost.
		entry, err = m.SetContext(id, r.Key, agent, values[len(values)-1], -1)
	} else {
		for _, v := range values {
			entry, err = m.AppendContext(id, r.Key, agent, v)
			if err != nil {
				break
			}
		}
	}

	if err != nil {
		// A rule failing must not fail the turn: the agent did its work and the
		// user is owed the answer. Recording it keeps the wrong-kind case — a
		// rule appending to a key someone else made a value — from being silent.
		m.emit(id, "context.rejected", agent, "", map[string]string{
			"key": r.Key, "error": err.Error(),
		})
		return
	}
	if r.Pin && !entry.Pinned {
		if _, err := m.PinContext(id, r.Key, agent, true); err != nil {
			m.emit(id, "context.rejected", agent, "", map[string]string{
				"key": r.Key, "error": err.Error(),
			})
		}
	}
}
