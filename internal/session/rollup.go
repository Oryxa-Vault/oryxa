package session

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Oryxa-Vault/oryxa/internal/connector"
	"github.com/Oryxa-Vault/oryxa/internal/sharedctx"
)

// Rolling a long list up into a summary, so a room stops forgetting quietly.
//
// The render bound keeps prompts finite by showing only an entry's newest items
// and saying how many it left out. That stops the prompt growing without limit
// and replaces it with a subtler problem: an agent answering from a partial room
// sounds exactly as confident as one answering from a whole one.
//
// A rollup is what those older items meant, at a size that fits. Three things
// decide its shape:
//
//	recorded    a summary is a model's output, so it cannot be recomputed on
//	            replay without the room changing after every restart. It is
//	            written once, as an event, and replay applies the text as data.
//	from source it is always built from the original items, never from the
//	            previous summary. Summarising a summary loses a little each pass,
//	            and a long room would end up describing itself in generalities.
//	a connector the summariser is an ordinary connector, so this adds a slot
//	            rather than a model dependency. Unconfigured, nothing rolls up
//	            and the count-only marker stays.

// RollupAfter is how many unsummarised items must fall off the end of a prompt
// before spending a model call on them. Low enough that a room does not carry a
// large blind spot for long; high enough that a call is not made per item.
const RollupAfter = 10

// WithSummariser names the connector that writes rollups. Empty disables them.
func (m *Manager) WithSummariser(agent string) *Manager {
	m.summariser = agent
	return m
}

// rollupState keeps one summary per key in flight at a time. Two concurrent
// rollups of one key would both read the same tail, both spend a call, and the
// loser's work would be thrown away.
type rollupState struct {
	mu      sync.Mutex
	running map[string]bool
}

func (r *rollupState) claim(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running == nil {
		r.running = map[string]bool{}
	}
	if r.running[key] {
		return false
	}
	r.running[key] = true
	return true
}

func (r *rollupState) release(key string) {
	r.mu.Lock()
	delete(r.running, key)
	r.mu.Unlock()
}

// maybeRollup summarises a key's overflow if there is enough of it.
//
// Runs off the turn path deliberately. Agents are a turn behind by design, so a
// summary that lands before the next turn is soon enough — and making a room's
// bookkeeping wait on a model call would put the framework's own latency in
// front of the answer someone asked for.
func (m *Manager) maybeRollup(sessionID, key string) {
	if m.summariser == "" {
		return
	}
	s, ok := m.get(sessionID)
	if !ok {
		return
	}
	items, through, need := s.ctx.NeedsRollup(key, RollupAfter)
	if !need {
		return
	}
	spec, ok := m.reg.Get(m.summariser)
	if !ok {
		m.emit(sessionID, "rollup.failed", m.summariser, "", map[string]string{
			"key": key, "error": "summariser is not a registered connector",
		})
		return
	}
	if !s.rollups.claim(key) {
		return
	}

	go func() {
		defer s.rollups.release(key)

		text, err := m.summarise(spec, items)
		if err != nil || strings.TrimSpace(text) == "" {
			// Never fatal, never retried in a loop: the next turn that grows the
			// tail will ask again. Recorded because a summariser that has stopped
			// working is otherwise invisible — the room just goes back to
			// forgetting, which is the state this exists to fix.
			reason := "summariser returned nothing"
			if err != nil {
				reason = err.Error()
			}
			m.emit(sessionID, "rollup.failed", m.summariser, "", map[string]string{
				"key": key, "error": reason,
			})
			return
		}

		// Event first, then the fold — the same order every other write here
		// uses, so what replay sees and what the room holds cannot disagree.
		if _, err := m.log.Append(sessionID, "context.rolled_up", m.summariser, "", map[string]any{
			"key": key, "text": text, "covers": len(items), "through": through,
		}); err != nil {
			m.logFailure(sessionID, "context.rolled_up", err)
			return
		}
		if _, err := s.ctx.SetRollup(key, text, m.summariser, len(items), through); err != nil {
			m.logFailure(sessionID, "context.rolled_up", err)
		}
	}()
}

// summarise runs one turn against the summariser connector.
//
// The items go in as {{input}}, so the connector is entirely ordinary — the same
// spec, executor and selectors as any agent. What the summary should emphasise
// belongs in that connector's own prompt, not in Oryxa.
func (m *Manager) summarise(spec *connector.Spec, items []sharedctx.Item) (string, error) {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "- %s: %s\n", it.By, it.Text)
	}

	ctx, cancel := context.WithTimeout(context.Background(), spec.TimeoutDuration())
	defer cancel()

	tc := connector.Ctx{
		Input:        strings.TrimRight(b.String(), "\n"),
		Turn:         "rollup_" + randHex(6),
		Conversation: "rollup_" + randHex(6), // a fresh thread: no room history
		Agent:        spec.Name,
		Vars:         spec.Vars,
		Captures:     map[string]string{},
	}

	var out []byte
	err := m.exec.Turn(ctx, spec, tc, func(p connector.Part) {
		if p.Kind == "text" {
			out = append(out, p.Text...)
		}
	})
	return strings.TrimSpace(string(out)), err
}

// applyRolledUp replays a recorded summary. Kept beside the writer so the two
// stay in step: this is the half that makes a rollup survive a restart.
func applyRolledUp(s *session, key, text, by string, covers int, through int64) {
	_, _ = s.ctx.SetRollup(key, text, by, covers, through)
}
