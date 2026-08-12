package session

import "strings"

// Input is one thing someone said to the room.
//
// It exists separately from Turn because saying something and answering it are
// different acts, and one input is no longer one turn: a message reaches every
// lane, and a lane that was busy picks up several at once when it next speaks.
// Author is who said something, and how much that name is worth.
//
// The two travel together because a name on its own is not evidence. "trusted"
// came from a proxy that established it, "key" from a room key bound to it, and
// "claimed" from someone typing it into a box. A log that recorded only the name
// would invite everything downstream to treat those three the same.
type Author struct {
	Name   string
	Source string // trusted | key | claimed
}

// Claimed is an unproven name — someone said this is who they are.
func Claimed(name string) Author { return Author{Name: name, Source: "claimed"} }

type Input struct {
	ID     string `json:"id"`
	Seq    int    `json:"seq"` // position in the room's inbox
	Author string `json:"author"`

	// How the author was established. Absent on inputs written before this
	// existed, which is honest: it is not known rather than claimed.
	Source string `json:"source,omitempty"`

	Text string `json:"text"`

	// To is who the sender named explicitly. Empty leaves it to the room's own
	// rules, which is how most messages arrive.
	To []string `json:"to,omitempty"`

	// Wake is who this message is for, decided once when it lands and recorded
	// so a room can show why an agent spoke — or why it did not.
	Wake []string `json:"wake"`
	Why  string   `json:"why,omitempty"`

	// Withdrawn inputs stay in the inbox so positions never shift under a
	// cursor. Lanes skip them; a lane that already read one keeps its answer,
	// because withdrawing something is not un-saying it to whoever heard it.
	Withdrawn bool `json:"withdrawn,omitempty"`
}

type TurnState string

const (
	TurnQueued    TurnState = "queued"
	TurnRunning   TurnState = "running"
	TurnDone      TurnState = "done"
	TurnFailed    TurnState = "failed"
	TurnCancelled TurnState = "cancelled"
)

// Turn is one agent answering everything said since it last spoke.
type Turn struct {
	ID    string    `json:"id"`
	Agent string    `json:"agent"`
	State TurnState `json:"state"`

	// Inputs is what this turn is answering — one message in a quiet room,
	// several in a busy one.
	Inputs []Input `json:"inputs,omitempty"`

	// Author and Text are what the Inputs come to. Kept as fields rather than
	// left to clients so that a viewer written before coalescing still renders a
	// turn correctly: one message reads exactly as it always did.
	Author string `json:"author"`
	Text   string `json:"text"`

	Error  string `json:"error,omitempty"`
	Output string `json:"output,omitempty"`

	// Group ties together the turns produced by one input, so a client can show
	// one question with several answers instead of repeating it. With coalescing
	// a turn can span several groups; this carries the newest, which is the one
	// a client is rendering against.
	Group string `json:"group,omitempty"`
}

func newTurn(agent string, inputs []Input) *Turn {
	t := &Turn{
		ID:     "t_" + randHex(6),
		Agent:  agent,
		Inputs: inputs,
		Text:   RenderInputs(inputs),
	}
	if n := len(inputs); n > 0 {
		t.Author = inputs[n-1].Author
		t.Group = inputs[n-1].ID
	}
	return t
}

// RenderInputs is what {{input}} substitutes.
//
// One message renders as itself, with no name attached — the overwhelmingly
// common case, and every connector written before coalescing depends on it
// looking exactly as it did.
//
// Several render as an attributed exchange, because that is what they are. Names
// are not decoration here: "ship it" and "don't ship it" are indistinguishable
// without them, and an agent handed an unattributed run of messages cannot tell
// a question from an answer to it.
func RenderInputs(inputs []Input) string {
	switch len(inputs) {
	case 0:
		return ""
	case 1:
		return inputs[0].Text
	}
	var b strings.Builder
	for i, in := range inputs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(in.Author)
		b.WriteString(": ")
		b.WriteString(in.Text)
	}
	return b.String()
}

func copyTurns(in []*Turn) []Turn {
	out := make([]Turn, 0, len(in))
	for _, t := range in {
		c := *t
		c.Inputs = append([]Input(nil), t.Inputs...)
		out = append(out, c)
	}
	return out
}
