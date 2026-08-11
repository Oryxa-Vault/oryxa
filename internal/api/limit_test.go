package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/events"
	"github.com/oryxa/oryxa/internal/session"
)

// limitedOryxa is newOryxa with a turn budget.
func limitedOryxa(t *testing.T, perRoom, total int) *httptest.Server {
	t.Helper()
	reg := connector.NewRegistry()
	log := events.NewMemory()
	exec := connector.NewExecutor()
	mgr := session.NewManager(reg, exec, log)
	srv := httptest.NewServer(New(reg, exec, mgr, log).
		WithPrivateAgents(true).
		WithTurnLimits(perRoom, total).
		Routes())
	t.Cleanup(srv.Close)
	return srv
}

func say(t *testing.T, base, sid, text string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text, "author": "someone"})
	req, _ := http.NewRequest("POST", base+"/v1/sessions/"+sid+"/input", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(withRoom(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// A turn is an agent doing work, and behind a command-line connector that is
// minutes of it. Unbounded, anyone who could reach a room could spend without
// limit.
func TestARoomRunsOutOfTurns(t *testing.T) {
	srv := limitedOryxa(t, 3, 0)
	registerStub(t, srv.URL)
	_, s := do(t, "POST", srv.URL+"/v1/sessions", map[string]any{"agents": []string{"a"}})
	sid := s["id"].(string)

	var refusedAt int
	for i := 1; i <= 10; i++ {
		if code := say(t, srv.URL, sid, fmt.Sprintf("message %d", i)); code == http.StatusTooManyRequests {
			refusedAt = i
			break
		}
	}
	if refusedAt == 0 {
		t.Fatal("a room with a budget of 3 accepted 10 messages")
	}
	// Each message wakes the one agent in the room, so the budget buys three.
	if refusedAt != 4 {
		t.Errorf("refused at message %d, want the 4th", refusedAt)
	}
}

// The wake ladder's whole point, made budgetary: a message nobody answers costs
// nothing, so "thanks" never eats a room's budget.
func TestChatterCostsNothing(t *testing.T) {
	srv := limitedOryxa(t, 3, 0)
	registerStub(t, srv.URL)
	_, s := do(t, "POST", srv.URL+"/v1/sessions", map[string]any{"agents": []string{"a"}})
	sid := s["id"].(string)

	for i := 0; i < 20; i++ {
		if code := say(t, srv.URL, sid, "thanks"); code != http.StatusAccepted {
			t.Fatalf("chatter was refused at %d: %d", i, code)
		}
	}
	// And a real message still gets through afterwards, because none was spent.
	if code := say(t, srv.URL, sid, "what is the plan"); code != http.StatusAccepted {
		t.Errorf("a real message after 20 acknowledgements returned %d", code)
	}
}

// One room cannot spend the whole server's budget, and the two limits are
// reported separately so the caller knows which one they hit.
func TestTheServerBudgetIsSeparateFromTheRoomBudget(t *testing.T) {
	srv := limitedOryxa(t, 100, 2)
	registerStub(t, srv.URL)
	_, s := do(t, "POST", srv.URL+"/v1/sessions", map[string]any{"agents": []string{"a"}})
	sid := s["id"].(string)

	var last int
	var body map[string]any
	for i := 0; i < 6; i++ {
		bodyBytes, _ := json.Marshal(map[string]string{"text": "go", "author": "x"})
		req, _ := http.NewRequest("POST", srv.URL+"/v1/sessions/"+sid+"/input", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(withRoom(req))
		if err != nil {
			t.Fatal(err)
		}
		last = resp.StatusCode
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if last == http.StatusTooManyRequests {
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatal("the server budget was never reached")
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "server") {
		t.Errorf("error should name the server budget, got %q", msg)
	}
}

// A 429 has to say when to come back, or a client can only guess and will
// usually guess by retrying immediately.
func TestRefusalSaysWhenToRetry(t *testing.T) {
	srv := limitedOryxa(t, 1, 0)
	registerStub(t, srv.URL)
	_, s := do(t, "POST", srv.URL+"/v1/sessions", map[string]any{"agents": []string{"a"}})
	sid := s["id"].(string)

	say(t, srv.URL, sid, "one")
	body, _ := json.Marshal(map[string]string{"text": "two", "author": "x"})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/sessions/"+sid+"/input", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(withRoom(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Fatal("no Retry-After header")
	}
	if n, err := strconv.Atoi(ra); err != nil || n <= 0 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", ra)
	}
}

func TestUnlimitedMeansUnlimited(t *testing.T) {
	srv := limitedOryxa(t, 0, 0)
	registerStub(t, srv.URL)
	_, s := do(t, "POST", srv.URL+"/v1/sessions", map[string]any{"agents": []string{"a"}})
	sid := s["id"].(string)

	for i := 0; i < 50; i++ {
		if code := say(t, srv.URL, sid, "go"); code != http.StatusAccepted {
			t.Fatalf("message %d refused with %d on an unlimited server", i, code)
		}
	}
}

// ---- registry ----

// Deleting an agent a live room holds leaves that room with a lane that can
// never run a turn, and nothing in the room says why. There is no recovery but
// registering it again.
func TestAnAgentInUseIsNotDeletedByAccident(t *testing.T) {
	srv := newOryxa(t)
	registerStub(t, srv.URL)
	_, s := do(t, "POST", srv.URL+"/v1/sessions", map[string]any{"agents": []string{"a"}})
	sid := s["id"].(string)

	code, body := do(t, "DELETE", srv.URL+"/v1/agents/a", nil)
	if code != http.StatusConflict {
		t.Fatalf("delete returned %d, want 409", code)
	}
	// Naming the rooms is the point: the caller cannot decide without them.
	rooms, _ := body["sessions"].([]any)
	if len(rooms) != 1 || rooms[0] != sid {
		t.Errorf("the conflict did not name the room: %v", body)
	}
	if _, ok := do(t, "GET", srv.URL+"/v1/agents/a", nil); ok["name"] != "a" {
		t.Error("the agent was removed despite the refusal")
	}

	// force is for when breaking the room is the point — an agent pointing
	// somewhere it should not be is worth that.
	if code, _ := do(t, "DELETE", srv.URL+"/v1/agents/a?force=true", nil); code != 204 {
		t.Errorf("forced delete returned %d", code)
	}
}

// A closed room is history, and history is allowed to name an agent that no
// longer exists.
func TestAClosedRoomDoesNotPinAnAgent(t *testing.T) {
	srv := newOryxa(t)
	registerStub(t, srv.URL)
	_, s := do(t, "POST", srv.URL+"/v1/sessions", map[string]any{"agents": []string{"a"}})
	sid := s["id"].(string)
	do(t, "POST", srv.URL+"/v1/sessions/"+sid+"/close", nil)

	if code, body := do(t, "DELETE", srv.URL+"/v1/agents/a", nil); code != 204 {
		t.Errorf("delete after close returned %d: %v", code, body)
	}
}

func TestAdminTokenGuardsTheRegistry(t *testing.T) {
	reg := connector.NewRegistry()
	log := events.NewMemory()
	exec := connector.NewExecutor()
	mgr := session.NewManager(reg, exec, log)
	srv := httptest.NewServer(New(reg, exec, mgr, log).
		WithPrivateAgents(true).WithAdminToken("admin-secret").Routes())
	defer srv.Close()

	spec, _ := json.Marshal(map[string]any{
		"name": "a", "base": "http://127.0.0.1:1",
		"turn": map[string]any{"method": "POST", "path": "/x"},
	})

	// Without it, registering is refused.
	resp, err := http.Post(srv.URL+"/v1/agents", "application/json", bytes.NewReader(spec))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("register without the admin token returned %d, want 403", resp.StatusCode)
	}

	// With it, it works.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/agents", bytes.NewReader(spec))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(AdminHeader, "admin-secret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 201 {
		t.Fatalf("register with the admin token returned %d", resp2.StatusCode)
	}

	// Reading the registry is not privileged: the viewer lists agents.
	list, err := http.Get(srv.URL + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	list.Body.Close()
	if list.StatusCode != 200 {
		t.Errorf("listing agents returned %d; reading is not an admin act", list.StatusCode)
	}

	// Nor is check, which probes an agent and changes nothing — putting the
	// Test Connection button behind a credential nobody in the viewer has
	// would be the wrong trade.
	probe, err := http.Post(srv.URL+"/v1/agents/a/check", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	probe.Body.Close()
	if probe.StatusCode == http.StatusForbidden {
		t.Error("check was treated as a registry write")
	}
}

func TestIsRegistryWrite(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/agents", true},
		{"DELETE", "/v1/agents/a", true},
		{"GET", "/v1/agents", false},
		{"GET", "/v1/agents/a", false},
		{"POST", "/v1/agents/a/check", false},
		{"POST", "/v1/sessions", false},
		{"POST", "/v1/sessions/s_1/input", false},
	}
	for _, c := range cases {
		r, _ := http.NewRequest(c.method, "http://x"+c.path, nil)
		if got := isRegistryWrite(r); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
