package session

import (
	"sync"
	"testing"

	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/events"
	"github.com/oryxa/oryxa/internal/sharedctx"
)

// Two writers holding the same version must not both win. This is the lost
// update optimistic concurrency exists to prevent, and it regressed once: the
// version check and the write were separate calls with a log append between
// them, so 4 of 16 concurrent writers were admitted instead of 1.
func TestConcurrentSetAtSameVersionAdmitsOne(t *testing.T) {
	reg := connector.NewRegistry()
	reg.Put(&connector.Spec{Name: "a", Base: "http://127.0.0.1:1", Turn: &connector.Step{}})
	m := NewManager(reg, connector.NewExecutor(), events.NewMemory())
	sum, err := m.Create("a")
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
