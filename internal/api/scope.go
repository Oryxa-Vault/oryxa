package api

import (
	"context"
	"crypto/subtle"
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

// boundNameKey carries the name a person key resolved to, from requireRoom to
// whichever handler needs to know who is acting.
type boundNameKey struct{}

// boundName is who this request proved to be, or "" if it arrived on the room
// secret and is therefore only claiming.
func boundName(r *http.Request) string {
	n, _ := r.Context().Value(boundNameKey{}).(string)
	return n
}

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
		if name, ok := s.mgr.Resolve(id, sessionSecret(r, id)); ok {
			// Resolved once, here, and carried on the context. Every handler
			// under this then gets the same answer without hashing the
			// credential again — and, more to the point, without any of them
			// being able to reach a different one.
			if name != "" {
				r = r.WithContext(context.WithValue(r.Context(), boundNameKey{}, name))
			}
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

// requireAdmin guards the two routes that change the agent registry.
//
// The registry is configuration, and everything else about this server treats
// configuration as something that comes from the machine rather than from the
// network: connectors are files, and what the shim may run is a file. `POST` and
// `DELETE /v1/agents` are the exception, and until now any token holder could
// use them — so anyone who could talk to a room could also delete the agent
// every other room depends on.
//
// Unset, this is the old behaviour and the banner says so, in the same shape as
// -token and -trust-header. Set, changing the registry needs a credential that
// reading a room does not.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken == "" || !isRegistryWrite(r) {
			next.ServeHTTP(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.adminToken)) == 1 ||
			subtle.ConstantTimeCompare([]byte(r.Header.Get(AdminHeader)), []byte(s.adminToken)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, fmt.Errorf(
			"changing the agent registry needs the admin token, sent as %s", AdminHeader))
	})
}

// AdminHeader carries the admin token. Separate from Authorization so a caller
// can hold both without one shadowing the other.
const AdminHeader = "X-Oryxa-Admin"

func isRegistryWrite(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/v1/agents") {
		return false
	}
	// check is a read that happens to be a POST: it probes an agent and changes
	// nothing. Treating it as a write would put the Test Connection button
	// behind a credential nobody using the viewer has.
	if strings.HasSuffix(r.URL.Path, "/check") {
		return false
	}
	return r.Method == http.MethodPost || r.Method == http.MethodDelete
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return h[7:]
	}
	return ""
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
	// Resolve rather than Authorize, so a person key gets a cookie too. A key
	// that opens the room over HTTP and not over the stream would be a key that
	// works everywhere except the viewer, which is where most people would use
	// it.
	name, ok := s.mgr.Resolve(id, secret)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such session, or the wrong session secret"))
		return
	}

	http.SetCookie(w, roomCookie(r, id, secret))
	// The name comes back so the viewer can stop offering a box to type one in.
	// Somebody whose identity is settled should not be shown a field implying
	// it is theirs to choose.
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "joined": true, "author": name})
}

// issueKey mints a key bound to a name, for whoever already holds this room.
//
// Guarded by requireRoom like everything else beneath a session, which is the
// right root: the person deciding somebody may be in the room is the person
// deciding what to call them. Oryxa is not deciding who anyone is — it is
// refusing to let a key be used under a name it was not issued for.
func (s *Server) issueKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Author string `json:"author"`
	}
	_ = decodeJSONBody(r, &req)

	key, hash, err := s.mgr.IssueKey(id, req.Author)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	// Only the hash is written down, so a key cannot be recovered from the log
	// by anyone who can read the room it belongs to.
	who, _ := s.identify(r, "")
	s.mgr.RecordInvite(id, who.Author, strings.TrimSpace(req.Author), hash)

	writeJSON(w, http.StatusCreated, map[string]any{
		"session": id,
		"author":  strings.TrimSpace(req.Author),
		"key":     key,
	})
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
