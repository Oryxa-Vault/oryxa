package session

import (
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
	"github.com/oryxa/oryxa/internal/sharedctx"
)

// ---- harness ----

// agentStub is an agent that records the prompt it was handed. Asserting on
// what actually arrived over the wire is the only way to know context reached
// the agent rather than merely reaching the template layer.
type agentStub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newAgent(t *testing.T, respond func(w http.ResponseWriter, prompt string)) *agentStub {
	t.Helper()
	a := &agentStub{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		a.seen = append(a.seen, body.Prompt)
		a.mu.Unlock()
		respond(w, body.Prompt)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *agentStub) prompts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func (a *agentStub) prompt(t *testing.T, n int) string {
	t.Helper()
	p := a.prompts()
	if n >= len(p) {
		t.Fatalf("wanted prompt %d, agent saw %d", n, len(p))
	}
	return p[n]
}

func replies(texts ...string) func(http.ResponseWriter, string) {
	var n int
	var mu sync.Mutex
	return func(w http.ResponseWriter, _ string) {
		mu.Lock()
		text := texts[min(n, len(texts)-1)]
		n++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"content": text})
	}
}

func sseReplies(chunks ...string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, _ string) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// gate is a one-shot barrier for tests that need to pin down the order two
// lanes do things in.
//
// The caller must `defer open()`. That is not tidiness: a handler still blocked
// when the test ends deadlocks httptest's Close, which turns a failing
// assertion into a hung run — and a suite that hangs when the code is wrong is
// worse than useless, because it hides the failure it was written to catch.
// Deferred calls run on t.Fatalf, so the barrier always opens.
func gate() (wait func(), open func()) {
	ch := make(chan struct{})
	var once sync.Once
	return func() { <-ch }, func() { once.Do(func() { close(ch) }) }
}

const defaultPrompt = "{{context}}\n---\n{{input}}"

func specTmpl(name string, a *agentStub, tmpl string, rules ...connector.ContextRule) *connector.Spec {
	return &connector.Spec{
		Name: name,
		Base: a.srv.URL,
		Turn: &connector.Step{
			Path:     "/turn",
			Body:     map[string]any{"prompt": tmpl},
			Response: &connector.Response{Text: []string{"$.content"}},
		},
		Context: rules,
	}
}

func spec(name string, a *agentStub, rules ...connector.ContextRule) *connector.Spec {
	return specTmpl(name, a, defaultPrompt, rules...)
}

func setup(t *testing.T, specs ...*connector.Spec) (*Manager, events.Store) {
	t.Helper()
	reg := connector.NewRegistry()
	for _, s := range specs {
		if err := reg.Put(s); err != nil {
			t.Fatalf("bad spec %s: %v", s.Name, err)
		}
	}
	log := events.NewMemory()
	return NewManager(reg, connector.NewExecutor(), log), log
}

func room(t *testing.T, m *Manager, agents ...string) string {
	t.Helper()
	sum, err := m.Create(agents...)
	if err != nil {
		t.Fatal(err)
	}
	return sum.ID
}

// waitTurns blocks until the room has finished n turns. Rules are applied
// before a turn enters history, so returning from here means every write that
// turn was going to make has landed.
func waitTurns(t *testing.T, m *Manager, id string, n int) []Turn {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := m.View(id)
		if !ok {
			t.Fatalf("session %s vanished", id)
		}
		if len(v.History) >= n {
			return v.History
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := m.View(id)
	t.Fatalf("timed out waiting for %d turns, have %d", n, len(v.History))
	return nil
}

func ask(t *testing.T, m *Manager, id, text string, wantTurns int) {
	t.Helper()
	if _, err := m.Submit(id, "alice", text); err != nil {
		t.Fatal(err)
	}
	waitTurns(t, m, id, wantTurns)
}

func entry(t *testing.T, m *Manager, id, key string) sharedctx.Entry {
	t.Helper()
	entries, ok := m.Context(id)
	if !ok {
		t.Fatalf("no session %s", id)
	}
	var have []string
	for _, e := range entries {
		if e.Key == key {
			return e
		}
		have = append(have, e.Key)
	}
	t.Fatalf("no context entry %q; room has %v", key, have)
	return sharedctx.Entry{}
}

func noEntry(t *testing.T, m *Manager, id, key string) {
	t.Helper()
	entries, _ := m.Context(id)
	for _, e := range entries {
		if e.Key == key {
			t.Fatalf("context %q exists with %+v, want nothing", key, e)
		}
	}
}

func items(e sharedctx.Entry) []string {
	var out []string
	for _, it := range e.Items {
		out = append(out, it.Text)
	}
	return out
}

func kinds(t *testing.T, log events.Store, id string) []string {
	t.Helper()
	evs, err := log.Since(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func countKind(t *testing.T, log events.Store, id, kind string) int {
	t.Helper()
	n := 0
	for _, k := range kinds(t, log, id) {
		if k == kind {
			n++
		}
	}
	return n
}

// ---- inbound: the room reaches the agent ----

func TestAgentSeesWhatAHumanWrote(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, _ := setup(t, spec("a", a))
	id := room(t, m, "a")

	if _, err := m.SetContext(id, "plan", "alice", "ship v1 by friday", 0); err != nil {
		t.Fatal(err)
	}
	ask(t, m, id, "what next?", 1)

	if got := a.prompt(t, 0); !strings.Contains(got, "plan: ship v1 by friday") {
		t.Fatalf("the room did not reach the agent:\n%s", got)
	}
}

// The whole point of the framework, in one test: two agents that know nothing
// about each other, and the second one works from what the first found.
func TestAgentSeesWhatAnotherAgentWrote(t *testing.T) {
	researcher := newAgent(t, replies("the api is rate limited"))
	writer := newAgent(t, replies("noted"))

	m, _ := setup(t,
		spec("researcher", researcher, connector.ContextRule{Key: "findings", From: connector.SourceText}),
		spec("writer", writer),
	)
	id := room(t, m, "researcher", "writer")

	ask(t, m, id, "what did you find?", 2)
	ask(t, m, id, "write it up", 4)

	got := writer.prompt(t, 1)
	if !strings.Contains(got, "the api is rate limited") {
		t.Fatalf("the writer never saw the researcher's finding:\n%s", got)
	}
}

func TestEmptyRoomSendsNoTemplateMarkers(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, _ := setup(t, specTmpl("a", a, "{{context}}\n{{context.plan}}\n{{input}}"))
	id := room(t, m, "a")

	ask(t, m, id, "hello", 1)

	got := a.prompt(t, 0)
	if strings.Contains(got, "{{") {
		t.Fatalf("template markers reached the agent: %q", got)
	}
	if strings.TrimSpace(got) != "hello" {
		t.Fatalf("empty room added noise to the prompt: %q", got)
	}
}

// An agent is asked a question against the room as it stood when the question
// was handed over. Another lane finishing mid-flight must not rewrite history
// underneath it.
func TestContextIsSnapshotAtTurnStart(t *testing.T) {
	slowStarted, slowHasPrompt := gate()
	waitRelease, release := gate()
	defer release()

	slow := newAgent(t, func(w http.ResponseWriter, _ string) {
		slowHasPrompt()
		waitRelease()
		_ = json.NewEncoder(w).Encode(map[string]any{"content": "slow done"})
	})
	fast := newAgent(t, func(w http.ResponseWriter, _ string) {
		slowStarted() // do not answer until the slow lane has its prompt in hand
		_ = json.NewEncoder(w).Encode(map[string]any{"content": "FRESH FINDING"})
	})

	m, _ := setup(t,
		spec("slow", slow),
		spec("fast", fast, connector.ContextRule{Key: "findings", From: connector.SourceText}),
	)
	id := room(t, m, "slow", "fast")

	if _, err := m.Submit(id, "alice", "go"); err != nil {
		t.Fatal(err)
	}
	waitTurns(t, m, id, 1) // the fast lane finished and its rule has been applied
	if got := items(entry(t, m, id, "findings")); len(got) != 1 {
		t.Fatalf("expected the fast agent to have written, got %v", got)
	}
	release()
	waitTurns(t, m, id, 2)

	if got := slow.prompt(t, 0); strings.Contains(got, "FRESH FINDING") {
		t.Fatalf("a write during the turn leaked into a prompt issued before it:\n%s", got)
	}
}

// ---- outbound: the agent reaches the room ----

func TestTextRuleAppendsEveryTurn(t *testing.T) {
	a := newAgent(t, replies("first finding", "second finding"))
	m, _ := setup(t, spec("a", a, connector.ContextRule{Key: "findings", From: connector.SourceText}))
	id := room(t, m, "a")

	ask(t, m, id, "one", 1)
	ask(t, m, id, "two", 2)

	e := entry(t, m, id, "findings")
	if e.Kind != sharedctx.KindAppend {
		t.Fatalf("kind = %q, want append by default", e.Kind)
	}
	want := []string{"first finding", "second finding"}
	if got := items(e); !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, it := range e.Items {
		if it.By != "a" {
			t.Fatalf("attributed to %q, want the agent name", it.By)
		}
	}
}

func TestValueRuleOverwrites(t *testing.T) {
	a := newAgent(t, replies("plan v1", "plan v2"))
	m, _ := setup(t, spec("a", a,
		connector.ContextRule{Key: "plan", Kind: "value", From: connector.SourceText}))
	id := room(t, m, "a")

	ask(t, m, id, "one", 1)
	first := entry(t, m, id, "plan").Version
	ask(t, m, id, "two", 2)

	e := entry(t, m, id, "plan")
	if e.Kind != sharedctx.KindValue {
		t.Fatalf("kind = %q", e.Kind)
	}
	if e.Value != "plan v2" {
		t.Fatalf("value = %q, want the newer one", e.Value)
	}
	if e.Version <= first {
		t.Fatalf("version did not advance: %d then %d", first, e.Version)
	}
}

func TestSelectorRuleReadsStructuredOutput(t *testing.T) {
	a := newAgent(t, func(w http.ResponseWriter, _ string) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": "here is the prose",
			"facts":   []string{"api is rate limited", "cache hit rate is 40%"},
		})
	})
	m, _ := setup(t, spec("a", a, connector.ContextRule{Key: "facts", From: "$.facts[*]"}))
	id := room(t, m, "a")

	ask(t, m, id, "go", 1)

	want := []string{"api is rate limited", "cache hit rate is 40%"}
	if got := items(entry(t, m, id, "facts")); !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A streaming agent emits its answer as a growing prefix. Without a gate the
// room fills with every prefix of it — the same failure `when` was added to
// response parsing to fix, appearing again one layer over.
func TestWhenGatesWhatReachesTheRoom(t *testing.T) {
	chunks := []string{
		`{"answer":"api","done":false}`,
		`{"answer":"api is","done":false}`,
		`{"answer":"api is rate limited","done":true}`,
	}

	t.Run("ungated writes every prefix", func(t *testing.T) {
		a := newAgent(t, sseReplies(chunks...))
		s := spec("a", a, connector.ContextRule{Key: "notes", From: "$.answer"})
		s.Turn.Response = &connector.Response{Format: "sse", Text: []string{"$.answer"}}
		m, _ := setup(t, s)
		id := room(t, m, "a")

		ask(t, m, id, "go", 1)

		if got := items(entry(t, m, id, "notes")); len(got) != 3 {
			t.Fatalf("got %d items, want 3 — the premise of this test is wrong: %v", len(got), got)
		}
	})

	t.Run("gated writes only the final", func(t *testing.T) {
		a := newAgent(t, sseReplies(chunks...))
		s := spec("a", a, connector.ContextRule{Key: "notes", From: "$.answer", When: "$.done"})
		s.Turn.Response = &connector.Response{Format: "sse", Text: []string{"$.answer"}}
		m, _ := setup(t, s)
		id := room(t, m, "a")

		ask(t, m, id, "go", 1)

		want := []string{"api is rate limited"}
		if got := items(entry(t, m, id, "notes")); !equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

func TestPinRuleMarksTheEntryAndCuratesThePrompt(t *testing.T) {
	pinner := newAgent(t, replies("the decision"))
	reader := newAgent(t, replies("ok"))

	m, _ := setup(t,
		spec("pinner", pinner,
			connector.ContextRule{Key: "decision", Kind: "value", From: connector.SourceText, Pin: true}),
		specTmpl("reader", reader, "{{context.pinned}}\n---\n{{input}}"),
	)
	id := room(t, m, "pinner", "reader")

	// Noise the pinned view must exclude.
	if _, err := m.AppendContext(id, "chatter", "alice", "irrelevant"); err != nil {
		t.Fatal(err)
	}
	ask(t, m, id, "decide", 2)
	ask(t, m, id, "read", 4)

	if e := entry(t, m, id, "decision"); !e.Pinned {
		t.Fatalf("rule did not pin the entry: %+v", e)
	}
	got := reader.prompt(t, 1)
	if !strings.Contains(got, "the decision") {
		t.Fatalf("pinned entry missing from the curated prompt:\n%s", got)
	}
	if strings.Contains(got, "irrelevant") {
		t.Fatalf("unpinned entry leaked into the curated prompt:\n%s", got)
	}
}

// Pinning is idempotent. A rule with pin: true runs every turn, and re-pinning
// an already-pinned entry would put a meaningless event in the log each time.
func TestPinIsNotRepeatedEveryTurn(t *testing.T) {
	a := newAgent(t, replies("one", "two", "three"))
	m, log := setup(t, spec("a", a,
		connector.ContextRule{Key: "plan", Kind: "value", From: connector.SourceText, Pin: true}))
	id := room(t, m, "a")

	ask(t, m, id, "1", 1)
	ask(t, m, id, "2", 2)
	ask(t, m, id, "3", 3)

	if n := countKind(t, log, id, "context.pinned"); n != 1 {
		t.Fatalf("emitted %d pin events across 3 turns, want 1", n)
	}
}

// ---- what must not reach the room ----

func TestFailedTurnWritesNothing(t *testing.T) {
	a := newAgent(t, func(w http.ResponseWriter, _ string) {
		http.Error(w, "boom", 500)
	})
	m, _ := setup(t, spec("a", a, connector.ContextRule{Key: "findings", From: connector.SourceText}))
	id := room(t, m, "a")

	h := func() []Turn { ask(t, m, id, "go", 1); v, _ := m.View(id); return v.History }()
	if h[0].State != TurnFailed {
		t.Fatalf("turn state = %q, want failed", h[0].State)
	}
	// Half an answer recorded as a finding is worse than no finding: the next
	// agent cannot tell the difference.
	noEntry(t, m, id, "findings")
}

func TestCancelledTurnWritesNothing(t *testing.T) {
	waitStarted, started := gate()
	waitRelease, release := gate()
	defer release()

	a := newAgent(t, func(w http.ResponseWriter, _ string) {
		started()
		waitRelease()
		_ = json.NewEncoder(w).Encode(map[string]any{"content": "too late"})
	})
	m, _ := setup(t, spec("a", a, connector.ContextRule{Key: "findings", From: connector.SourceText}))
	id := room(t, m, "a")

	if _, err := m.Submit(id, "alice", "go"); err != nil {
		t.Fatal(err)
	}
	waitStarted()
	if err := m.Cancel(id, "alice"); err != nil {
		t.Fatal(err)
	}
	waitTurns(t, m, id, 1)
	release()

	v, _ := m.View(id)
	if v.History[0].State != TurnCancelled {
		t.Fatalf("turn state = %q, want cancelled", v.History[0].State)
	}
	noEntry(t, m, id, "findings")
}

func TestEmptyOutputWritesNothing(t *testing.T) {
	a := newAgent(t, replies("   "))
	m, _ := setup(t, spec("a", a, connector.ContextRule{Key: "findings", From: connector.SourceText}))
	id := room(t, m, "a")

	ask(t, m, id, "go", 1)
	noEntry(t, m, id, "findings")
}

func TestConnectorWithoutRulesChangesNothing(t *testing.T) {
	a := newAgent(t, replies("hello"))
	m, log := setup(t, spec("a", a))
	id := room(t, m, "a")

	ask(t, m, id, "go", 1)

	if entries, _ := m.Context(id); len(entries) != 0 {
		t.Fatalf("a connector with no rules wrote %v", entries)
	}
	for _, k := range kinds(t, log, id) {
		if strings.HasPrefix(k, "context.") {
			t.Fatalf("unexpected %s event", k)
		}
	}
}

// A rule appending to a key someone already made a value cannot work. The turn
// still succeeded, so it must still be delivered — but the refusal has to be
// visible rather than silent.
func TestWrongKindIsRejectedWithoutFailingTheTurn(t *testing.T) {
	a := newAgent(t, replies("an answer"))
	m, log := setup(t, spec("a", a, connector.ContextRule{Key: "notes", From: connector.SourceText}))
	id := room(t, m, "a")

	if _, err := m.SetContext(id, "notes", "alice", "a value, not a list", 0); err != nil {
		t.Fatal(err)
	}
	ask(t, m, id, "go", 1)

	v, _ := m.View(id)
	if v.History[0].State != TurnDone {
		t.Fatalf("turn state = %q, want done", v.History[0].State)
	}
	if v.History[0].Output != "an answer" {
		t.Fatalf("output = %q, the answer must still be delivered", v.History[0].Output)
	}
	if e := entry(t, m, id, "notes"); e.Value != "a value, not a list" {
		t.Fatalf("entry was mutated: %+v", e)
	}
	if countKind(t, log, id, "context.rejected") != 1 {
		t.Fatalf("refusal was silent; log holds %v", kinds(t, log, id))
	}
}

// ---- concurrency ----

// Eight lanes finish at once and all write the same key. Every finding must
// survive: this is the shape the lost-update bug took the first time, and the
// append path is the one most rules use.
func TestParallelLanesAppendWithoutLoss(t *testing.T) {
	const n = 8
	var specs []*connector.Spec
	var names []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("agent%d", i)
		a := newAgent(t, replies(fmt.Sprintf("finding from %s", name)))
		specs = append(specs, spec(name, a, connector.ContextRule{Key: "findings", From: connector.SourceText}))
		names = append(names, name)
	}
	m, _ := setup(t, specs...)
	id := room(t, m, names...)

	ask(t, m, id, "everyone report", n)

	got := items(entry(t, m, id, "findings"))
	if len(got) != n {
		t.Fatalf("got %d findings from %d agents: %v", len(got), n, got)
	}
	seen := map[string]bool{}
	for _, g := range got {
		if seen[g] {
			t.Fatalf("duplicate finding %q in %v", g, got)
		}
		seen[g] = true
	}
	for _, name := range names {
		if !seen["finding from "+name] {
			t.Fatalf("%s's finding was lost: %v", name, got)
		}
	}
}

// Value rules from parallel lanes overwrite by design — the agent holds no
// version and never saw one. What must hold is that the store stays coherent
// and every write is still in the log, so nothing is actually lost.
func TestParallelValueRulesStayCoherent(t *testing.T) {
	const n = 6
	var specs []*connector.Spec
	var names []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("agent%d", i)
		a := newAgent(t, replies(fmt.Sprintf("plan from %s", name)))
		specs = append(specs, spec(name, a,
			connector.ContextRule{Key: "plan", Kind: "value", From: connector.SourceText}))
		names = append(names, name)
	}
	m, log := setup(t, specs...)
	id := room(t, m, names...)

	ask(t, m, id, "everyone plan", n)

	e := entry(t, m, id, "plan")
	if !strings.HasPrefix(e.Value, "plan from agent") {
		t.Fatalf("value is not one of the writes: %q", e.Value)
	}
	if e.By == "" {
		t.Fatalf("winning write is unattributed: %+v", e)
	}
	if got := countKind(t, log, id, "context.set"); got != n {
		t.Fatalf("log holds %d writes, want all %d", got, n)
	}
}

// ---- durability ----

func TestRuleWritesSurviveRestart(t *testing.T) {
	a := newAgent(t, replies("a durable finding"))
	s := spec("a", a,
		connector.ContextRule{Key: "findings", From: connector.SourceText},
		connector.ContextRule{Key: "plan", Kind: "value", From: connector.SourceText, Pin: true},
	)

	reg := connector.NewRegistry()
	if err := reg.Put(s); err != nil {
		t.Fatal(err)
	}
	log := events.NewMemory()

	m := NewManager(reg, connector.NewExecutor(), log)
	id := room(t, m, "a")
	ask(t, m, id, "go", 1)

	// A different Manager over the same log: what a restart looks like.
	restarted := NewManager(reg, connector.NewExecutor(), log)
	if _, err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}

	if got := items(entry(t, restarted, id, "findings")); !equal(got, []string{"a durable finding"}) {
		t.Fatalf("append entry did not replay: %v", got)
	}
	e := entry(t, restarted, id, "plan")
	if e.Value != "a durable finding" {
		t.Fatalf("value entry did not replay: %+v", e)
	}
	if !e.Pinned {
		t.Fatalf("pin did not replay: %+v", e)
	}
}

// A rule-written entry must be reachable by the next turn after a restart, not
// merely present in a snapshot — the prompt path and the replay path have to
// agree.
func TestReplayedContextReachesTheNextPrompt(t *testing.T) {
	a := newAgent(t, replies("earlier finding", "later answer"))
	s := spec("a", a, connector.ContextRule{Key: "findings", From: connector.SourceText})

	reg := connector.NewRegistry()
	if err := reg.Put(s); err != nil {
		t.Fatal(err)
	}
	log := events.NewMemory()

	m := NewManager(reg, connector.NewExecutor(), log)
	id := room(t, m, "a")
	ask(t, m, id, "one", 1)

	restarted := NewManager(reg, connector.NewExecutor(), log)
	if _, err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	ask(t, restarted, id, "two", 2)

	if got := a.prompt(t, 1); !strings.Contains(got, "earlier finding") {
		t.Fatalf("context from before the restart never reached the agent:\n%s", got)
	}
}

// The turn Submit returns is the body of the submit response. It must be copied
// while Submit still owns the struct exclusively: enqueue publishes it to the
// lane goroutine, which marks it running immediately, and a read after that
// point builds an API response out of a half-written value.
//
// Only fails under -race, which is why it hammers rather than asserts.
func TestSubmitReturnsAStableTurn(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, _ := setup(t, spec("a", a))
	id := room(t, m, "a")

	const n = 50
	for i := 0; i < n; i++ {
		got, err := m.Submit(id, "alice", "go")
		if err != nil {
			t.Fatal(err)
		}
		// Whatever the lane does next, the caller was handed a queued turn.
		if got.State != TurnQueued {
			t.Fatalf("submit %d returned state %q, want queued", i, got.State)
		}
		if got.ID == "" || got.Group == "" {
			t.Fatalf("submit %d returned an incomplete turn: %+v", i, got)
		}
	}
	waitTurns(t, m, id, n)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- what the log says the agent was shown ----

// startedContext reads the context digest off the nth turn.started.
func startedContext(t *testing.T, log events.Store, id string, n int) contextDigest {
	t.Helper()
	evs, err := log.Since(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range evs {
		if e.Kind != "turn.started" {
			continue
		}
		if seen == n {
			var d struct {
				Context contextDigest `json:"context"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("turn.started data: %v", err)
			}
			return d.Context
		}
		seen++
	}
	t.Fatalf("no turn.started #%d in %v", n, kinds(t, log, id))
	return contextDigest{}
}

// The log records every part an agent sends back; the digest is the other half
// of that. It has to describe the snapshot the agent was actually handed — a
// digest that drifted from the prompt would be worse than none, because it
// would be believed.
func TestTurnStartedRecordsTheContextTheAgentSaw(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, log := setup(t, spec("a", a))
	id := room(t, m, "a")

	if _, err := m.SetContext(id, "plan", "alice", "ship v1 by friday", 0); err != nil {
		t.Fatal(err)
	}
	ask(t, m, id, "what next?", 1)

	d := startedContext(t, log, id, 0)

	// defaultPrompt is "{{context}}\n---\n{{input}}", so everything before the
	// separator is exactly what the agent received as context.
	rendered, _, _ := strings.Cut(a.prompt(t, 0), "\n---\n")
	if d.Chars != len(rendered) {
		t.Fatalf("log claims %d chars of context, agent got %d:\n%q", d.Chars, len(rendered), rendered)
	}
	if got := strings.Join(d.Keys, ","); got != "plan" {
		t.Fatalf("keys = %q, want %q", got, "plan")
	}
	if d.Version == 0 {
		t.Fatal("version is 0; a turn that saw a written room must name the write it saw")
	}
}

func TestEmptyRoomIsRecordedAsEmpty(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, log := setup(t, spec("a", a))
	id := room(t, m, "a")

	ask(t, m, id, "go", 1)

	d := startedContext(t, log, id, 0)
	if d.Chars != 0 || len(d.Keys) != 0 || d.Version != 0 {
		t.Fatalf("empty room recorded as %+v, want a zero digest", d)
	}
}

// The digest is what makes context growth visible. A room that quietly outgrew
// the model's window looks, in every other event, exactly like one that did not:
// the turn succeeds and the agent simply returns less than it should.
func TestContextDigestGrowsWithTheRoom(t *testing.T) {
	a := newAgent(t, replies("a finding"))
	m, log := setup(t, spec("a", a, connector.ContextRule{Key: "findings", From: connector.SourceText}))
	id := room(t, m, "a")

	ask(t, m, id, "one", 1)
	ask(t, m, id, "two", 2)
	ask(t, m, id, "three", 3)

	var chars []int
	var versions []int64
	for i := 0; i < 3; i++ {
		d := startedContext(t, log, id, i)
		chars = append(chars, d.Chars)
		versions = append(versions, d.Version)
	}
	if chars[0] != 0 {
		t.Fatalf("first turn saw %d chars, want an empty room", chars[0])
	}
	if !(chars[0] < chars[1] && chars[1] < chars[2]) {
		t.Fatalf("context size per turn = %v, want strict growth", chars)
	}
	// Each turn names a newer write than the one before, so the digest points at
	// a distinct fold of the log rather than a running total.
	if !(versions[0] < versions[1] && versions[1] < versions[2]) {
		t.Fatalf("versions = %v, want each turn to name a newer write", versions)
	}
}

// ---- bounding the prompt without losing the room ----

// The bound is a rendering decision, not a fold decision. If it trimmed the
// store instead, a restart would replay the full log and un-trim it, so what an
// agent saw would depend on when the server last came up.
func TestLongRoomIsTrimmedInThePromptButKeptInTheStore(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, log := setup(t, spec("a", a))
	id := room(t, m, "a")

	const total = sharedctx.MaxItemsPerEntry + 6
	for i := 1; i <= total; i++ {
		if _, err := m.AppendContext(id, "findings", "alice", fmt.Sprintf("finding %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	ask(t, m, id, "what do we know?", 1)

	prompt := a.prompt(t, 0)
	if !strings.Contains(prompt, "(6 earlier items not shown)") {
		t.Fatalf("prompt did not admit it was trimmed:\n%s", prompt)
	}
	if strings.Contains(prompt, "finding 1\n") {
		t.Fatalf("oldest finding reached the prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, fmt.Sprintf("finding %d", total)) {
		t.Fatalf("newest finding missing from prompt:\n%s", prompt)
	}

	// The room itself is untouched: the API and the log still hold everything.
	if got := len(entry(t, m, id, "findings").Items); got != total {
		t.Fatalf("store holds %d items, want all %d", got, total)
	}
	if d := startedContext(t, log, id, 0); d.Elided != 6 {
		t.Fatalf("digest recorded elided=%d, want 6", d.Elided)
	}
}

// A room inside the bound must be reported as untrimmed, or `elided` stops
// meaning anything.
func TestShortRoomRecordsNoElision(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, log := setup(t, spec("a", a))
	id := room(t, m, "a")

	if _, err := m.AppendContext(id, "findings", "alice", "the api is rate limited"); err != nil {
		t.Fatal(err)
	}
	ask(t, m, id, "go", 1)

	if d := startedContext(t, log, id, 0); d.Elided != 0 {
		t.Fatalf("elided=%d for a one-item room", d.Elided)
	}
	if got := a.prompt(t, 0); strings.Contains(got, "not shown") {
		t.Fatalf("short room marked as trimmed:\n%s", got)
	}
}

// ---- charging a turn for what it read, not for what the room held ----

// Every connector written before shared context existed reads none of it.
// Quoting the room's size as their prompt size would make each of them look
// like it was about to overrun a window it never touches.
func TestConnectorThatIgnoresContextIsChargedNothing(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, log := setup(t, specTmpl("a", a, "{{input}}"))
	id := room(t, m, "a")

	if _, err := m.AppendContext(id, "findings", "alice", strings.Repeat("x", 500)); err != nil {
		t.Fatal(err)
	}
	ask(t, m, id, "go", 1)

	d := startedContext(t, log, id, 0)
	if d.Chars != 0 || len(d.Reads) != 0 {
		t.Fatalf("a connector reading no context was charged %+v", d)
	}
	// What the room held is a different question, and still answered.
	if strings.Join(d.Keys, ",") != "findings" {
		t.Fatalf("keys = %v, want the room's contents", d.Keys)
	}
}

// The point of the curated set is that it stays cheap while the room grows. A
// digest quoting the room's size could not show that, which is what made it
// worth fixing.
func TestPinnedReaderIsChargedForPinnedOnly(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, log := setup(t, specTmpl("a", a, "{{context.pinned}}\n---\n{{input}}"))
	id := room(t, m, "a")

	if _, err := m.AppendContext(id, "findings", "alice", strings.Repeat("x", 500)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetContext(id, "decision", "alice", "hold the release", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PinContext(id, "decision", "alice", true); err != nil {
		t.Fatal(err)
	}
	ask(t, m, id, "go", 1)

	d := startedContext(t, log, id, 0)
	if strings.Join(d.Reads, ",") != "context.pinned" {
		t.Fatalf("reads = %v, want the pinned binding", d.Reads)
	}
	rendered, _, _ := strings.Cut(a.prompt(t, 0), "\n---\n")
	if d.Chars != len(rendered) {
		t.Fatalf("charged %d, prompt carried %d:\n%q", d.Chars, len(rendered), rendered)
	}
	if d.Chars >= 500 {
		t.Fatalf("charged %d, which can only include the unpinned findings", d.Chars)
	}
}

func TestSingleKeyReaderIsChargedForThatKey(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, log := setup(t, specTmpl("a", a, "{{context.findings}}\n---\n{{input}}"))
	id := room(t, m, "a")

	if _, err := m.AppendContext(id, "findings", "alice", "the api is rate limited"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetContext(id, "noise", "alice", strings.Repeat("y", 400), 0); err != nil {
		t.Fatal(err)
	}
	ask(t, m, id, "go", 1)

	d := startedContext(t, log, id, 0)
	rendered, _, _ := strings.Cut(a.prompt(t, 0), "\n---\n")
	if d.Chars != len(rendered) || d.Chars >= 400 {
		t.Fatalf("charged %d for a single key, prompt carried %d", d.Chars, len(rendered))
	}
}

// Elision follows what was read: a pinned-only reader is not warned about items
// trimmed from a list it never saw.
func TestElisionFollowsWhatWasRead(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, log := setup(t, specTmpl("a", a, "{{context.pinned}}\n---\n{{input}}"))
	id := room(t, m, "a")

	for i := 0; i < sharedctx.MaxItemsPerEntry+9; i++ {
		if _, err := m.AppendContext(id, "findings", "alice", fmt.Sprintf("f%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.SetContext(id, "decision", "alice", "hold", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PinContext(id, "decision", "alice", true); err != nil {
		t.Fatal(err)
	}
	ask(t, m, id, "go", 1)

	if d := startedContext(t, log, id, 0); d.Elided != 0 {
		t.Fatalf("elided=%d for a reader that never saw the trimmed list", d.Elided)
	}
}

// ---- a turn that succeeds without saying anything ----

func emptyEvent(t *testing.T, log events.Store, id string) map[string]any {
	t.Helper()
	evs, err := log.Since(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == "turn.empty" {
			var d map[string]any
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("turn.empty data: %v", err)
			}
			return d
		}
	}
	t.Fatalf("no turn.empty event; log has %v", kinds(t, log, id))
	return nil
}

// An agent that returns nothing is not an error — the request worked. But a
// silent success looks like every other success, and the framework in the middle
// gets blamed for whatever went wrong upstream.
//
// This agent answers with an empty string. Oryxa cannot tell that apart from a
// selector that missed — both leave the executor as activity rather than text —
// so it reports what it knows and offers the selectors to check.
func TestAgentAnsweringWithNothingIsReported(t *testing.T) {
	a := newAgent(t, func(w http.ResponseWriter, _ string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"content": ""})
	})
	m, log := setup(t, spec("a", a))
	id := room(t, m, "a")

	ask(t, m, id, "go", 1)

	d := emptyEvent(t, log, id)
	if got, _ := d["reason"].(string); !strings.Contains(got, "no text came out") {
		t.Fatalf("reason = %q, want the nothing-readable case", got)
	}
	// The turn still succeeded; this is a report, not a failure.
	v, _ := m.View(id)
	if v.History[0].State != TurnDone {
		t.Fatalf("turn state = %q, want done", v.History[0].State)
	}
}

// The case worth telling apart: the agent answered fine and the connector's
// selector missed it. Everything succeeds and the answer is simply absent, which
// is the most common mistake in a new connector and the hardest to see.
func TestSelectorThatMatchesNothingSaysSo(t *testing.T) {
	a := newAgent(t, func(w http.ResponseWriter, _ string) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{`{"answer":"the api is rate limited"}`, `{"answer":" and slow"}`} {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
	})
	// $.content is wrong for this agent: it sends $.answer.
	s := specTmpl("a", a, defaultPrompt)
	s.Turn.Response = &connector.Response{Format: "sse", Text: []string{"$.content"}}
	m, log := setup(t, s)
	id := room(t, m, "a")

	ask(t, m, id, "go", 1)

	d := emptyEvent(t, log, id)
	if n, _ := d["parts"].(float64); n < 2 {
		t.Fatalf("parts = %v, want the chunks the agent actually sent", d["parts"])
	}
	if got, _ := d["reason"].(string); !strings.Contains(got, "no text came out") {
		t.Fatalf("reason = %q, want the nothing-readable case", got)
	}
	// The selectors are echoed so the next step doesn't require opening the file.
	sel, _ := d["text"].([]any)
	if len(sel) != 1 || sel[0] != "$.content" {
		t.Fatalf("text selectors = %v, want the ones that missed", d["text"])
	}
}

// Whitespace is not an answer.
func TestWhitespaceOnlyCountsAsEmpty(t *testing.T) {
	a := newAgent(t, replies("   \n  "))
	m, log := setup(t, spec("a", a))
	id := room(t, m, "a")

	ask(t, m, id, "go", 1)
	emptyEvent(t, log, id)
}

// A turn that answered must not be reported as silent, or the event means
// nothing.
func TestNormalTurnEmitsNoEmptyEvent(t *testing.T) {
	a := newAgent(t, replies("an answer"))
	m, log := setup(t, spec("a", a))
	id := room(t, m, "a")

	ask(t, m, id, "go", 1)
	for _, k := range kinds(t, log, id) {
		if k == "turn.empty" {
			t.Fatal("a turn that answered was reported as empty")
		}
	}
}

// The third case: nothing arrived at all. Distinct from an empty answer, because
// the culprit is upstream rather than in the response or the connector.
func TestAgentThatStreamsNothingIsReported(t *testing.T) {
	a := newAgent(t, func(w http.ResponseWriter, _ string) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Connection opens, nothing is ever sent.
	})
	s := specTmpl("a", a, defaultPrompt)
	s.Turn.Response = &connector.Response{Format: "sse", Text: []string{"$.content"}}
	m, log := setup(t, s)
	id := room(t, m, "a")

	ask(t, m, id, "go", 1)

	d := emptyEvent(t, log, id)
	if got, _ := d["reason"].(string); !strings.Contains(got, "nothing at all") {
		t.Fatalf("reason = %q, want the nothing-arrived case", got)
	}
	if n, _ := d["parts"].(float64); n != 0 {
		t.Fatalf("parts = %v, want 0", d["parts"])
	}
}
