// Package events is the append-only log every session is a fold over.
//
// Late join, reconnect, replay and audit are all the same operation here —
// read from a sequence number, then subscribe. That is the whole reason the log
// is the source of truth rather than a side effect.
package events

import (
	"encoding/json"
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

// Store is the seam SQLite drops into later. Handlers never touch anything else.
type Store interface {
	Append(sessionID, kind, actor, turn string, data any) Event
	Since(sessionID string, since int64) []Event
	Subscribe(sessionID string) (<-chan Event, func())
}

type memLog struct {
	mu       sync.RWMutex
	bySess   map[string][]Event
	seq      map[string]int64
	subs     map[string]map[chan Event]struct{}
	capacity int
}

func NewMemory() Store {
	return &memLog{
		bySess:   map[string][]Event{},
		seq:      map[string]int64{},
		subs:     map[string]map[chan Event]struct{}{},
		capacity: 64,
	}
}

func (l *memLog) Append(sessionID, kind, actor, turn string, data any) Event {
	var raw json.RawMessage
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			raw = b
		}
	}

	l.mu.Lock()
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
	subs := make([]chan Event, 0, len(l.subs[sessionID]))
	for ch := range l.subs[sessionID] {
		subs = append(subs, ch)
	}
	l.mu.Unlock()

	// A slow subscriber must not stall the turn loop. It drops frames and
	// recovers with ?since= on reconnect, which the log already supports.
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
	return ev
}

func (l *memLog) Since(sessionID string, since int64) []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	all := l.bySess[sessionID]
	out := make([]Event, 0, len(all))
	for _, e := range all {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	return out
}

func (l *memLog) Subscribe(sessionID string) (<-chan Event, func()) {
	ch := make(chan Event, l.capacity)
	l.mu.Lock()
	if l.subs[sessionID] == nil {
		l.subs[sessionID] = map[chan Event]struct{}{}
	}
	l.subs[sessionID][ch] = struct{}{}
	l.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.subs[sessionID], ch)
			l.mu.Unlock()
			close(ch)
		})
	}
}
