package connector

import (
	"os"
	"strings"
)

// ContextView is the room's shared state, already rendered as text.
//
// This package deliberately does not know how a context entry is shaped. It
// substitutes strings; sharedctx decides what an entry reads like. Keeping that
// line means the prompt format can change without touching templating, and the
// connector package keeps its one job.
type ContextView struct {
	All    string            // every entry
	Pinned string            // the curated subset
	Keys   map[string]string // one entry each, by key
}

// Ctx supplies the values a spec can reference as {{...}}.
type Ctx struct {
	Input         string
	Turn          string
	Conversation  string
	Handle        string
	CallbackURL   string
	CallbackToken string
	Vars          map[string]string
	Captures      map[string]string
	Context       ContextView
}

// lookup resolves one {{name}} reference. The second result reports whether the
// name was known at all, so unknown references can be left visible rather than
// silently becoming empty strings.
func (c Ctx) lookup(name string) (string, bool) {
	switch {
	case name == "input":
		return c.Input, true
	case name == "conversation":
		return c.Conversation, true
	case name == "turn":
		// Unique per turn. Protocols that want a fresh run/message id each time
		// (AG-UI does) have nothing else to use.
		return c.Turn, true
	case name == "handle":
		if c.Handle != "" {
			return c.Handle, true
		}
		// A handle captured by open must win over the fallback, or a spec whose
		// open step returns a remote id would silently keep addressing the
		// agent by our session id instead.
		if v, ok := c.Captures["handle"]; ok && v != "" {
			return v, true
		}
		// An agent with no open step still needs continuity: fall back to the
		// Oryxa session id so {{handle}} is always usable.
		return c.Conversation, true
	case name == "callback_url":
		return c.CallbackURL, true
	case name == "callback_token":
		return c.CallbackToken, true
	case name == "context":
		return c.Context.All, true
	case strings.HasPrefix(name, "context."):
		key := strings.TrimPrefix(name, "context.")
		// `pinned` is reserved. A room can still hold an entry by that name —
		// it appears in {{context}} and over the API — but the shorthand wins,
		// because a reserved word that sometimes means something else is worse
		// than one that always means the same thing.
		if key == "pinned" {
			return c.Context.Pinned, true
		}
		// A missing key renders empty rather than staying literal, which is the
		// opposite of the rule for vars below. The reason is that they fail
		// differently: a missing var is a broken config, while a context key
		// nobody has written yet is the normal state of a room that just
		// started. Leaving `{{context.plan}}` in the text would put braces in
		// front of the model to report a condition that isn't an error.
		return c.Context.Keys[key], true
	case strings.HasPrefix(name, "vars."):
		v, ok := c.Vars[strings.TrimPrefix(name, "vars.")]
		return v, ok
	case strings.HasPrefix(name, "env."):
		return os.Getenv(strings.TrimPrefix(name, "env.")), true
	}
	if v, ok := c.Captures[name]; ok {
		return v, true
	}
	// Bare names also resolve against vars, so `{{app}}` works as shorthand.
	if v, ok := c.Vars[name]; ok {
		return v, true
	}
	return "", false
}

// ContextRefs reports every {{context...}} reference in a step's templates, in
// the order they appear and including repeats, so a caller can tell how much of
// the room this step will actually put in a request.
//
// Reading the template rather than watching the render is exact here: nothing
// substitutes recursively, so a reference either appears in the spec or cannot
// appear in the request. It is also answerable before the request is built,
// which is what lets turn.started describe the prompt it is announcing.
//
// A nil step reads nothing, which is the honest answer for a connector that
// never mentions context: not "the room was empty" but "this one does not look".
func (s *Step) ContextRefs() []string {
	if s == nil {
		return nil
	}
	var out []string
	collect := func(str string) {
		for _, name := range refsIn(str) {
			if name == "context" || strings.HasPrefix(name, "context.") {
				out = append(out, name)
			}
		}
	}
	collect(s.Path)
	for k, v := range s.Headers {
		collect(k)
		collect(v)
	}
	walkStrings(s.Body, collect)
	return out
}

// walkStrings visits every string in a decoded JSON/YAML value, mirroring what
// Render walks so the two cannot disagree about where templates can appear.
func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case map[string]any:
		for k, val := range t {
			fn(k)
			walkStrings(val, fn)
		}
	case map[any]any:
		for k, val := range t {
			if ks, ok := k.(string); ok {
				fn(ks)
			}
			walkStrings(val, fn)
		}
	case []any:
		for _, val := range t {
			walkStrings(val, fn)
		}
	}
}

// refsIn lists the {{name}} references in s, using the same scan as
// RenderString so what is counted is exactly what would be substituted.
func refsIn(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			return out
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			return out
		}
		j += i
		out = append(out, strings.TrimSpace(s[i+2:j]))
		s = s[j+2:]
	}
}

// RenderString substitutes every {{name}} in s. Unknown names are left as-is so
// a typo shows up in the request instead of vanishing.
func (c Ctx) RenderString(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		j += i
		b.WriteString(s[:i])
		name := strings.TrimSpace(s[i+2 : j])
		if v, ok := c.lookup(name); ok {
			b.WriteString(v)
		} else {
			b.WriteString(s[i : j+2])
		}
		s = s[j+2:]
	}
}

// Render walks any decoded JSON/YAML value and renders every string in it.
func (c Ctx) Render(v any) any {
	switch t := v.(type) {
	case string:
		return c.RenderString(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[c.RenderString(k)] = c.Render(val)
		}
		return out
	case map[any]any: // yaml.v2-style maps, tolerated defensively
		out := make(map[string]any, len(t))
		for k, val := range t {
			ks, ok := k.(string)
			if !ok {
				continue
			}
			out[c.RenderString(ks)] = c.Render(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = c.Render(val)
		}
		return out
	default:
		return v
	}
}

func (c Ctx) RenderHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = c.RenderString(v)
	}
	return out
}
