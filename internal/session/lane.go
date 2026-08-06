package session

import (
	"context"
	"sync"
)

// A lane is one agent's place in a room: its own queue, its own turn loop, its
// own conversation with its own framework.
//
// The serialization requirement is per agent, not per session. An agent's
// conversation is sequential, so its turns must not overlap — but two agents
// have two conversations and no reason to wait on each other. One goroutine per
// lane gives exactly that: strict order within an agent, full parallelism
// across them. A five-agent room answers in the time of its slowest agent
// rather than the sum of all five.
type lane struct {
	agent string

	mu      sync.Mutex
	queue   []*Turn
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

func (l *lane) enqueue(t *Turn) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queue = append(l.queue, t)
	return len(l.queue)
}

// take pops the next turn and marks it running. Returns nil when the lane is
// empty, which is the loop's signal to go back to sleep.
func (l *lane) take() *Turn {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) == 0 {
		return nil
	}
	t := l.queue[0]
	l.queue = l.queue[1:]
	t.State = TurnRunning
	l.current = t
	return t
}

func (l *lane) done(t *Turn, history *[]*Turn, hmu *sync.Mutex) {
	l.mu.Lock()
	l.current = nil
	l.mu.Unlock()

	hmu.Lock()
	*history = append(*history, t)
	hmu.Unlock()
}

func (l *lane) withdraw(turnID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, t := range l.queue {
		if t.ID == turnID {
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			return true
		}
	}
	return false
}

func (l *lane) running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current != nil
}

func (l *lane) snapshot() (queue []Turn, current *Turn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range l.queue {
		queue = append(queue, *t)
	}
	if l.current != nil {
		c := *l.current
		current = &c
	}
	return queue, current
}

func (l *lane) stop() {
	l.mu.Lock()
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
