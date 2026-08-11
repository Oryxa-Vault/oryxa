package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func warnText(res *CheckResult) string { return strings.Join(res.Warnings, " | ") }

// An agent that says it failed must not be reported as a healthy turn.
//
// Found against a real command-line agent whose credentials had expired: the
// turn came back carrying nothing but an authentication error, and `check`
// called it ok and warned that no text selector matched — sending someone to
// debug a selector that was working perfectly. It matched nothing because there
// was nothing to match.
func TestCheckFailsWhenTheAgentReportsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"type":"assistant","error":"authentication_failed"}`)
	}))
	defer srv.Close()

	spec := &Spec{
		Name: "sad", Base: srv.URL,
		Turn: &Step{Method: "POST", Path: "/run",
			Response: &Response{Format: "json", Error: "$.error", Text: []string{"$.message"}}},
	}
	res := NewExecutor().Check(context.Background(), spec, "hi")

	if res.OK || res.Turn.OK {
		t.Errorf("a turn carrying only an error was reported as ok")
	}
	if !strings.Contains(res.Error, "authentication_failed") {
		t.Errorf("the agent's own words did not reach the result: %q", res.Error)
	}
	if strings.Contains(warnText(res), "no text selector matched") {
		t.Errorf("blamed the selector for an error the agent reported: %v", res.Warnings)
	}
}

// An error alongside an answer is a partial answer, not a failed turn: several
// protocols report a recoverable problem mid-stream and carry on.
func TestCheckWarnsWhenAnErrorAccompaniesAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"error":"one tool timed out"}`+"\n")
		fmt.Fprint(w, `{"message":"here is the answer anyway"}`+"\n")
	}))
	defer srv.Close()

	spec := &Spec{
		Name: "partial", Base: srv.URL,
		Turn: &Step{Method: "POST", Path: "/run",
			Response: &Response{Format: "ndjson", Error: "$.error", Text: []string{"$.message"}}},
	}
	res := NewExecutor().Check(context.Background(), spec, "hi")

	if !res.Turn.OK {
		t.Errorf("a turn that answered was failed for a recoverable error")
	}
	if !strings.Contains(warnText(res), "one tool timed out") {
		t.Errorf("the error was swallowed: %v", res.Warnings)
	}
}

// Both cases below are the real failures seen against Google ADK driving a
// reasoning model. Each one passed `check` with a green turn while the answer
// was wrong, which is exactly the class these heuristics exist to catch.
func TestCheckCatchesSilentlyWrongConnectors(t *testing.T) {
	t.Run("reasoning spliced into the answer", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, p := range []map[string]any{
				{"text": "We need to answer exactly. ", "thought": true},
				{"text": "hello", "thought": false},
			} {
				b, _ := json.Marshal(map[string]any{
					"content": map[string]any{"parts": []any{p}},
				})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
		}))
		defer srv.Close()

		spec := &Spec{
			Name: "leaky", Base: srv.URL,
			Turn: &Step{Method: "POST", Path: "/run",
				Response: &Response{Format: "sse", Text: []string{"$.content.parts[*].text"}}},
		}
		res := NewExecutor().Check(context.Background(), spec, "hi")
		if !strings.Contains(warnText(res), "reasoning model") {
			t.Fatalf("expected a reasoning warning, got: %v", res.Warnings)
		}
	})

	t.Run("no warning once the selector excludes thought", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, p := range []map[string]any{
				{"text": "thinking ", "thought": true},
				{"text": "hello", "thought": false},
			} {
				b, _ := json.Marshal(map[string]any{
					"content": map[string]any{"parts": []any{p}},
				})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
		}))
		defer srv.Close()

		spec := &Spec{
			Name: "clean", Base: srv.URL,
			Turn: &Step{Method: "POST", Path: "/run",
				Response: &Response{Format: "sse", Text: []string{"$.content.parts[!thought].text"}}},
		}
		res := NewExecutor().Check(context.Background(), spec, "hi")
		if strings.Contains(warnText(res), "reasoning model") {
			t.Fatalf("selector already excludes thought; should not warn: %v", res.Warnings)
		}
	})

	t.Run("answer emitted twice", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			// deltas, then the same content again as a final aggregated message
			fmt.Fprint(w, "data: {\"partial\":true,\"text\":\"hello from adk\"}\n\n")
			fmt.Fprint(w, "data: {\"partial\":false,\"text\":\"hello from adk\"}\n\n")
		}))
		defer srv.Close()

		spec := &Spec{
			Name: "dup", Base: srv.URL,
			Turn: &Step{Method: "POST", Path: "/run",
				Response: &Response{Format: "sse", Text: []string{"$.text"}}},
		}
		res := NewExecutor().Check(context.Background(), spec, "hi")
		w := warnText(res)
		if !strings.Contains(w, "emitted twice") {
			t.Fatalf("expected a duplication warning, got: %v", res.Warnings)
		}
		if !strings.Contains(w, "`when:`") {
			t.Fatalf("expected the mixed-partial hint, got: %v", res.Warnings)
		}
	})

	t.Run("gated stream is quiet", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"partial\":true,\"text\":\"hello from adk\"}\n\n")
			fmt.Fprint(w, "data: {\"partial\":false,\"text\":\"hello from adk\"}\n\n")
		}))
		defer srv.Close()

		spec := &Spec{
			Name: "gated", Base: srv.URL,
			Turn: &Step{Method: "POST", Path: "/run",
				Response: &Response{Format: "sse", When: "$.partial", Text: []string{"$.text"}}},
		}
		res := NewExecutor().Check(context.Background(), spec, "hi")
		if w := warnText(res); strings.Contains(w, "emitted twice") || strings.Contains(w, "`when:`") {
			t.Fatalf("gated stream should be clean, got: %v", res.Warnings)
		}
	})
}

// A legitimately repetitive answer must not be mistaken for a doubled stream.
func TestDoubledIsExactOnly(t *testing.T) {
	for _, s := range []string{
		"", "short", "yes yes",
		"One two three four five.",
		"the cat sat on the mat and then the dog",
	} {
		if d := doubled(s); d != "" {
			t.Errorf("doubled(%q) = %q, want no match", s, d)
		}
	}
	if doubled("hello from adkhello from adk") == "" {
		t.Error("exact doubling should be detected")
	}
}
