package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// CheckResult answers "is it me or is it them" without starting a session.
// Integrations decay silently as frameworks move; this is what keeps
// "integrates easily" true after launch rather than only at launch.
type CheckResult struct {
	Agent        string      `json:"agent"`
	OK           bool        `json:"ok"`
	Reachable    bool        `json:"reachable"`
	Open         *StepResult `json:"open,omitempty"`
	Turn         *TurnResult `json:"turn,omitempty"`
	Capabilities []string    `json:"capabilities,omitempty"`
	Warnings     []string    `json:"warnings,omitempty"`
	Error        string      `json:"error,omitempty"`
}

type StepResult struct {
	OK     bool   `json:"ok"`
	Handle string `json:"handle,omitempty"`
	Error  string `json:"error,omitempty"`
}

type TurnResult struct {
	OK      bool   `json:"ok"`
	Ms      int64  `json:"ms"`
	Parts   int    `json:"parts"`
	TextLen int    `json:"text_len"`
	Sample  string `json:"sample,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Check runs a real probe turn. Nothing here is simulated — a check that passes
// while the real path fails would be worse than no check.
func (e *Executor) Check(ctx context.Context, spec *Spec, probe string) *CheckResult {
	res := &CheckResult{Agent: spec.Name, Capabilities: spec.Capabilities}

	base := Ctx{Vars: spec.Vars}.RenderString(spec.Base)
	if err := dialable(ctx, base); err != nil {
		res.Error = err.Error()
		return res
	}
	res.Reachable = true

	conv := "oryxa_check_" + randSuffix()
	tc := Ctx{
		Input:        probe,
		Turn:         "t_" + randSuffix(),
		Conversation: conv,
		Vars:         spec.Vars,
		Captures:     map[string]string{},
	}

	if spec.Open != nil {
		handle, caps, err := e.Open(ctx, spec, tc)
		res.Open = &StepResult{OK: err == nil, Handle: handle}
		if err != nil {
			res.Open.Error = err.Error()
			res.Error = "open failed: " + err.Error()
			return res
		}
		tc.Handle, tc.Captures = handle, caps
		if handle == "" && len(spec.Open.Capture) > 0 {
			res.Warnings = append(res.Warnings,
				"open succeeded but captured no handle; {{handle}} will fall back to the session id")
		}
	}

	var parts, textLen int
	var sample strings.Builder
	var full strings.Builder
	var raws []json.RawMessage

	start := time.Now()
	err := e.Turn(ctx, spec, tc, func(p Part) {
		parts++
		if len(p.Raw) > 0 {
			raws = append(raws, p.Raw)
		}
		if p.Kind == "text" {
			textLen += len(p.Text)
			full.WriteString(p.Text)
			if sample.Len() < 200 {
				sample.WriteString(p.Text)
			}
		}
	})
	tr := &TurnResult{
		OK:      err == nil,
		Ms:      time.Since(start).Milliseconds(),
		Parts:   parts,
		TextLen: textLen,
		Sample:  strings.TrimSpace(sample.String()),
	}
	if err != nil {
		tr.Error = err.Error()
		res.Turn = tr
		res.Error = "turn failed: " + err.Error()
		return res
	}
	res.Turn = tr

	if parts == 0 {
		res.Warnings = append(res.Warnings, "agent returned no parts")
	}
	if textLen == 0 && parts > 0 {
		res.Warnings = append(res.Warnings,
			"no text selector matched; output arrived as opaque activity. Check turn.response.text")
	}
	if spec.Turn.Response == nil || spec.Turn.Response.Format == "" {
		res.Warnings = append(res.Warnings,
			"no response.format set; defaulting to json (set sse or ndjson if the agent streams)")
	}
	res.Warnings = append(res.Warnings, sniff(full.String(), raws, spec.Turn.Response)...)
	res.OK = res.Error == ""
	return res
}

// sniff looks for the ways a connector can pass while being wrong.
//
// Both known cases produced plausible-looking output and a green check: an
// answer with the model's private reasoning spliced in, and an answer emitted
// twice. Neither raised an error anywhere. These tests are structural — they
// read the shape of the payload, never the meaning of the words — because
// judging an agent's text is the framework's business, not ours.
func sniff(text string, raws []json.RawMessage, rs *Response) []string {
	var w []string

	if d := doubled(text); d != "" {
		w = append(w, fmt.Sprintf(
			"output looks emitted twice (%q...). The agent probably streams deltas "+
				"and then a final aggregated message — gate it with `when:`", trunc(d, 40)))
	}

	// Reasoning markers present in the payload but not excluded by the selector.
	if keys := reasoningKeys(raws); len(keys) > 0 && !excludesAny(rs, keys) {
		w = append(w, fmt.Sprintf(
			"payload carries %s; a reasoning model's private thinking may be in the "+
				"answer — exclude it with [!%s]", strings.Join(keys, " and "), keys[0]))
	}

	// Chunks disagree about being partial, with nothing gating them.
	if mixedPartial(raws) && (rs == nil || rs.When == "") {
		w = append(w, "chunks mix partial and final forms with no `when:` gate; "+
			"deltas and the final message are likely both being counted")
	}
	return w
}

// doubled reports the repeated half when text is exactly one string twice over.
func doubled(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 16 || len(s)%2 != 0 {
		return ""
	}
	if h := len(s) / 2; s[:h] == s[h:] {
		return s[:h]
	}
	return ""
}

var reasoningFields = []string{"thought", "reasoning", "reasoning_content"}

// reasoningKeys returns the reasoning markers that appear truthy anywhere in
// the payloads, at any depth.
func reasoningKeys(raws []json.RawMessage) []string {
	seen := map[string]bool{}
	for _, r := range raws {
		var v any
		if json.Unmarshal(r, &v) != nil {
			continue
		}
		walk(v, func(k string, val any) {
			for _, f := range reasoningFields {
				if k == f && truthy(val) {
					seen[f] = true
				}
			}
		})
	}
	var out []string
	for _, f := range reasoningFields {
		if seen[f] {
			out = append(out, f)
		}
	}
	return out
}

// mixedPartial reports whether `partial` appears with both truthy and falsy
// values across the payloads — the signature of deltas plus a final message.
func mixedPartial(raws []json.RawMessage) bool {
	var sawTrue, sawFalse bool
	for _, r := range raws {
		var v any
		if json.Unmarshal(r, &v) != nil {
			continue
		}
		walk(v, func(k string, val any) {
			if k != "partial" {
				return
			}
			if truthy(val) {
				sawTrue = true
			} else {
				sawFalse = true
			}
		})
	}
	return sawTrue && sawFalse
}

func excludesAny(rs *Response, keys []string) bool {
	if rs == nil {
		return false
	}
	for _, sel := range rs.Text {
		for _, k := range keys {
			if strings.Contains(sel, "[!"+k+"]") || strings.Contains(sel, "["+k+"]") {
				return true
			}
		}
	}
	return false
}

func walk(v any, fn func(key string, val any)) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			fn(k, val)
			walk(val, fn)
		}
	case []any:
		for _, e := range t {
			walk(e, fn)
		}
	}
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != "" && t != "false"
	case float64:
		return t != 0
	default:
		return true
	}
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func dialable(ctx context.Context, base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("bad base url %q: %w", base, err)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", base, err)
	}
	_ = conn.Close()
	return nil
}

func randSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
}
