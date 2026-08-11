package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func ev(seq int64, kind, actor string, data map[string]any) event {
	b, _ := json.Marshal(data)
	return event{Seq: seq, Kind: kind, Actor: actor, Data: b}
}

func render(t *testing.T, raw bool, evs ...event) string {
	t.Helper()
	var buf bytes.Buffer
	p := newPrinter(raw)
	p.w = &buf
	for _, e := range evs {
		p.print(e)
	}
	return buf.String()
}

// Two agents answering one question is the whole point of a room, and it was the
// one thing the transcript could not show.
//
// This is the real event order from a live session with Claude Code and Codex.
// Announcing an agent when its turn *started* printed both headers back to back
// and then ran both answers together as one paragraph:
//
//	claude-code ‹   codex ‹ I'll check the README…Oryxa lets multiple people…
//
// The log was right the whole time; only this renderer was wrong.
func TestParallelTurnsStayAttributed(t *testing.T) {
	out := render(t, false,
		ev(1, "input.submitted", "shubham", map[string]any{"text": "what does this repo do?", "group": "in_1"}),
		ev(2, "turn.started", "shubham", map[string]any{"agent": "claude-code"}),
		ev(3, "turn.started", "shubham", map[string]any{"agent": "codex"}),
		ev(4, "output.part", "claude-code", map[string]any{"kind": "text", "text": "Oryxa makes agent frameworks multi-user."}),
		ev(5, "output.part", "codex", map[string]any{"kind": "text", "text": "It turns single-user frameworks into shared rooms."}),
		ev(6, "turn.finished", "claude-code", map[string]any{"chars": 40}),
		ev(7, "turn.finished", "codex", map[string]any{"chars": 49}),
	)

	for _, want := range []string{
		"  claude-code ‹ Oryxa makes agent frameworks multi-user.",
		"  codex ‹ It turns single-user frameworks into shared rooms.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing an attributed block:\n%s\n--- got ---\n%s", want, out)
		}
	}
	// The exact shape of the old bug: two headers with nothing between them.
	if strings.Contains(out, "‹   codex ‹") || strings.Contains(out, "‹ codex ‹") {
		t.Errorf("headers ran together:\n%s", out)
	}
	// And neither answer may end up inside the other's line.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "multi-user.") && strings.Contains(line, "shared rooms.") {
			t.Errorf("two agents' answers were concatenated:\n%s", line)
		}
	}
}

// A single agent must be unchanged by all of this: one header, one continuous
// stream, deltas still concatenating into one answer rather than one line each.
func TestOneAgentStreamsAsOneBlock(t *testing.T) {
	out := render(t, false,
		ev(1, "turn.started", "a", map[string]any{"agent": "writer"}),
		ev(2, "output.part", "writer", map[string]any{"kind": "text", "text": "the "}),
		ev(3, "output.part", "writer", map[string]any{"kind": "text", "text": "answer "}),
		ev(4, "output.part", "writer", map[string]any{"kind": "text", "text": "arrives"}),
		ev(5, "turn.finished", "writer", map[string]any{"chars": 18}),
	)
	if !strings.Contains(out, "  writer ‹ the answer arrives") {
		t.Errorf("deltas did not concatenate:\n%q", out)
	}
	if strings.Count(out, "writer ‹") != 1 {
		t.Errorf("one agent produced more than one header:\n%q", out)
	}
}

// The header moved from turn.started to first-text, so a turn that says nothing
// would have vanished entirely — and a silent agent is the failure people blame
// the framework for. The server already names which half it came from, and the
// terminal now says that rather than a thinner version of it.
func TestASilentTurnIsDiagnosed(t *testing.T) {
	out := render(t, false,
		ev(1, "turn.started", "a", map[string]any{"agent": "quiet"}),
		ev(2, "turn.finished", "quiet", map[string]any{"chars": 0}),
		ev(3, "turn.empty", "quiet", map[string]any{
			"reason": "the agent sent 4 parts and no text came out of them",
			"text":   []string{"$.output", "$.text"},
			"when":   "$.partial",
		}),
	)
	if !strings.Contains(out, "quiet ‹ finished without saying anything") {
		t.Errorf("a silent turn left no trace:\n%q", out)
	}
	// The selectors are the whole point: they are what you compare against the
	// raw view to settle whether it was the agent or the connector.
	for _, want := range []string{"$.output | $.text", "when: $.partial"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// `text` is a string on every other event and a list here. One shared struct
// fails the whole decode on that mismatch and reports an empty reason.
func TestSilentTurnDecodesDespiteTheTextTypeClash(t *testing.T) {
	out := render(t, false,
		ev(1, "turn.empty", "quiet", map[string]any{
			"reason": "the agent sent nothing at all",
			"parts":  0,
		}),
	)
	if !strings.Contains(out, "the agent sent nothing at all") {
		t.Errorf("reason was lost to a decode error:\n%q", out)
	}
}

// An agent speaking again after another interrupted it gets its own block back,
// rather than having its second half attributed to whoever spoke in between.
func TestSpeakerChangeReopensABlock(t *testing.T) {
	out := render(t, false,
		ev(1, "turn.started", "a", map[string]any{"agent": "one"}),
		ev(2, "turn.started", "a", map[string]any{"agent": "two"}),
		ev(3, "output.part", "one", map[string]any{"kind": "text", "text": "first"}),
		ev(4, "output.part", "two", map[string]any{"kind": "text", "text": "second"}),
		ev(5, "output.part", "one", map[string]any{"kind": "text", "text": "third"}),
		ev(6, "turn.finished", "one", map[string]any{"chars": 10}),
		ev(7, "turn.finished", "two", map[string]any{"chars": 6}),
	)
	if strings.Count(out, "one ‹") != 2 {
		t.Errorf("interrupted agent did not get a second block:\n%q", out)
	}
	if strings.Contains(out, "secondthird") {
		t.Errorf("two agents' text ran together:\n%q", out)
	}
}

// The question is printed once however many agents it woke.
func TestOneQuestionIsPrintedOncePerGroup(t *testing.T) {
	out := render(t, false,
		ev(1, "input.submitted", "priya", map[string]any{"text": "ship it?", "group": "in_9"}),
		ev(2, "input.submitted", "priya", map[string]any{"text": "ship it?", "group": "in_9"}),
	)
	if n := strings.Count(out, "ship it?"); n != 1 {
		t.Errorf("question printed %d times, want 1:\n%q", n, out)
	}
}

// Activity in raw mode must not land in the middle of somebody's sentence.
func TestRawActivityDoesNotBreakALine(t *testing.T) {
	out := render(t, true,
		ev(1, "turn.started", "a", map[string]any{"agent": "one"}),
		ev(2, "output.part", "one", map[string]any{"kind": "text", "text": "before"}),
		ev(3, "output.part", "one", map[string]any{"kind": "activity"}),
		ev(4, "output.part", "one", map[string]any{"kind": "text", "text": "after"}),
		ev(5, "turn.finished", "one", map[string]any{"chars": 11}),
	)
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "before") && strings.Contains(line, "·") {
			t.Errorf("activity was spliced into a sentence:\n%q", line)
		}
	}
}
