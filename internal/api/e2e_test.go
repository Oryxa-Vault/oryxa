package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/events"
	"github.com/oryxa/oryxa/internal/session"
)

// newOryxa builds a server that trusts API-registered agents, because every
// test here registers one pointing at an httptest server on loopback — which is
// precisely what the default refuses, and precisely what -allow-private-agents
// exists for. The refusal itself is tested in TestRegisteredAgentsCannotReach
// PrivateAddresses, against a server built the ordinary way.
func newOryxa(t *testing.T) *httptest.Server {
	t.Helper()
	reg := connector.NewRegistry()
	log := events.NewMemory()
	exec := connector.NewExecutor()
	mgr := session.NewManager(reg, exec, log)
	srv := httptest.NewServer(New(reg, exec, mgr, log).WithPrivateAgents(true).Routes())
	t.Cleanup(srv.Close)
	return srv
}

// roomSecrets remembers what each room's create call handed back, so the tests
// read the same as they did before scoping existed. The alternative — threading
// a secret through every call site — would test the plumbing rather than the
// behaviour each test is actually about.
var roomSecrets sync.Map // session id -> secret

// withRoom adds the secret for whichever room a request is aimed at.
func withRoom(req *http.Request) *http.Request {
	if id := roomIDFromPath(req.URL.Path); id != "" {
		if v, ok := roomSecrets.Load(id); ok {
			req.Header.Set(SessionHeader, v.(string))
		}
	}
	return req
}

// getRoom is http.Get carrying the room secret.
func getRoom(rawurl string) (*http.Response, error) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(withRoom(req))
}

func do(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(withRoom(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	// A create response is the only one that carries a secret; catch it here so
	// every later call to that room is admitted automatically.
	if id, _ := out["id"].(string); id != "" {
		if sec, _ := out["secret"].(string); sec != "" {
			roomSecrets.Store(id, sec)
		}
	}
	return resp.StatusCode, out
}

// waitTurn polls the session until the turn leaves the queue/running states.
// waitTurn waits for the turn that answered a given input.
//
// Not "the turn whose id this is": a turn is no longer created per input. A
// busy room coalesces, so one turn can answer several messages at once — and
// what a caller means by this is "has mine been dealt with", which the turn's
// own inputs answer directly.
func waitTurn(t *testing.T, base, sessID, inputID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, view := do(t, "GET", base+"/v1/sessions/"+sessID, nil)
		hist, _ := view["history"].([]any)
		for _, h := range hist {
			m := h.(map[string]any)
			ins, _ := m["inputs"].([]any)
			for _, raw := range ins {
				if in, ok := raw.(map[string]any); ok && in["id"] == inputID {
					return m
				}
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("input %s was never answered", inputID)
	return nil
}

// The core must be identical for two agents that look nothing like each other.
// Only the connector spec differs.
func TestEndToEndAcrossTwoAgentShapes(t *testing.T) {
	// Agent A: streams SSE, nested content shape, has an explicit open step.
	var openCalls int
	muxA := http.NewServeMux()
	muxA.HandleFunc("POST /apps/{app}/users/{user}/sessions/{sid}", func(w http.ResponseWriter, r *http.Request) {
		openCalls++
		fmt.Fprintf(w, `{"id":%q}`, "remote-"+r.PathValue("sid"))
	})
	muxA.HandleFunc("POST /run_sse", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		for _, c := range []string{"A", "B", "C"} {
			fmt.Fprintf(w, "data: {\"content\":{\"parts\":[{\"text\":%q}]}}\n\n", c)
			w.(http.Flusher).Flush()
		}
	})
	agentA := httptest.NewServer(muxA)
	defer agentA.Close()

	// Agent B: one JSON response, flat shape, no open step at all.
	agentB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprintf(w, `{"output":"echo: %v"}`, body["prompt"])
	}))
	defer agentB.Close()

	ory := newOryxa(t)

	specA := map[string]any{
		"name": "alpha", "base": agentA.URL,
		"vars":         map[string]string{"app": "research", "user": "oryxa"},
		"capabilities": []string{"stream", "multi_turn"},
		"open": map[string]any{
			"method": "POST", "path": "/apps/{{app}}/users/{{user}}/sessions/{{conversation}}",
			"body": map[string]any{}, "capture": map[string]string{"handle": "$.id"},
		},
		"turn": map[string]any{
			"method": "POST", "path": "/run_sse",
			"body": map[string]any{"session_id": "{{handle}}", "new_message": "{{input}}"},
			"response": map[string]any{
				"format": "sse", "text": []string{"$.content.parts[*].text"},
			},
		},
	}
	specB := map[string]any{
		"name": "beta", "base": agentB.URL,
		"turn": map[string]any{
			"method": "POST", "path": "/invoke",
			"body":     map[string]any{"prompt": "{{input}}"},
			"response": map[string]any{"format": "json", "text": []string{"$.output"}},
		},
	}

	for _, spec := range []map[string]any{specA, specB} {
		if code, body := do(t, "POST", ory.URL+"/v1/agents", spec); code != 201 {
			t.Fatalf("register %v: %d %v", spec["name"], code, body)
		}
	}

	t.Run("alpha streams sse", func(t *testing.T) {
		_, s := do(t, "POST", ory.URL+"/v1/sessions", map[string]string{"agent": "alpha"})
		sid := s["id"].(string)
		_, tr := do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input",
			map[string]string{"text": "go", "author": "alice"})
		got := waitTurn(t, ory.URL, sid, tr["id"].(string))
		if got["state"] != "done" {
			t.Fatalf("state = %v (%v)", got["state"], got["error"])
		}
		if got["output"] != "ABC" {
			t.Fatalf("output = %q, want ABC", got["output"])
		}
	})

	t.Run("beta returns plain json", func(t *testing.T) {
		_, s := do(t, "POST", ory.URL+"/v1/sessions", map[string]string{"agent": "beta"})
		sid := s["id"].(string)
		_, tr := do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input",
			map[string]string{"text": "what is the plan", "author": "bob"})
		got := waitTurn(t, ory.URL, sid, tr["id"].(string))
		if got["state"] != "done" {
			t.Fatalf("state = %v (%v)", got["state"], got["error"])
		}
		if got["output"] != "echo: what is the plan" {
			t.Fatalf("output = %q", got["output"])
		}
	})

	t.Run("open runs once per session, not once per turn", func(t *testing.T) {
		openCalls = 0
		_, s := do(t, "POST", ory.URL+"/v1/sessions", map[string]string{"agent": "alpha"})
		sid := s["id"].(string)
		for i := 0; i < 3; i++ {
			_, tr := do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input",
				map[string]string{"text": "go", "author": "alice"})
			waitTurn(t, ory.URL, sid, tr["id"].(string))
		}
		if openCalls != 1 {
			t.Fatalf("open called %d times, want 1", openCalls)
		}
	})
}

// Many people, one queue: turns run one at a time and stay in submission order.
func TestConcurrentInputIsSerializedInOrder(t *testing.T) {
	var mu sync.Mutex
	var concurrent, maxConcurrent int
	var order []string

	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		concurrent++
		if concurrent > maxConcurrent {
			maxConcurrent = concurrent
		}
		order = append(order, fmt.Sprint(body["prompt"]))
		mu.Unlock()

		time.Sleep(40 * time.Millisecond)

		mu.Lock()
		concurrent--
		mu.Unlock()
		fmt.Fprintf(w, `{"output":"%v"}`, body["prompt"])
	}))
	defer agent.Close()

	ory := newOryxa(t)
	do(t, "POST", ory.URL+"/v1/agents", map[string]any{
		"name": "serial", "base": agent.URL,
		"turn": map[string]any{
			"method": "POST", "path": "/run",
			"body":     map[string]any{"prompt": "{{input}}"},
			"response": map[string]any{"format": "json", "text": []string{"$.output"}},
		},
	})
	_, s := do(t, "POST", ory.URL+"/v1/sessions", map[string]string{"agent": "serial"})
	sid := s["id"].(string)

	// Three people send at once.
	var ids []string
	// The text deliberately avoids anyone's name: a message that names a person
	// in the room is now read as being for them, and no agent answers it.
	for i, who := range []string{"alice", "bob", "carol"} {
		_, tr := do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input",
			map[string]string{"text": fmt.Sprintf("line %d", i+1), "author": who})
		ids = append(ids, tr["id"].(string))
	}
	for _, id := range ids {
		if got := waitTurn(t, ory.URL, sid, id); got["state"] != "done" {
			t.Fatalf("turn %s: %v %v", id, got["state"], got["error"])
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent != 1 {
		t.Fatalf("max concurrent turns = %d, want 1 — the agent's conversation is serial", maxConcurrent)
	}
	// Coalescing means these may arrive in one turn or three. What must hold is
	// that the agent saw them in the order they were said.
	seen := strings.Join(order, "\n")
	a, b, c := strings.Index(seen, "line 1"), strings.Index(seen, "line 2"), strings.Index(seen, "line 3")
	if a < 0 || b < a || c < b {
		t.Fatalf("agent saw them out of order:\n%s", seen)
	}
}

// A queued turn can be withdrawn before it runs.
func TestWithdrawQueuedTurn(t *testing.T) {
	release := make(chan struct{})
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, `{"output":"ok"}`)
	}))
	defer agent.Close()

	ory := newOryxa(t)
	do(t, "POST", ory.URL+"/v1/agents", map[string]any{
		"name": "slow", "base": agent.URL,
		"turn": map[string]any{"method": "POST", "path": "/run",
			"body":     map[string]any{"p": "{{input}}"},
			"response": map[string]any{"format": "json", "text": []string{"$.output"}}},
	})
	_, s := do(t, "POST", ory.URL+"/v1/sessions", map[string]string{"agent": "slow"})
	sid := s["id"].(string)

	do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input", map[string]string{"text": "first"})
	_, second := do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input", map[string]string{"text": "second"})

	code, _ := do(t, "DELETE", ory.URL+"/v1/sessions/"+sid+"/input/"+second["id"].(string), nil)
	if code != 204 {
		t.Fatalf("withdraw returned %d", code)
	}
	close(release)

	_, view := do(t, "GET", ory.URL+"/v1/sessions/"+sid, nil)
	q, _ := view["queue"].([]any)
	if len(q) != 0 {
		t.Fatalf("queue = %v, want empty", q)
	}
}

// Late join must be the same code path as live streaming: subscribe, backfill,
// follow — with no duplicates and nothing missed.
func TestStreamBackfillsThenFollows(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"output":"hi"}`)
	}))
	defer agent.Close()

	ory := newOryxa(t)
	do(t, "POST", ory.URL+"/v1/agents", map[string]any{
		"name": "quick", "base": agent.URL,
		"turn": map[string]any{"method": "POST", "path": "/run",
			"body":     map[string]any{"p": "{{input}}"},
			"response": map[string]any{"format": "json", "text": []string{"$.output"}}},
	})
	_, s := do(t, "POST", ory.URL+"/v1/sessions", map[string]string{"agent": "quick"})
	sid := s["id"].(string)

	_, tr := do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input", map[string]string{"text": "one"})
	waitTurn(t, ory.URL, sid, tr["id"].(string))

	// Join late, from the very beginning.
	resp, err := getRoom(ory.URL + "/v1/sessions/" + sid + "/stream?since=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Guard the wire format a browser depends on: EventSource only fires
	// onmessage for events with no `event:` field, so naming them would break
	// every browser client silently.
	t.Run("no named event field", func(t *testing.T) {
		r, err := getRoom(ory.URL + "/v1/sessions/" + sid + "/stream?since=0")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		if strings.Contains(string(buf[:n]), "event:") {
			t.Fatalf("stream emits a named event field; EventSource.onmessage will never fire:\n%s", buf[:n])
		}
	})

	seen := make(chan []string, 1)
	go func() {
		var kinds []string
		seq := map[float64]bool{}
		finished := 0
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
				continue
			}
			n := ev["seq"].(float64)
			if seq[n] {
				kinds = append(kinds, "DUPLICATE")
				seen <- kinds
				return
			}
			seq[n] = true
			kinds = append(kinds, ev["kind"].(string))
			// Stop only after the second turn, so we prove the stream followed
			// past the backfill rather than stopping at the end of it.
			if ev["kind"] == "turn.finished" {
				finished++
				if finished == 2 {
					seen <- kinds
					return
				}
			}
		}
		seen <- kinds
	}()

	// A second turn arrives after the subscriber attached.
	time.Sleep(50 * time.Millisecond)
	_, tr2 := do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input", map[string]string{"text": "two"})
	waitTurn(t, ory.URL, sid, tr2["id"].(string))

	select {
	case kinds := <-seen:
		joined := strings.Join(kinds, ",")
		if strings.Contains(joined, "DUPLICATE") {
			t.Fatalf("stream delivered an event twice: %v", kinds)
		}
		if !strings.HasPrefix(joined, "session.created,input.submitted,turn.started") {
			t.Fatalf("backfill missing or out of order: %v", kinds)
		}
		if strings.Count(joined, "turn.finished") < 2 {
			t.Fatalf("did not follow past backfill: %v", kinds)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not deliver both turns")
	}
}

func TestCheckReportsUnmatchedSelector(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"unexpected":"shape"}`)
	}))
	defer agent.Close()

	ory := newOryxa(t)
	do(t, "POST", ory.URL+"/v1/agents", map[string]any{
		"name": "odd", "base": agent.URL,
		"turn": map[string]any{"method": "POST", "path": "/run",
			"response": map[string]any{"format": "json", "text": []string{"$.output"}}},
	})
	code, res := do(t, "POST", ory.URL+"/v1/agents/odd/check", map[string]string{"probe": "hi"})
	if code != 200 {
		t.Fatalf("check returned %d: %v", code, res)
	}
	warnings, _ := res["warnings"].([]any)
	var joined string
	for _, w := range warnings {
		joined += fmt.Sprint(w)
	}
	if !strings.Contains(joined, "no text selector matched") {
		t.Fatalf("expected a selector warning, got %v", warnings)
	}
}

func TestCheckFailsWhenAgentIsDown(t *testing.T) {
	ory := newOryxa(t)
	do(t, "POST", ory.URL+"/v1/agents", map[string]any{
		"name": "down", "base": "http://127.0.0.1:1",
		"turn": map[string]any{"method": "POST", "path": "/run"},
	})
	code, res := do(t, "POST", ory.URL+"/v1/agents/down/check", nil)
	if code != 502 {
		t.Fatalf("code = %d, want 502", code)
	}
	if res["reachable"] != false {
		t.Fatalf("reachable = %v, want false", res["reachable"])
	}
}

// One room, several agents: input fans out to each, still one turn at a time,
// and each agent keeps its own conversation handle.
func TestRoomWithSeveralAgents(t *testing.T) {
	var mu sync.Mutex
	var concurrent, maxConcurrent int
	opens := map[string]int{}

	mk := func(name string) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /open", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			opens[name]++
			mu.Unlock()
			fmt.Fprintf(w, `{"id":"%s-conv"}`, name)
		})
		mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			concurrent++
			if concurrent > maxConcurrent {
				maxConcurrent = concurrent
			}
			mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			mu.Lock()
			concurrent--
			mu.Unlock()
			fmt.Fprintf(w, `{"output":"%s says %v (conv=%v)"}`, name, body["p"], body["conv"])
		})
		return httptest.NewServer(mux)
	}

	a, b, c := mk("alpha"), mk("beta"), mk("gamma")
	defer a.Close()
	defer b.Close()
	defer c.Close()

	ory := newOryxa(t)
	for name, srv := range map[string]*httptest.Server{"alpha": a, "beta": b, "gamma": c} {
		do(t, "POST", ory.URL+"/v1/agents", map[string]any{
			"name": name, "base": srv.URL,
			"open": map[string]any{"method": "POST", "path": "/open",
				"capture": map[string]string{"handle": "$.id"}},
			"turn": map[string]any{"method": "POST", "path": "/run",
				"body":     map[string]any{"p": "{{input}}", "conv": "{{handle}}"},
				"response": map[string]any{"format": "json", "text": []string{"$.output"}}},
		})
	}

	code, s := do(t, "POST", ory.URL+"/v1/sessions",
		map[string]any{"agents": []string{"alpha", "beta", "gamma"}})
	if code != 201 {
		t.Fatalf("create returned %d: %v", code, s)
	}
	sid := s["id"].(string)
	if got := s["agents"].([]any); len(got) != 3 {
		t.Fatalf("agents = %v, want 3", got)
	}

	do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input",
		map[string]string{"text": "what is the plan", "author": "alice"})

	deadline := time.Now().Add(5 * time.Second)
	var hist []any
	for time.Now().Before(deadline) {
		_, v := do(t, "GET", ory.URL+"/v1/sessions/"+sid, nil)
		hist, _ = v["history"].([]any)
		if len(hist) == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(hist) != 3 {
		t.Fatalf("got %d turns, want one per agent", len(hist))
	}

	seen := map[string]string{}
	group := ""
	for _, h := range hist {
		m := h.(map[string]any)
		if m["state"] != "done" {
			t.Fatalf("turn %v: %v %v", m["agent"], m["state"], m["error"])
		}
		seen[m["agent"].(string)] = m["output"].(string)
		g, _ := m["group"].(string)
		if group == "" {
			group = g
		} else if g != group {
			t.Fatalf("turns from one input should share a group: %q vs %q", group, g)
		}
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, ok := seen[name]; !ok {
			t.Fatalf("no turn for %s; got %v", name, seen)
		}
		// Each agent must be addressed on its own conversation, not a shared one.
		if want := name + "-conv"; !strings.Contains(seen[name], want) {
			t.Fatalf("%s answered on the wrong handle: %q", name, seen[name])
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// Different agents must overlap: three lanes, three conversations, no
	// reason for any of them to wait on the others.
	if maxConcurrent < 2 {
		t.Fatalf("max concurrent turns = %d; agents should run in parallel", maxConcurrent)
	}
	for name, n := range opens {
		if n != 1 {
			t.Fatalf("%s opened %d times, want once per session", name, n)
		}
	}
}

// One agent failing must not stop the others answering.
func TestOneAgentFailingDoesNotSinkTheRoom(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"output":"fine"}`)
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 500)
	}))
	defer bad.Close()

	ory := newOryxa(t)
	for name, srv := range map[string]*httptest.Server{"good": good, "bad": bad} {
		do(t, "POST", ory.URL+"/v1/agents", map[string]any{
			"name": name, "base": srv.URL,
			"turn": map[string]any{"method": "POST", "path": "/run",
				"body":     map[string]any{"p": "{{input}}"},
				"response": map[string]any{"format": "json", "text": []string{"$.output"}}},
		})
	}
	_, s := do(t, "POST", ory.URL+"/v1/sessions", map[string]any{"agents": []string{"bad", "good"}})
	sid := s["id"].(string)
	do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input", map[string]string{"text": "what is the plan"})

	deadline := time.Now().Add(5 * time.Second)
	states := map[string]string{}
	for time.Now().Before(deadline) {
		_, v := do(t, "GET", ory.URL+"/v1/sessions/"+sid, nil)
		hist, _ := v["history"].([]any)
		states = map[string]string{}
		for _, h := range hist {
			m := h.(map[string]any)
			states[m["agent"].(string)] = m["state"].(string)
		}
		if len(states) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if states["good"] != "done" {
		t.Fatalf("healthy agent should still answer, got %v", states)
	}
	if states["bad"] != "failed" {
		t.Fatalf("broken agent should be marked failed, got %v", states)
	}
}

func TestUnknownAgentIsRejectedAtSessionCreate(t *testing.T) {
	ory := newOryxa(t)
	if code, _ := do(t, "POST", ory.URL+"/v1/sessions", map[string]string{"agent": "ghost"}); code != 400 {
		t.Fatalf("code = %d, want 400", code)
	}
}

// The serialization requirement is per agent, not per session. These two tests
// pin both halves: one agent never overlaps itself, and different agents do.
func TestOneAgentNeverOverlapsItself(t *testing.T) {
	var mu sync.Mutex
	var concurrent, maxConcurrent int
	var order []string

	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		concurrent++
		if concurrent > maxConcurrent {
			maxConcurrent = concurrent
		}
		order = append(order, fmt.Sprint(body["p"]))
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		concurrent--
		mu.Unlock()
		fmt.Fprintf(w, `{"output":"%v"}`, body["p"])
	}))
	defer agent.Close()

	ory := newOryxa(t)
	do(t, "POST", ory.URL+"/v1/agents", map[string]any{
		"name": "solo", "base": agent.URL,
		"turn": map[string]any{"method": "POST", "path": "/run",
			"body":     map[string]any{"p": "{{input}}"},
			"response": map[string]any{"format": "json", "text": []string{"$.output"}}},
	})
	_, s := do(t, "POST", ory.URL+"/v1/sessions", map[string]any{"agent": "solo"})
	sid := s["id"].(string)

	var ids []string
	// The text deliberately avoids anyone's name: a message that names a person
	// in the room is now read as being for them, and no agent answers it.
	for i, who := range []string{"alice", "bob", "carol"} {
		_, tr := do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input",
			map[string]string{"text": fmt.Sprintf("line %d", i+1), "author": who})
		ids = append(ids, tr["id"].(string))
	}
	for _, id := range ids {
		if got := waitTurn(t, ory.URL, sid, id); got["state"] != "done" {
			t.Fatalf("turn %s: %v %v", id, got["state"], got["error"])
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent != 1 {
		t.Fatalf("one agent ran %d turns at once; its conversation is sequential", maxConcurrent)
	}
	// One turn or three, depending on how much coalesced. What must hold is the
	// order the agent saw them in.
	seen := strings.Join(order, "\n")
	a, b, c := strings.Index(seen, "line 1"), strings.Index(seen, "line 2"), strings.Index(seen, "line 3")
	if a < 0 || b < a || c < b {
		t.Fatalf("agent saw them out of order:\n%s", seen)
	}
}

// A slow agent must not hold up a fast one. Serialising the room would make a
// question take the sum of every agent instead of the slowest.
func TestSlowAgentDoesNotBlockFastOne(t *testing.T) {
	mk := func(delay time.Duration) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			fmt.Fprint(w, `{"output":"ok"}`)
		}))
	}
	slow, fast := mk(600*time.Millisecond), mk(20*time.Millisecond)
	defer slow.Close()
	defer fast.Close()

	ory := newOryxa(t)
	for name, srv := range map[string]*httptest.Server{"slow": slow, "fast": fast} {
		do(t, "POST", ory.URL+"/v1/agents", map[string]any{
			"name": name, "base": srv.URL,
			"turn": map[string]any{"method": "POST", "path": "/run",
				"body":     map[string]any{"p": "{{input}}"},
				"response": map[string]any{"format": "json", "text": []string{"$.output"}}},
		})
	}
	_, s := do(t, "POST", ory.URL+"/v1/sessions",
		map[string]any{"agents": []string{"slow", "fast"}})
	sid := s["id"].(string)

	start := time.Now()
	do(t, "POST", ory.URL+"/v1/sessions/"+sid+"/input", map[string]string{"text": "go"})

	// Wait for the fast lane only.
	deadline := time.Now().Add(5 * time.Second)
	var fastAt time.Duration
	for time.Now().Before(deadline) {
		_, v := do(t, "GET", ory.URL+"/v1/sessions/"+sid, nil)
		hist, _ := v["history"].([]any)
		for _, h := range hist {
			if m := h.(map[string]any); m["agent"] == "fast" && m["state"] == "done" {
				fastAt = time.Since(start)
			}
		}
		if fastAt > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fastAt == 0 {
		t.Fatal("fast agent never finished")
	}
	if fastAt > 400*time.Millisecond {
		t.Fatalf("fast agent took %v; it waited on the slow one", fastAt)
	}
}

// Shared context end to end: append cannot conflict, a stale value write is
// refused with what is current, and everything survives a restart because it is
// a fold over the same log.
func TestSharedContextOverHTTP(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"output":"ok"}`)
	}))
	defer agent.Close()

	ory := newOryxa(t)
	do(t, "POST", ory.URL+"/v1/agents", map[string]any{
		"name": "a", "base": agent.URL,
		"turn": map[string]any{"method": "POST", "path": "/run",
			"response": map[string]any{"format": "json", "text": []string{"$.output"}}},
	})
	_, s := do(t, "POST", ory.URL+"/v1/sessions", map[string]any{"agent": "a"})
	sid := s["id"].(string)
	base := ory.URL + "/v1/sessions/" + sid + "/context"

	t.Run("append accumulates from several people", func(t *testing.T) {
		do(t, "POST", base+"/findings", map[string]string{"append": "postgres is fine", "author": "alice"})
		do(t, "POST", base+"/findings", map[string]string{"append": "sqlite is simpler", "author": "bob"})

		_, got := do(t, "GET", base, nil)
		entries := got["context"].([]any)
		var findings map[string]any
		for _, e := range entries {
			if m := e.(map[string]any); m["key"] == "findings" {
				findings = m
			}
		}
		if findings == nil {
			t.Fatal("findings missing")
		}
		if items := findings["items"].([]any); len(items) != 2 {
			t.Fatalf("got %d items, want 2", len(items))
		}
	})

	t.Run("stale value write is refused with what is current", func(t *testing.T) {
		code, first := do(t, "POST", base+"/plan", map[string]any{
			"value": "use postgres", "author": "alice"})
		if code != 200 {
			t.Fatalf("first write returned %d", code)
		}
		v := int64(first["version"].(float64))

		// A write at the version we saw succeeds.
		req, _ := http.NewRequest("POST", base+"/plan",
			strings.NewReader(`{"value":"postgres, WAL on","author":"bob"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", fmt.Sprint(v))
		resp, err := http.DefaultClient.Do(withRoom(req))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("write at current version returned %d", resp.StatusCode)
		}

		// A write at the stale version is refused, and says what is current.
		req2, _ := http.NewRequest("POST", base+"/plan",
			strings.NewReader(`{"value":"use sqlite","author":"carol"}`))
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("If-Match", fmt.Sprint(v))
		resp2, err := http.DefaultClient.Do(withRoom(req2))
		if err != nil {
			t.Fatal(err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != 409 {
			t.Fatalf("stale write returned %d, want 409", resp2.StatusCode)
		}
		var conflict map[string]any
		_ = json.NewDecoder(resp2.Body).Decode(&conflict)
		if conflict["current"] != "postgres, WAL on" {
			t.Fatalf("409 must carry the current value, got %v", conflict)
		}
	})

	t.Run("conflicts are recorded, not just returned", func(t *testing.T) {
		_, evs := do(t, "GET", ory.URL+"/v1/sessions/"+sid+"/events", nil)
		found := false
		for _, e := range evs["events"].([]any) {
			if e.(map[string]any)["kind"] == "conflict.rejected" {
				found = true
			}
		}
		if !found {
			t.Fatal("a rejected write left no trace in the log")
		}
	})

	t.Run("pinning marks the curated subset", func(t *testing.T) {
		if code, _ := do(t, "POST", base+"/plan/pin", map[string]any{"pinned": true}); code != 200 {
			t.Fatalf("pin returned %d", code)
		}
		_, got := do(t, "GET", base, nil)
		for _, e := range got["context"].([]any) {
			if m := e.(map[string]any); m["key"] == "plan" && m["pinned"] != true {
				t.Fatal("plan did not stay pinned")
			}
		}
	})
}
