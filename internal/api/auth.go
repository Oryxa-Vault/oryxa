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
	writeJSON(w, 200, map[string]any{"mode": mode, "authed": s.authed(r)})
}

func decodeJSONBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(v)
}
