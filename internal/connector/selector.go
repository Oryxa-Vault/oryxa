package connector

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Select evaluates a small path expression against decoded JSON and returns
// every string it matched.
//
// Supported: dot notation, [*] to expand an array, [n] to index one, and
// [key] / [!key] to keep only elements where a field is truthy / falsy. That is
// deliberately less than JSONPath — it covers real response shapes without
// importing an engine whose edge cases we would then own.
//
//	$.content.parts[*].text
//	$.content.parts[!thought].text     reasoning models: skip the thinking
//	$.choices[0].delta.content
//	$.output
//
// The [!key] form exists because reasoning models interleave their thinking
// with their answer in the same array, flagged by a field. Without it a
// connector concatenates the model's scratchpad into the reply.
func Select(data any, path string) []string {
	steps := parsePath(path)
	if len(steps) == 0 {
		return stringify(data)
	}
	cur := []any{data}
	for _, st := range steps {
		var next []any
		for _, node := range cur {
			next = append(next, st.apply(node)...)
		}
		if len(next) == 0 {
			return nil
		}
		cur = next
	}
	var out []string
	for _, n := range cur {
		out = append(out, stringify(n)...)
	}
	return out
}

type step struct {
	key     string
	index   int  // used when isIdx
	isAll   bool // [*]
	isIdx   bool // [n]
	hasKey  bool
	predKey string // [field] or [!field]
	predNot bool   // true for [!field]
}

func (s step) apply(node any) []any {
	if s.hasKey {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		v, ok := m[s.key]
		if !ok {
			return nil
		}
		node = v
	}
	switch {
	case s.predKey != "":
		arr, ok := node.([]any)
		if !ok {
			return nil
		}
		var out []any
		for _, e := range arr {
			if truthyField(e, s.predKey) != s.predNot {
				out = append(out, e)
			}
		}
		return out
	case s.isAll:
		arr, ok := node.([]any)
		if !ok {
			return nil
		}
		return arr
	case s.isIdx:
		arr, ok := node.([]any)
		if !ok || s.index < 0 || s.index >= len(arr) {
			return nil
		}
		return []any{arr[s.index]}
	default:
		return []any{node}
	}
}

// truthyField reports whether an element carries a truthy value at key. A
// missing field counts as false, so [!thought] keeps plain elements.
func truthyField(node any, key string) bool {
	m, ok := node.(map[string]any)
	if !ok {
		return false
	}
	switch v := m[key].(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != "" && v != "false"
	case float64:
		return v != 0
	default:
		return true
	}
}

func parsePath(path string) []step {
	p := strings.TrimSpace(path)
	p = strings.TrimPrefix(p, "$")
	p = strings.TrimPrefix(p, ".")
	if p == "" {
		return nil
	}
	var steps []step
	for _, seg := range strings.Split(p, ".") {
		if seg == "" {
			continue
		}
		name := seg
		var brackets []string
		for {
			i := strings.Index(name, "[")
			if i < 0 {
				break
			}
			j := strings.Index(name[i:], "]")
			if j < 0 {
				break
			}
			j += i
			brackets = append(brackets, name[i+1:j])
			name = name[:i] + name[j+1:]
		}
		st := step{}
		if name != "" {
			st.key, st.hasKey = name, true
		}
		if len(brackets) == 0 {
			steps = append(steps, st)
			continue
		}
		// First bracket attaches to this key; further ones become bare steps.
		for bi, b := range brackets {
			s := st
			if bi > 0 {
				s = step{}
			}
			switch {
			case b == "*":
				s.isAll = true
			case strings.HasPrefix(b, "!"):
				s.predKey, s.predNot = strings.TrimPrefix(b, "!"), true
			default:
				if n, err := strconv.Atoi(b); err == nil {
					s.isIdx, s.index = true, n
				} else if b != "" {
					s.predKey = b
				}
			}
			steps = append(steps, s)
		}
	}
	return steps
}

func stringify(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case float64:
		return []string{strconv.FormatFloat(t, 'f', -1, 64)}
	case bool:
		return []string{strconv.FormatBool(t)}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, stringify(e)...)
		}
		return out
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return []string{string(b)}
	}
}

// SelectFirst tries each path in order and returns the first non-empty match.
// Returning the first match rather than concatenating all of them keeps a
// fallback selector from duplicating text the primary one already produced.
func SelectFirst(data any, paths []string) []string {
	for _, p := range paths {
		if got := Select(data, p); len(got) > 0 {
			return got
		}
	}
	return nil
}

// Truthy reports whether a path resolves to something meaning "yes", used for
// the optional done signal.
func Truthy(data any, path string) bool {
	if path == "" {
		return false
	}
	for _, s := range Select(data, path) {
		switch strings.ToLower(s) {
		case "", "false", "0", "null":
		default:
			return true
		}
	}
	return false
}

// Matches evaluates a condition against a payload. Three forms:
//
//	$.partial                        truthy
//	$.type == TEXT_MESSAGE_CONTENT   equal to
//	$.type != REASONING_CONTENT      not equal to
//
// Equality exists because protocols with typed events — AG-UI is the common
// case — distinguish an answer from reasoning by the value of a field rather
// than by its presence, and both carry the same payload shape otherwise.
func Matches(data any, cond string) bool {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return true
	}
	path, op, want := splitCondition(cond)
	if op == "" {
		return Truthy(data, path)
	}
	found := false
	for _, s := range Select(data, path) {
		if s == want {
			found = true
			break
		}
	}
	if op == "!=" {
		return !found
	}
	return found
}

func splitCondition(cond string) (path, op, want string) {
	for _, o := range []string{"==", "!="} {
		if i := strings.Index(cond, o); i >= 0 {
			path = strings.TrimSpace(cond[:i])
			want = strings.TrimSpace(cond[i+len(o):])
			want = strings.Trim(want, `"'`)
			return path, o, want
		}
	}
	return cond, "", ""
}
