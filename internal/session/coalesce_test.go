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
)

func waitPrompts(t *testing.T, a *agentStub, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p := a.prompts(); len(p) >= n {
			return p
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("agent saw %d prompts, wanted %d", len(a.prompts()), n)
	return nil
}

// The heart of it: people type faster than models answer, and that must cost
// rounds rather than messages. Six messages during one call are one turn, not
// six queued behind each other.
func TestMessagesDuringATurnBecomeOneTurn(t *testing.T) {
	busy, release := gate()
	defer release()
	first := true

	a := newAgent(t, func(w http.ResponseWriter, _ string) {
		if first {
			first = false
			busy() // hold the lane open while the room keeps talking
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"content": "ok"})
	})
	m, _ := setup(t, spec("a", a))
	id := room(t, m, "a")

	if _, err := m.Submit(id, Claimed("alice"), "first"); err != nil {
		t.Fatal(err)
	}
	waitPrompts(t, a, 1) // the lane is now inside a turn

	for i, msg := range []string{"two", "three", "four", "five", "six"} {
		if _, err := m.Submit(id, Claimed(fmt.Sprintf("p%d", i)), msg); err != nil {
			t.Fatal(err)
		}
	}
	release()

	got := waitPrompts(t, a, 2)
	if len(got) != 2 {
		t.Fatalf("five messages during one turn produced %d turns, want 1 more", len(got)-1)
	}
	for _, msg := range []string{"two", "three", "four", "five", "six"} {
		if !strings.Contains(got[1], msg) {
			t.Fatalf("%q was dropped from the coalesced turn:\n%s", msg, got[1])
		}
	}
}

// A single message renders exactly as it always did. Every connector written
// before coalescing depends on that.
func TestOneMessageIsUnchanged(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, _ := setup(t, specTmpl("a", a, "{{input}}"))
	id := room(t, m, "a")

	ask(t, m, id, "just this", 1)

	if got := a.prompt(t, 0); got != "just this" {
		t.Fatalf("a lone message was rewritten: %q", got)
	}
}

// Several render attributed, because "ship it" and "don't ship it" are the same
// string without names, and an agent cannot tell a question from an answer.
func TestCoalescedMessagesCarryTheirAuthors(t *testing.T) {
	got := RenderInputs([]Input{
		{Author: "alice", Text: "checkout is 503ing"},
		{Author: "bob", Text: "since when?"},
		{Author: "alice", Text: "14:02"},
	})
	want := "alice: checkout is 503ing\nbob: since when?\nalice: 14:02"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Each lane reads at its own pace. A slow agent coalescing more than a fast one
// is the system working, not falling behind.
func TestLanesCoalesceIndependently(t *testing.T) {
	slowBusy, releaseSlow := gate()
	defer releaseSlow()
	firstSlow := true

	slow := newAgent(t, func(w http.ResponseWriter, _ string) {
		if firstSlow {
			firstSlow = false
			slowBusy()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"content": "slow"})
	})
	fast := newAgent(t, replies("fast"))

	m, _ := setup(t, spec("slow", slow), spec("fast", fast))
	id := room(t, m, "slow", "fast")

	if _, err := m.Submit(id, Claimed("alice"), "one"); err != nil {
		t.Fatal(err)
	}
	waitPrompts(t, slow, 1)
	waitPrompts(t, fast, 1)

	// One at a time, each waited for: otherwise the fast lane might coalesce
	// these too, which would be correct behaviour and a flaky test.
	for i, msg := range []string{"two", "three"} {
		if _, err := m.Submit(id, Claimed("alice"), msg); err != nil {
			t.Fatal(err)
		}
		waitPrompts(t, fast, i+2)
	}
	releaseSlow()

	slowSeen := waitPrompts(t, slow, 2)
	if len(slowSeen) != 2 {
		t.Fatalf("the slow lane ran %d turns; it should have coalesced into 2", len(slowSeen))
	}
	if !strings.Contains(slowSeen[1], "two") || !strings.Contains(slowSeen[1], "three") {
		t.Fatalf("the slow lane lost a message:\n%s", slowSeen[1])
	}
}

func TestWithdrawnInputIsNeverRead(t *testing.T) {
	busy, release := gate()
	defer release()
	first := true

	a := newAgent(t, func(w http.ResponseWriter, _ string) {
		if first {
			first = false
			busy()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"content": "ok"})
	})
	m, _ := setup(t, spec("a", a))
	id := room(t, m, "a")

	if _, err := m.Submit(id, Claimed("alice"), "opening"); err != nil {
		t.Fatal(err)
	}
	waitPrompts(t, a, 1)

	regret, err := m.Submit(id, Claimed("alice"), "IGNORE ME")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit(id, Claimed("alice"), "keep this"); err != nil {
		t.Fatal(err)
	}
	if err := m.Withdraw(id, regret.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	release()

	got := waitPrompts(t, a, 2)
	if strings.Contains(got[1], "IGNORE ME") {
		t.Fatalf("a withdrawn message still reached the agent:\n%s", got[1])
	}
	if !strings.Contains(got[1], "keep this") {
		t.Fatalf("withdrawing one message dropped another:\n%s", got[1])
	}
}

// Withdrawing does not shift positions. A lane's cursor is an index, so removing
// an entry would move every lane silently backwards.
func TestWithdrawKeepsInboxPositionsStable(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, _ := setup(t, spec("a", a))
	id := room(t, m, "a")

	one, _ := m.Submit(id, Claimed("alice"), "one")
	two, _ := m.Submit(id, Claimed("alice"), "two")
	if err := m.Withdraw(id, one.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	three, _ := m.Submit(id, Claimed("alice"), "three")

	if one.Seq != 0 || two.Seq != 1 || three.Seq != 2 {
		t.Fatalf("positions shifted: %d %d %d", one.Seq, two.Seq, three.Seq)
	}
}

// A message a lane already read is not un-said to it. Withdrawing is regret,
// not time travel.
func TestWithdrawAfterReadingIsRefused(t *testing.T) {
	a := newAgent(t, replies("ok"))
	m, _ := setup(t, spec("a", a))
	id := room(t, m, "a")

	in, _ := m.Submit(id, Claimed("alice"), "already answered")
	waitTurns(t, m, id, 1)

	// Still withdrawable as a record — but the answer stands.
	_ = m.Withdraw(id, in.ID, "alice")
	if h, _ := m.View(id); len(h.History) != 1 || h.History[0].Output == "" {
		t.Fatal("withdrawing removed an answer that had already been given")
	}
}

// A restart must not ask the same question again. The cursor is rebuilt from
// what each lane's turns say they consumed.
func TestRestartDoesNotReAskWhatWasAlreadyAnswered(t *testing.T) {
	a := newAgent(t, replies("ok"))
	s := spec("a", a)
	reg := connector.NewRegistry()
	if err := reg.Put(s); err != nil {
		t.Fatal(err)
	}
	log := events.NewMemory()

	m := NewManager(reg, connector.NewExecutor(), log)
	id := room(t, m, "a")
	ask(t, m, id, "answered before the restart", 1)
	seen := len(a.prompts())

	restarted := NewManager(reg, connector.NewExecutor(), log)
	if _, err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if got := len(a.prompts()); got != seen {
		t.Fatalf("a restart re-asked %d question(s)", got-seen)
	}
	// And the room still works afterwards.
	ask(t, restarted, id, "after", 2)
	if got := a.prompt(t, seen); !strings.Contains(got, "after") {
		t.Fatalf("the rehydrated room did not deliver a new message: %q", got)
	}
}

// Nothing said while an agent was away is lost — it is read when it next speaks.
func TestWhatWasSaidWhileBusyIsReadNext(t *testing.T) {
	busy, release := gate()
	defer release()
	first := true

	a := newAgent(t, func(w http.ResponseWriter, _ string) {
		if first {
			first = false
			busy()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"content": "ok"})
	})
	m, _ := setup(t, spec("a", a))
	id := room(t, m, "a")

	m.Submit(id, Claimed("alice"), "opening")
	waitPrompts(t, a, 1)
	m.Submit(id, Claimed("bob"), "a thing said while it was busy")

	v, _ := m.View(id)
	if len(v.Waiting) != 1 || v.Waiting[0].Text != "a thing said while it was busy" {
		t.Fatalf("waiting = %+v", v.Waiting)
	}
	release()

	got := waitPrompts(t, a, 2)
	if !strings.Contains(got[1], "a thing said while it was busy") {
		t.Fatalf("it was never read:\n%s", got[1])
	}
	after, _ := m.View(id)
	if len(after.Waiting) != 0 {
		t.Fatalf("still waiting after being read: %+v", after.Waiting)
	}
}
