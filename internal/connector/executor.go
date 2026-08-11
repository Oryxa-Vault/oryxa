package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Part is one piece of an agent's output.
//
// Only text is interpreted. Everything else the agent emits is passed through
// as opaque activity: parsing a framework's own event schema would couple us to
// its release cycle and break the rule that we never look inside the agent.
type Part struct {
	Kind string          `json:"kind"` // text | activity | error
	Text string          `json:"text,omitempty"`
	Raw  json.RawMessage `json:"raw,omitempty"`
}

type Executor struct {
	Client *http.Client
}

func NewExecutor() *Executor {
	// The guard lives in the dialer rather than in Validate because the
	// destination is only known for certain at connect time — and it applies to
	// redirects for free, which a check on `base` would not.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = guardedDial
	return &Executor{Client: &http.Client{Transport: tr}}
}

// Open runs the optional open step and returns the captured handle, if any.
func (e *Executor) Open(ctx context.Context, spec *Spec, tc Ctx) (string, map[string]string, error) {
	if spec.Open == nil {
		return "", nil, nil
	}
	resp, body, err := e.do(ctx, spec, spec.Open, tc)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return "", nil, fmt.Errorf("read open response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", nil, fmt.Errorf("open returned %s: %s", resp.Status, snippet(raw))
	}

	captures := map[string]string{}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// A non-JSON body is not fatal: an agent may acknowledge with plain text
		// and carry continuity on the session id we already have.
		return "", captures, nil
	}
	for name, path := range spec.Open.Capture {
		if got := Select(decoded, path); len(got) > 0 {
			captures[name] = got[0]
		}
	}
	return captures["handle"], captures, nil
}

// Turn runs one turn, emitting parts as they arrive. It returns when the agent's
// response is complete.
func (e *Executor) Turn(ctx context.Context, spec *Spec, tc Ctx, emit func(Part)) error {
	resp, body, err := e.do(ctx, spec, spec.Turn, tc)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(body, 1<<20))
		return fmt.Errorf("agent returned %s: %s", resp.Status, snippet(raw))
	}

	format := "json"
	var rs *Response
	if spec.Turn.Response != nil {
		rs = spec.Turn.Response
		if rs.Format != "" {
			format = rs.Format
		}
	}
	// Applied here rather than in each consumer, so the manager, the terminal
	// and the viewer all assemble the same answer without any of them knowing
	// this setting exists.
	emit = joining(rs, emit)

	switch format {
	case "sse":
		return e.readSSE(ctx, body, rs, emit)
	case "ndjson":
		return e.readNDJSON(ctx, body, rs, emit)
	default:
		return e.readJSON(body, rs, emit)
	}
}

func (e *Executor) do(ctx context.Context, spec *Spec, step *Step, tc Ctx) (*http.Response, io.Reader, error) {
	method := step.Method
	if method == "" {
		method = http.MethodPost
	}
	// base is templated too, so one connector file works wherever it runs:
	//   base: http://{{env.AGENT_HOST}}:2024
	// A host and a container reach the same agent by different names, and
	// keeping two near-identical files in sync is how they drift.
	url := joinURL(tc.RenderString(spec.Base), tc.RenderString(step.Path))

	// Carried on the context so it reaches the dialer, which is the only place
	// that knows what the name actually resolved to.
	ctx = withOrigin(ctx, spec.Source)

	var bodyReader io.Reader
	if step.Body != nil {
		payload, err := json.Marshal(tc.Render(step.Body))
		if err != nil {
			return nil, nil, fmt.Errorf("encode body: %w", err)
		}
		bodyReader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	if step.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "text/event-stream, application/json")
	for k, v := range tc.RenderHeaders(spec.Headers) {
		req.Header.Set(k, v)
	}
	for k, v := range tc.RenderHeaders(step.Headers) {
		req.Header.Set(k, v)
	}

	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	return resp, resp.Body, nil
}

func (e *Executor) readSSE(ctx context.Context, r io.Reader, rs *Response, emit func(Part)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)

	var data []string
	flush := func() {
		if len(data) == 0 {
			return
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		if payload == "" || payload == "[DONE]" {
			return
		}
		emitPayload(payload, rs, emit)
	}

	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// other SSE fields (event:, id:, retry:) carry no payload for us
		}
	}
	flush()
	if err := sc.Err(); err != nil && !isClosed(err) {
		return err
	}
	return nil
}

func (e *Executor) readNDJSON(ctx context.Context, r io.Reader, rs *Response, emit func(Part)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		emitPayload(line, rs, emit)
	}
	if err := sc.Err(); err != nil && !isClosed(err) {
		return err
	}
	return nil
}

func (e *Executor) readJSON(r io.Reader, rs *Response, emit func(Part)) error {
	raw, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	emitPayload(string(raw), rs, emit)
	return nil
}

// emitPayload turns one decoded chunk into parts.
//
// When no selector matches we emit the chunk as text rather than nothing: being
// wrong toward "show the user something" beats a silent empty response, which is
// the failure that costs an afternoon of debugging.
func emitPayload(payload string, rs *Response, emit func(Part)) {
	var decoded any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		emit(Part{Kind: "text", Text: payload})
		return
	}
	raw := json.RawMessage(payload)

	if rs != nil && rs.Error != "" {
		if msgs := Select(decoded, rs.Error); len(msgs) > 0 {
			emit(Part{Kind: "error", Text: strings.Join(msgs, " "), Raw: raw})
			return
		}
	}

	// Gated-out chunks become activity rather than vanishing: they are still
	// part of what the agent did, and silently dropping them would hide a
	// mis-set `when` behind an empty reply.
	if rs != nil && rs.When != "" && !Matches(decoded, rs.When) {
		emit(Part{Kind: "activity", Raw: raw})
		return
	}

	var texts []string
	if rs != nil && len(rs.Text) > 0 {
		texts = SelectFirst(decoded, rs.Text)
	} else {
		texts = stringify(decoded)
	}

	if len(texts) == 0 {
		emit(Part{Kind: "activity", Raw: raw})
		return
	}
	for _, t := range texts {
		emit(Part{Kind: "text", Text: t, Raw: raw})
	}
}

// joining puts response.join between text parts.
//
// The separator lands on the text of the second and later parts rather than
// being emitted on its own: a part is one thing the agent produced, and an
// extra part carrying only whitespace would show up in the raw view as
// something the agent never sent. Raw is untouched either way — what the agent
// sent stays exactly what it sent, and this only changes how the pieces read
// once assembled.
func joining(rs *Response, emit func(Part)) func(Part) {
	if rs == nil || rs.Join == "" {
		return emit
	}
	var spoken bool
	return func(p Part) {
		if p.Kind == "text" && p.Text != "" {
			if spoken {
				p.Text = rs.Join + p.Text
			}
			spoken = true
		}
		emit(p)
	}
}

func joinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if path == "" {
		return base
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func isClosed(err error) bool {
	return err == io.EOF || err == io.ErrUnexpectedEOF ||
		strings.Contains(err.Error(), "use of closed") ||
		strings.Contains(err.Error(), "context canceled")
}
