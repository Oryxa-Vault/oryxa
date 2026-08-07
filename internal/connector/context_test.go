package connector

import (
	"sort"
	"strings"
	"testing"
)

func viewCtx() Ctx {
	return Ctx{
		Input: "what next?",
		Context: ContextView{
			All:    "findings:\n- api is rate limited\n\nplan: ship friday",
			Pinned: "plan: ship friday",
			Keys: map[string]string{
				"findings": "- api is rate limited",
				"plan":     "ship friday",
			},
		},
	}
}

func TestContextSubstitutesWholeRoom(t *testing.T) {
	got := viewCtx().RenderString("{{context}}\n\n{{input}}")
	want := "findings:\n- api is rate limited\n\nplan: ship friday\n\nwhat next?"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestContextKeySubstitutesOneEntry(t *testing.T) {
	if got := viewCtx().RenderString("plan is: {{context.plan}}"); got != "plan is: ship friday" {
		t.Fatalf("got %q", got)
	}
}

func TestContextPinnedIsReserved(t *testing.T) {
	if got := viewCtx().RenderString("{{context.pinned}}"); got != "plan: ship friday" {
		t.Fatalf("got %q", got)
	}
}

// The reserved word must mean the same thing every time. A room that happens to
// contain an entry named "pinned" is unusual; a template whose meaning depends
// on whether it does would be worse than unusual.
func TestPinnedShorthandWinsOverAKeyOfThatName(t *testing.T) {
	c := viewCtx()
	c.Context.Keys["pinned"] = "an entry that happens to be called pinned"

	if got := c.RenderString("{{context.pinned}}"); got != "plan: ship friday" {
		t.Fatalf("a key named pinned shadowed the shorthand: %q", got)
	}
}

// The opposite of the rule for vars, deliberately. A key nobody has written yet
// is the normal state of a new room, not a typo, and putting literal braces in
// front of a model to report that is worse than putting nothing.
func TestMissingContextKeyRendersEmpty(t *testing.T) {
	got := viewCtx().RenderString("[{{context.nothing_here}}]")
	if got != "[]" {
		t.Fatalf("got %q, want empty substitution", got)
	}
}

func TestEmptyRoomLeavesNoBracesInThePrompt(t *testing.T) {
	var c Ctx
	c.Input = "hello"
	got := c.RenderString("{{context}}\n{{context.plan}}\n{{input}}")
	if strings.Contains(got, "{{") {
		t.Fatalf("template markers reached the prompt: %q", got)
	}
	if strings.TrimSpace(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

// A missing var still stays literal — that failure is a broken config and
// should be visible in the request.
func TestMissingVarStillStaysLiteral(t *testing.T) {
	var c Ctx
	if got := c.RenderString("{{vars.nope}}"); got != "{{vars.nope}}" {
		t.Fatalf("got %q, want the literal preserved", got)
	}
}

// Connectors splice context into whatever field their framework calls the
// prompt, which is usually nested several levels down a JSON body.
func TestContextRendersInsideANestedBody(t *testing.T) {
	body := map[string]any{
		"newMessage": map[string]any{
			"parts": []any{
				map[string]any{"text": "{{context.pinned}}"},
				map[string]any{"text": "{{input}}"},
			},
		},
	}
	out := viewCtx().Render(body).(map[string]any)
	parts := out["newMessage"].(map[string]any)["parts"].([]any)

	if got := parts[0].(map[string]any)["text"]; got != "plan: ship friday" {
		t.Fatalf("part 0 = %q", got)
	}
	if got := parts[1].(map[string]any)["text"]; got != "what next?" {
		t.Fatalf("part 1 = %q", got)
	}
}

// ---- rule validation ----

func specWith(rules ...ContextRule) *Spec {
	return &Spec{
		Name: "a", Base: "http://x", Turn: &Step{Path: "/t"},
		Context: rules,
	}
}

func TestContextRuleValidation(t *testing.T) {
	cases := []struct {
		name string
		rule ContextRule
		want string // substring of the error; empty means it must validate
	}{
		{"text source", ContextRule{Key: "notes", From: SourceText}, ""},
		{"selector source", ContextRule{Key: "n", From: "$.out"}, ""},
		{"explicit append", ContextRule{Key: "n", From: SourceText, Kind: "append"}, ""},
		{"explicit value", ContextRule{Key: "n", From: SourceText, Kind: "value"}, ""},
		{"gated", ContextRule{Key: "n", From: "$.o", When: "$.done"}, ""},
		{"pinned", ContextRule{Key: "n", From: SourceText, Pin: true}, ""},

		{"no key", ContextRule{From: SourceText}, "key is required"},
		{"blank key", ContextRule{Key: "  ", From: SourceText}, "key is required"},
		{"no from", ContextRule{Key: "n"}, "from is required"},
		{"bad kind", ContextRule{Key: "n", From: SourceText, Kind: "merge"}, "kind must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := specWith(tc.rule).Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("valid rule rejected: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("invalid rule accepted")
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Two rules on one key would race every turn and the winner would depend on
// rule order in the file, which is not something anyone should have to know.
func TestDuplicateRuleKeyIsRefused(t *testing.T) {
	err := specWith(
		ContextRule{Key: "notes", From: SourceText},
		ContextRule{Key: "notes", From: "$.other"},
	).Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("got %v, want a duplicate-key error", err)
	}
}

func TestConnectorsWithoutRulesStillValidate(t *testing.T) {
	if err := specWith().Validate(); err != nil {
		t.Fatalf("a spec with no context rules must stay valid: %v", err)
	}
}

func TestRulesRoundTripThroughYAML(t *testing.T) {
	s, err := ParseYAML([]byte(`
name: researcher
base: http://127.0.0.1:9
turn:
  path: /run
context:
  - key: findings
    from: $text
  - key: plan
    kind: value
    from: $.output.plan
    when: $.done
    pin: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Context) != 2 {
		t.Fatalf("got %d rules", len(s.Context))
	}
	if !s.Context[0].FromText() {
		t.Fatalf("rule 0 should read the turn text")
	}
	r := s.Context[1]
	if r.Kind != "value" || r.From != "$.output.plan" || r.When != "$.done" || !r.Pin {
		t.Fatalf("rule 1 parsed as %+v", r)
	}
}

// ---- what a template will actually ask the room for ----

func TestContextRefsFindsEveryBinding(t *testing.T) {
	s := &Step{
		Path:    "/threads/{{handle}}/runs",
		Headers: map[string]string{"X-Room": "{{context.plan}}"},
		Body: map[string]any{
			"system": "{{context.pinned}}",
			"messages": []any{
				map[string]any{"content": "{{context}} then {{context.findings}}"},
			},
			"unrelated": "{{input}}",
		},
	}
	got := s.ContextRefs()
	sort.Strings(got)
	want := "context,context.findings,context.pinned,context.plan"
	if strings.Join(got, ",") != want {
		t.Fatalf("refs = %q, want %q", strings.Join(got, ","), want)
	}
}

// The case that matters most: every connector shipped before shared context
// existed reads none of it, and must be charged for none of it.
func TestContextRefsIgnoresATemplateThatNeverMentionsIt(t *testing.T) {
	s := &Step{
		Path: "/run/{{handle}}",
		Body: map[string]any{"message": "{{input}}", "app": "{{vars.app}}"},
	}
	if got := s.ContextRefs(); len(got) != 0 {
		t.Fatalf("refs = %v, want none", got)
	}
}

// A template that splices the room in twice pays for it twice.
func TestContextRefsCountsRepeats(t *testing.T) {
	s := &Step{Body: map[string]any{"a": "{{context}}", "b": "{{context}}"}}
	if got := s.ContextRefs(); len(got) != 2 {
		t.Fatalf("refs = %v, want two", got)
	}
}

func TestNilStepReadsNothing(t *testing.T) {
	var s *Step
	if got := s.ContextRefs(); got != nil {
		t.Fatalf("refs = %v, want nil", got)
	}
}

// ContextRefs reads the spec while Render walks the request; if they disagreed
// about where a template can appear, the digest would miss one.
func TestContextRefsWalksWhereRenderWalks(t *testing.T) {
	body := map[string]any{
		"nested": []any{map[string]any{"deep": "{{context.findings}}"}},
	}
	s := &Step{Body: body}
	if got := s.ContextRefs(); len(got) != 1 {
		t.Fatalf("refs = %v, want the nested reference", got)
	}
	out := viewCtx().Render(body).(map[string]any)
	deep := out["nested"].([]any)[0].(map[string]any)["deep"].(string)
	if strings.Contains(deep, "{{") {
		t.Fatalf("Render left the nested reference unsubstituted: %q", deep)
	}
}

// {{agent}} is what separates several connectors that point at one runtime.
//
// This is the "many roles, one agent" shape: four connectors, four briefs, one
// process behind them. Without a per-connector value in the template they all
// address the same remote conversation, because with no open step {{handle}}
// falls back to the session id and every lane in a room shares it.
func TestAgentNameDistinguishesConnectorsOnOneRuntime(t *testing.T) {
	const tmpl = "{{conversation}}-{{agent}}"
	seen := map[string]bool{}
	for _, name := range []string{"researcher", "analyst", "critic", "writer"} {
		got := Ctx{Conversation: "s_1", Agent: name}.RenderString(tmpl)
		if seen[got] {
			t.Fatalf("%q collided with an earlier role", got)
		}
		seen[got] = true
	}
	if len(seen) != 4 {
		t.Fatalf("four roles produced %d distinct threads", len(seen))
	}
}

// The failure it prevents, stated directly: same room, no agent name, one thread.
func TestWithoutTheAgentNameRolesShareAThread(t *testing.T) {
	a := Ctx{Conversation: "s_1"}.RenderString("{{handle}}")
	b := Ctx{Conversation: "s_1"}.RenderString("{{handle}}")
	if a != b {
		t.Fatalf("premise wrong: %q != %q", a, b)
	}
	if a != "s_1" {
		t.Fatalf("handle fell back to %q, want the session id", a)
	}
}
