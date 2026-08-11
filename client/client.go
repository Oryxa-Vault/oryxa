// Package client talks to a running Oryxa.
//
// This is the only Go package here that is not internal, and the reason is
// narrow: it is a thin wrapper over `/v1`, so it is stable exactly as far as
// `/v1` is. Everything else — the session model, the event store, the connector
// executor — stays internal until 1.0, because those interfaces are young and
// exporting one is a promise that cannot be withdrawn.
//
// Nothing here is required. The HTTP API is the contract (see openapi.yaml) and
// any language can call it; this exists so a Go service does not have to write
// the same twenty request shapes again.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is safe for concurrent use.
type Client struct {
	base  string
	token string
	http  *http.Client

	// Room secrets, by session id. Opening a room records its secret and every
	// later call to that room carries it, so scoping costs the caller nothing in
	// the common case — you opened it, so you are in it.
	//
	// Held in memory only. A secret that outlives the process would have to be
	// written somewhere, and this package has no business choosing where.
	mu      sync.RWMutex
	secrets map[string]string
}

// Option configures a Client.
type Option func(*Client)

// WithToken sends a bearer token, for a server started with -token.
func WithToken(t string) Option { return func(c *Client) { c.token = t } }

// WithHTTPClient supplies your own, for timeouts, tracing or a test transport.
// Streaming ignores its timeout: a stream stays open for the life of a room.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithRoom records a room's secret, for joining a room somebody else opened.
// The secret is shown once, when the room is created.
func WithRoom(id, secret string) Option {
	return func(c *Client) { c.secrets[id] = secret }
}

func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		base:    strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 60 * time.Second},
		secrets: map[string]string{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ---- types ----

// Connector is how Oryxa reaches one agent. The same object you would write as
// YAML on disk; YAML is a file format, not the interface.
type Connector struct {
	Name         string            `json:"name"`
	Base         string            `json:"base"`
	Headers      map[string]string `json:"headers,omitempty"`
	Vars         map[string]string `json:"vars,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Interests    []string          `json:"interests,omitempty"`
	Timeout      string            `json:"timeout,omitempty"`
	Open         *Step             `json:"open,omitempty"`
	Turn         *Step             `json:"turn"`
	Context      []ContextRule     `json:"context,omitempty"`
}

type Step struct {
	Method   string            `json:"method,omitempty"`
	Path     string            `json:"path,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     any               `json:"body,omitempty"`
	Capture  map[string]string `json:"capture,omitempty"`
	Response *ResponseSpec     `json:"response,omitempty"`
}

type ResponseSpec struct {
	Format string   `json:"format,omitempty"`
	Text   []string `json:"text,omitempty"`
	Done   string   `json:"done,omitempty"`
	Error  string   `json:"error,omitempty"`
	When   string   `json:"when,omitempty"`
}

type ContextRule struct {
	Key  string `json:"key"`
	Kind string `json:"kind,omitempty"`
	From string `json:"from"`
	When string `json:"when,omitempty"`
	Pin  bool   `json:"pin,omitempty"`
}

type Session struct {
	ID      string            `json:"id"`
	Agents  []string          `json:"agents"`
	State   string            `json:"state"`
	Handles map[string]string `json:"handles,omitempty"`
	Created time.Time         `json:"created"`

	// Secret is set only by Open, and only once — it is what admits you to this
	// room. Keep it if anyone else needs in; the server cannot reissue it.
	Secret string `json:"secret,omitempty"`
}

type SessionView struct {
	Session
	Waiting []Input `json:"waiting,omitempty"`
	Current *Turn   `json:"current,omitempty"`
	History []Turn  `json:"history"`
}

// Input is one thing someone said. Wake and Why record who it reached and on
// what grounds, which is the only way to find out why an agent stayed quiet.
type Input struct {
	ID        string   `json:"id"`
	Seq       int      `json:"seq"`
	Author    string   `json:"author"`
	Text      string   `json:"text"`
	To        []string `json:"to,omitempty"`
	Wake      []string `json:"wake"`
	Why       string   `json:"why,omitempty"`
	Withdrawn bool     `json:"withdrawn,omitempty"`
}

// Turn is one agent answering everything said since it last spoke — one message
// in a quiet room, several in a busy one.
type Turn struct {
	ID     string  `json:"id"`
	Agent  string  `json:"agent"`
	State  string  `json:"state"`
	Inputs []Input `json:"inputs,omitempty"`
	Author string  `json:"author"`
	Text   string  `json:"text"`
	Output string  `json:"output,omitempty"`
	Error  string  `json:"error,omitempty"`
}

type ContextEntry struct {
	Key     string        `json:"key"`
	Kind    string        `json:"kind"`
	Pinned  bool          `json:"pinned"`
	Value   string        `json:"value,omitempty"`
	Version int64         `json:"version,omitempty"`
	By      string        `json:"by,omitempty"`
	Items   []ContextItem `json:"items,omitempty"`
	Rollup  *Rollup       `json:"rollup,omitempty"`
}

type ContextItem struct {
	By   string    `json:"by"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	Seq  int64     `json:"seq"`
}

type Rollup struct {
	Text    string `json:"text"`
	Covers  int    `json:"covers"`
	Through int64  `json:"through"`
	By      string `json:"by,omitempty"`
}

type Event struct {
	Seq     int64           `json:"seq"`
	Session string          `json:"session"`
	TS      time.Time       `json:"ts"`
	Kind    string          `json:"kind"`
	Actor   string          `json:"actor,omitempty"`
	Turn    string          `json:"turn,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type CheckResult struct {
	Agent     string   `json:"agent"`
	OK        bool     `json:"ok"`
	Reachable bool     `json:"reachable"`
	Error     string   `json:"error,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	Turn      struct {
		OK      bool   `json:"ok"`
		MS      int    `json:"ms"`
		Parts   int    `json:"parts"`
		TextLen int    `json:"text_len"`
		Sample  string `json:"sample,omitempty"`
		Error   string `json:"error,omitempty"`
	} `json:"turn"`
}

// Error is a non-2xx response. Status is kept because the useful distinctions
// are status-shaped: 409 is a conflict worth merging, 401 is a missing token.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("oryxa: %d %s", e.Status, e.Message) }

// Conflict is a refused write, carrying what is current so the caller can merge
// rather than re-read and guess what changed.
type Conflict struct {
	Key     string `json:"key"`
	Current string `json:"current"`
	Version int64  `json:"version"`
	By      string `json:"by"`
	Message string `json:"error"`
}

func (c *Conflict) Error() string { return "oryxa: " + c.Message }

// ---- agents ----

func (c *Client) Agents(ctx context.Context) ([]Connector, error) {
	var out struct {
		Agents []Connector `json:"agents"`
	}
	return out.Agents, c.do(ctx, "GET", "/v1/agents", nil, &out)
}

// Register adds or replaces a connector. It takes effect immediately and
// survives a restart; no filesystem access is involved.
func (c *Client) Register(ctx context.Context, spec Connector) (*Connector, error) {
	var out Connector
	return &out, c.do(ctx, "POST", "/v1/agents", spec, &out)
}

func (c *Client) RemoveAgent(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/v1/agents/"+name, nil, nil)
}

// Check probes an agent with a real turn.
//
// A failing agent is a result here, not a transport error: the server answers
// 502 with the full report, and the warnings on it are the reason to call this
// at all. Only a 404 — no such connector — comes back as an error.
func (c *Client) Check(ctx context.Context, name, probe string) (*CheckResult, error) {
	status, raw, err := c.raw(ctx, "POST", "/v1/agents/"+name+"/check",
		map[string]string{"probe": probe})
	if err != nil {
		return nil, err
	}
	var out CheckResult
	if len(raw) > 0 && json.Unmarshal(raw, &out) == nil && out.Agent != "" {
		return &out, nil
	}
	return nil, &Error{Status: status, Message: strings.TrimSpace(string(raw))}
}

// ---- rooms ----

func (c *Client) Open(ctx context.Context, agents ...string) (*Session, error) {
	var out Session
	if err := c.do(ctx, "POST", "/v1/sessions",
		map[string]any{"agents": agents}, &out); err != nil {
		return nil, err
	}
	// Remembered so the caller never has to thread it through by hand: you
	// opened this room, so every later call to it carries the secret.
	c.Join(out.ID, out.Secret)
	return &out, nil
}

// Join records a room's secret, for a room somebody else opened. It is a local
// act — nothing is sent — because the secret is the whole credential and the
// server has no list of members to add you to.
func (c *Client) Join(id, secret string) {
	if id == "" || secret == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.secrets[id] = secret
}

// roomSecret returns the secret for a room, if this client holds one.
func (c *Client) roomSecret(id string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.secrets[id]
}

// roomIDFromPath pulls a session id out of /v1/sessions/{id}/... so every call
// carries its own room's secret without each method having to remember to.
func roomIDFromPath(path string) string {
	const prefix = "/v1/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	var out struct {
		Sessions []Session `json:"sessions"`
	}
	return out.Sessions, c.do(ctx, "GET", "/v1/sessions", nil, &out)
}

func (c *Client) Session(ctx context.Context, id string) (*SessionView, error) {
	var out SessionView
	return &out, c.do(ctx, "GET", "/v1/sessions/"+id, nil, &out)
}

// Say appends one message. It does not wait for anyone: the returned Input
// records who it woke and why, and those agents answer on their own schedule.
// Pass `to` to address specific agents and override the room's own rules.
func (c *Client) Say(ctx context.Context, id, author, text string, to ...string) (*Input, error) {
	body := map[string]any{"text": text, "author": author}
	if len(to) > 0 {
		body["to"] = to
	}
	var out Input
	return &out, c.do(ctx, "POST", "/v1/sessions/"+id+"/input", body, &out)
}

func (c *Client) Withdraw(ctx context.Context, id, inputID string) error {
	return c.do(ctx, "DELETE", "/v1/sessions/"+id+"/input/"+inputID, nil, nil)
}

func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1/sessions/"+id+"/cancel", nil, nil)
}

func (c *Client) Close(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1/sessions/"+id+"/close", nil, nil)
}

// ---- shared context ----

func (c *Client) Context(ctx context.Context, id string) ([]ContextEntry, error) {
	var out struct {
		Context []ContextEntry `json:"context"`
	}
	return out.Context, c.do(ctx, "GET", "/v1/sessions/"+id+"/context", nil, &out)
}

// Append adds to an add-only list. It cannot conflict.
func (c *Client) Append(ctx context.Context, id, key, author, text string) (*ContextEntry, error) {
	var out ContextEntry
	return &out, c.do(ctx, "POST", "/v1/sessions/"+id+"/context/"+key,
		map[string]any{"append": text, "author": author}, &out)
}

// Set writes a single value. Pass the version you last saw as ifMatch and a
// stale write comes back as *Conflict carrying what is current; pass -1 to
// overwrite regardless.
func (c *Client) Set(ctx context.Context, id, key, author, value string, ifMatch int64) (*ContextEntry, error) {
	var out ContextEntry
	h := map[string]string{}
	if ifMatch >= 0 {
		h["If-Match"] = strconv.FormatInt(ifMatch, 10)
	}
	err := c.request(ctx, "POST", "/v1/sessions/"+id+"/context/"+key, h,
		map[string]any{"value": value, "author": author}, &out)
	return &out, err
}

func (c *Client) Pin(ctx context.Context, id, key, author string, pinned bool) (*ContextEntry, error) {
	var out ContextEntry
	return &out, c.do(ctx, "POST", "/v1/sessions/"+id+"/context/"+key+"/pin",
		map[string]any{"pinned": pinned, "author": author}, &out)
}

// ---- the log ----

func (c *Client) Events(ctx context.Context, id string, since int64) ([]Event, error) {
	var out struct {
		Events []Event `json:"events"`
	}
	return out.Events, c.do(ctx, "GET",
		fmt.Sprintf("/v1/sessions/%s/events?since=%d", id, since), nil, &out)
}

// Stream replays from `since` and then follows live, calling fn for each event.
// It returns when the context is cancelled, the server closes, or fn returns
// false. since=0 is the whole room: replay and follow are one code path.
func (c *Client) Stream(ctx context.Context, id string, since int64, fn func(Event) bool) error {
	url := fmt.Sprintf("%s/v1/sessions/%s/stream?since=%d", c.base, id, since)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// A Go client can send a header, so it needs no cookie dance — that exists
	// for the viewer, where EventSource cannot.
	if secret := c.roomSecret(id); secret != "" {
		req.Header.Set("X-Oryxa-Session", secret)
	}
	// No timeout: a stream stays open for as long as the room does.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &Error{Status: resp.StatusCode, Message: resp.Status}
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var data []string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			if len(data) == 0 {
				continue // keep-alive
			}
			var ev Event
			payload := strings.Join(data, "\n")
			data = data[:0]
			if json.Unmarshal([]byte(payload), &ev) != nil {
				continue
			}
			if !fn(ev) {
				return nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// ---- plumbing ----

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.request(ctx, method, path, nil, body, out)
}

func (c *Client) request(ctx context.Context, method, path string, headers map[string]string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if id := roomIDFromPath(path); id != "" {
		if secret := c.roomSecret(id); secret != "" {
			req.Header.Set("X-Oryxa-Session", secret)
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if resp.StatusCode == 409 {
		var conflict Conflict
		if json.Unmarshal(raw, &conflict) == nil && conflict.Message != "" {
			return &conflict
		}
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &Error{Status: resp.StatusCode, Message: msg}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// raw returns the status and body without deciding whether it is an error, for
// the endpoints where a non-2xx carries the answer rather than a failure.
func (c *Client) raw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, raw, nil
}
