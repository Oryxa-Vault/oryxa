package api

import (
	"fmt"
	"net/http"
	"strings"
)

// Read scoping.
//
// The API token says you may talk to this server. The session secret says which
// rooms are yours. Before this, the token was both: one credential opened every
// room, including rooms its holder was never part of, which is what made Oryxa
// laptop-safe rather than deployable.
//
// A capability rather than a list of names, because Oryxa has no accounts and
// deliberately never will — it accepts identity, it does not establish it.
// Scoping on author names would be scoping on a string the caller picks, which
// looks like access control and stops nobody. A secret works the same whether
// identity comes from a proxy or from a text box in the viewer.
const (
	// SessionHeader carries the secret for ordinary API calls.
	SessionHeader = "X-Oryxa-Session"

	// sessionCookiePrefix carries it for the live stream. EventSource cannot set
	// headers, which is the same constraint that made the API token a cookie —
	// and the same answer. Scoped by path to one room, so a browser holding
	// several rooms never sends one room's secret to another.
	sessionCookiePrefix = "oryxa_room_"
)

func sessionCookieName(id string) string { return sessionCookiePrefix + id }

// sessionSecret finds the secret on a request: header first, then the cookie
// for this specific room.
func sessionSecret(r *http.Request, id string) string {
	if s := strings.TrimSpace(r.Header.Get(SessionHeader)); s != "" {
		return s
	}
	if c, err := r.Cookie(sessionCookieName(id)); err == nil {
		return c.Value
	}
	return ""
}

// requireRoom guards everything under /v1/sessions/{id}.
//
// Creating and listing sit outside it: creating is how a secret is obtained in
// the first place, and the list deliberately carries no room contents. Knowing a
// room exists is not the same as being able to read it, in the same way that
// seeing a meeting on a wall calendar is not being in it.
func (s *Server) requireRoom(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := roomIDFromPath(r.URL.Path)
		// join is how a secret is exchanged for a cookie, so guarding it with
		// the cookie it exists to set would lock the door from the inside. It
		// checks the secret itself.
		if id == "" || strings.HasSuffix(r.URL.Path, "/join") {
			next.ServeHTTP(w, r)
			return
		}
		if s.mgr.Authorize(id, sessionSecret(r, id)) {
			next.ServeHTTP(w, r)
			return
		}
		// The same answer whether the room is missing or merely not yours.
		// Distinguishing them turns this into an oracle for which rooms exist,
		// which is most of what the scoping is protecting.
		writeErr(w, http.StatusNotFound, fmt.Errorf(
			"no such session, or the wrong session secret. Pass it as %s, "+
				"or sign in to the room in the viewer", SessionHeader))
	})
}

// roomIDFromPath pulls the id out of /v1/sessions/{id}/... and returns "" for
// anything else, including /v1/sessions itself.
func roomIDFromPath(path string) string {
	const prefix = "/v1/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// joinRoom exchanges a secret for a cookie scoped to that room, so the viewer's
// EventSource can authenticate. Mirrors /v1/auth/login exactly, and for the same
// reason: the stream cannot send a header.
func (s *Server) joinRoom(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Secret string `json:"secret"`
	}
	_ = decodeJSONBody(r, &req)

	secret := strings.TrimSpace(req.Secret)
	if secret == "" {
		secret = sessionSecret(r, id)
	}
	if !s.mgr.Authorize(id, secret) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such session, or the wrong session secret"))
		return
	}

	http.SetCookie(w, roomCookie(r, id, secret))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "joined": true})
}

// roomCookie carries one room's secret to the stream.
//
// Scoped by path to that room alone, so a browser sitting in five rooms holds
// five cookies and can never send one room's secret to another.
func roomCookie(r *http.Request, id, secret string) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName(id),
		Value:    secret,
		Path:     "/v1/sessions/" + id,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	}
}
