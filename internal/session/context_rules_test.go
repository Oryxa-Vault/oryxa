package session

import (
	"encoding/json"
	"testing"

	"github.com/Oryxa-Vault/oryxa/internal/connector"
)

func chunksOf(t *testing.T, lines ...string) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(lines))
	for _, l := range lines {
		out = append(out, json.RawMessage(l))
	}
	return out
}

// An agent that says several things in one turn says them in order, and what it
// concluded is the last of them.
//
// From a real room: Codex opens a turn with "I'll check the repository
// references", then answers. Both are agent_message items, so the rule recorded
// two findings — one of substance and one of throat-clearing, and afterwards
// nothing distinguishes them.
func TestLastKeepsOnlyTheConclusion(t *testing.T) {
	chunks := chunksOf(t,
		`{"item":{"type":"agent_message","text":"I'll check the repository references."}}`,
		`{"item":{"type":"agent_message","text":"oryxa-shim exposes command-line agents over HTTP."}}`,
	)
	rule := connector.ContextRule{
		Key: "findings", From: "$.item.text",
		When: "$.item.type == agent_message", Last: true,
	}
	got := ruleValues(rule, "", chunks)
	if len(got) != 1 {
		t.Fatalf("kept %d values, want 1: %v", len(got), got)
	}
	if got[0] != "oryxa-shim exposes command-line agents over HTTP." {
		t.Errorf("kept the wrong one: %q", got[0])
	}
}

// Without it, every match is kept — which is what a rule reading tool results or
// citations wants, and why this is a flag rather than the default.
func TestWithoutLastEveryMatchIsKept(t *testing.T) {
	chunks := chunksOf(t,
		`{"item":{"type":"agent_message","text":"first"}}`,
		`{"item":{"type":"agent_message","text":"second"}}`,
	)
	rule := connector.ContextRule{
		Key: "findings", From: "$.item.text", When: "$.item.type == agent_message",
	}
	if got := ruleValues(rule, "", chunks); len(got) != 2 {
		t.Errorf("kept %d values, want 2: %v", len(got), got)
	}
}

// A turn that said one thing is unaffected, and a turn that said nothing still
// writes nothing.
func TestLastIsHarmlessOnOneOrNone(t *testing.T) {
	rule := connector.ContextRule{
		Key: "findings", From: "$.item.text",
		When: "$.item.type == agent_message", Last: true,
	}
	one := ruleValues(rule, "", chunksOf(t, `{"item":{"type":"agent_message","text":"only"}}`))
	if len(one) != 1 || one[0] != "only" {
		t.Errorf("single match: %v", one)
	}
	none := ruleValues(rule, "", chunksOf(t, `{"item":{"type":"reasoning","text":"thinking"}}`))
	if len(none) != 0 {
		t.Errorf("gated-out chunks produced %v", none)
	}
}

// $text is already the whole turn, so last has nothing to choose between.
func TestLastDoesNotDisturbATextRule(t *testing.T) {
	rule := connector.ContextRule{Key: "findings", From: connector.SourceText, Last: true}
	got := ruleValues(rule, "the whole assembled answer", nil)
	if len(got) != 1 || got[0] != "the whole assembled answer" {
		t.Errorf("got %v", got)
	}
}
