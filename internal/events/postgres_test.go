package events

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// Run against a real postgres. Skipped when ORYXA_TEST_DATABASE_URL is unset so
// `go test ./...` stays dependency-free:
//
//	docker run -d --name oryxa-pg -e POSTGRES_PASSWORD=oryxa -e POSTGRES_USER=oryxa \
//	  -e POSTGRES_DB=oryxa -p 5433:5432 postgres:16-alpine
//	ORYXA_TEST_DATABASE_URL='postgres://oryxa:oryxa@localhost:5433/oryxa?sslmode=disable' \
//	  go test ./internal/events/
func openTestStore(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv("ORYXA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ORYXA_TEST_DATABASE_URL to run postgres tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func sessionID(t *testing.T) string {
	return fmt.Sprintf("s_test_%s_%d", t.Name(), time.Now().UnixNano())
}

func TestPostgresAppendAndRead(t *testing.T) {
	store := openTestStore(t)
	sid := sessionID(t)

	for i := 1; i <= 3; i++ {
		ev, err := store.Append(sid, "test.kind", "alice", "t1",
			map[string]any{"n": i})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if ev.Seq != int64(i) {
			t.Fatalf("seq = %d, want %d", ev.Seq, i)
		}
	}

	all, err := store.Since(sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d events, want 3", len(all))
	}
	if all[0].Actor != "alice" || all[0].Kind != "test.kind" || all[0].Turn != "t1" {
		t.Fatalf("attribution lost: %+v", all[0])
	}

	// ?since= is what makes late join, reconnect and replay one mechanism.
	rest, err := store.Since(sid, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 || rest[0].Seq != 2 {
		t.Fatalf("since=1 gave %d events starting at %d", len(rest), rest[0].Seq)
	}
}

// The sequence must stay gapless and unique under concurrent writers, or replay
// silently reorders history.
func TestPostgresConcurrentAppendsAreSerialised(t *testing.T) {
	store := openTestStore(t)
	sid := sessionID(t)

	const n = 40
	var wg sync.WaitGroup
	seqs := make([]int64, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev, err := store.Append(sid, "concurrent", "w", "", map[string]int{"i": i})
			seqs[i], errs[i] = ev.Seq, err
		}(i)
	}
	wg.Wait()

	seen := map[int64]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if seen[seqs[i]] {
			t.Fatalf("sequence %d handed out twice", seqs[i])
		}
		seen[seqs[i]] = true
	}
	for i := int64(1); i <= n; i++ {
		if !seen[i] {
			t.Fatalf("gap in sequence at %d", i)
		}
	}
}

// Durability is the whole point: a second store over the same database must see
// everything the first one wrote.
func TestPostgresSurvivesReopen(t *testing.T) {
	store := openTestStore(t)
	sid := sessionID(t)

	if _, err := store.Append(sid, "session.created", "", "",
		map[string]any{"agents": []string{"alpha"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(sid, "input.submitted", "alice", "t1",
		map[string]any{"text": "still here?"}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened := openTestStore(t)
	evs, err := reopened.Since(sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("after reopen got %d events, want 2", len(evs))
	}
	if evs[1].Actor != "alice" {
		t.Fatalf("attribution lost across reopen: %+v", evs[1])
	}

	ids, err := reopened.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id == sid {
			found = true
		}
	}
	if !found {
		t.Fatal("session not listed after reopen; rehydrate would miss it")
	}
}

// Subscribers must only see committed events, and must not be able to stall a
// writer by being slow.
func TestPostgresSubscribeDeliversAfterCommit(t *testing.T) {
	store := openTestStore(t)
	sid := sessionID(t)

	ch, unsub := store.Subscribe(sid)
	defer unsub()

	if _, err := store.Append(sid, "live", "bob", "", map[string]int{"x": 1}); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-ch:
		if ev.Kind != "live" || ev.Actor != "bob" {
			t.Fatalf("wrong event: %+v", ev)
		}
		// It must already be readable — publishing before commit would let a
		// subscriber see what a rollback never wrote.
		got, err := store.Since(sid, ev.Seq-1)
		if err != nil || len(got) == 0 {
			t.Fatalf("published event not committed: %v %v", got, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber never received the event")
	}
}

func TestMemoryAndPostgresAgreeOnSemantics(t *testing.T) {
	pg := openTestStore(t)
	mem := NewMemory()

	for _, tc := range []struct {
		name  string
		store Store
	}{{"memory", mem}, {"postgres", pg}} {
		t.Run(tc.name, func(t *testing.T) {
			sid := sessionID(t)
			for i := 0; i < 3; i++ {
				if _, err := tc.store.Append(sid, "k", "a", "", nil); err != nil {
					t.Fatal(err)
				}
			}
			evs, err := tc.store.Since(sid, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) != 3 || evs[0].Seq != 1 || evs[2].Seq != 3 {
				t.Fatalf("sequence differs between stores: %+v", evs)
			}
			if evs[0].Data != nil {
				t.Fatalf("nil data should stay nil, got %s", evs[0].Data)
			}
		})
	}
}
