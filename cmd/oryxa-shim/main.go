// Command oryxa-shim puts an HTTP surface in front of agents that only speak
// stdin and stdout, so a command-line agent can be reached by an ordinary
// connector.
//
//	oryxa-shim -agents shim.yaml
//
//	GET  /health
//	GET  /v1/agents                      configured agents
//	POST /v1/agents/{name}/open          -> {"handle": "..."}
//	POST /v1/agents/{name}/turn          -> NDJSON, one line per event
//
// This is a separate process on purpose, and the reason is not tidiness.
// Oryxa registers connectors over HTTP at `POST /v1/agents`, so a connector
// spec is something a caller can supply at runtime. If the spec could name a
// command to run, that endpoint would be remote code execution behind one
// shared token. Keeping exec out here means a connector stays what it claims to
// be — a description of HTTP calls — and the only thing that decides what may
// run is a file on this machine.
//
// Nothing in this program knows the name of any agent. What to run comes from
// the config file, the same way what to call comes from a connector.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	// Loopback by default. This process starts other processes; the safe
	// default is that only this machine can ask it to.
	addr := flag.String("addr", "127.0.0.1:8090", "listen address")
	file := flag.String("agents", "shim.yaml", "agent definitions (ORYXA_SHIM_AGENTS)")
	token := flag.String("token", os.Getenv("ORYXA_SHIM_TOKEN"),
		"require this bearer token; open to anyone who can reach the port if empty")
	flag.Parse()

	if env := os.Getenv("ORYXA_SHIM_AGENTS"); env != "" && *file == "shim.yaml" {
		*file = env
	}

	agents, err := LoadFile(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading %s: %v\n", *file, err)
		os.Exit(1)
	}
	if len(agents.byName) == 0 {
		fmt.Fprintf(os.Stderr, "%s defines no agents\n", *file)
		os.Exit(1)
	}

	srv := &server{agents: agents, token: *token}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a turn streams for as long as the agent takes.
	}

	banner(*addr, *file, *token, agents)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errc := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	case sig := <-stop:
		// Turns are killed rather than drained. A CLI agent holds no state we
		// could finish writing, and the alternative is waiting out a
		// fifteen-minute turn on every restart.
		fmt.Printf("\n  %v — stopping; running turns are killed\n", sig)
	}
	_ = httpSrv.Close()
}

func banner(addr, file, token string, agents *Agents) {
	fmt.Printf("\n  oryxa-shim %s\n", version)
	fmt.Printf("  ├─ listening   %s\n", addr)
	fmt.Printf("  ├─ agents      %d from %s\n", len(agents.byName), file)
	if token == "" {
		fmt.Printf("  ├─ auth        none — anyone who can reach %s can run these commands\n", addr)
	} else {
		fmt.Printf("  ├─ auth        shared token\n")
	}
	if !isLoopback(addr) {
		// Worth its own line rather than a footnote. Everything else here is a
		// preference; this one hands the ability to start processes to the
		// network.
		fmt.Printf("  ├─ WARNING     %s is not loopback — this port starts processes\n", addr)
	}
	fmt.Printf("  └─ ready\n\n")
	for _, a := range agents.List() {
		fmt.Printf("     • %-14s %s\n", a.Name, a.Summary())
	}
	fmt.Println()
}

func isLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	switch strings.Trim(host, "[]") {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// ---- routes ----

type server struct {
	agents *Agents
	token  string
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/agents", s.guard(s.listAgents))
	mux.HandleFunc("POST /v1/agents/{name}/open", s.guard(s.open))
	mux.HandleFunc("POST /v1/agents/{name}/turn", s.guard(s.turn))
	return mux
}

func (s *server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && bearer(r) != s.token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return h[7:]
	}
	return ""
}

func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	// Names and shapes, never the argv. The command line is what this file
	// exists to keep off the wire; echoing it back would give it away to
	// exactly the caller it is being kept from.
	out := []map[string]any{}
	for _, a := range s.agents.List() {
		out = append(out, map[string]any{
			"name":      a.Name,
			"multiturn": a.Resume != nil,
			"timeout":   a.TimeoutDuration().String(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

// open starts a conversation and returns the handle later turns resume.
//
// For an agent that names its own session the handle is not known until the
// first turn has run, so this returns the Oryxa conversation id and the shim
// keeps the mapping. A connector captures whatever comes back and stops caring
// which of the two it got.
func (s *server) open(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agents.Get(r.PathValue("name"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such agent"})
		return
	}
	var req struct {
		Conversation string `json:"conversation"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	handle := a.Open(req.Conversation)
	writeJSON(w, http.StatusOK, map[string]string{"handle": handle})
}

// turn runs one turn and streams the agent's output as NDJSON.
//
// The response is flushed line by line so an Oryxa lane sees the agent working
// rather than a wall of text at the end. Anything the command writes that is
// not JSON is wrapped in an envelope instead of being passed through raw —
// otherwise a stray warning on stderr arrives at a connector with no selector
// matching it, and the executor's "show the user something" fallback turns it
// into the agent's answer.
func (s *server) turn(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agents.Get(r.PathValue("name"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such agent"})
		return
	}
	var req struct {
		Handle string `json:"handle"`
		Input  string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "input is required"})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	emit := func(line []byte) {
		_, _ = w.Write(append(line, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}

	// The request context carries the client going away, which is how an Oryxa
	// cancel reaches the process: the stream closes, and Run kills the group.
	if err := a.Run(r.Context(), req.Handle, req.Input, emit); err != nil {
		// Carried in `error` rather than `text` so a connector's error selector
		// finds it. Headers are already sent by now, so this is the only way a
		// failure to even start reaches the room.
		line, _ := json.Marshal(envelope{Type: shimEvent, Stream: "error", Error: err.Error()})
		emit(line)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
