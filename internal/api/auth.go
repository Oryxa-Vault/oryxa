package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Auth is a single shared token guarding the API.
//
// Deliberately the smallest thing that is honestly secure rather than the
// beginning of an identity system: one token, compared in constant time, or
// nothing at all. Per-session and per-person identity belong with presence and
// membership, and inventing half of it here would be worse than not having it.
//
// Two ways to present it, because a browser cannot do the first:
//
//	Authorization: Bearer <token>   API clients, the CLI, anything scripted
//	oryxa_token cookie              the viewer — EventSource cannot set headers,
//	                                which is exactly why the cookie exists
const tokenCookie = "oryxa_token"

func (s *Server) authed(r *http.Request) bool {
	if s.token == "" {
		return true // open mode; the server says so loudly at startup
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if s.tokenMatches(strings.TrimPrefix(h, "Bearer ")) {
			return true
		}
	}
	if c, err := r.Cookie(tokenCookie); err == nil {
		if s.tokenMatches(c.Value) {
			return true
		}
	}
	return false
}

func (s *Server) tokenMatches(given string) bool {
	return subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) == 1
}

// requireAuth guards the API. The viewer itself stays reachable so it can show
// a sign-in prompt; it has no data in it until the API answers.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") ||
			r.URL.Path == "/v1/auth/login" || r.URL.Path == "/v1/auth/status" {
			next.ServeHTTP(w, r)
			return
		}
		if s.authed(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="oryxa"`)
		writeErr(w, 401, fmt.Errorf("unauthorized"))
	})
}

// login exchanges the token for a cookie so the viewer's EventSource streams
// can authenticate. The token never goes in a URL, where it would end up in
// access logs and browser history.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	_ = decodeJSONBody(r, &req)

	if s.token == "" {
		writeJSON(w, 200, map[string]any{"ok": true, "auth": "open"})
		return
	}
	if !s.tokenMatches(req.Token) {
		writeErr(w, 401, fmt.Errorf("invalid token"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    req.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure is set only behind TLS: forcing it on a plain-http localhost
		// server would drop the cookie and lock the viewer out.
		Secure: r.TLS != nil,
	})
	writeJSON(w, 200, map[string]any{"ok": true, "auth": "token"})
}

// authStatus lets the viewer decide whether to show a prompt without guessing
// from a failed request.
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	mode := "open"
	if s.token != "" {
		mode = "token"
	}
	out := map[string]any{"mode": mode, "authed": s.authed(r), "identity": "claimed"}
	if s.trustHeader != "" {
		out["identity"] = "trusted"
		if who, ok := s.identify(r, ""); ok {
			out["author"] = who.Author
		}
	}
	writeJSON(w, 200, out)
}

func decodeJSONBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(v)
}

// ---- who is acting ----

// Identity is established by whatever already runs in front of Oryxa, not by
// Oryxa. Deployments have SSO, a reverse proxy, an internal gateway; building
// accounts here would duplicate that badly and everyone would route around it.
//
// So: set -trust-header to the header your proxy already sets
// (X-Forwarded-User, X-Auth-Request-Email, Cf-Access-Authenticated-User-Email…)
// and the log records who acted. Leave it unset and authors stay self-declared,
// which the log says plainly rather than implying otherwise.
//
// SAFETY: a trusted header is only trustworthy if nothing can reach Oryxa
// except that proxy. Bind to localhost or a private network. Exposing the port
// publicly with this on lets anyone claim to be anyone — which is why it is
// opt-in and why a missing header is rejected rather than treated as anonymous.
type Identity struct {
	Author string
	Source string // "trusted" or "claimed"
}

// identify resolves who is acting. The bool is false when a trusted header is
// required but absent — meaning the request did not come through the proxy.
// Three sources, strongest first, and the order is the whole of it.
//
// A proxy that established identity outranks everything, because it answers a
// question Oryxa deliberately never asks: who this person is in the world. A
// person key outranks a claim, because someone who already held the room bound
// that name to that key and the holder cannot use it under another name. A claim
// is last and is only a claim — which is now a statement about one specific
// case, a caller holding the room's bearer secret, rather than about everybody.
//
// What each is worth travels with it, in Source, because a log that records
// names without recording how they were established invites everything
// downstream to treat all three the same.
func (s *Server) identify(r *http.Request, claimed string) (Identity, bool) {
	if s.trustHeader != "" {
		got := strings.TrimSpace(r.Header.Get(s.trustHeader))
		if got == "" {
			return Identity{}, false
		}
		return Identity{Author: got, Source: "trusted"}, true
	}
	// The claim is not consulted. A key bound to priya cannot be used to speak
	// as arsh, which is the entire point of having issued it.
	if bound := boundName(r); bound != "" {
		return Identity{Author: bound, Source: "key"}, true
	}
	author := strings.TrimSpace(claimed)
	if author == "" {
		author = "anonymous"
	}
	return Identity{Author: author, Source: "claimed"}, true
}
