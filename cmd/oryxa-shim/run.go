package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/oryxa/oryxa/internal/connector"
)

// shimEvent is the type on every line this program writes itself, as opposed to
// the ones the command wrote. Namespaced so it cannot collide with an event type
// the agent already uses.
const shimEvent = "oryxa.shim"

type envelope struct {
	Type   string `json:"type"`
	Stream string `json:"stream"`          // stdout | stderr | exit | error
	Text   string `json:"text,omitempty"`  // what was written
	Code   *int   `json:"code,omitempty"`  // exit status
	Error  string `json:"error,omitempty"` // set only when the turn failed
}

// Run executes one turn, writing the command's output as NDJSON.
//
// Every line the command writes to stdout is passed through untouched when it
// is JSON, and wrapped when it is not. Untouched matters: the whole point of
// the connector spec is that Oryxa never looks inside an agent, and rewriting
// an agent's own events on the way past would be looking inside from a
// different direction.
func (a *Agent) Run(ctx context.Context, handle, input string, emit func([]byte)) error {
	c := a.state(handle)

	tc := connector.Ctx{
		Input:        input,
		Handle:       a.handleOf(c),
		Conversation: handle,
	}

	raw := a.argv(c)
	argv := make([]string, 0, len(raw))
	viaStdin := true
	for _, arg := range raw {
		if strings.Contains(arg, "{{input}}") {
			viaStdin = false
		}
		argv = append(argv, tc.RenderString(arg))
	}

	ctx, cancel := context.WithTimeout(ctx, a.TimeoutDuration())
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = a.Dir
	cmd.Env = a.environ(tc)
	setPGID(cmd)
	// Kill the whole group, not just the process we started. A coding agent
	// spawns builds and test runners; killing only the parent leaves those
	// holding the pipes, and a cancelled turn goes on burning CPU.
	cmd.Cancel = func() error { return killGroup(cmd) }
	// Wait returns even if a grandchild is still holding stdout open.
	cmd.WaitDelay = 5 * time.Second

	// A prompt that is not in the argv goes to stdin, which is how a long one
	// should travel anyway: argument lists have a length limit and a room's
	// shared context can reach it.
	if viaStdin {
		cmd.Stdin = strings.NewReader(input)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		// Naming the binary here saves the single most common first hour with
		// this program, which is a PATH problem reported as an agent problem.
		return fmt.Errorf("starting %s: %w", argv[0], err)
	}

	// Two scanners, one writer — and one gate.
	//
	// The gate matters on the cancelled path below, where this function returns
	// while a scanner may still be reading. Without it a late line is written to
	// a response nobody is reading any more.
	var mu sync.Mutex
	var closed bool
	safeEmit := func(line []byte) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		emit(line)
	}
	defer func() {
		mu.Lock()
		closed = true
		mu.Unlock()
	}()

	var (
		wg       sync.WaitGroup
		captured string
		capMu    sync.Mutex
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		scan(stdout, func(line string) {
			if decoded, ok := decode(line); ok {
				if id := a.captureFrom(decoded); id != "" {
					capMu.Lock()
					if captured == "" {
						captured = id
					}
					capMu.Unlock()
				}
				safeEmit([]byte(line))
				return
			}
			safeEmit(wrap("stdout", line))
		})
	}()
	go func() {
		defer wg.Done()
		// stderr is never the answer, so it is always wrapped — a connector
		// gates it into opaque activity and it shows up in the room as
		// something that happened rather than as something the agent said.
		// Keeping it is the difference between a failing turn you can read and
		// one that is simply empty.
		scan(stderr, func(line string) { safeEmit(wrap("stderr", line)) })
	}()

	// Read to EOF when the turn ends by itself, and stop reading when it is
	// cancelled.
	//
	// EOF is the right signal only in the first case. A coding agent starts
	// builds, test runners and dev servers, and those inherit its stdout — so
	// killing the group can leave a grandchild holding the pipe open with
	// nothing more to say. Waiting for EOF there waits for that grandchild.
	// Cmd.WaitDelay exists for exactly this, but it only bounds the wait inside
	// Wait, which reading first never reaches.
	//
	// The cost is that a cancelled turn may drop a trailing line. That is the
	// right trade: the client that would have read it has already gone.
	scanned := make(chan struct{})
	go func() {
		wg.Wait()
		close(scanned)
	}()
	select {
	case <-scanned:
	case <-ctx.Done():
	}

	waitErr := cmd.Wait()
	a.record(c, captured)

	code := cmd.ProcessState.ExitCode()
	env := envelope{Type: shimEvent, Stream: "exit", Code: &code}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		env.Error = fmt.Sprintf("%s timed out after %s", a.Name, a.TimeoutDuration())
	case ctx.Err() == context.Canceled:
		// A cancelled turn is not a failed one. Oryxa cancelling a turn closes
		// the stream, so nobody reads what we write here anyway; recording it as
		// an error would only mislead whoever reads the log later.
		env.Text = "cancelled"
	case waitErr != nil:
		env.Error = fmt.Sprintf("%s exited %d", a.Name, code)
	}
	line, _ := json.Marshal(env)
	safeEmit(line)
	return nil
}

// environ inherits the process environment and adds the agent's own.
//
// Inherited rather than filtered: these agents read credentials, model
// configuration and their own settings out of a home directory, and an
// allowlist tight enough to be worth having is tight enough to break the login.
// The boundary that does the work here is the config file — what may run — not
// what it can see once it is running.
func (a *Agent) environ(tc connector.Ctx) []string {
	if len(a.Env) == 0 {
		return nil // nil means inherit
	}
	env := os.Environ()
	for k, v := range a.Env {
		env = append(env, k+"="+tc.RenderString(v))
	}
	return env
}

func (a *Agent) captureFrom(decoded any) string {
	if a.sessionMode() != "capture" {
		return ""
	}
	if got := connector.Select(decoded, a.Capture); len(got) > 0 {
		return got[0]
	}
	return ""
}

func decode(line string) (any, bool) {
	var v any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		return nil, false
	}
	// A bare string or number is valid JSON and is not an event. Passing it
	// through would hand a connector a payload no selector can address.
	if _, ok := v.(map[string]any); !ok {
		return nil, false
	}
	return v, true
}

func wrap(stream, text string) []byte {
	line, _ := json.Marshal(envelope{Type: shimEvent, Stream: stream, Text: text})
	return line
}

func scan(r io.Reader, fn func(string)) {
	sc := bufio.NewScanner(r)
	// Matching the executor's own limit. A single JSON event from a coding
	// agent can carry a whole file.
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			fn(line)
		}
	}
}
