package session

import (
	"strings"
	"testing"

	"github.com/oryxa/oryxa/internal/connector"
)

func room7(t *testing.T) (*connector.Registry, []string) {
	t.Helper()
	reg := connector.NewRegistry()
	specs := map[string][]string{
		"analyst":  {"spend", "budget", "cost", "roi"},
		"writer":   {"copy", "draft", "blog"},
		"designer": {"brand", "visual", "logo"},
		"seo":      {"ranking", "keywords", "traffic"},
		"social":   {"instagram", "linkedin", "posting"},
		"pr":       {"press", "journalist", "coverage"},
		"ops":      {"tooling", "vendor", "contract"},
	}
	var names []string
	for name, interests := range specs {
		s := &connector.Spec{
			Name: name, Base: "http://127.0.0.1:1",
			Turn: &connector.Step{Path: "/t"}, Interests: interests,
		}
		if err := reg.Put(s); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return reg, names
}

func who(w wake) string { return strings.Join(w.Agents, ",") + " (" + w.Why + ")" }

func TestWakeLadder(t *testing.T) {
	reg, agents := room7(t)
	people := map[string]bool{"priya": true, "arsh": true, "sam": true}

	cases := []struct {
		name, text string
		to         []string
		wantAgents int
		wantOne    string
		wantWhy    string
	}{
		{"explicit to", "what is our spend", []string{"analyst"}, 1, "analyst", "addressed"},
		{"at-mention", "@seo how are rankings", nil, 1, "seo", "mentioned"},
		{"named in passing", "can the designer look at this", nil, 1, "designer", "named"},
		{"interest", "we should use more ai tools, what about cost", nil, 1, "analyst", "interest: cost"},
		{"person named", "arsh can you check that", nil, 0, "", "for arsh"},
		{"at-mentioned person", "@priya thoughts?", nil, 0, "", "for priya"},
		{"open", "the board review is in nine days", nil, 7, "", "open to the room"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := decideWake(tc.text, tc.to, agents, reg, people)
			if len(w.Agents) != tc.wantAgents {
				t.Fatalf("woke %s, want %d agents", who(w), tc.wantAgents)
			}
			if tc.wantOne != "" && w.Agents[0] != tc.wantOne {
				t.Fatalf("woke %s, want %s", who(w), tc.wantOne)
			}
			if w.Why != tc.wantWhy {
				t.Fatalf("why = %q, want %q", w.Why, tc.wantWhy)
			}
		})
	}
}

// The rung that makes a seven-agent room usable: asking a colleague a question
// must not summon every agent in the building.
func TestAskingAPersonWakesNobody(t *testing.T) {
	reg, agents := room7(t)
	people := map[string]bool{"priya": true, "arsh": true}
	if w := decideWake("arsh what happened with the vendor", nil, agents, reg, people); len(w.Agents) != 0 {
		t.Fatalf("asking arsh woke %s", who(w))
	}
}

// ...but a person's name does not silence an agent that was actually asked.
func TestAnAgentNamedAlongsideAPersonStillAnswers(t *testing.T) {
	reg, agents := room7(t)
	people := map[string]bool{"priya": true}
	w := decideWake("priya, ask the analyst what the spend looks like", nil, agents, reg, people)
	if len(w.Agents) != 1 || w.Agents[0] != "analyst" {
		t.Fatalf("woke %s, want just the analyst", who(w))
	}
}

// Word boundaries, so "same" never wakes "sam" and "costs" does not match "cost".
func TestMatchingIsWholeWord(t *testing.T) {
	reg, agents := room7(t)
	people := map[string]bool{"sam": true}
	if w := decideWake("we need the same thing again", nil, agents, reg, people); w.Why != "open to the room" {
		t.Fatalf("'same' matched something: %s", who(w))
	}
	if w := decideWake("analysts are expensive", nil, agents, reg, people); w.Why != "open to the room" {
		t.Fatalf("'analysts' matched the analyst: %s", who(w))
	}
}

func TestUnknownTargetFallsThroughRatherThanSilencingTheRoom(t *testing.T) {
	reg, agents := room7(t)
	w := decideWake("what should we do about the retainer", []string{"nobody-here"}, agents, reg, nil)
	if w.Why != "open to the room" || len(w.Agents) != 7 {
		t.Fatalf("an unknown target left the room silent: %s", who(w))
	}
}

// Two agents can both be wanted.
func TestSeveralCanBeWokenAtOnce(t *testing.T) {
	reg, agents := room7(t)
	w := decideWake("@analyst @seo can you two compare notes", nil, agents, reg, nil)
	if len(w.Agents) != 2 {
		t.Fatalf("woke %s, want two", who(w))
	}
}

// An email is full of words that look like topics and is not about any of them.
func TestAnAddressIsNotATopic(t *testing.T) {
	reg, agents := room7(t)
	for _, text := range []string{
		"ping me at seo@ourcompany.com if you disagree",
		"the numbers are at https://analytics.ourbrand.io/traffic",
		"forward it to press@agency.co",
	} {
		if w := decideWake(text, nil, agents, reg, nil); len(w.Agents) != 7 {
			t.Fatalf("%q woke %s; an address is not a subject", text, who(w))
		}
	}
}

// Real topics next to an address still work.
func TestATopicBesideAnAddressStillWakes(t *testing.T) {
	reg, agents := room7(t)
	w := decideWake("mail seo@ourcompany.com about the budget", nil, agents, reg, nil)
	if len(w.Agents) != 1 || w.Agents[0] != "analyst" {
		t.Fatalf("woke %s, want the analyst on 'budget'", who(w))
	}
}

// The most expensive thing the ladder can do is wake seven agents for "ok".
func TestChatterWakesNobody(t *testing.T) {
	reg, agents := room7(t)
	for _, text := range []string{"ok", "thanks", "lol", "on it", "got it", "morning everyone", "yeah agreed"} {
		w := decideWake(text, nil, agents, reg, nil)
		if len(w.Agents) != 0 || w.Why != "chatter" {
			t.Fatalf("%q woke %s", text, who(w))
		}
	}
}

// Staying quiet on a real question is worse than answering a short one, so the
// rule is narrow: a question is never chatter, and neither is a real sentence.
func TestShortQuestionsAndRealSentencesStillReachTheRoom(t *testing.T) {
	reg, agents := room7(t)
	for _, text := range []string{
		"why?",
		"is that right?",
		"the board review is in nine days",
		"ship it",   // two words, and not an acknowledgement
		"cancel that",
	} {
		if w := decideWake(text, nil, agents, reg, nil); len(w.Agents) != 7 {
			t.Fatalf("%q was treated as chatter: %s", text, who(w))
		}
	}
}
