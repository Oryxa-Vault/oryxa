// Package api exposes Oryxa over HTTP. Handlers translate; they never decide.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/events"
	"github.com/oryxa/oryxa/internal/session"
	"github.com/oryxa/oryxa/internal/sharedctx"
	"github.com/oryxa/oryxa/internal/web"
)

type Server struct {
	reg         *connector.Registry
	exec        *connector.Executor
	mgr         *session.Manager
	log         events.Store
	token       string
	trustHeader string
}

func New(reg *connector.Registry, exec *connector.Executor, mgr *session.Manager, log events.Store) *Server {
	return &Server{reg: reg, exec: exec, mgr: mgr, log: log}
}

// WithToken guards the API with a shared token. Empty leaves it open.
func (s *Server) WithToken(token string) *Server {
	s.token = token
	return s
}

// WithTrustedHeader takes the acting user from a header set by whatever runs in
// front of Oryxa. Empty leaves authors self-declared.
func (s *Server) WithTrustedHeader(h string) *Server {
	s.trustHeader = h
	return s
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	// Agents
	mux.HandleFunc("POST /v1/agents", s.putAgent)
	mux.HandleFunc("GET /v1/agents", s.listAgents)
	mux.HandleFunc("GET /v1/agents/{name}", s.getAgent)
	mux.HandleFunc("DELETE /v1/agents/{name}", s.deleteAgent)
	mux.HandleFunc("POST /v1/agents/{name}/check", s.checkAgent)

	// Sessions
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", s.getSession)
	mux.HandleFunc("POST /v1/sessions/{id}/input", s.submitInput)
	mux.HandleFunc("DELETE /v1/sessions/{id}/input/{tid}", s.withdrawInput)
	mux.HandleFunc("POST /v1/sessions/{id}/cancel", s.cancelTurn)
	mux.HandleFunc("POST /v1/sessions/{id}/close", s.closeSession)

	// Shared context
	mux.HandleFunc("GET /v1/sessions/{id}/context", s.getContext)
	mux.HandleFunc("POST /v1/sessions/{id}/context/{key}", s.writeContext)
	mux.HandleFunc("POST /v1/sessions/{id}/context/{key}/pin", s.pinContext)

	// Log
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.getEvents)
	mux.HandleFunc("GET /v1/sessions/{id}/stream", s.stream)

	// Auth
	mux.HandleFunc("POST /v1/auth/login", s.login)
	mux.HandleFunc("GET /v1/auth/status", s.authStatus)

	// Viewer, embedded in the binary so there is nothing extra to deploy.
	mux.Handle("/", web.Handler())

	return logging(s.requireAuth(mux))
}

// ---- agents ----

func (s *Server) putAgent(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var spec *connector.Spec
	if isYAML(r) {
		spec, err = connector.ParseYAML(body)
	} else {
		spec, err = connector.ParseJSON(body)
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.reg.Put(spec); err != nil {
		writeErr(w, 400, err)
		return
	}
	who, _ := s.identify(r, "")
	s.recordAgent(who.Author, spec)
	writeJSON(w, 201, spec)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"agents": s.reg.List()})
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.reg.Get(r.PathValue("name"))
	if !ok {
		writeErr(w, 404, fmt.Errorf("agent not found"))
		return
	}
	writeJSON(w, 200, spec)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.reg.Delete(name) {
		writeErr(w, 404, fmt.Errorf("agent not found"))
		return
	}
	who, _ := s.identify(r, "")
	s.recordAgentRemoved(who.Author, name)
	w.WriteHeader(204)
}

func (s *Server) checkAgent(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.reg.Get(r.PathValue("name"))
	if !ok {
		writeErr(w, 404, fmt.Errorf("agent not found"))
		return
	}
	var req struct {
		Probe string `json:"probe"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Probe == "" {
		req.Probe = "ping from oryxa check"
	}
	ctx, cancel := context.WithTimeout(r.Context(), spec.TimeoutDuration())
	defer cancel()

	res := s.exec.Check(ctx, spec, req.Probe)
	code := 200
	if !res.OK {
		code = 502
	}
	writeJSON(w, code, res)
}

// ---- sessions ----

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent  string   `json:"agent"`
		Agents []string `json:"agents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	// `agent` stays valid so existing callers keep working; `agents` opens a
	// room to several at once.
	agents := req.Agents
	if req.Agent != "" {
		agents = append([]string{req.Agent}, agents...)
	}
	sess, err := s.mgr.Create(agents...)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, sess)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"sessions": s.mgr.List()})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	v, ok := s.mgr.View(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, fmt.Errorf("session not found"))
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) submitInput(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string `json:"text"`
		Author string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeErr(w, 400, fmt.Errorf("text is required"))
		return
	}
	who, ok := s.identify(r, req.Author)
	if !ok {
		writeErr(w, 401, fmt.Errorf("missing %s; this request did not come through the trusted proxy", s.trustHeader))
		return
	}
	in, err := s.mgr.Submit(r.PathValue("id"), who.Author, req.Text)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	// Submitting no longer creates turns — who answers, and when, is each lane's
	// business. The response is shaped like the old one anyway: `state` and
	// `group` are what clients branch on, and one input is still one group. The
	// one field that could not survive is `agent`, which named only the first
	// agent in the room and meant nothing once a turn stopped being per-input.
	writeJSON(w, 202, map[string]any{
		"id": in.ID, "author": in.Author, "text": in.Text,
		"seq": in.Seq, "state": "queued", "group": in.ID,
	})
}

func (s *Server) withdrawInput(w http.ResponseWriter, r *http.Request) {
	who, ok := s.identify(r, r.URL.Query().Get("author"))
	if !ok {
		writeErr(w, 401, fmt.Errorf("missing %s", s.trustHeader))
		return
	}
	if err := s.mgr.Withdraw(r.PathValue("id"), r.PathValue("tid"), who.Author); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) cancelTurn(w http.ResponseWriter, r *http.Request) {
	who, _ := s.identify(r, r.URL.Query().Get("author"))
	if err := s.mgr.Cancel(r.PathValue("id"), who.Author); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(202)
}

func (s *Server) closeSession(w http.ResponseWriter, r *http.Request) {
	who, _ := s.identify(r, r.URL.Query().Get("author"))
	if err := s.mgr.Close(r.PathValue("id"), who.Author); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(204)
}

// ---- log ----

func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.mgr.Exists(id) {
		writeErr(w, 404, fmt.Errorf("session not found"))
		return
	}
	evs, err := s.log.Since(id, sinceParam(r))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"events": evs})
}

// stream replays from ?since= then follows. Late join, reconnect and replay are
// the same code path — that is the point of the log being the source of truth.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.mgr.Exists(id) {
		writeErr(w, 404, fmt.Errorf("session not found"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, fmt.Errorf("streaming unsupported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	ch, unsub := s.log.Subscribe(id)
	defer unsub()

	// Subscribe first, then backfill: the reverse order drops anything that
	// lands between the two.
	backfill, err := s.log.Since(id, sinceParam(r))
	if err != nil {
		// Headers are already sent, so report it in-band rather than as a status.
		fmt.Fprintf(w, "data: {\"kind\":\"stream.error\",\"error\":%q}\n\n", err.Error())
		flusher.Flush()
		return
	}
	var last int64
	for _, ev := range backfill {
		writeSSE(w, ev)
		last = ev.Seq
	}
	flusher.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Seq <= last {
				continue // already delivered in backfill
			}
			last = ev.Seq
			writeSSE(w, ev)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// ---- helpers ----

// writeSSE deliberately omits an `event:` field. Naming the event would make
// EventSource dispatch to a listener per kind and never fire onmessage, so a
// browser client would have to enumerate every kind we might add. The kind is
// already in the payload; `id:` stays so reconnects can resume (see sinceParam).
func writeSSE(w http.ResponseWriter, ev events.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b)
}

// sinceParam prefers Last-Event-ID, which EventSource sends automatically on
// reconnect. Without it a dropped connection would replay from ?since= and
// duplicate everything the client already has.
func sinceParam(r *http.Request) int64 {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	n, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	return n
}

func isYAML(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.Contains(ct, "yaml") || strings.Contains(ct, "yml")
}

func statusFor(err error) int {
	switch {
	case err == nil:
		return 200
	case errors.Is(err, sharedctx.ErrNotFound):
		return 404
	case errors.Is(err, sharedctx.ErrWrongKind):
		return 409
	case strings.Contains(err.Error(), "not found"):
		return 404
	case strings.Contains(err.Error(), "closed"):
		return 409
	default:
		return 400
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if !strings.HasSuffix(r.URL.Path, "/stream") {
			fmt.Printf("%s %s %s\n", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

// ---- shared context ----

func (s *Server) getContext(w http.ResponseWriter, r *http.Request) {
	entries, ok := s.mgr.Context(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, fmt.Errorf("session not found"))
		return
	}
	if entries == nil {
		entries = []sharedctx.Entry{}
	}
	writeJSON(w, 200, map[string]any{"context": entries})
}

// writeContext appends or sets, depending on the body. Append is the default
// because most shared content is add-only and cannot conflict.
func (s *Server) writeContext(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Append string  `json:"append"`
		Value  *string `json:"value"`
		Author string  `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	who, ok := s.identify(r, req.Author)
	if !ok {
		writeErr(w, 401, fmt.Errorf("missing %s", s.trustHeader))
		return
	}
	id, key := r.PathValue("id"), r.PathValue("key")

	if req.Value != nil {
		// If-Match carries the version the writer last saw. Absent means "no
		// opinion" rather than "must not exist": requiring it would make the
		// common case of a first write awkward.
		ifMatch := int64(-1)
		if v := r.Header.Get("If-Match"); v != "" {
			n, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64)
			if err != nil {
				writeErr(w, 400, fmt.Errorf("If-Match must be a version number"))
				return
			}
			ifMatch = n
		}
		e, err := s.mgr.SetContext(id, key, who.Author, *req.Value, ifMatch)
		if err != nil {
			var c *sharedctx.Conflict
			if errors.As(err, &c) {
				// 409 with what is current, so the caller can merge instead of
				// re-reading and guessing what changed.
				writeJSON(w, 409, map[string]any{
					"error": c.Error(), "key": c.Key,
					"current": c.Current, "version": c.Version, "by": c.By,
				})
				return
			}
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, 200, e)
		return
	}

	if strings.TrimSpace(req.Append) == "" {
		writeErr(w, 400, fmt.Errorf("send either append or value"))
		return
	}
	e, err := s.mgr.AppendContext(id, key, who.Author, req.Append)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, e)
}

func (s *Server) pinContext(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pinned *bool  `json:"pinned"`
		Author string `json:"author"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pinned := true
	if req.Pinned != nil {
		pinned = *req.Pinned
	}
	who, _ := s.identify(r, req.Author)
	e, err := s.mgr.PinContext(r.PathValue("id"), r.PathValue("key"), who.Author, pinned)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, e)
}
