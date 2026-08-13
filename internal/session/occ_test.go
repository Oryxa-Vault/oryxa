package session

import (
	"errors"
	"sync"
	"testing"

	"github.com/Oryxa-Vault/oryxa/internal/connector"
	"github.com/Oryxa-Vault/oryxa/internal/events"
	"github.com/Oryxa-Vault/oryxa/internal/sharedctx"
)

// Two writers holding the same version must not both win. This is the lost
// update optimistic concurrency exists to prevent, and it regressed once: the
// version check and the write were separate calls with a log append between
// them, so 4 of 16 concurrent writers were admitted instead of 1.
func TestConcurrentSetAtSameVersionAdmitsOne(t *testing.T) {
	reg := connector.NewRegistry()
	reg.Put(&connector.Spec{Name: "a", Base: "http://127.0.0.1:1", Turn: &connector.Step{}})
	m := NewManager(reg, connector.NewExecutor(), events.NewMemory())
	sum, _, err := m.Create("a")
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.SetContext(sum.ID, "plan", "alice", "v1", 0)
	if err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Every writer holds the same version they just read.
			_, err := m.SetContext(sum.ID, "plan", "w", "concurrent", first.Version)
			mu.Lock()
			defer mu.Unlock()
			var c *sharedctx.Conflict
			if err == nil {
				accepted++
			} else if !asConflict(err, &c) {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if accepted != 1 {
		t.Fatalf("%d of %d writers at the same version were accepted; want exactly 1", accepted, n)
	}
}

func asConflict(err error, c **sharedctx.Conflict) bool {
	x, ok := err.(*sharedctx.Conflict)
	if ok {
		*c = x
	}
	return ok
}

// A version held for a key that was never written is still a stale write. It
// reads as an update, and answering it with a create would tell the caller its
// update landed when the thing it believed it was updating never existed.
func TestVersionForAKeyThatNeverExistedConflicts(t *testing.T) {
	m, _ := setup(t, spec("a", newAgent(t, replies("ok"))))
	id := room(t, m, "a")

	_, err := m.SetContext(id, "plan", "alice", "ship friday", 7)
	var conflict *sharedctx.Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if conflict.Version != 0 {
		t.Fatalf("conflict reports version %d, want 0 for a key with no history", conflict.Version)
	}
	noEntry(t, m, id, "plan")
}

// Zero still means "create if absent", which is how a first write is made
// without the caller having to know the key is new.
func TestZeroVersionStillCreates(t *testing.T) {
	m, _ := setup(t, spec("a", newAgent(t, replies("ok"))))
	id := room(t, m, "a")

	if _, err := m.SetContext(id, "plan", "alice", "ship friday", 0); err != nil {
		t.Fatalf("first write with ifMatch=0: %v", err)
	}
	if got := entry(t, m, id, "plan").Value; got != "ship friday" {
		t.Fatalf("plan = %q", got)
	}
}
