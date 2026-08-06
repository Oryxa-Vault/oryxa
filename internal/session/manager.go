// Package session owns the room: many people, one agent, one turn at a time.
//
// Turns are serialized because the agent's own conversation is serialized. A
// session that ran turns concurrently would be lying about what is underneath,
// so input arriving mid-turn queues rather than interleaving.
//
// Concurrency rule: every mutable field of a Session or Turn is guarded by
// Session.mu, and nothing outside this package ever receives a pointer to one.
// Readers get value copies, so an HTTP handler can never encode a struct while
// the turn loop is writing it.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/events"
)

type State string

const (
	StateIdle    State = "idle"
	StateRunning State = "running"
	StateClosed  State = "closed"
)

type TurnState string

const (
	TurnQueued    TurnState = "queued"
	TurnRunning   TurnState = "running"
	TurnDone      TurnState = "done"
	TurnFailed    TurnState = "failed"
	TurnCancelled TurnState = "cancelled"
)

type Turn struct {
	ID     string    `json:"id"`
	Agent  string    `json:"agent"`
	Author string    `json:"author"`
	Text   string    `json:"text"`
	State  TurnState `json:"state"`
	Error  string    `json:"error,omitempty"`
	Output string    `json:"output,omitempty"`

	// Group ties together the turns produced by one input, so a client can
	// show one question with several answers instead of repeating it.
	Group string `json:"group,omitempty"`
}

// Summary is the value returned by create and list.
type Summary struct {
	ID      string            `json:"id"`
	Agent   string            `json:"agent"` // first agent; kept for older clients
	Agents  []string          `json:"agents"`
	State   State             `json:"state"`
	Handle  string            `json:"handle,omitempty"`
	Handles map[string]string `json:"handles,omitempty"`
	Created time.Time         `json:"created"`
}

// View is the snapshot returned by GET /sessions/{id}.
type View struct {
	Summary
	Queue   []Turn `json:"queue"`
	Current *Turn  `json:"current,omitempty"`
	History []Turn `json:"history"`
}

type session struct {
	id      string
	agents  []string
	created time.Time

	mu      sync.Mutex
	state   State
	queue   []*Turn
	current *Turn
	history []*Turn

	// Each agent keeps its own conversation with its own framework, so handles
	// and captures are per agent rather than per session.
	handles  map[string]string
	captures map[string]map[string]string
	opened   map[string]bool

	cancel context.CancelFunc

	wake   chan struct{}
	closed chan struct{}
}

var (
	ErrNoSession = errors.New("session not found")
	ErrNoAgent   = errors.New("agent not registered")
	ErrClosed    = errors.New("session is closed")
	ErrNoTurn    = errors.New("turn not found or already running")
)

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*session

	reg  *connector.Registry
	exec *connector.Executor
	log  events.Store

	failMu         sync.Mutex
	appendFailures int
}

func NewManager(reg *connector.Registry, exec *connector.Executor, log events.Store) *Manager {
	return &Manager{
		sessions: map[string]*session{},
		reg:      reg,
		exec:     exec,
		log:      log,
	}
}

// Create opens a room. More than one agent may be present: input then fans out
// to each of them, one turn apiece, still one turn at a time.
func (m *Manager) Create(agents ...string) (Summary, error) {
	if len(agents) == 0 {
		return Summary{}, fmt.Errorf("%w: none given", ErrNoAgent)
	}
	seen := map[string]bool{}
	var list []string
	for _, a := range agents {
		if a == "" || seen[a] {
			continue
		}
		if _, ok := m.reg.Get(a); !ok {
			return Summary{}, fmt.Errorf("%w: %s", ErrNoAgent, a)
		}
		seen[a] = true
		list = append(list, a)
	}

	s := &session{
		id:       "s_" + randHex(8),
		agents:   list,
		created:  time.Now().UTC(),
		state:    StateIdle,
		handles:  map[string]string{},
		captures: map[string]map[string]string{},
		opened:   map[string]bool{},
		wake:     make(chan struct{}, 1),
		closed:   make(chan struct{}),
	}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()

	m.emit(s.id, "session.created", "", "", map[string]any{
		"agent": list[0], "agents": list,
	})
	go m.loop(s)
	return s.summary(), nil
}

// emit writes to the log. Appends are best-effort at the call site but never
// silent: a durable store that starts failing must be visible, because replay,
// audit and recovery are all reading what this wrote.
func (m *Manager) emit(sessionID, kind, actor, turn string, data any) {
	if _, err := m.log.Append(sessionID, kind, actor, turn, data); err != nil {
		m.logFailure(sessionID, kind, err)
	}
}

func (m *Manager) logFailure(sessionID, kind string, err error) {
	m.failMu.Lock()
	m.appendFailures++
	n := m.appendFailures
	m.failMu.Unlock()
	fmt.Fprintf(os.Stderr, "oryxa: event append failed (session=%s kind=%s, %d total): %v\n",
		sessionID, kind, n, err)
}

// AppendFailures reports how many log writes have failed. Non-zero means the
// session history on disk is incomplete.
func (m *Manager) AppendFailures() int {
	m.failMu.Lock()
	defer m.failMu.Unlock()
	return m.appendFailures
}

func (m *Manager) get(id string) (*session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Exists reports whether a session id is known, without exposing internals.
func (m *Manager) Exists(id string) bool {
	_, ok := m.get(id)
	return ok
}

func (m *Manager) List() []Summary {
	m.mu.RLock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.RUnlock()

	out := make([]Summary, 0, len(all))
	for _, s := range all {
		out = append(out, s.summary())
	}
	// Newest first, and stable: map order is randomised, so an unsorted list
	// would reshuffle under a client that polls it.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created.Equal(out[j].Created) {
			return out[i].ID < out[j].ID
		}
		return out[i].Created.After(out[j].Created)
	})
	return out
}

func (m *Manager) View(id string) (View, bool) {
	s, ok := m.get(id)
	if !ok {
		return View{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	v := View{
		Summary: s.summaryLocked(),
		Queue:   copyTurns(s.queue),
		History: copyTurns(s.history),
	}
	if s.current != nil {
		cur := *s.current
		v.Current = &cur
	}
	return v, true
}

// Submit queues input. Anyone in the room may send; the author travels with it.
func (m *Manager) Submit(id, author, text string) (Turn, error) {
	s, ok := m.get(id)
	if !ok {
		return Turn{}, ErrNoSession
	}
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return Turn{}, ErrClosed
	}
	// One input becomes one turn per agent present. Everyone in the room asked
	// everyone in the room.
	group := "g_" + randHex(6)
	var made []*Turn
	for _, a := range s.agents {
		t := &Turn{
			ID: "t_" + randHex(6), Agent: a, Author: author,
			Text: text, State: TurnQueued, Group: group,
		}
		s.queue = append(s.queue, t)
		made = append(made, t)
	}
	pos := len(s.queue)
	out := *made[0]
	s.mu.Unlock()

	for _, t := range made {
		m.emit(s.id, "input.submitted", author, t.ID, map[string]any{
			"text": text, "position": pos, "agent": t.Agent, "group": group,
		})
	}
	s.nudge()
	return out, nil
}

// Withdraw removes a queued turn. Only queued turns can be withdrawn — a
// running turn is the agent's business, and cancel is the tool for that.
func (m *Manager) Withdraw(id, turnID, actor string) error {
	s, ok := m.get(id)
	if !ok {
		return ErrNoSession
	}
	s.mu.Lock()
	for i, t := range s.queue {
		if t.ID == turnID {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.mu.Unlock()
			m.emit(s.id, "input.withdrawn", actor, turnID, nil)
			return nil
		}
	}
	s.mu.Unlock()
	return ErrNoTurn
}

func (m *Manager) Cancel(id, actor string) error {
	s, ok := m.get(id)
	if !ok {
		return ErrNoSession
	}
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		return ErrNoTurn
	}
	cancel()
	m.emit(s.id, "turn.cancel_requested", actor, "", nil)
	return nil
}

func (m *Manager) Close(id, actor string) error {
	s, ok := m.get(id)
	if !ok {
		return ErrNoSession
	}
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return nil
	}
	s.state = StateClosed
	if s.cancel != nil {
		s.cancel()
	}
	close(s.closed)
	s.mu.Unlock()
	m.emit(s.id, "session.closed", actor, "", nil)
	return nil
}

func (s *session) summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summaryLocked()
}

func (s *session) summaryLocked() Summary {
	sum := Summary{
		ID: s.id, Agents: append([]string(nil), s.agents...),
		State: s.state, Created: s.created,
		Handles: cloneMap(s.handles),
	}
	if len(s.agents) > 0 {
		sum.Agent = s.agents[0]
		sum.Handle = s.handles[s.agents[0]]
	}
	return sum
}

func (s *session) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// loop is the serialization point: one goroutine per session, one turn at a time.
func (m *Manager) loop(s *session) {
	for {
		select {
		case <-s.closed:
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			if s.state == StateClosed || len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			t := s.queue[0]
			s.queue = s.queue[1:]
			t.State = TurnRunning
			s.current = t
			s.state = StateRunning
			s.mu.Unlock()

			m.run(s, t)

			s.mu.Lock()
			s.current = nil
			s.history = append(s.history, t)
			if s.state != StateClosed {
				s.state = StateIdle
			}
			s.mu.Unlock()
		}
	}
}

func (m *Manager) run(s *session, t *Turn) {
	agent := t.Agent
	spec, ok := m.reg.Get(agent)
	if !ok {
		s.finish(t, TurnFailed, "agent no longer registered: "+agent, "")
		m.emit(s.id, "turn.failed", agent, t.ID, map[string]string{"error": "agent no longer registered"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), spec.TimeoutDuration())
	s.mu.Lock()
	s.cancel = cancel
	captures := cloneMap(s.captures[agent])
	handle := s.handles[agent]
	opened := s.opened[agent]
	author, text := t.Author, t.Text
	s.mu.Unlock()

	defer func() {
		cancel()
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()

	m.emit(s.id, "turn.started", author, t.ID, map[string]string{
		"text": text, "agent": agent,
	})

	tc := connector.Ctx{
		Input:        text,
		Turn:         t.ID,
		Conversation: s.id,
		Handle:       handle,
		Vars:         spec.Vars,
		Captures:     captures,
	}

	// Open lazily, once per agent: an agent that is not running yet should fail
	// its own first turn, not session creation and not the other agents.
	if spec.Open != nil && !opened {
		h, caps, err := m.exec.Open(ctx, spec, tc)
		if err != nil {
			s.finish(t, TurnFailed, err.Error(), "")
			m.emit(s.id, "turn.failed", agent, t.ID, map[string]string{"error": err.Error()})
			return
		}
		s.mu.Lock()
		s.opened[agent] = true
		s.handles[agent] = h
		if s.captures[agent] == nil {
			s.captures[agent] = map[string]string{}
		}
		for k, v := range caps {
			s.captures[agent][k] = v
		}
		captures = cloneMap(s.captures[agent])
		s.mu.Unlock()

		tc.Handle, tc.Captures = h, captures
		m.emit(s.id, "session.opened", agent, t.ID, map[string]string{
			"handle": h, "agent": agent,
		})
	}

	var out []byte
	err := m.exec.Turn(ctx, spec, tc, func(p connector.Part) {
		if p.Kind == "text" {
			out = append(out, p.Text...)
		}
		m.emit(s.id, "output.part", agent, t.ID, p)
	})

	switch {
	case err != nil && ctx.Err() != nil:
		s.finish(t, TurnCancelled, "cancelled", "")
		m.emit(s.id, "turn.cancelled", agent, t.ID, nil)
	case err != nil:
		// One agent failing must not take the room down: the other turns in
		// this group still run.
		s.finish(t, TurnFailed, err.Error(), "")
		m.emit(s.id, "turn.failed", agent, t.ID, map[string]string{"error": err.Error()})
	default:
		s.finish(t, TurnDone, "", string(out))
		m.emit(s.id, "turn.finished", agent, t.ID, map[string]any{"chars": len(out)})
	}
}

func (s *session) finish(t *Turn, state TurnState, errMsg, output string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t.State, t.Error, t.Output = state, errMsg, output
}

func copyTurns(in []*Turn) []Turn {
	out := make([]Turn, 0, len(in))
	for _, t := range in {
		out = append(out, *t)
	}
	return out
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000")))
	}
	return hex.EncodeToString(b)
}

// ---- restart recovery ----

// Rehydrate rebuilds sessions from the log. Because a session is a fold over
// its events, recovery is a replay rather than a separate persistence path —
// which is the payoff of making the log the source of truth.
//
// Turns are treated by what the log last said about them:
//
//	queued        never started, so it is safe to run now — re-queued
//	running       outcome unknown: the agent may well have finished after we
//	              died. Re-running risks doing the work twice, so it is marked
//	              interrupted instead. Guessing quietly is the one option that
//	              would be wrong either way.
//	done/failed   left as history
func (m *Manager) Rehydrate() (int, error) {
	ids, err := m.log.Sessions()
	if err != nil {
		return 0, fmt.Errorf("list sessions: %w", err)
	}
	n := 0
	for _, id := range ids {
		evs, err := m.log.Since(id, 0)
		if err != nil {
			return n, fmt.Errorf("read %s: %w", id, err)
		}
		if s := rebuild(id, evs); s != nil {
			m.mu.Lock()
			m.sessions[id] = s
			m.mu.Unlock()
			if s.state != StateClosed {
				go m.loop(s)
				s.nudge() // anything re-queued starts immediately
			}
			n++
		}
	}
	return n, nil
}

func rebuild(id string, evs []events.Event) *session {
	if len(evs) == 0 {
		return nil
	}
	s := &session{
		id:       id,
		state:    StateIdle,
		handles:  map[string]string{},
		captures: map[string]map[string]string{},
		opened:   map[string]bool{},
		wake:     make(chan struct{}, 1),
		closed:   make(chan struct{}),
	}
	turns := map[string]*Turn{}
	var order []string

	for _, ev := range evs {
		var d struct {
			Agent  string   `json:"agent"`
			Agents []string `json:"agents"`
			Handle string   `json:"handle"`
			Text   string   `json:"text"`
			Group  string   `json:"group"`
			Error  string   `json:"error"`
			Kind   string   `json:"kind"`
			Part   string   `json:"text_part"`
		}
		if len(ev.Data) > 0 {
			_ = json.Unmarshal(ev.Data, &d)
		}

		switch ev.Kind {
		case "session.created":
			s.created = ev.TS
			if len(d.Agents) > 0 {
				s.agents = d.Agents
			} else if d.Agent != "" {
				s.agents = []string{d.Agent}
			}
		case "session.opened":
			if d.Agent != "" {
				s.handles[d.Agent] = d.Handle
				s.opened[d.Agent] = true
			}
		case "session.closed":
			s.state = StateClosed
		case "input.submitted":
			if _, seen := turns[ev.Turn]; seen {
				continue
			}
			t := &Turn{
				ID: ev.Turn, Agent: d.Agent, Author: ev.Actor,
				Text: d.Text, State: TurnQueued, Group: d.Group,
			}
			turns[ev.Turn] = t
			order = append(order, ev.Turn)
		case "input.withdrawn":
			delete(turns, ev.Turn)
		case "turn.started":
			if t := turns[ev.Turn]; t != nil {
				t.State = TurnRunning
			}
		case "output.part":
			// Rebuild the answer from the text parts already recorded, so a
			// completed turn reads the same after a restart as before it.
			if t := turns[ev.Turn]; t != nil && d.Kind == "text" {
				var p struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				}
				if json.Unmarshal(ev.Data, &p) == nil && p.Kind == "text" {
					t.Output += p.Text
				}
			}
		case "turn.finished":
			if t := turns[ev.Turn]; t != nil {
				t.State = TurnDone
			}
		case "turn.failed":
			if t := turns[ev.Turn]; t != nil {
				t.State, t.Error = TurnFailed, d.Error
			}
		case "turn.cancelled":
			if t := turns[ev.Turn]; t != nil {
				t.State = TurnCancelled
			}
		}
	}

	if s.created.IsZero() {
		s.created = evs[0].TS
	}
	for _, tid := range order {
		t := turns[tid]
		if t == nil {
			continue
		}
		switch t.State {
		case TurnQueued:
			s.queue = append(s.queue, t)
		case TurnRunning:
			t.State = TurnFailed
			t.Error = "interrupted by restart; outcome unknown"
			s.history = append(s.history, t)
		default:
			s.history = append(s.history, t)
		}
	}
	if s.state == StateClosed {
		close(s.closed)
	}
	return s
}
