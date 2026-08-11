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

func collect(t *testing.T, spec *Spec, tc Ctx) ([]Part, error) {
	t.Helper()
	var parts []Part
	err := NewExecutor().Turn(context.Background(), spec, tc, func(p Part) {
		parts = append(parts, p)
	})
	return parts, err
}

func textOf(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Two agents with completely different response shapes must run through the
// same executor with no code change — only a different spec.
func TestTurnAcrossDifferentAgentShapes(t *testing.T) {
	t.Run("sse with nested parts", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range []string{"Hel", "lo ", "world"} {
				fmt.Fprintf(w, "data: {\"content\":{\"parts\":[{\"text\":%q}]}}\n\n", chunk)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		spec := &Spec{
			Name: "a", Base: srv.URL,
			Turn: &Step{
				Method: "POST", Path: "/run_sse",
				Body:     map[string]any{"new_message": "{{input}}"},
				Response: &Response{Format: "sse", Text: []string{"$.content.parts[*].text"}},
			},
		}
		parts, err := collect(t, spec, Ctx{Input: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if got := textOf(parts); got != "Hello world" {
			t.Fatalf("got %q, want %q", got, "Hello world")
		}
	})

	t.Run("plain json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"output":"Hello world"}`)
		}))
		defer srv.Close()

		spec := &Spec{
			Name: "b", Base: srv.URL,
			Turn: &Step{
				Method: "POST", Path: "/run",
				Body:     map[string]any{"input": "{{input}}"},
				Response: &Response{Format: "json", Text: []string{"$.output"}},
			},
		}
		parts, err := collect(t, spec, Ctx{Input: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if got := textOf(parts); got != "Hello world" {
			t.Fatalf("got %q, want %q", got, "Hello world")
		}
	})

	t.Run("ndjson deltas", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "{\"delta\":\"Hello \"}\n{\"delta\":\"world\"}\n")
		}))
		defer srv.Close()

		spec := &Spec{
			Name: "c", Base: srv.URL,
			Turn: &Step{
				Method:   "POST",
				Response: &Response{Format: "ndjson", Text: []string{"$.delta"}},
			},
		}
		parts, err := collect(t, spec, Ctx{Input: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if got := textOf(parts); got != "Hello world" {
			t.Fatalf("got %q, want %q", got, "Hello world")
		}
	})
}

// Unrecognised payloads become opaque activity, never silently dropped.
func TestUnmatchedSelectorBecomesActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"some":{"other":"shape"}}`)
	}))
	defer srv.Close()

	spec := &Spec{
		Name: "d", Base: srv.URL,
		Turn: &Step{Response: &Response{Format: "json", Text: []string{"$.output"}}},
	}
	parts, err := collect(t, spec, Ctx{})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Kind != "activity" {
		t.Fatalf("got %#v, want one activity part", parts)
	}
}

// Non-JSON output is shown rather than discarded: an empty response is the
// failure mode that wastes an afternoon.
func TestNonJSONBecomesText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "just plain text")
	}))
	defer srv.Close()

	spec := &Spec{Name: "e", Base: srv.URL, Turn: &Step{}}
	parts, err := collect(t, spec, Ctx{})
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(parts); got != "just plain text" {
		t.Fatalf("got %q", got)
	}
}

func TestErrorSelectorAndHTTPError(t *testing.T) {
	t.Run("declared error selector", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"error":{"message":"model unavailable"}}`)
		}))
		defer srv.Close()
		spec := &Spec{
			Name: "f", Base: srv.URL,
			Turn: &Step{Response: &Response{Format: "json", Error: "$.error.message"}},
		}
		parts, _ := collect(t, spec, Ctx{})
		if len(parts) != 1 || parts[0].Kind != "error" || parts[0].Text != "model unavailable" {
			t.Fatalf("got %#v", parts)
		}
	})

	t.Run("http status becomes a turn error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", 500)
		}))
		defer srv.Close()
		spec := &Spec{Name: "g", Base: srv.URL, Turn: &Step{}}
		if _, err := collect(t, spec, Ctx{}); err == nil {
			t.Fatal("expected an error for a 500 response")
		}
	})
}

// open runs first and its captured handle must reach the turn request.
func TestOpenCapturesHandleAndTurnUsesIt(t *testing.T) {
	var sawSession string
	mux := http.NewServeMux()
	mux.HandleFunc("/open", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"remote-123"}`)
	})
	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = decodeJSON(r, &body)
		sawSession, _ = body["session_id"].(string)
		fmt.Fprint(w, `{"output":"ok"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	spec := &Spec{
		Name: "h", Base: srv.URL,
		Open: &Step{Method: "POST", Path: "/open", Capture: map[string]string{"handle": "$.id"}},
		Turn: &Step{
			Method: "POST", Path: "/run",
			Body:     map[string]any{"session_id": "{{handle}}"},
			Response: &Response{Format: "json", Text: []string{"$.output"}},
		},
	}
	e := NewExecutor()
	handle, caps, err := e.Open(context.Background(), spec, Ctx{Conversation: "s_1"})
	if err != nil {
		t.Fatal(err)
	}
	if handle != "remote-123" {
		t.Fatalf("handle = %q, want remote-123", handle)
	}
	if err := e.Turn(context.Background(), spec,
		Ctx{Input: "hi", Conversation: "s_1", Handle: handle, Captures: caps},
		func(Part) {}); err != nil {
		t.Fatal(err)
	}
	if sawSession != "remote-123" {
		t.Fatalf("turn sent session_id=%q, want remote-123", sawSession)
	}
}

func TestValidateRejectsBadSpecs(t *testing.T) {
	for name, s := range map[string]*Spec{
		"no name":     {Base: "http://x", Turn: &Step{}},
		"no base":     {Name: "a", Turn: &Step{}},
		"bad base":    {Name: "a", Base: "localhost:8000", Turn: &Step{}},
		"no turn":     {Name: "a", Base: "http://x"},
		"bad format":  {Name: "a", Base: "http://x", Turn: &Step{Response: &Response{Format: "xml"}}},
		"bad timeout": {Name: "a", Base: "http://x", Timeout: "soon", Turn: &Step{}},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// An agent sending whole messages is not an agent sending token deltas, and
// nothing in a payload says which. Concatenating two complete sentences with
// nothing between them produced "…one-line purpose.`oryxa-shim` exposes…" in a
// real room.
func TestJoinSeparatesWholeMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"text":"I'll check the repository."}`+"\n")
		fmt.Fprint(w, `{"text":"It exposes command-line agents."}`+"\n")
	}))
	defer srv.Close()

	spec := &Spec{
		Name: "discrete", Base: srv.URL,
		Turn: &Step{Method: "POST", Path: "/",
			Response: &Response{Format: "ndjson", Text: []string{"$.text"}, Join: "\n\n"}},
	}
	var got string
	var raws []string
	if err := NewExecutor().Turn(context.Background(), spec, Ctx{}, func(p Part) {
		got += p.Text
		if len(p.Raw) > 0 {
			raws = append(raws, string(p.Raw))
		}
	}); err != nil {
		t.Fatal(err)
	}
	want := "I'll check the repository.\n\nIt exposes command-line agents."
	if got != want {
		t.Errorf("assembled text:\n got %q\nwant %q", got, want)
	}
	// What the agent sent stays what it sent. join changes how the pieces read
	// once assembled, never the record of what arrived.
	for _, raw := range raws {
		if strings.Contains(raw, "\n\n") {
			t.Errorf("the separator leaked into a raw payload: %s", raw)
		}
	}
}

// The default is unchanged, because it is right for the agents that stream:
// "hel" and "lo" are one word and a separator would break every one of them.
func TestWithoutJoinDeltasStillConcatenate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, d := range []string{"hel", "lo ", "there"} {
			fmt.Fprintf(w, `{"delta":%q}`+"\n", d)
		}
	}))
	defer srv.Close()

	spec := &Spec{
		Name: "streamy", Base: srv.URL,
		Turn: &Step{Method: "POST", Path: "/",
			Response: &Response{Format: "ndjson", Text: []string{"$.delta"}}},
	}
	var got string
	if err := NewExecutor().Turn(context.Background(), spec, Ctx{}, func(p Part) {
		got += p.Text
	}); err != nil {
		t.Fatal(err)
	}
	if got != "hello there" {
		t.Errorf("got %q, want %q", got, "hello there")
	}
}

// The separator goes between parts, never before the first.
func TestJoinDoesNotLeadTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"text":"only one"}`)
	}))
	defer srv.Close()

	spec := &Spec{
		Name: "single", Base: srv.URL,
		Turn: &Step{Method: "POST", Path: "/",
			Response: &Response{Format: "json", Text: []string{"$.text"}, Join: "\n\n"}},
	}
	var got string
	_ = NewExecutor().Turn(context.Background(), spec, Ctx{}, func(p Part) { got += p.Text })
	if got != "only one" {
		t.Errorf("got %q", got)
	}
}
