// Package session owns the room: many people, many agents, one event log.
//
// Three things shape everything here.
//
// Serialization is per agent, not per room. An agent's own conversation is
// sequential so its turns must not overlap, but two agents have two
// conversations and no reason to wait on each other. One goroutine per lane
// gives strict order within an agent and full parallelism across them.
//
// Listening and speaking are different acts. Saying something appends one event
// and schedules nothing; a lane holds a cursor into what the room has said and
// takes everything since it as a single turn when it next speaks. So a message
// arriving mid-turn costs a round rather than a turn, an agent that stays quiet
// owns no pending work, and being behind the speaker stops being a backlog.
//
// Concurrency rule: a lane's mutable state is guarded by its own mutex, the
// room's inbox by its own, and nothing outside this package ever receives a
// pointer to either. Readers get value copies, so an HTTP handler can never
// encode a struct while a turn loop is writing it.
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
	// Waiting is what has been said that no lane has read yet. It replaces the
	// old per-agent queue, which no longer exists: an agent that has not caught
	// up owes no turns, it is simply further back in the room.
	Waiting []Input `json:"waiting,omitempty"`
	Current *Turn   `json:"current,omitempty"`
	History []Turn  `json:"history"`
}

type session struct {
	id      string
	agents  []string
	created time.Time

	// The hash of the secret that opens this room. See access.go — the plaintext
	// is returned once, at creation, and never stored anywhere.
	secretHash string

	mu    sync.Mutex
	state State

	// One lane per agent: a cursor into the inbox, a conversation handle, and a
	// turn loop — so turns are strictly ordered within an agent and run in
	// parallel across agents.
	lanes map[string]*lane

	hmu     sync.Mutex
	history []*Turn

	// Everything said to the room, in order. Append-only, and never compacted:
	// positions are what lane cursors point at, so removing one would silently
	// move every lane backwards.
	imu   sync.Mutex
	inbox []Input
	// Everyone who has said something here. A name is known to belong to a
	// person because a person used it, which needs no accounts and no config.
	speakers map[string]bool

	ctx *sharedctx.Store

	// Serialises the read-check-write of a value entry. The version check and
	// the write are separate operations with a log append between them, so
	// without this two writers holding the same version both pass the check and
	// both write — the lost update optimistic concurrency exists to prevent.
	// Per session, so different rooms never block each other.
	ctxWrite sync.Mutex

	// One summary per key in flight at a time.
	rollups rollupState

	closed chan struct{}
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

	// Connector that writes rollups. Empty means a long room keeps its
	// count-only marker rather than a summary.
	summariser string
}

func NewManager(reg *connector.Registry, exec *connector.Executor, log events.Store) *Manager {
	return &Manager{
		sessions: map[string]*session{},
		reg:      reg,
		exec:     exec,
		log:      log,
	}
}

// Create opens a room. More than one agent may be present; which of them
// answers any given message is decided when that message lands.
//
// It returns the room's secret, and this is the only time that secret exists in
// readable form. Whoever holds it can reach the room and nobody else can,
// including other holders of the API token.
func (m *Manager) Create(agents ...string) (Summary, string, error) {
	if len(agents) == 0 {
		return Summary{}, "", fmt.Errorf("%w: none given", ErrNoAgent)
	}
	seen := map[string]bool{}
	var list []string
	for _, a := range agents {
		if a == "" || seen[a] {
			continue
		}
		if _, ok := m.reg.Get(a); !ok {
			return Summary{}, "", fmt.Errorf("%w: %s", ErrNoAgent, a)
		}
		seen[a] = true
		list = append(list, a)
	}

	secret, hash := newSecret()
	s := &session{
		id:         "s_" + randHex(8),
		agents:     list,
		created:    time.Now().UTC(),
		state:      StateIdle,
		lanes:      map[string]*lane{},
		ctx:        sharedctx.New(),
		closed:     make(chan struct{}),
		secretHash: hash,
	}
	for _, a := range list {
		s.lanes[a] = newLane(a)
	}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()

	// Only the hash goes to the log. A room's own members can read its events,
	// so the plaintext there would mean anyone who ever had access keeps it
	// after the secret is rotated — and a database backup would carry every key
	// in the building.
	m.emit(s.id, "session.created", "", "", map[string]any{
		"agent": list[0], "agents": list, "secret_sha256": hash,
	})
	m.startLanes(s)
	return s.summary(), secret, nil
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
	inbox := s.inboxSnapshot()
	furthest := len(inbox)
	for _, l := range s.allLanes() {
		if cur := l.snapshot(); cur != nil && v.Current == nil {
			v.Current = cur
		}
		// Waiting is what the room's least caught-up lane has not read.
		if n := l.pending(inbox); n > 0 && len(inbox)-n < furthest {
			furthest = len(inbox) - n
		}
	}
	for _, in := range inbox[furthest:] {
		if !in.Withdrawn {
			v.Waiting = append(v.Waiting, in)
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
func (m *Manager) Submit(id, author, text string, to ...string) (Input, error) {
	s, ok := m.get(id)
	if !ok {
		return Input{}, ErrNoSession
	}
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return Input{}, ErrClosed
	}
	s.mu.Unlock()

	// One append, not one turn per agent. Who answers and when is a lane's
	// business; saying something is not scheduling work for anybody.
	s.mu.Lock()
	agents := append([]string(nil), s.agents...)
	s.mu.Unlock()

	s.imu.Lock()
	if s.speakers == nil {
		s.speakers = map[string]bool{}
	}
	s.speakers[author] = true
	// Decided once, when the message lands, so every lane and every replay agree
	// on who it was for — and so the room can show why an agent stayed quiet.
	w := decideWake(text, to, agents, m.reg, s.speakers)
	in := Input{
		ID:     "in_" + randHex(6),
		Seq:    len(s.inbox),
		Author: author,
		Text:   text,
		To:     to,
		Wake:   w.Agents,
		Why:    w.Why,
	}
	s.inbox = append(s.inbox, in)
	s.imu.Unlock()

	m.emit(s.id, "input.submitted", author, in.ID, map[string]any{
		"text": text, "seq": in.Seq, "group": in.ID,
		"wake": w.Agents, "why": w.Why,
	})
	for _, l := range s.allLanes() {
		l.nudge()
	}
	return in, nil
}

// inboxSnapshot is what a lane reads against. Copied under the lock because the
// slice header moves as the room fills, and a lane indexing a stale one would
// read past its end.
func (s *session) inboxSnapshot() []Input {
	s.imu.Lock()
	defer s.imu.Unlock()
	return append([]Input(nil), s.inbox...)
}

// Withdraw un-says something nobody has read yet.
//
// It marks rather than removes: inbox positions are what lane cursors point at,
// so deleting one would move every lane silently backwards. A lane that already
// read it keeps its answer — withdrawing is not un-saying it to whoever heard.
func (m *Manager) Withdraw(id, inputID, actor string) error {
	s, ok := m.get(id)
	if !ok {
		return ErrNoSession
	}
	s.imu.Lock()
	found := false
	for i := range s.inbox {
		if s.inbox[i].ID == inputID && !s.inbox[i].Withdrawn {
			s.inbox[i].Withdrawn = true
			found = true
			break
		}
	}
	s.imu.Unlock()
	if !found {
		return ErrNoTurn
	}
	m.emit(s.id, "input.withdrawn", actor, inputID, nil)
	return nil
}

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
			t := l.take(s.inboxSnapshot())
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

	ids := make([]string, 0, len(t.Inputs))
	for _, in := range t.Inputs {
		ids = append(ids, in.ID)
	}
	m.emit(s.id, "turn.started", author, t.ID, map[string]any{
		"text": text, "agent": agent, "context": seen, "inputs": ids,
	})

	tc := connector.Ctx{
		Input:        text,
		Turn:         t.ID,
		Conversation: s.id,
		Handle:       handle,
		Agent:        agent,
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
		// The log carries more than conversations. A system stream rebuilt as a
		// room would appear in the sidebar as an empty session nobody opened.
		if events.Reserved(id) {
			continue
		}
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
			Inputs []string `json:"inputs"`
			Wake   []string `json:"wake"`
			Why    string   `json:"why"`
			Error  string   `json:"error"`
			Kind   string   `json:"kind"`
			Key    string   `json:"key"`
			Value  string   `json:"value"`
			Secret string   `json:"secret_sha256"`
		}
		if len(ev.Data) > 0 {
			_ = json.Unmarshal(ev.Data, &d)
		}

		switch ev.Kind {
		case "session.created":
			s.created = ev.TS
			// Rooms created before scoping carry no hash. Left empty, and
			// Authorize refuses them rather than guessing — see Unscoped, which
			// names them at startup so an upgrade does not turn into a
			// disappearing room nobody can explain.
			s.secretHash = d.Secret
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
			// An input is a thing said, not work owed. It goes to the inbox;
			// which lanes read it, and when, is replayed from turn.started.
			if s.speakers == nil {
				s.speakers = map[string]bool{}
			}
			s.speakers[ev.Actor] = true
			s.inbox = append(s.inbox, Input{
				ID: ev.Turn, Seq: len(s.inbox), Author: ev.Actor, Text: d.Text,
				Wake: d.Wake, Why: d.Why,
			})
		case "input.withdrawn":
			for i := range s.inbox {
				if s.inbox[i].ID == ev.Turn {
					s.inbox[i].Withdrawn = true
					break
				}
			}
		case "turn.started":
			if _, seen := turns[ev.Turn]; seen {
				continue
			}
			t := &Turn{
				ID: ev.Turn, Agent: d.Agent, Author: ev.Actor,
				Text: d.Text, State: TurnRunning, Group: ev.Turn,
			}
			for _, id := range d.Inputs {
				for _, in := range s.inbox {
					if in.ID == id {
						t.Inputs = append(t.Inputs, in)
						break
					}
				}
			}
			if n := len(t.Inputs); n > 0 {
				t.Group = t.Inputs[n-1].ID
			}
			turns[ev.Turn] = t
			order = append(order, ev.Turn)
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
		case "context.rolled_up":
			var d struct {
				Key, Text string
				Covers    int
				Through   int64
			}
			if json.Unmarshal(ev.Data, &d) == nil {
				applyRolledUp(s, d.Key, d.Text, ev.Actor, d.Covers, d.Through)
			}
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
		// A turn that ran consumed its inputs, so the cursor moves past them
		// whatever became of the turn. Re-reading them because the process died
		// would ask the agent the same question twice.
		if l != nil {
			for _, in := range t.Inputs {
				l.seek(in.Seq + 1)
			}
		}
		switch t.State {
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
	entry, err := s.ctx.Append(key, by, text, ev.Seq, ev.TS)
	if err == nil {
		// Off the turn path: a room's bookkeeping must not put the framework's
		// own latency in front of the answer someone asked for.
		m.maybeRollup(id, key)
	}
	return entry, err
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
