// Package sharedctx is the room's shared state: notes, decisions and working
// values that everyone in a session can read and write.
//
// Like sessions, it is a fold over the event log rather than a store of its
// own, so it survives a restart for free and every change is attributed.
//
// Two kinds of entry, because most shared state is one of two shapes:
//
//	append   add-only list. Conflicts are impossible by construction, which is
//	         why it is the default: findings, notes and decisions are what most
//	         shared content actually is, and every entry it absorbs is a merge
//	         problem nobody has to solve.
//
//	value    a single mutable value under optimistic concurrency. A stale write
//	         is refused with the current value rather than silently overwriting.
//	         Loud conflicts beat clever merges: a silent overwrite leaves state
//	         that is syntactically fine and semantically wrong, which surfaces
//	         much later as an agent behaving oddly.
package sharedctx

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

type Kind string

const (
	KindAppend Kind = "append"
	KindValue  Kind = "value"
)

type Item struct {
	By   string    `json:"by"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	Seq  int64     `json:"seq"`
}

type Entry struct {
	Key    string `json:"key"`
	Kind   Kind   `json:"kind"`
	Pinned bool   `json:"pinned"`

	// Value entries carry Value and Version; append entries carry Items.
	Value   string `json:"value,omitempty"`
	Version int64  `json:"version,omitempty"`
	By      string `json:"by,omitempty"`
	Items   []Item `json:"items,omitempty"`
}

// Conflict is returned when a value write is based on a version that is no
// longer current. It carries what is current so the caller can merge rather
// than re-fetch and guess.
type Conflict struct {
	Key     string
	Current string
	Version int64
	By      string
}

func (c *Conflict) Error() string {
	return fmt.Sprintf("stale write to %q: current version is %d", c.Key, c.Version)
}

var (
	ErrWrongKind = errors.New("entry is a different kind")
	ErrNotFound  = errors.New("no such entry")
)

// Store holds one session's shared context.
type Store struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

func New() *Store { return &Store{entries: map[string]*Entry{}} }

// Append adds to an add-only entry, creating it if needed. It cannot conflict.
func (s *Store) Append(key, by, text string, seq int64, at time.Time) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
		e = &Entry{Key: key, Kind: KindAppend}
		s.entries[key] = e
	}
	if e.Kind != KindAppend {
		return Entry{}, fmt.Errorf("%w: %q is a value", ErrWrongKind, key)
	}
	e.Items = append(e.Items, Item{By: by, At: at.UTC(), Text: text, Seq: seq})
	e.Version = seq
	return *e, nil
}

// Set writes a value entry. ifMatch is the version the writer last saw; zero
// means "only if this does not exist yet", and -1 means "overwrite regardless".
func (s *Store) Set(key, by, value string, ifMatch, seq int64, _ time.Time) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if ok && e.Kind != KindValue {
		return Entry{}, fmt.Errorf("%w: %q is an append list", ErrWrongKind, key)
	}
	if !ok {
		if ifMatch > 0 {
			return Entry{}, &Conflict{Key: key, Version: 0}
		}
		e = &Entry{Key: key, Kind: KindValue}
		s.entries[key] = e
	} else if ifMatch != -1 && e.Version != ifMatch {
		// The point of refusing: whoever wrote last had context this writer did
		// not, and picking one silently discards it.
		return Entry{}, &Conflict{
			Key: key, Current: e.Value, Version: e.Version, By: e.By,
		}
	}
	e.Value, e.Version, e.By = value, seq, by
	return *e, nil
}

func (s *Store) Pin(key string, pinned bool) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	e.Pinned = pinned
	return *e, nil
}

// Snapshot returns every entry, sorted by key. Callers get copies: nothing here
// hands out a pointer a reader could encode while a writer mutates it.
func (s *Store) Snapshot() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		c := *e
		c.Items = append([]Item(nil), e.Items...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (s *Store) Get(key string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[key]
	if !ok {
		return Entry{}, false
	}
	c := *e
	c.Items = append([]Item(nil), e.Items...)
	return c, true
}

// Pinned returns the curated set, which is the part meant to be small enough to
// put in front of an agent without swamping it.
func (s *Store) Pinned() []Entry {
	var out []Entry
	for _, e := range s.Snapshot() {
		if e.Pinned {
			out = append(out, e)
		}
	}
	return out
}
