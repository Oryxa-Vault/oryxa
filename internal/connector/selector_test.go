package connector

import (
	"encoding/json"
	"reflect"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad test json: %v", err)
	}
	return v
}

func TestSelect(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		path string
		want []string
	}{
		{
			name: "nested array expansion",
			doc:  `{"content":{"parts":[{"text":"a"},{"text":"b"}]}}`,
			path: "$.content.parts[*].text",
			want: []string{"a", "b"},
		},
		{
			name: "explicit index",
			doc:  `{"choices":[{"delta":{"content":"hi"}}]}`,
			path: "$.choices[0].delta.content",
			want: []string{"hi"},
		},
		{
			name: "top level field",
			doc:  `{"output":"done"}`,
			path: "$.output",
			want: []string{"done"},
		},
		{
			name: "path without dollar",
			doc:  `{"output":"done"}`,
			path: "output",
			want: []string{"done"},
		},
		{
			name: "missing path yields nothing",
			doc:  `{"output":"done"}`,
			path: "$.nope.deeper",
			want: nil,
		},
		{
			name: "number is stringified",
			doc:  `{"n":42}`,
			path: "$.n",
			want: []string{"42"},
		},
		{
			name: "array of strings",
			doc:  `{"xs":["a","b"]}`,
			path: "$.xs[*]",
			want: []string{"a", "b"},
		},
		{
			name: "empty strings are dropped",
			doc:  `{"content":{"parts":[{"text":""},{"text":"b"}]}}`,
			path: "$.content.parts[*].text",
			want: []string{"b"},
		},
		{
			// Reasoning models interleave thinking with the answer in one
			// array; without a predicate the scratchpad ends up in the reply.
			name: "skip parts flagged as thought",
			doc:  `{"content":{"parts":[{"text":"thinking","thought":true},{"text":"answer"}]}}`,
			path: "$.content.parts[!thought].text",
			want: []string{"answer"},
		},
		{
			name: "keep only parts flagged as thought",
			doc:  `{"content":{"parts":[{"text":"thinking","thought":true},{"text":"answer"}]}}`,
			path: "$.content.parts[thought].text",
			want: []string{"thinking"},
		},
		{
			name: "thought false is treated as absent",
			doc:  `{"content":{"parts":[{"text":"a","thought":false},{"text":"b"}]}}`,
			path: "$.content.parts[!thought].text",
			want: []string{"a", "b"},
		},
		{
			name: "predicate matching nothing yields nothing",
			doc:  `{"content":{"parts":[{"text":"a","thought":true}]}}`,
			path: "$.content.parts[!thought].text",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Select(decode(t, c.doc), c.path)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Select(%q) = %#v, want %#v", c.path, got, c.want)
			}
		})
	}
}

// A fallback selector must not duplicate text the primary one already produced.
func TestSelectFirstStopsAtFirstMatch(t *testing.T) {
	doc := decode(t, `{"a":"first","b":"second"}`)
	got := SelectFirst(doc, []string{"$.a", "$.b"})
	if !reflect.DeepEqual(got, []string{"first"}) {
		t.Fatalf("got %#v, want [first]", got)
	}
	got = SelectFirst(doc, []string{"$.missing", "$.b"})
	if !reflect.DeepEqual(got, []string{"second"}) {
		t.Fatalf("fallback: got %#v, want [second]", got)
	}
}

func TestTruthy(t *testing.T) {
	doc := decode(t, `{"done":true,"not":false,"zero":0,"s":"yes"}`)
	for path, want := range map[string]bool{
		"$.done":    true,
		"$.not":     false,
		"$.zero":    false,
		"$.s":       true,
		"$.missing": false,
		"":          false,
	} {
		if got := Truthy(doc, path); got != want {
			t.Errorf("Truthy(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestRenderString(t *testing.T) {
	c := Ctx{
		Input:        "hello",
		Conversation: "s_1",
		Vars:         map[string]string{"app": "research"},
		Captures:     map[string]string{"handle": "h_9"},
	}
	cases := map[string]string{
		"{{input}}":            "hello",
		"/apps/{{app}}/x":      "/apps/research/x",
		"/apps/{{vars.app}}/x": "/apps/research/x",
		"{{handle}}":           "h_9",
		"{{conversation}}":     "s_1",
		"a {{input}} b":        "a hello b",
		"no placeholders":      "no placeholders",
	}
	for in, want := range cases {
		if got := c.RenderString(in); got != want {
			t.Errorf("RenderString(%q) = %q, want %q", in, got, want)
		}
	}
}

// An unknown name must survive rendering so a typo is visible in the request
// rather than silently becoming an empty string.
func TestRenderStringKeepsUnknownNames(t *testing.T) {
	c := Ctx{}
	if got := c.RenderString("{{typo_here}}"); got != "{{typo_here}}" {
		t.Fatalf("got %q, want the placeholder preserved", got)
	}
}

// handle falls back to the session id so specs can always reference {{handle}}.
func TestHandleFallsBackToConversation(t *testing.T) {
	c := Ctx{Conversation: "s_42"}
	if got := c.RenderString("{{handle}}"); got != "s_42" {
		t.Fatalf("got %q, want s_42", got)
	}
}

func TestRenderWalksNestedBody(t *testing.T) {
	c := Ctx{Input: "hi", Vars: map[string]string{"app": "r"}}
	body := map[string]any{
		"app_name": "{{app}}",
		"message": map[string]any{
			"parts": []any{map[string]any{"text": "{{input}}"}},
		},
	}
	out, _ := json.Marshal(c.Render(body))
	want := `{"app_name":"r","message":{"parts":[{"text":"hi"}]}}`
	if string(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
}

// Typed-event protocols (AG-UI and friends) tell an answer from reasoning by
// the value of a field, not its presence — both carry a delta.
func TestMatchesConditions(t *testing.T) {
	text := decode(t, `{"type":"TEXT_MESSAGE_CONTENT","delta":"hi"}`)
	reason := decode(t, `{"type":"REASONING_MESSAGE_CONTENT","delta":"thinking"}`)
	partial := decode(t, `{"partial":true}`)
	final := decode(t, `{"partial":false}`)

	cases := []struct {
		doc  any
		cond string
		want bool
	}{
		{text, "$.type == TEXT_MESSAGE_CONTENT", true},
		{reason, "$.type == TEXT_MESSAGE_CONTENT", false},
		{text, `$.type == "TEXT_MESSAGE_CONTENT"`, true},
		{reason, "$.type != REASONING_MESSAGE_CONTENT", false},
		{text, "$.type != REASONING_MESSAGE_CONTENT", true},
		{partial, "$.partial", true},
		{final, "$.partial", false},
		{text, "", true},
		{text, "$.missing == x", false},
	}
	for _, c := range cases {
		if got := Matches(c.doc, c.cond); got != c.want {
			t.Errorf("Matches(%q) = %v, want %v", c.cond, got, c.want)
		}
	}
}
