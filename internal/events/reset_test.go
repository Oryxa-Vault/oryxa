package events

import "testing"

func TestResetEmptiesTheStore(t *testing.T) {
	s := NewMemory()
	for _, id := range []string{"s_one", "s_two", "s_three"} {
		if _, err := s.Append(id, "session.created", "alice", "", nil); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.Reset()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("Reset reported %d sessions, want 3", n)
	}

	sessions, err := s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions survived the reset: %v", sessions)
	}
	evs, err := s.Since("s_one", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("events survived the reset: %v", evs)
	}
}

// Sequence numbers must restart too. A store that kept counting would hand the
// first room after a reset a seq of 400, and every "since" cursor a client had
// would silently point past the start of a session that no longer exists.
func TestResetRestartsSequenceNumbers(t *testing.T) {
	s := NewMemory()
	for i := 0; i < 5; i++ {
		if _, err := s.Append("s_one", "output.part", "agent", "t1", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Reset(); err != nil {
		t.Fatal(err)
	}

	ev, err := s.Append("s_one", "session.created", "alice", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 1 {
		t.Fatalf("first event after reset had seq %d, want 1", ev.Seq)
	}
}

func TestResetOnAnEmptyStoreIsFine(t *testing.T) {
	n, err := NewMemory().Reset()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reported %d sessions on an empty store", n)
	}
}

// The store has to keep working afterwards — reset is a startup step, not a
// teardown, and everything that follows it is a normal run.
func TestStoreIsUsableAfterReset(t *testing.T) {
	s := NewMemory()
	if _, err := s.Append("old", "session.created", "alice", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reset(); err != nil {
		t.Fatal(err)
	}

	ch, cancel := s.Subscribe("fresh")
	defer cancel()
	if _, err := s.Append("fresh", "session.created", "bob", "", nil); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-ch:
		if ev.Session != "fresh" || ev.Seq != 1 {
			t.Fatalf("got %+v", ev)
		}
	default:
		t.Fatal("subscriber taken out after a reset received nothing")
	}

	got, err := s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("sessions = %v, want just the new one", got)
	}
}
