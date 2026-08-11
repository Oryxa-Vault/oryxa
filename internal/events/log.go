// Package events is the append-only log every session is a fold over.
//
// Late join, reconnect, replay and audit are all the same operation here —
// read from a sequence number, then subscribe. That is the whole reason the log
// is the source of truth rather than a side effect, and why restart recovery is
// rehydration rather than separate machinery.
package events

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Seq     int64           `json:"seq"`
	Session string          `json:"session"`
	TS      time.Time       `json:"ts"`
	Kind    string          `json:"kind"`
	Actor   string          `json:"actor,omitempty"`
	Turn    string          `json:"turn,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// SystemStream carries what the server knows that is not a conversation —
// connector registrations, today.
//
// Reusing the log rather than adding a table is the point: registrations then
// get durability, ordering, attribution and replay from the same mechanism
// sessions already use, and a connector's edit history comes free.
const SystemStream = "_system"

// Reserved reports whether a stream id is bookkeeping rather than a room.
// Session ids are minted as "s_"+hex, so the underscore prefix cannot collide.
func Reserved(id string) bool { return strings.HasPrefix(id, "_") }

// Store is the log. Append returns an error because a durable implementation
// can fail, and an append-only log that silently drops writes is worse than one
// that stops: replay, audit and recovery all quietly become wrong.
type Store interface {
	Append(sessionID, kind, actor, turn string, data any) (Event, error)
	Since(sessionID string, since int64) ([]Event, error)
	Subscribe(sessionID string) (<-chan Event, func())

	// Sessions lists every session the store knows about, oldest first. Used to
	// rehydrate after a restart.
	Sessions() ([]string, error)

	// Reset empties the store and reports how many sessions it dropped.
	//
	// It exists for the development loop, where a durable log is a liability:
	// every restart brings back every room you were finished with, and the ones
	// you care about are buried under them. It is deliberately not reachable
	// over the API — a log that anything holding a token could erase would not
	// be worth keeping in the first place.
	Reset() (int, error)

	Close() error
}

func encode(data any) (json.RawMessage, error) {
	if data == nil {
		return nil, nil
	}
	return json.Marshal(data)
}

// fanout is the in-process subscriber set, shared by every implementation.
// Cross-process fan-out (NATS, LISTEN/NOTIFY) would wrap this rather than
// replace it: subscribers are local either way.
type fanout struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
	cap  int
}

func newFanout() *fanout {
	return &fanout{subs: map[string]map[chan Event]struct{}{}, cap: 64}
}

// publish sends to every current subscriber, holding the lock throughout.
//
// The lock covers the sends, not merely the lookup, and that is the whole point:
// a send on a closed channel panics, and `select` with a `default` does not
// change that — default only guards a send that would *block*. Copying the
// subscriber list and then sending after unlocking leaves a window where
// unsubscribe can close a channel that publish is about to send on, and the
// panic is in a goroutine, so it takes the process down rather than one stream.
//
// Reaching it needs no unusual timing: a viewer closing a tab while its room is
// mid-turn is the exact shape, and a room is mid-turn most of the time it is
// interesting. Holding the lock is cheap because every send below is
// non-blocking — the critical section is a bounded number of operations that
// cannot wait on anything.
func (f *fanout) publish(ev Event) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// A slow subscriber must not stall the turn loop. It drops frames and
	// recovers with ?since= on reconnect, which the log already supports.
	for ch := range f.subs[ev.Session] {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (f *fanout) subscribe(sessionID string) (<-chan Event, func()) {
	ch := make(chan Event, f.cap)
	f.mu.Lock()
	if f.subs[sessionID] == nil {
		f.subs[sessionID] = map[chan Event]struct{}{}
	}
	f.subs[sessionID][ch] = struct{}{}
	f.mu.Unlock()

	// The close happens under the same lock as the delete, so it cannot land
	// between publish choosing this channel and publish sending on it. Once is
	// belt and braces: a handler that cancels twice would otherwise close twice,
	// which panics just as loudly.
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			delete(f.subs[sessionID], ch)
			close(ch)
		})
	}
}

// ---- in-memory ----

type memLog struct {
	mu     sync.RWMutex
	bySess map[string][]Event
	order  []string
	seq    map[string]int64
	fan    *fanout
}

// NewMemory returns a store that keeps everything in process. Nothing survives
// a restart; use NewPostgres for anything you care about.
func NewMemory() Store {
	return &memLog{
		bySess: map[string][]Event{},
		seq:    map[string]int64{},
		fan:    newFanout(),
	}
}

func (l *memLog) Append(sessionID, kind, actor, turn string, data any) (Event, error) {
	raw, err := encode(data)
	if err != nil {
		return Event{}, err
	}

	l.mu.Lock()
	if _, seen := l.bySess[sessionID]; !seen {
		l.order = append(l.order, sessionID)
	}
	l.seq[sessionID]++
	ev := Event{
		Seq:     l.seq[sessionID],
		Session: sessionID,
		TS:      time.Now().UTC(),
		Kind:    kind,
		Actor:   actor,
		Turn:    turn,
		Data:    raw,
	}
	l.bySess[sessionID] = append(l.bySess[sessionID], ev)
	l.mu.Unlock()

	l.fan.publish(ev)
	return ev, nil
}

func (l *memLog) Reset() (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.order)
	l.bySess = map[string][]Event{}
	l.seq = map[string]int64{}
	l.order = nil
	// Subscribers are left alone: they are attached to sessions that no longer
	// exist and will see nothing more, which is the correct outcome. Closing
	// their channels here would race with readers still draining them.
	return n, nil
}

func (l *memLog) Since(sessionID string, since int64) ([]Event, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	all := l.bySess[sessionID]
	out := make([]Event, 0, len(all))
	for _, e := range all {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	return out, nil
}

func (l *memLog) Subscribe(sessionID string) (<-chan Event, func()) {
	return l.fan.subscribe(sessionID)
}

func (l *memLog) Sessions() ([]string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]string(nil), l.order...), nil
}

func (l *memLog) Close() error { return nil }
