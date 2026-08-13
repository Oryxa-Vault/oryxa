package session

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Oryxa-Vault/oryxa/internal/connector"
	"github.com/Oryxa-Vault/oryxa/internal/events"
	"github.com/Oryxa-Vault/oryxa/internal/sharedctx"
)

// summariser stands in for a model. It reports how many items it was handed, so
// a test can tell what the rollup was actually built from.
func summariser(t *testing.T, name string) (*connector.Spec, *agentStub) {
	t.Helper()
	a := newAgent(t, func(w http.ResponseWriter, prompt string) {
		n := strings.Count(prompt, "\n- ") + strings.Count(prompt, "- ")
		if strings.HasPrefix(prompt, "- ") {
			n = strings.Count(prompt, "- ")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": fmt.Sprintf("summary of %d findings", n),
		})
	})
	return spec(name, a), a
}

func fill(t *testing.T, m *Manager, id, key string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := m.AppendContext(id, key, "alice", fmt.Sprintf("finding %d", i)); err != nil {
			t.Fatal(err)
		}
	}
}

func waitRollup(t *testing.T, m *Manager, id, key string) sharedctx.Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e := entry(t, m, id, key)
		if e.Rollup != nil {
			return e
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("no rollup on %q within the deadline", key)
	return sharedctx.Entry{}
}

func TestRoomStopsForgettingOnceItOutgrowsThePrompt(t *testing.T) {
	sum, _ := summariser(t, "roller")
	quiet := newAgent(t, replies("ok"))
	m, _ := setup(t, sum, spec("a", quiet))
	m.WithSummariser("roller")
	id := room(t, m, "a")

	// 20 fit; the rollup wants RollupAfter beyond that.
	fill(t, m, id, "findings", sharedctx.MaxItemsPerEntry+RollupAfter)

	e := waitRollup(t, m, id, "findings")
	if e.Rollup.Covers != RollupAfter {
		t.Fatalf("rollup covers %d items, want %d", e.Rollup.Covers, RollupAfter)
	}
	if !strings.Contains(e.Rollup.Text, "summary of") {
		t.Fatalf("summariser output not stored: %q", e.Rollup.Text)
	}
	// Nothing is deleted — the bound was always about rendering.
	if len(e.Items) != sharedctx.MaxItemsPerEntry+RollupAfter {
		t.Fatalf("rollup deleted items: %d remain", len(e.Items))
	}
}

func TestBelowTheThresholdNothingIsSummarised(t *testing.T) {
	sum, stub := summariser(t, "roller")
	m, _ := setup(t, sum, spec("a", newAgent(t, replies("ok"))))
	m.WithSummariser("roller")
	id := room(t, m, "a")

	fill(t, m, id, "findings", sharedctx.MaxItemsPerEntry+RollupAfter-1)
	time.Sleep(150 * time.Millisecond)

	if e := entry(t, m, id, "findings"); e.Rollup != nil {
		t.Fatalf("summarised a tail of %d, below the threshold of %d", e.Rollup.Covers, RollupAfter)
	}
	if n := len(stub.prompts()); n != 0 {
		t.Fatalf("spent %d model calls under the threshold", n)
	}
}

func TestWithoutASummariserTheMarkerStays(t *testing.T) {
	m, _ := setup(t, spec("a", newAgent(t, replies("ok"))))
	id := room(t, m, "a")

	fill(t, m, id, "findings", sharedctx.MaxItemsPerEntry+RollupAfter+5)
	time.Sleep(150 * time.Millisecond)

	e := entry(t, m, id, "findings")
	if e.Rollup != nil {
		t.Fatal("rolled up with no summariser configured")
	}
	if !strings.Contains(sharedctx.RenderEntry(e), "not shown") {
		t.Fatal("the count-only marker disappeared too")
	}
}

// The point of the whole feature: what the summary says reaches the agent.
func TestTheSummaryReachesThePrompt(t *testing.T) {
	sum, _ := summariser(t, "roller")
	reader := newAgent(t, replies("ok"))
	m, _ := setup(t, sum, specTmpl("reader", reader, "{{context.findings}}"))
	m.WithSummariser("roller")
	id := room(t, m, "reader")

	fill(t, m, id, "findings", sharedctx.MaxItemsPerEntry+RollupAfter)
	waitRollup(t, m, id, "findings")
	ask(t, m, id, "go", 1)

	got := reader.prompt(t, 0)
	if !strings.Contains(got, "earlier items, summarised") {
		t.Fatalf("the summary never reached the agent:\n%s", got)
	}
	if strings.Contains(got, "not shown") {
		t.Fatalf("covered items were still reported as missing:\n%s", got)
	}
}

// A summary is a model's output, so replay must apply the recorded text rather
// than recompute it — otherwise the room reads differently after every restart.
func TestRollupSurvivesARestartWithoutBeingRecomputed(t *testing.T) {
	sum, stub := summariser(t, "roller")
	reg := connector.NewRegistry()
	for _, s := range []*connector.Spec{sum, spec("a", newAgent(t, replies("ok")))} {
		if err := reg.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	log := events.NewMemory()
	m := NewManager(reg, connector.NewExecutor(), log).WithSummariser("roller")
	id := room(t, m, "a")
	fill(t, m, id, "findings", sharedctx.MaxItemsPerEntry+RollupAfter)
	before := waitRollup(t, m, id, "findings")
	calls := len(stub.prompts())

	restarted := NewManager(reg, connector.NewExecutor(), log).WithSummariser("roller")
	if _, err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	after := entry(t, restarted, id, "findings")

	if after.Rollup == nil {
		t.Fatal("the rollup did not replay")
	}
	if after.Rollup.Text != before.Rollup.Text {
		t.Fatalf("replay produced different text:\n%q\n%q", before.Rollup.Text, after.Rollup.Text)
	}
	if after.Rollup.Covers != before.Rollup.Covers || after.Rollup.Through != before.Rollup.Through {
		t.Fatalf("coverage changed on replay: %+v then %+v", before.Rollup, after.Rollup)
	}
	if got := len(stub.prompts()); got != calls {
		t.Fatalf("replay spent %d extra model calls; a summary must be data, not a recomputation", got-calls)
	}
}

// A summariser that has stopped working must be visible. Silently going back to
// forgetting is the exact state this feature exists to fix.
func TestAFailingSummariserIsRecordedNotSilent(t *testing.T) {
	broken := spec("roller", newAgent(t, func(w http.ResponseWriter, _ string) {
		http.Error(w, "down", 500)
	}))
	m, log := setup(t, broken, spec("a", newAgent(t, replies("ok"))))
	m.WithSummariser("roller")
	id := room(t, m, "a")

	fill(t, m, id, "findings", sharedctx.MaxItemsPerEntry+RollupAfter)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && countKind(t, log, id, "rollup.failed") == 0 {
		time.Sleep(3 * time.Millisecond)
	}
	if countKind(t, log, id, "rollup.failed") == 0 {
		t.Fatalf("a broken summariser was silent; log holds %v", kinds(t, log, id))
	}
	if e := entry(t, m, id, "findings"); e.Rollup != nil {
		t.Fatal("a failed summary was stored anyway")
	}
}

func TestUnregisteredSummariserIsReported(t *testing.T) {
	m, log := setup(t, spec("a", newAgent(t, replies("ok"))))
	m.WithSummariser("does-not-exist")
	id := room(t, m, "a")

	fill(t, m, id, "findings", sharedctx.MaxItemsPerEntry+RollupAfter)
	if countKind(t, log, id, "rollup.failed") == 0 {
		t.Fatalf("a missing summariser was silent; log holds %v", kinds(t, log, id))
	}
}

// A rollup is built from the original items every time. Summarising a summary
// loses a little on each pass.
func TestRerollReadsTheOriginalItems(t *testing.T) {
	sum, stub := summariser(t, "roller")
	m, _ := setup(t, sum, spec("a", newAgent(t, replies("ok"))))
	m.WithSummariser("roller")
	id := room(t, m, "a")

	fill(t, m, id, "findings", sharedctx.MaxItemsPerEntry+RollupAfter)
	waitRollup(t, m, id, "findings")
	first := len(stub.prompts())

	fill(t, m, id, "findings", RollupAfter)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(stub.prompts()) == first {
		time.Sleep(3 * time.Millisecond)
	}
	prompts := stub.prompts()
	if len(prompts) == first {
		t.Fatal("a grown tail never triggered a second rollup")
	}
	last := prompts[len(prompts)-1]
	if !strings.Contains(last, "finding 1\n") && !strings.HasPrefix(last, "- alice: finding 1") {
		t.Fatalf("the re-roll did not start from the oldest item:\n%s", last[:min(200, len(last))])
	}
	if strings.Contains(last, "summary of") {
		t.Fatal("the re-roll was handed the previous summary instead of the items")
	}
}
