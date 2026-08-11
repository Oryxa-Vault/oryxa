package events

import (
	"sync"
	"testing"
)

// A subscriber going away while its room is publishing used to kill the server.
//
// publish copied the subscriber list, released the lock, then sent; unsubscribe
// deleted the channel and closed it. Between the copy and the send, a channel
// could be closed — and a send on a closed channel panics. `select` with a
// `default` does not help: default guards a send that would *block*, not one
// that is illegal. The panic is in the publishing goroutine, so it takes the
// process rather than the one stream.
//
// It needs no unusual timing to reach. A viewer closing a tab while its room is
// mid-turn is exactly this, and a room is mid-turn most of the time anyone is
// watching it.
func TestASubscriberLeavingDuringPublishDoesNotPanic(t *testing.T) {
	for round := 0; round < 200; round++ {
		f := newFanout()

		const subs = 24
		cancels := make([]func(), subs)
		for i := range cancels {
			_, cancel := f.subscribe("s1")
			cancels[i] = cancel
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				f.publish(Event{Session: "s1", Seq: int64(i)})
			}
		}()
		go func() {
			defer wg.Done()
			for _, cancel := range cancels {
				cancel()
			}
		}()
		wg.Wait()
	}
}

// Cancelling twice is a handler with two exit paths, not a misuse worth
// panicking over.
func TestCancellingASubscriptionTwiceIsSafe(t *testing.T) {
	f := newFanout()
	_, cancel := f.subscribe("s1")
	cancel()
	cancel()
	f.publish(Event{Session: "s1"})
}

// The subscriber still sees what was published while it was there, and still
// sees the close once it leaves — the fix must not quietly drop either.
func TestASubscriberReceivesThenSeesTheClose(t *testing.T) {
	f := newFanout()
	ch, cancel := f.subscribe("s1")

	f.publish(Event{Session: "s1", Seq: 7})
	f.publish(Event{Session: "other", Seq: 8}) // a different room
	cancel()

	got := []int64{}
	for ev := range ch {
		got = append(got, ev.Seq)
	}
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("received %v, want just [7]", got)
	}
}

// A subscriber that stops reading must not stall the turn loop; it drops frames
// and recovers with ?since= on reconnect.
func TestASlowSubscriberDoesNotBlockPublishing(t *testing.T) {
	f := newFanout()
	_, cancel := f.subscribe("s1")
	defer cancel()

	// Well past the buffer. Without the non-blocking send this never returns.
	for i := 0; i < f.cap*4; i++ {
		f.publish(Event{Session: "s1", Seq: int64(i)})
	}
}
