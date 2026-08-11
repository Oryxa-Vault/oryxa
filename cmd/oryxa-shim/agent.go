package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Agent describes how to run one command-line agent.
//
// It is to a process what a connector is to an HTTP endpoint: a description,
// not code. The same templating engine renders both, so `{{input}}` and
// `{{handle}}` mean the same thing in this file as they do in a connector.
type Agent struct {
	Name string `yaml:"name"`

	// Dir is where the command runs. For a coding agent this is the repository
	// it is allowed to see, which makes it a scope as much as a path.
	Dir string `yaml:"dir"`

	Timeout string `yaml:"timeout"`

	// Env adds to the inherited environment. The environment is inherited
	// rather than filtered because these agents authenticate out of a home
	// directory and a filtered one simply fails to log in — a restriction that
	// only breaks things is worse than no restriction, because it reads like
	// protection.
	Env map[string]string `yaml:"env"`

	// Session says where the id that carries a conversation comes from.
	//
	//	generate  we choose it, and pass it to the command
	//	capture   the command chooses it, and we read it out of the first turn
	//
	// Both exist because both are real: some CLIs accept an id you name, others
	// mint their own and tell you afterwards.
	Session string `yaml:"session"`

	// Capture selects the session id out of a turn's output, for session:
	// capture. Same path syntax as a connector's selectors.
	Capture string `yaml:"capture"`

	// First and Resume are complete argv, not a base plus extras.
	//
	// Deliberately repetitive. Resuming is not always a flag you append —
	// some CLIs make it a subcommand that changes the shape of the whole
	// command line — and a scheme clever enough to express that is a scheme
	// that breaks on the third agent.
	First  []string `yaml:"first"`
	Resume []string `yaml:"resume"`

	mu    sync.Mutex
	convs map[string]*conv
}

// conv is what the shim remembers about one conversation.
type conv struct {
	// remote is the id the command knows this conversation by. For session:
	// generate it is known from the start; for capture it appears after the
	// first turn.
	remote  string
	started bool
}

type file struct {
	Agents []*Agent `yaml:"agents"`
}

// Agents is the loaded set, by name.
type Agents struct {
	byName map[string]*Agent
}

func LoadFile(path string) (*Agents, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f file
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	out := &Agents{byName: map[string]*Agent{}}
	for i, a := range f.Agents {
		if err := a.validate(); err != nil {
			return nil, fmt.Errorf("agents[%d]: %w", i, err)
		}
		if _, dup := out.byName[a.Name]; dup {
			return nil, fmt.Errorf("agents[%d]: duplicate name %q", i, a.Name)
		}
		a.convs = map[string]*conv{}
		out.byName[a.Name] = a
	}
	return out, nil
}

func (r *Agents) Get(name string) (*Agent, bool) {
	a, ok := r.byName[name]
	return a, ok
}

func (r *Agents) List() []*Agent {
	out := make([]*Agent, 0, len(r.byName))
	for _, a := range r.byName {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (a *Agent) validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if len(a.First) == 0 {
		return fmt.Errorf("%s: first is required (the command to run)", a.Name)
	}
	switch a.sessionMode() {
	case "generate", "none":
	case "capture":
		if strings.TrimSpace(a.Capture) == "" {
			return fmt.Errorf("%s: session: capture needs a capture selector", a.Name)
		}
	default:
		return fmt.Errorf("%s: session must be generate, capture or none, got %q", a.Name, a.Session)
	}
	// Resuming with nothing to resume by would silently start a fresh
	// conversation every turn, which looks like an agent with no memory rather
	// than like a misconfiguration.
	if len(a.Resume) > 0 && a.sessionMode() == "none" {
		return fmt.Errorf("%s: resume is set but session is none, so there is no id to resume by", a.Name)
	}
	if a.Timeout != "" {
		if _, err := time.ParseDuration(a.Timeout); err != nil {
			return fmt.Errorf("%s: timeout %q is not a duration: %w", a.Name, a.Timeout, err)
		}
	}
	return nil
}

func (a *Agent) sessionMode() string {
	if strings.TrimSpace(a.Session) == "" {
		return "none"
	}
	return strings.TrimSpace(a.Session)
}

// TimeoutDuration defaults to fifteen minutes.
//
// Far longer than the connector default, because these agents are not answering
// a question — they are reading files, running builds and editing code, and a
// five-minute cap would cancel real work halfway through.
func (a *Agent) TimeoutDuration() time.Duration {
	if a.Timeout == "" {
		return 15 * time.Minute
	}
	d, err := time.ParseDuration(a.Timeout)
	if err != nil {
		return 15 * time.Minute
	}
	return d
}

// Summary is the one line the startup banner prints.
func (a *Agent) Summary() string {
	dir := a.Dir
	if dir == "" {
		dir = "."
	}
	kind := "single-turn"
	if len(a.Resume) > 0 {
		kind = "multi-turn (" + a.sessionMode() + ")"
	}
	return fmt.Sprintf("%-24s in %s · %s", a.First[0], dir, kind)
}

// Open registers a conversation and returns the handle turns will carry.
func (a *Agent) Open(conversation string) string {
	handle := strings.TrimSpace(conversation)
	if handle == "" {
		handle = uuid()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	c := &conv{}
	if a.sessionMode() == "generate" {
		// A fresh id per conversation rather than reusing the Oryxa session id:
		// several lanes in one room share that id, so handing it to the command
		// would put four differently-briefed agents in one conversation.
		c.remote = uuid()
	}
	a.convs[handle] = c
	return handle
}

// state returns the conversation record, creating one if the handle is unknown.
//
// Unknown is normal rather than exceptional: a shim restart loses the map while
// the room keeps the handle. Starting a fresh command session is the honest
// degradation — the agent has forgotten, and pretending otherwise by resuming an
// id the command never issued would fail on every turn from then on.
func (a *Agent) state(handle string) *conv {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.convs[handle]
	if !ok {
		c = &conv{}
		if a.sessionMode() == "generate" {
			c.remote = uuid()
		}
		a.convs[handle] = c
	}
	return c
}

// argv picks the command line for this turn and reports the id it resumes by.
func (a *Agent) argv(c *conv) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Resume only once there is something to resume: a capture-mode first turn
	// has no id yet, and a first turn that failed before naming its session
	// must be retried as a first turn rather than resumed into nothing.
	if len(a.Resume) > 0 && c.started && c.remote != "" {
		return a.Resume
	}
	return a.First
}

func (a *Agent) handleOf(c *conv) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return c.remote
}

// record marks a turn as run, and stores a captured session id.
func (a *Agent) record(c *conv, captured string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c.started = true
	if captured != "" && a.sessionMode() == "capture" {
		c.remote = captured
	}
}

// uuid is a v4 UUID. Written here rather than pulled in as a dependency: this
// is the only place the program needs one, and a CLI that asks for a session id
// usually wants exactly this shape.
func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice, and a shim that refuses to
		// start a conversation is worse than one with a time-derived id.
		return fmt.Sprintf("%016x-%016x", time.Now().UnixNano(), time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
