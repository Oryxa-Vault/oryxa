package session

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/oryxa/oryxa/internal/connector"
)

// Who speaks.
//
// Waking and answering used to be the same act: every lane took a turn for every
// message and every turn called a model. In a room with seven agents that is
// seven answers to "morning", and the useful thing an agent could do — follow a
// conversation quietly and be worth asking later — was not expressible.
//
// A lane can now read without speaking. The decision here is what tells it
// which, and it is deliberately made of string matching rather than judgement:
// every rung is free, explainable, and recorded on the input so the room can
// show why someone answered.
//
// The rungs, in order, first match wins:
//
//	addressed   `to:` on the input, or @name in the text
//	named       the agent's own name appears
//	interested  a word the connector declared it engages on
//	someone's   the text names a person who has spoken here, and no agent
//	            matched — so this is people talking, and agents stay out
//	open        nothing matched: everyone answers, which is what the room did
//	            before any of this existed
//
// The last two are what make a room of seven usable. Without "someone's", asking
// a colleague a question summons every agent in the building.
type wake struct {
	Agents []string `json:"agents"`
	Why    string   `json:"why"`
}

func (w wake) wants(agent string) bool {
	for _, a := range w.Agents {
		if a == agent {
			return true
		}
	}
	return false
}

// decideWake works out who should answer one message.
//
// agents is everyone in the room; speakers is everyone who has said something in
// it, which is how the room knows a name belongs to a person rather than to
// nobody. Both are cheap and local — no model, no embedding, no network.
func decideWake(text string, to []string, agents []string, reg *connector.Registry, speakers map[string]bool) wake {
	if len(to) > 0 {
		var kept []string
		for _, a := range to {
			if contains(agents, a) {
				kept = append(kept, a)
			}
		}
		if len(kept) > 0 {
			return wake{Agents: kept, Why: "addressed"}
		}
	}

	words := tokens(text)
	mentioned := mentions(text)

	// @name is a stronger signal than a name in passing, so it is checked first
	// and does not have to compete with an agent whose interest also matched.
	var byMention, byName, byInterest []string
	var interest string
	for _, a := range agents {
		low := strings.ToLower(a)
		switch {
		case mentioned[low]:
			byMention = append(byMention, a)
		case words[low]:
			byName = append(byName, a)
		default:
			if spec, ok := reg.Get(a); ok {
				for _, want := range spec.Interests {
					if w := strings.ToLower(strings.TrimSpace(want)); w != "" && words[w] {
						byInterest = append(byInterest, a)
						interest = w
						break
					}
				}
			}
		}
	}
	switch {
	case len(byMention) > 0:
		return wake{Agents: byMention, Why: "mentioned"}
	case len(byName) > 0:
		return wake{Agents: byName, Why: "named"}
	}

	// A person named outranks an interest, and the ordering is load-bearing.
	// "arsh, what happened with the vendor" is a question for Arsh — an agent
	// that declared an interest in "vendor" answering it is exactly the
	// over-eagerness this rung exists to stop. Being asked by name is a stronger
	// signal than a word someone happened to use.
	for name := range speakers {
		low := strings.ToLower(name)
		if low != "" && (words[low] || mentioned[low]) {
			return wake{Why: "for " + name}
		}
	}

	if len(byInterest) > 0 {
		return wake{Agents: byInterest, Why: "interest: " + interest}
	}

	// Nothing claimed this and it is too small to be claiming anything. In a
	// room with people in it "ok", "thanks" and "sounds good" are most of the
	// traffic, and each one waking seven agents is the single most expensive
	// thing this ladder can do — measured at seven model calls for the message
	// "x". Chatter is not addressed to anyone, so nobody answers it.
	if chatter(text) {
		return wake{Why: "chatter"}
	}

	return wake{Agents: append([]string(nil), agents...), Why: "open to the room"}
}

// chatter reports a message with nothing in it to act on.
//
// Deliberately narrow: a few words, no question, and nothing anyone claimed.
// Anything longer is treated as substance and still reaches the room, because
// staying quiet on a real question is a worse failure than answering a short
// one.
// acks are messages that acknowledge rather than say anything. A closed list
// rather than a length rule, because length does not separate them: "ship it"
// and "on it" are both two words and only one of them is worth seven model
// calls. A list can only ever be incomplete, which costs a wasted turn — a
// length rule is wrong in the other direction, and silence on a real message is
// the more expensive mistake.
var acks = map[string]bool{
	"ok": true, "okay": true, "k": true, "kk": true, "yep": true, "yeah": true,
	"yes": true, "yup": true, "no": true, "nope": true, "nah": true,
	"thanks": true, "thank": true, "thx": true, "ty": true, "cheers": true,
	"lol": true, "haha": true, "np": true, "agreed": true, "agree": true,
	"noted": true, "cool": true, "nice": true, "great": true, "perfect": true,
	"hi": true, "hey": true, "hello": true, "morning": true, "afternoon": true,
	"everyone": true, "all": true, "folks": true, "team": true,
	"got": true, "it": true, "on": true, "same": true, "sure": true,
}

func chatter(text string) bool {
	if strings.Contains(text, "?") {
		return false
	}
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(fields) == 0 {
		return true
	}
	for _, f := range fields {
		if !acks[f] {
			return false
		}
	}
	return true
}

// addressish matches emails and URLs, which are full of words that look like
// topics and are not. "ping me at seo@ourcompany.com" is not a question about
// SEO, and an agent that answers it has read an address as a subject.
var addressish = regexp.MustCompile(`(?i)\b\S+@\S+\.\S+|https?://\S+|\b\S+\.(com|io|net|org|co|dev|ai)\b\S*`)

// tokens lowercases and splits on anything that is not a letter or digit, so
// "spend?" matches an interest in "spend" and "same" never matches "sam".
func tokens(s string) map[string]bool {
	out := map[string]bool{}
	s = addressish.ReplaceAllString(strings.ToLower(s), " ")
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	}) {
		out[f] = true
	}
	return out
}

// mentions pulls @handles out, which is how people actually address each other.
func mentions(s string) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < len(s); i++ {
		if s[i] != '@' {
			continue
		}
		j := i + 1
		for j < len(s) && (isWordByte(s[j])) {
			j++
		}
		if j > i+1 {
			out[strings.ToLower(s[i+1:j])] = true
		}
		i = j
	}
	return out
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' || b == '-' || b == '_'
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// WhoWakes exposes the ladder without a room, so a connector author can find out
// why their agent stayed quiet before wiring anything up.
//
// Silence is the hardest thing to debug in this design: nothing errors, nothing
// is logged at the agent, and the connector looks fine. Being able to ask the
// question directly — offline, against the files on disk — is the difference
// between a minute and an afternoon.
func WhoWakes(text string, to, agents, speakers []string, reg *connector.Registry) (woken []string, why string) {
	people := map[string]bool{}
	for _, s := range speakers {
		people[s] = true
	}
	w := decideWake(text, to, agents, reg, people)
	return w.Agents, w.Why
}
