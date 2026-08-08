package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oryxa/oryxa/client"
	"github.com/oryxa/oryxa/internal/api"
	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/events"
	"github.com/oryxa/oryxa/internal/session"
)

// Exercised against a real server rather than a stubbed transport. A client that
// compiles is not a client that works, and the shapes it decodes are the whole
// reason it exists.
func serve(t *testing.T) (*client.Client, *httptest.Server) {
	t.Helper()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprintf(w, `{"output":"heard: %v"}`, body["prompt"])
	}))
	t.Cleanup(agent.Close)

	reg := connector.NewRegistry()
	log := events.NewMemory()
	mgr := session.NewManager(reg, connector.NewExecutor(), log)
	srv := httptest.NewServer(api.New(reg, connector.NewExecutor(), mgr, log).Routes())
	t.Cleanup(srv.Close)

	c := client.New(srv.URL)
	if _, err := c.Register(context.Background(), client.Connector{
		Name: "helper", Base: agent.URL,
		Interests: []string{"budget"},
		Turn: &client.Step{
			Method: "POST", Path: "/run",
			Body:     map[string]any{"prompt": "{{input}}"},
			Response: &client.ResponseSpec{Format: "json", Text: []string{"$.output"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestClientRoundTrip(t *testing.T) {
	ctx := context.Background()
	c, _ := serve(t)

	agents, err := c.Agents(ctx)
	if err != nil || len(agents) != 1 || agents[0].Name != "helper" {
		t.Fatalf("agents = %+v, %v", agents, err)
	}
	if got := agents[0].Interests; len(got) != 1 || got[0] != "budget" {
		t.Fatalf("interests did not survive the round trip: %v", got)
	}

	room, err := c.Open(ctx, "helper")
	if err != nil {
		t.Fatal(err)
	}
	in, err := c.Say(ctx, room.ID, "alice", "what should we do about the budget")
	if err != nil {
		t.Fatal(err)
	}
	// The wake decision is the thing you cannot get any other way.
	if len(in.Wake) != 1 || in.Wake[0] != "helper" || in.Why != "interest: budget" {
		t.Fatalf("wake = %v why = %q", in.Wake, in.Why)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, err := c.Session(ctx, room.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(v.History) > 0 {
			if !strings.Contains(v.History[0].Output, "heard:") {
				t.Fatalf("output = %q", v.History[0].Output)
			}
			if len(v.History[0].Inputs) != 1 {
				t.Fatalf("turn covered %d inputs", len(v.History[0].Inputs))
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the turn never finished")
}

// A stale write must come back as something the caller can merge from, not as a
// bare 409 they have to re-read after.
func TestClientSurfacesAConflictWithWhatIsCurrent(t *testing.T) {
	ctx := context.Background()
	c, _ := serve(t)
	room, _ := c.Open(ctx, "helper")

	first, err := c.Set(ctx, room.ID, "plan", "alice", "ship friday", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Set(ctx, room.ID, "plan", "bob", "ship monday", first.Version); err != nil {
		t.Fatal(err)
	}
	_, err = c.Set(ctx, room.ID, "plan", "carol", "ship never", first.Version)

	var conflict *client.Conflict
	if !asConflict(err, &conflict) {
		t.Fatalf("stale write returned %T (%v), want *client.Conflict", err, err)
	}
	if conflict.Current != "ship monday" {
		t.Fatalf("conflict carried %q; the caller cannot merge without it", conflict.Current)
	}
}

func asConflict(err error, out **client.Conflict) bool {
	c, ok := err.(*client.Conflict)
	if ok {
		*out = c
	}
	return ok
}

func TestClientAppendAndPin(t *testing.T) {
	ctx := context.Background()
	c, _ := serve(t)
	room, _ := c.Open(ctx, "helper")

	if _, err := c.Append(ctx, room.ID, "findings", "alice", "the api is rate limited"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pin(ctx, room.ID, "findings", "alice", true); err != nil {
		t.Fatal(err)
	}
	entries, err := c.Context(ctx, room.ID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if !entries[0].Pinned || len(entries[0].Items) != 1 {
		t.Fatalf("entry = %+v", entries[0])
	}
}

// Replay and follow are the same call, which is the property worth pinning.
func TestClientStreamReplaysThenFollows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, _ := serve(t)
	room, _ := c.Open(ctx, "helper")
	if _, err := c.Say(ctx, room.ID, "alice", "said before anyone was listening"); err != nil {
		t.Fatal(err)
	}

	seen := make(chan string, 32)
	go func() {
		_ = c.Stream(ctx, room.ID, 0, func(ev client.Event) bool {
			if ev.Kind == "input.submitted" {
				var d struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(ev.Data, &d)
				seen <- d.Text
			}
			return true
		})
	}()

	if got := <-seen; got != "said before anyone was listening" {
		t.Fatalf("replay gave %q", got)
	}
	if _, err := c.Say(ctx, room.ID, "bob", "and this one after"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-seen:
		if got != "and this one after" {
			t.Fatalf("follow gave %q", got)
		}
	case <-ctx.Done():
		t.Fatal("the stream replayed but never followed")
	}
}

func TestClientCheckReturnsWarningsRatherThanAnError(t *testing.T) {
	ctx := context.Background()
	c, _ := serve(t)
	// A selector that fits nothing: reachable, answers, produces no text.
	if _, err := c.Register(ctx, client.Connector{
		Name: "mismatched", Base: "http://127.0.0.1:1",
		Turn: &client.Step{Path: "/run", Response: &client.ResponseSpec{Text: []string{"$.nope"}}},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := c.Check(ctx, "mismatched", "hello")
	if err == nil && res.OK {
		t.Fatal("an unreachable agent checked out fine")
	}
	if res.Agent != "mismatched" {
		t.Fatalf("check returned nothing usable: %+v (%v)", res, err)
	}
}
