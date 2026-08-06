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
	Timeout      string            `yaml:"timeout" json:"timeout,omitempty"`

	// Open runs once per Oryxa session, to establish continuity on the agent
	// side. Optional: agents without a conversation concept omit it.
	Open *Step `yaml:"open" json:"open,omitempty"`

	// Turn runs once per turn. Required.
	Turn *Step `yaml:"turn" json:"turn"`
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
