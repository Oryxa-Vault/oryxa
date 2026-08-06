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
	"strings"
	"sync"
	"time"

	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/events"
	"github.com/oryxa/oryxa/internal/sharedctx"
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

	mu    sync.Mutex
	state State

	// One lane per agent. Each carries that agent's queue, its conversation
	// handle, and its own turn loop — so turns are strictly ordered within an
	// agent and run in parallel across agents.
	lanes map[string]*lane

	hmu     sync.Mutex
	history []*Turn

	ctx *sharedctx.Store

	// Serialises the read-check-write of a value entry. The version check and
	// the write are separate operations with a log append between them, so
	// without this two writers holding the same version both pass the check and
	// both write — the lost update optimistic concurrency exists to prevent.
	// Per session, so different rooms never block each other.
	ctxWrite sync.Mutex

	closed chan struct{}
}

func (s *session) lane(agent string) *lane {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lanes[agent]
}

func (s *session) allLanes() []*lane {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*lane, 0, len(s.lanes))
	for _, a := range s.agents {
		if l := s.lanes[a]; l != nil {
			out = append(out, l)
		}
	}
	return out
}

// derivedState reports running when any lane is busy. A room is working if
// anyone in it is.
func (s *session) derivedState() State {
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return StateClosed
	}
	s.mu.Unlock()
	for _, l := range s.allLanes() {
		if l.running() {
			return StateRunning
		}
	}
	return StateIdle
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
		id:      "s_" + randHex(8),
		agents:  list,
		created: time.Now().UTC(),
		state:   StateIdle,
		lanes:   map[string]*lane{},
		ctx:     sharedctx.New(),
		closed:  make(chan struct{}),
	}
	for _, a := range list {
		s.lanes[a] = newLane(a)
	}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()

	m.emit(s.id, "session.created", "", "", map[string]any{
		"agent": list[0], "agents": list,
	})
	m.startLanes(s)
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
	v := View{Summary: s.summary()}

	// Merge the lanes. Several agents can be mid-turn at once, so "current" is
	// whichever lane answered first; the queue is everything still waiting.
	for _, l := range s.allLanes() {
		q, cur := l.snapshot()
		v.Queue = append(v.Queue, q...)
		if cur != nil && v.Current == nil {
			v.Current = cur
		}
	}
	s.hmu.Lock()
	v.History = copyTurns(s.history)
	s.hmu.Unlock()
	return v, true
}

// Submit queues input. Anyone in the room may send; the author travels with it.
// One input becomes one turn per agent — everyone in the room asked everyone in
// the room — and each lands in that agent's own lane.
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
	agents := append([]string(nil), s.agents...)
	s.mu.Unlock()

	group := "g_" + randHex(6)
	var first Turn
	for i, a := range agents {
		l := s.lane(a)
		if l == nil {
			continue
		}
		t := &Turn{
			ID: "t_" + randHex(6), Agent: a, Author: author,
			Text: text, State: TurnQueued, Group: group,
		}
		// Copy before enqueueing, not after. enqueue publishes the turn to the
		// lane's goroutine, which marks it running under the lane lock the moment
		// it picks it up — so an unlocked read afterwards races, and the value
		// that loses is the one returned to the caller as their submit response.
		if i == 0 {
			first = *t
		}
		pos := l.enqueue(t)
		m.emit(s.id, "input.submitted", author, t.ID, map[string]any{
			"text": text, "position": pos, "agent": a, "group": group,
		})
		l.nudge()
	}
	return first, nil
}

// Withdraw removes a queued turn. Only queued turns can be withdrawn — a
// running turn is the agent's business, and cancel is the tool for that.
func (m *Manager) Withdraw(id, turnID, actor string) error {
	s, ok := m.get(id)
	if !ok {
		return ErrNoSession
	}
	for _, l := range s.allLanes() {
		if l.withdraw(turnID) {
			m.emit(s.id, "input.withdrawn", actor, turnID, nil)
			return nil
		}
	}
	return ErrNoTurn
}

// Cancel stops every turn currently running in the room.
func (m *Manager) Cancel(id, actor string) error {
	s, ok := m.get(id)
	if !ok {
		return ErrNoSession
	}
	stopped := 0
	for _, l := range s.allLanes() {
		if l.running() {
			l.stop()
			stopped++
		}
	}
	if stopped == 0 {
		return ErrNoTurn
	}
	m.emit(s.id, "turn.cancel_requested", actor, "", map[string]int{"lanes": stopped})
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
	close(s.closed)
	s.mu.Unlock()

	for _, l := range s.allLanes() {
		l.stop()
		l.nudge() // wake the loop so it observes the close and exits
	}
	m.emit(s.id, "session.closed", actor, "", nil)
	return nil
}

func (s *session) summary() Summary {
	state := s.derivedState()
	s.mu.Lock()
	sum := s.summaryLocked()
	s.mu.Unlock()
	sum.State = state
	return sum
}

func (s *session) summaryLocked() Summary {
	sum := Summary{
		ID: s.id, Agents: append([]string(nil), s.agents...),
		State: s.state, Created: s.created,
		Handles: map[string]string{},
	}
	for a, l := range s.lanes {
		l.mu.Lock()
		if l.handle != "" {
			sum.Handles[a] = l.handle
		}
		l.mu.Unlock()
	}
	if len(s.agents) > 0 {
		sum.Agent = s.agents[0]
		sum.Handle = sum.Handles[s.agents[0]]
	}
	return sum
}

// startLanes runs one goroutine per agent. Turns are ordered within a lane and
// parallel across lanes, which is the actual constraint: an agent's own
// conversation is sequential, two agents' conversations are independent.
func (m *Manager) startLanes(s *session) {
	for _, l := range s.allLanes() {
		go m.laneLoop(s, l)
	}
}

func (m *Manager) laneLoop(s *session, l *lane) {
	for {
		select {
		case <-s.closed:
			return
		case <-l.wake:
		}
		for {
			s.mu.Lock()
			closed := s.state == StateClosed
			s.mu.Unlock()
			if closed {
				break
			}
			t := l.take()
			if t == nil {
				break
			}
			m.run(s, l, t)
			l.done(t, &s.history, &s.hmu)
		}
	}
}

func (m *Manager) run(s *session, l *lane, t *Turn) {
	agent := l.agent
	spec, ok := m.reg.Get(agent)
	if !ok {
		finish(l, t, TurnFailed, "agent no longer registered: "+agent, "")
		m.emit(s.id, "turn.failed", agent, t.ID, map[string]string{"error": "agent no longer registered"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), spec.TimeoutDuration())
	l.mu.Lock()
	l.cancel = cancel
	captures := cloneMap(l.caps)
	handle := l.handle
	opened := l.opened
	l.mu.Unlock()

	author, text := t.Author, t.Text

	defer func() {
		cancel()
		l.mu.Lock()
		l.cancel = nil
		l.mu.Unlock()
	}()

	// Snapshot the room before announcing the turn, and report what it held.
	// Reading it after the event would describe a different snapshot than the one
	// the agent is handed, which is exactly the discrepancy this is here to rule
	// out: the log records every part an agent sends back, and until now nothing
	// recorded what it was shown.
	view, seen := contextSnapshot(s.ctx, spec.Turn.ContextRefs())

	m.emit(s.id, "turn.started", author, t.ID, map[string]any{
		"text": text, "agent": agent, "context": seen,
	})

	tc := connector.Ctx{
		Input:        text,
		Turn:         t.ID,
		Conversation: s.id,
		Handle:       handle,
		Vars:         spec.Vars,
		Captures:     captures,
		Context:      view,
	}

	// Open lazily, once per agent: an agent that is not running yet should fail
	// its own first turn, not session creation and not the other lanes.
	if spec.Open != nil && !opened {
		h, caps, err := m.exec.Open(ctx, spec, tc)
		if err != nil {
			finish(l, t, TurnFailed, err.Error(), "")
			m.emit(s.id, "turn.failed", agent, t.ID, map[string]string{"error": err.Error()})
			return
		}
		l.mu.Lock()
		l.opened = true
		l.handle = h
		for k, v := range caps {
			l.caps[k] = v
		}
		captures = cloneMap(l.caps)
		l.mu.Unlock()

		tc.Handle, tc.Captures = h, captures
		m.emit(s.id, "session.opened", agent, t.ID, map[string]string{
			"handle": h, "agent": agent,
		})
	}

	// Raw chunks are kept only when a rule actually selects into them. A long
	// stream is a lot of JSON to hold for nothing, and the overwhelming majority
	// of connectors declare no rules at all.
	keepChunks := selectsFromPayload(spec)

	var out []byte
	var chunks []json.RawMessage
	parts, textParts := 0, 0
	err := m.exec.Turn(ctx, spec, tc, func(p connector.Part) {
		parts++
		if p.Kind == "text" {
			textParts++
			out = append(out, p.Text...)
		}
		if keepChunks && len(p.Raw) > 0 {
			chunks = append(chunks, p.Raw)
		}
		m.emit(s.id, "output.part", agent, t.ID, p)
	})

	switch {
	case err != nil && ctx.Err() != nil:
		finish(l, t, TurnCancelled, "cancelled", "")
		m.emit(s.id, "turn.cancelled", agent, t.ID, nil)
	case err != nil:
		// One agent failing must not take the room down: the other lanes are
		// already running independently and are unaffected.
		finish(l, t, TurnFailed, err.Error(), "")
		m.emit(s.id, "turn.failed", agent, t.ID, map[string]string{"error": err.Error()})
	default:
		finish(l, t, TurnDone, "", string(out))
		m.emit(s.id, "turn.finished", agent, t.ID, map[string]any{"chars": len(out)})
		// A turn that succeeds without saying anything is reported as such. It is
		// not an error — the request worked — but leaving it to look like any
		// other success means the room simply goes quiet, and the framework in the
		// middle gets blamed for whatever actually happened upstream.
		if strings.TrimSpace(string(out)) == "" {
			m.emit(s.id, "turn.empty", agent, t.ID, emptyTurn(parts, textParts, spec))
		}
		// After turn.finished, so the log reads in the order things happened:
		// the agent answered, and then the room changed because of it.
		m.applyContextRules(s.id, spec, agent, string(out), chunks)
	}
}

// emptyTurn says why a turn produced no text. Two cases, because two is what is
// actually observable here, and they have different culprits:
//
//	nothing arrived    upstream: a budget spent on reasoning before the agent
//	                   answered, a rate limit, a model that declined
//	nothing readable   the payload arrived and no text came out of it
//
// The second does not narrow further. A selector that does not fit the payload
// and an agent that answered with an empty string both reach the executor and
// leave as activity rather than text, so claiming which one it was would be a
// guess — and a confident wrong guess sends someone to the wrong file.
//
// What is offered instead is the selectors themselves, because reading them
// against the raw view is the step that actually settles it, and having them
// here saves opening the connector to find out what they were.
func emptyTurn(parts, textParts int, spec *connector.Spec) map[string]any {
	d := map[string]any{"parts": parts, "text_parts": textParts}
	if parts == 0 {
		d["reason"] = "the agent sent nothing at all"
		return d
	}
	d["reason"] = fmt.Sprintf(
		"the agent sent %d parts and no text came out of them; check these selectors against the raw view", parts)
	if spec.Turn != nil && spec.Turn.Response != nil {
		d["text"] = spec.Turn.Response.Text
		if w := spec.Turn.Response.When; w != "" {
			d["when"] = w
		}
	}
	return d
}

func selectsFromPayload(spec *connector.Spec) bool {
	for _, r := range spec.Context {
		if !r.FromText() {
			return true
		}
	}
	return false
}

// finish mutates the turn under its lane's lock, so a reader taking a snapshot
// can never encode a half-written struct.
func finish(l *lane, t *Turn, state TurnState, errMsg, output string) {
	l.mu.Lock()
	defer l.mu.Unlock()
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
				m.startLanes(s)
				for _, l := range s.allLanes() {
					l.nudge() // anything re-queued starts immediately
				}
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
		id:     id,
		state:  StateIdle,
		lanes:  map[string]*lane{},
		ctx:    sharedctx.New(),
		closed: make(chan struct{}),
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
			Key    string   `json:"key"`
			Value  string   `json:"value"`
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
			for _, a := range s.agents {
				s.lanes[a] = newLane(a)
			}
		case "session.opened":
			if d.Agent != "" {
				if l := s.lanes[d.Agent]; l != nil {
					l.handle, l.opened = d.Handle, true
				}
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

		// Shared context is a fold over the same log, so it comes back with
		// everything else rather than needing its own persistence.
		case "context.appended":
			_, _ = s.ctx.Append(d.Key, ev.Actor, d.Text, ev.Seq, ev.TS)
		case "context.set":
			_, _ = s.ctx.Set(d.Key, ev.Actor, d.Value, -1, ev.Seq, ev.TS)
		case "context.pinned":
			_, _ = s.ctx.Pin(d.Key, true)
		case "context.unpinned":
			_, _ = s.ctx.Pin(d.Key, false)
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
		// Route each recovered turn back to its own lane.
		l := s.lanes[t.Agent]
		if l == nil && len(s.agents) > 0 {
			l = s.lanes[s.agents[0]]
		}
		switch t.State {
		case TurnQueued:
			if l != nil {
				l.queue = append(l.queue, t)
			}
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

// ---- shared context ----

// Context returns a snapshot of the room's shared state.
func (m *Manager) Context(id string) ([]sharedctx.Entry, bool) {
	s, ok := m.get(id)
	if !ok {
		return nil, false
	}
	return s.ctx.Snapshot(), true
}

// AppendContext adds to an add-only entry. It cannot conflict.
func (m *Manager) AppendContext(id, key, by, text string) (sharedctx.Entry, error) {
	s, ok := m.get(id)
	if !ok {
		return sharedctx.Entry{}, ErrNoSession
	}
	// The event is written first so the log stays the source of truth and the
	// entry carries the sequence number the write actually landed at.
	ev, err := m.log.Append(s.id, "context.appended", by, "", map[string]any{
		"key": key, "text": text,
	})
	if err != nil {
		m.logFailure(s.id, "context.appended", err)
		return sharedctx.Entry{}, err
	}
	return s.ctx.Append(key, by, text, ev.Seq, ev.TS)
}

// SetContext writes a value entry under optimistic concurrency. ifMatch is the
// version the writer last saw; -1 overwrites regardless.
func (m *Manager) SetContext(id, key, by, value string, ifMatch int64) (sharedctx.Entry, error) {
	s, ok := m.get(id)
	if !ok {
		return sharedctx.Entry{}, ErrNoSession
	}

	s.ctxWrite.Lock()
	defer s.ctxWrite.Unlock()

	// Check the precondition before writing an event: a refused write must not
	// leave a record claiming it happened.
	//
	// Both halves matter. A version that does not match the current one is the
	// obvious conflict; a version held for a key that was never written is the
	// quiet one, and creating the entry anyway would tell a caller its update
	// succeeded when what it believed it was updating never existed.
	cur, exists := s.ctx.Get(key)
	switch {
	case exists && ifMatch != -1 && cur.Version != ifMatch:
		m.emit(s.id, "conflict.rejected", by, "", map[string]any{
			"key": key, "expected": ifMatch, "current": cur.Version,
		})
		return sharedctx.Entry{}, &sharedctx.Conflict{
			Key: key, Current: cur.Value, Version: cur.Version, By: cur.By,
		}
	case !exists && ifMatch > 0:
		m.emit(s.id, "conflict.rejected", by, "", map[string]any{
			"key": key, "expected": ifMatch, "current": 0,
		})
		return sharedctx.Entry{}, &sharedctx.Conflict{Key: key, Version: 0}
	}

	ev, err := m.log.Append(s.id, "context.set", by, "", map[string]any{
		"key": key, "value": value,
	})
	if err != nil {
		m.logFailure(s.id, "context.set", err)
		return sharedctx.Entry{}, err
	}
	return s.ctx.Set(key, by, value, -1, ev.Seq, ev.TS)
}

func (m *Manager) PinContext(id, key, by string, pinned bool) (sharedctx.Entry, error) {
	s, ok := m.get(id)
	if !ok {
		return sharedctx.Entry{}, ErrNoSession
	}
	if _, exists := s.ctx.Get(key); !exists {
		return sharedctx.Entry{}, sharedctx.ErrNotFound
	}
	kind := "context.unpinned"
	if pinned {
		kind = "context.pinned"
	}
	m.emit(s.id, kind, by, "", map[string]any{"key": key})
	return s.ctx.Pin(key, pinned)
}
