// Package connector describes how to call an agent over HTTP.
//
// Nothing in this package knows the name of any agent framework. A connector is
// a description of HTTP calls; the same spec must be able to express ADK,
// LangGraph, or a plain endpoint without favouring any of them.
package connector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Spec is a complete description of how to reach one agent.
type Spec struct {
	Name         string            `yaml:"name" json:"name"`
	Base         string            `yaml:"base" json:"base"`
	Headers      map[string]string `yaml:"headers" json:"headers,omitempty"`
	Vars         map[string]string `yaml:"vars" json:"vars,omitempty"`
	Capabilities []string          `yaml:"capabilities" json:"capabilities,omitempty"`

	// Interests are words that should bring this agent into a conversation it
	// was not directly asked to join. Matching is whole-word and
	// case-insensitive; nothing here is a model call.
	Interests []string `yaml:"interests" json:"interests,omitempty"`
	Timeout   string   `yaml:"timeout" json:"timeout,omitempty"`

	// Open runs once per Oryxa session, to establish continuity on the agent
	// side. Optional: agents without a conversation concept omit it.
	Open *Step `yaml:"open" json:"open,omitempty"`

	// Turn runs once per turn. Required.
	Turn *Step `yaml:"turn" json:"turn"`

	// Context is what this agent contributes back to the room after a turn.
	//
	// Declared here rather than inside turn.response because it is not a detail
	// of parsing: it is the answer to "what does this agent leave behind for
	// everyone else", which is the whole reason a room has shared state.
	Context []ContextRule `yaml:"context" json:"context,omitempty"`

	// Source is where this spec came from, and it decides where the spec is
	// allowed to point. Never parsed and never serialised: it is the server's
	// judgement about a request, so accepting it from the request itself would
	// hand the decision to the thing being judged. See Origin.
	Source Origin `yaml:"-" json:"-"`
}

// ContextRule extracts one piece of a finished turn into shared context.
//
// This is how an agent writes to the room without knowing the room exists. The
// alternative — an Oryxa tool the agent must call — would work only for
// frameworks with tool calling, would need a prompt change per agent, and would
// let a model decline to record what it found. A rule is declarative, applies
// to any framework, and cannot be talked out of.
type ContextRule struct {
	Key string `yaml:"key" json:"key"`

	// Kind is append (default) or value. Append is the default for the same
	// reason it is the store's default: it cannot conflict.
	Kind string `yaml:"kind" json:"kind,omitempty"`

	// From is either $text — the turn's assembled text output, which every
	// agent produces — or a selector into the response payload for agents that
	// return structure worth keeping separate from their prose.
	From string `yaml:"from" json:"from"`

	// When gates which chunks a selector reads, exactly as response.when gates
	// which chunks become text. A streaming agent emits its answer many times
	// over; without a gate the room fills with every prefix of it.
	When string `yaml:"when" json:"when,omitempty"`

	// Pin marks the entry as part of the curated set that {{context.pinned}}
	// renders.
	Pin bool `yaml:"pin" json:"pin,omitempty"`
}

// FromText reports whether the rule reads the turn's own output rather than
// selecting into the response payload.
func (r ContextRule) FromText() bool { return strings.TrimSpace(r.From) == SourceText }

// SourceText is the `from` value meaning "whatever this agent said this turn".
const SourceText = "$text"

func (r ContextRule) kindOrDefault() string {
	if r.Kind == "" {
		return "append"
	}
	return r.Kind
}

// Step is one HTTP call.
type Step struct {
	Method   string            `yaml:"method" json:"method,omitempty"`
	Path     string            `yaml:"path" json:"path,omitempty"`
	Headers  map[string]string `yaml:"headers" json:"headers,omitempty"`
	Body     any               `yaml:"body" json:"body,omitempty"`
	Capture  map[string]string `yaml:"capture" json:"capture,omitempty"`
	Response *Response         `yaml:"response" json:"response,omitempty"`
}

// Response says how to read what comes back.
type Response struct {
	Format string   `yaml:"format" json:"format,omitempty"` // sse | ndjson | json
	Text   []string `yaml:"text" json:"text,omitempty"`     // tried in order
	Done   string   `yaml:"done" json:"done,omitempty"`
	Error  string   `yaml:"error" json:"error,omitempty"`

	// When gates which chunks contribute text. Streaming APIs commonly emit
	// both incremental deltas and a final aggregated message; without a gate a
	// client concatenates both and the answer appears twice. Protocols with
	// typed events need the equality form to tell an answer from reasoning.
	//
	//	when: $.partial
	//	when: $.type == TEXT_MESSAGE_CONTENT
	When string `yaml:"when" json:"when,omitempty"`
}

func (s *Spec) Has(cap string) bool {
	for _, c := range s.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

func (s *Spec) TimeoutDuration() time.Duration {
	if s.Timeout == "" {
		return 5 * time.Minute
	}
	d, err := time.ParseDuration(s.Timeout)
	if err != nil {
		return 5 * time.Minute
	}
	return d
}

// Validate reports problems in the spec itself, before any network call.
func (s *Spec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(s.Base) == "" {
		return fmt.Errorf("base is required")
	}
	// A templated base is resolved at call time, so only literal ones can be
	// checked here.
	if !strings.Contains(s.Base, "{{") &&
		!strings.HasPrefix(s.Base, "http://") && !strings.HasPrefix(s.Base, "https://") {
		return fmt.Errorf("base must be an http(s) URL, got %q", s.Base)
	}
	if s.Turn == nil {
		return fmt.Errorf("turn is required")
	}
	if s.Turn.Response != nil {
		switch s.Turn.Response.Format {
		case "", "sse", "ndjson", "json":
		default:
			return fmt.Errorf("turn.response.format must be sse, ndjson or json, got %q", s.Turn.Response.Format)
		}
	}
	if s.Timeout != "" {
		if _, err := time.ParseDuration(s.Timeout); err != nil {
			return fmt.Errorf("timeout %q is not a duration: %w", s.Timeout, err)
		}
	}
	seen := map[string]bool{}
	for i, r := range s.Context {
		where := fmt.Sprintf("context[%d]", i)
		switch {
		case strings.TrimSpace(r.Key) == "":
			return fmt.Errorf("%s: key is required", where)
		case strings.TrimSpace(r.From) == "":
			return fmt.Errorf("%s: from is required (%s or a selector)", where, SourceText)
		}
		switch r.kindOrDefault() {
		case "append", "value":
		default:
			return fmt.Errorf("%s: kind must be append or value, got %q", where, r.Kind)
		}
		// Two rules on one key would race each other every turn and the winner
		// would depend on rule order, which is not a thing anyone should have
		// to reason about.
		if seen[r.Key] {
			return fmt.Errorf("%s: duplicate rule for key %q", where, r.Key)
		}
		seen[r.Key] = true
	}
	return nil
}

func ParseYAML(b []byte) (*Spec, error) {
	var s Spec
	if err := yaml.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, s.Validate()
}

func ParseJSON(b []byte) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, s.Validate()
}

// Registry holds the configured agents.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]*Spec
}

func NewRegistry() *Registry {
	return &Registry{byName: map[string]*Spec{}}
}

func (r *Registry) Put(s *Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[s.Name] = s
	return nil
}

func (r *Registry) Get(name string) (*Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byName[name]
	return s, ok
}

func (r *Registry) Delete(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.byName[name]
	delete(r.byName, name)
	return ok
}

// List returns the registered specs, sorted by name. Map iteration order in Go
// is randomised, so an unsorted list would reshuffle on every poll and a client
// rendering it would move items out from under the user's cursor.
func (r *Registry) List() []*Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Spec, 0, len(r.byName))
	for _, s := range r.byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LoadDir registers every .yaml/.yml/.json connector in a directory.
func (r *Registry) LoadDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return n, fmt.Errorf("%s: %w", path, err)
		}
		var s *Spec
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml":
			s, err = ParseYAML(b)
		case ".json":
			s, err = ParseJSON(b)
		default:
			continue
		}
		if err != nil {
			return n, fmt.Errorf("%s: %w", path, err)
		}
		if err := r.Put(s); err != nil {
			return n, fmt.Errorf("%s: %w", path, err)
		}
		n++
	}
	return n, nil
}
