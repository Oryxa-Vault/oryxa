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
// Elided is how many items the room held but the prompt left out. Zero is the
// normal case; anything else means this turn answered from a partial room, which
// is worth knowing before trusting what it said.
type contextDigest struct {
	Version int64    `json:"version"`
	Keys    []string `json:"keys,omitempty"`
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
func contextSnapshot(st *sharedctx.Store) (connector.ContextView, contextDigest) {
	entries := st.Snapshot()
	keys := make(map[string]string, len(entries))
	names := make([]string, 0, len(entries))
	var pinned []sharedctx.Entry
	var version int64
	for _, e := range entries {
		keys[e.Key] = sharedctx.RenderEntry(e)
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
	return view, contextDigest{
		Version: version,
		Keys:    names,
		Chars:   len(view.All),
		Elided:  sharedctx.Elided(entries),
	}
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
