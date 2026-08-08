package session

import (
	"context"
	"sync"
)

// A lane is one agent's place in a room: a cursor into what the room has said,
// its own turn loop, and its own conversation with its own framework.
//
// The serialization requirement is per agent, not per session. An agent's
// conversation is sequential, so its turns must not overlap — but two agents
// have two conversations and no reason to wait on each other. One goroutine per
// lane gives exactly that: strict order within an agent, full parallelism across
// them. A five-agent room answers in the time of its slowest agent rather than
// the sum of all five.
//
// A cursor rather than a queue, because listening and speaking are different
// acts. A queue holds work an agent still owes; a cursor holds only a position.
// When someone types faster than a model answers — which is always, in a room
// with people in it — a queue grows without bound and the agent falls further
// behind with every message. A cursor cannot: whatever arrived while the agent
// was busy is one turn's worth of reading when it next speaks, however much of
// it there is. Being behind the speaker is what listening is, and it stops being
// a backlog the moment it stops being a queue.
type lane struct {
	agent string

	mu sync.Mutex
	// cursor is how far into the room's inbox this agent has read. Everything
	// before it has been taken; everything after it is what its next turn covers.
	cursor  int
	current *Turn
	handle  string
	opened  bool
	caps    map[string]string
	cancel  context.CancelFunc

	wake   chan struct{}
	closed chan struct{}
}

func newLane(agent string) *lane {
	return &lane{
		agent:  agent,
		caps:   map[string]string{},
		wake:   make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
}

func (l *lane) nudge() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// take builds this lane's next turn from everything said since its cursor, and
// returns nil when there is nothing new — the loop's signal to go back to sleep.
//
// Coalescing is the whole point. Six people typing during one model call become
// one turn, not six, so the cost of a busy room is its number of rounds rather
// than its number of messages, and the lane is at most one turn behind however
// fast the room moves.
func (l *lane) take(inbox []Input) *Turn {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cursor >= len(inbox) {
		return nil
	}

	fresh := inbox[l.cursor:]

	// Read everything, speak only when wanted. If nothing here is for this agent
	// the cursor stays put — so when it is finally asked, its turn covers the
	// whole conversation it sat through rather than starting from the question.
	// That is the difference between an agent that was present and one that has
	// just walked in.
	var taken []Input
	wanted := false
	for _, in := range fresh {
		if in.Withdrawn {
			continue
		}
		taken = append(taken, in)
		for _, a := range in.Wake {
			if a == l.agent {
				wanted = true
				break
			}
		}
	}
	if len(taken) == 0 {
		l.cursor = len(inbox) // nothing but withdrawals; not owed
		return nil
	}
	if !wanted {
		return nil
	}
	l.cursor = len(inbox)

	t := newTurn(l.agent, taken)
	t.State = TurnRunning
	l.current = t
	return t
}

// pending reports how much this lane has not read yet, which is what a client
// showing "3 waiting" is actually describing.
func (l *lane) pending(inbox []Input) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cursor >= len(inbox) {
		return 0
	}
	n := 0
	for _, in := range inbox[l.cursor:] {
		if !in.Withdrawn {
			n++
		}
	}
	return n
}

func (l *lane) done(t *Turn, history *[]*Turn, hmu *sync.Mutex) {
	l.mu.Lock()
	l.current = nil
	l.mu.Unlock()

	hmu.Lock()
	*history = append(*history, t)
	hmu.Unlock()
}

func (l *lane) running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current != nil
}

func (l *lane) snapshot() *Turn {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.current == nil {
		return nil
	}
	c := *l.current
	c.Inputs = append([]Input(nil), l.current.Inputs...)
	return &c
}

// seek moves the cursor without running anything. Restart recovery uses it: a
// turn that was already taken must not be taken again just because the process
// died before it finished.
func (l *lane) seek(to int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if to > l.cursor {
		l.cursor = to
	}
}

func (l *lane) stop() {
	l.mu.Lock()
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
