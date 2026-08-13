package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Oryxa-Vault/oryxa/internal/connector"
	"github.com/Oryxa-Vault/oryxa/internal/events"
	"github.com/Oryxa-Vault/oryxa/internal/session"
)

func spec(name string) *connector.Spec {
	return &connector.Spec{
		Name: name, Base: "http://127.0.0.1:1",
		Turn: &connector.Step{Path: "/t"},
	}
}

func registryWith(t *testing.T, specs ...*connector.Spec) *connector.Registry {
	t.Helper()
	reg := connector.NewRegistry()
	for _, s := range specs {
		if err := reg.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// server builds enough of one to exercise the record/restore path.
func server(t *testing.T, reg *connector.Registry, log events.Store) *Server {
	t.Helper()
	return New(reg, connector.NewExecutor(), session.NewManager(reg, connector.NewExecutor(), log), log)
}

// The failure this exists to prevent: a room survives a restart and its agent
// does not, so every turn fails with "agent no longer registered" beside a
// transcript that came back intact.
func TestRegisteredAgentSurvivesARestart(t *testing.T) {
	log := events.NewMemory()
	s := server(t, connector.NewRegistry(), log)
	s.recordAgent("alice", spec("from-the-ui"))

	// A restart: a brand new registry, nothing loaded from disk.
	fresh := connector.NewRegistry()
	n, shadowed, err := RestoreAgents(log, fresh, connector.FromAPI)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("restored %d agents, want 1", n)
	}
	if len(shadowed) != 0 {
		t.Fatalf("nothing was on disk, so nothing can be shadowed: %v", shadowed)
	}
	if _, ok := fresh.Get("from-the-ui"); !ok {
		t.Fatal("the agent did not come back")
	}
}

func TestRestoreKeepsTheWholeSpec(t *testing.T) {
	log := events.NewMemory()
	s := server(t, connector.NewRegistry(), log)

	full := spec("rich")
	full.Capabilities = []string{"stream", "multi_turn"}
	full.Vars = map[string]string{"app": "demo"}
	full.Headers = map[string]string{"x-key": "{{env.K}}"}
	full.Context = []connector.ContextRule{{Key: "findings", From: connector.SourceText}}
	s.recordAgent("alice", full)

	fresh := connector.NewRegistry()
	if _, _, err := RestoreAgents(log, fresh, connector.FromAPI); err != nil {
		t.Fatal(err)
	}
	got, ok := fresh.Get("rich")
	if !ok {
		t.Fatal("missing")
	}
	if !got.Has("stream") || got.Vars["app"] != "demo" || got.Headers["x-key"] != "{{env.K}}" {
		t.Fatalf("spec came back thinner than it went in: %+v", got)
	}
	if len(got.Context) != 1 || got.Context[0].Key != "findings" {
		t.Fatalf("context rules were lost: %+v", got.Context)
	}
}

// A file connector deleted over the API must stay deleted: on restart the file
// reloads first, and the removal replays over it.
func TestRemovalOutlivesTheFileItDeleted(t *testing.T) {
	log := events.NewMemory()
	s := server(t, registryWith(t, spec("from-a-file")), log)
	s.recordAgentRemoved("alice", "from-a-file")

	reloaded := registryWith(t, spec("from-a-file")) // LoadDir at startup
	if _, _, err := RestoreAgents(log, reloaded, connector.FromAPI); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Get("from-a-file"); ok {
		t.Fatal("a deleted agent came back from the file")
	}
}

func TestLatestRegistrationWins(t *testing.T) {
	log := events.NewMemory()
	s := server(t, connector.NewRegistry(), log)

	first := spec("edited")
	first.Timeout = "1m"
	s.recordAgent("alice", first)

	second := spec("edited")
	second.Timeout = "9m"
	s.recordAgent("bob", second)

	fresh := connector.NewRegistry()
	if _, _, err := RestoreAgents(log, fresh, connector.FromAPI); err != nil {
		t.Fatal(err)
	}
	got, _ := fresh.Get("edited")
	if got.Timeout != "9m" {
		t.Fatalf("timeout = %q, want the later edit", got.Timeout)
	}
}

// The API wins a name collision with a file, and says so rather than doing it
// quietly — a file sitting there looking authoritative while something else
// wins is the kind of thing that costs an afternoon.
func TestApiOverridesAFileAndReportsIt(t *testing.T) {
	log := events.NewMemory()
	s := server(t, connector.NewRegistry(), log)
	edited := spec("shared-name")
	edited.Timeout = "7m"
	s.recordAgent("alice", edited)

	onDisk := spec("shared-name")
	onDisk.Timeout = "1m"
	reg := registryWith(t, onDisk)

	_, shadowed, err := RestoreAgents(log, reg, connector.FromAPI)
	if err != nil {
		t.Fatal(err)
	}
	if len(shadowed) != 1 || shadowed[0] != "shared-name" {
		t.Fatalf("shadowing was not reported: %v", shadowed)
	}
	got, _ := reg.Get("shared-name")
	if got.Timeout != "7m" {
		t.Fatalf("the file won; timeout = %q", got.Timeout)
	}
}

// Registrations are attributed, which is the point of putting them in the log
// rather than a table.
func TestRegistrationRecordsWhoDidIt(t *testing.T) {
	log := events.NewMemory()
	s := server(t, connector.NewRegistry(), log)
	s.recordAgent("priya", spec("x"))
	s.recordAgentRemoved("sam", "x")

	evs, err := log.Since(events.SystemStream, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[0].Actor != "priya" || evs[1].Actor != "sam" {
		t.Fatalf("actors were %q and %q", evs[0].Actor, evs[1].Actor)
	}
	if evs[0].Kind != kindAgentRegistered || evs[1].Kind != kindAgentRemoved {
		t.Fatalf("kinds were %q and %q", evs[0].Kind, evs[1].Kind)
	}
}

// The system stream is not a room. Rebuilt as one it would appear in the
// sidebar as an empty session nobody opened.
func TestSystemStreamIsNotRehydratedAsASession(t *testing.T) {
	log := events.NewMemory()
	reg := connector.NewRegistry()
	s := server(t, reg, log)
	s.recordAgent("alice", spec("x"))

	mgr := session.NewManager(reg, connector.NewExecutor(), log)
	if _, err := mgr.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	for _, sum := range mgr.List() {
		if events.Reserved(sum.ID) {
			t.Fatalf("the system stream became a session: %+v", sum)
		}
	}
	if n := len(mgr.List()); n != 0 {
		t.Fatalf("rehydrated %d sessions from a log holding only agent events", n)
	}
}

// One stale connector must not stop a server from starting: validation rules can
// tighten between versions, and refusing to boot over a stored spec that no
// longer parses trades a whole server for one agent.
func TestAnUnparseableStoredAgentIsSkippedNotFatal(t *testing.T) {
	log := events.NewMemory()
	if _, err := log.Append(events.SystemStream, kindAgentRegistered, "alice", "",
		map[string]any{"spec": map[string]any{"name": "broken"}}); err != nil { // no base, no turn
		t.Fatal(err)
	}
	s := server(t, connector.NewRegistry(), log)
	s.recordAgent("alice", spec("fine"))

	fresh := connector.NewRegistry()
	n, _, err := RestoreAgents(log, fresh, connector.FromAPI)
	if err != nil {
		t.Fatalf("one bad spec stopped the restore: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored %d, want the good one only", n)
	}
	if _, ok := fresh.Get("fine"); !ok {
		t.Fatal("the good agent was lost with the bad one")
	}
}

func TestRestoreOnAnEmptyLogIsFine(t *testing.T) {
	n, shadowed, err := RestoreAgents(events.NewMemory(), connector.NewRegistry(), connector.FromAPI)
	if err != nil || n != 0 || shadowed != nil {
		t.Fatalf("n=%d shadowed=%v err=%v", n, shadowed, err)
	}
}

func TestReservedOnlyMatchesBookkeeping(t *testing.T) {
	if !events.Reserved(events.SystemStream) {
		t.Fatal("the system stream must be reserved")
	}
	for _, id := range []string{"s_abc123", "session", ""} {
		if events.Reserved(id) {
			t.Fatalf("%q was treated as reserved", id)
		}
	}
	if !strings.HasPrefix(events.SystemStream, "_") {
		t.Fatal("the reserved prefix and the constant disagree")
	}
}

// The hole: POST /v1/agents accepts a `base` this server then fetches, so a
// caller holding the token could use it to read anything the server can reach —
// internal services, and on a cloud instance the metadata endpoint that holds
// its credentials. Registration still succeeds; the fetch is what is refused,
// because the destination is only knowable once a name has resolved.
func TestRegisteredAgentsCannotReachPrivateAddresses(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"output":"role credentials"}`))
	}))
	defer internal.Close()

	reg := connector.NewRegistry()
	log := events.NewMemory()
	exec := connector.NewExecutor()
	mgr := session.NewManager(reg, exec, log)
	// Built the ordinary way: no WithPrivateAgents.
	srv := httptest.NewServer(New(reg, exec, mgr, log).Routes())
	defer srv.Close()

	spec := map[string]any{
		"name": "forged", "base": internal.URL,
		"turn": map[string]any{"method": "POST", "path": "/",
			"response": map[string]any{"format": "json", "text": []string{"$.output"}}},
	}
	body, _ := json.Marshal(spec)
	resp, err := http.Post(srv.URL+"/v1/agents", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("registration failed with %d; the refusal belongs at fetch time", resp.StatusCode)
	}

	got, ok := reg.Get("forged")
	if !ok {
		t.Fatal("agent was not registered")
	}
	if got.Source != connector.FromAPI {
		t.Fatalf("registered agent was not marked untrusted: Source = %q", got.Source)
	}

	// And the probe, which is the reachability oracle.
	resp2, err := http.Post(srv.URL+"/v1/agents/forged/check", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out map[string]any
	json.NewDecoder(resp2.Body).Decode(&out)
	if out["reachable"] == true {
		t.Errorf("check reported an internal address as reachable: %v", out)
	}
}

// A restart must not promote what the API registered into what a file may do.
func TestRestoredAgentsStayUntrusted(t *testing.T) {
	log := events.NewMemory()
	reg := connector.NewRegistry()
	srv := New(reg, connector.NewExecutor(), session.NewManager(reg, connector.NewExecutor(), log), log)
	spec, err := connector.ParseYAML([]byte("name: later\nbase: http://example.com\nturn:\n  path: /\n"))
	if err != nil {
		t.Fatal(err)
	}
	srv.recordAgent("someone", spec)

	fresh := connector.NewRegistry()
	if _, _, err := RestoreAgents(log, fresh, connector.FromAPI); err != nil {
		t.Fatal(err)
	}
	got, ok := fresh.Get("later")
	if !ok {
		t.Fatal("agent was not restored")
	}
	if got.Source != connector.FromAPI {
		t.Errorf("a restart promoted an API agent to trusted: Source = %q", got.Source)
	}
}
