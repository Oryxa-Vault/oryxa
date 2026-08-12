package session

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Access control for one room.
//
// A session carries a secret, issued once when it is created and required on
// every request that reads or writes it. That is what read scoping is: the API
// token says you may talk to this server, and the session secret says which
// rooms are yours. Without it one token opened every room, including rooms the
// holder was never part of.
//
// A capability rather than a list of names, and the reason is that Oryxa has no
// accounts and deliberately never will — it accepts identity, it does not
// establish it. Scoping on author names would be scoping on a string the caller
// chooses, which looks exactly like access control and stops nobody. A secret
// nobody can guess works the same whether identity comes from a proxy or from a
// text box, and it degrades honestly: lose it and you are out, share it and they
// are in, which is what a room actually means.
//
// Participants are recorded alongside it, and are a different thing: the record
// of who has been here, not the gate. They are what owner-waking and directed
// output will read once they exist.

// newSecret returns a fresh room secret and the hash to keep.
//
// Only the hash is stored, and only the hash reaches the event log, so a secret
// cannot be recovered from a database backup or from the log a room's own
// members can read.
func newSecret() (secret string, hash string) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice. Refusing to create the room is
		// the only safe move: a predictable secret is worse than no room.
		panic("session: no entropy for a room secret: " + err.Error())
	}
	secret = hex.EncodeToString(b[:])
	return secret, hashSecret(secret)
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Resolve reports whether a secret opens a room, and who it says you are.
//
// Two kinds of credential come through the same door. The room secret is a
// bearer key: it opens the room and says nothing about who is holding it, so a
// caller presenting it still has to name themselves and the name is a claim. A
// person key is bound to a name when it is issued, and presenting it *is* being
// that person — the server stops reading the name from the request.
//
// That distinction is the whole of the identity fix, and it keeps the line this
// project drew: Oryxa still does not establish who anyone is. Whoever runs the
// room decides that this key is Priya, exactly as they decide who gets into the
// room at all. Oryxa binds the two together and refuses to let one be used under
// another's name.
//
// The empty name is not a failure. It means "opened by the room secret", which
// is how the owner, the CLI and anything holding its own identity get in.
func (m *Manager) Resolve(id, secret string) (name string, ok bool) {
	m.mu.RLock()
	s, found := m.sessions[id]
	m.mu.RUnlock()
	if !found || secret == "" {
		return "", false
	}

	s.mu.Lock()
	roomHash := s.secretHash
	keys := make(map[string]string, len(s.keys))
	for n, h := range s.keys {
		keys[n] = h
	}
	s.mu.Unlock()

	if roomHash == "" {
		return "", false // predates scoping; see Unscoped
	}
	got := hashSecret(secret)
	if subtle.ConstantTimeCompare([]byte(got), []byte(roomHash)) == 1 {
		return "", true
	}
	// Every key is compared even after a match, so the time taken does not
	// depend on which person's key was presented or how many exist.
	var matched string
	for n, h := range keys {
		if subtle.ConstantTimeCompare([]byte(got), []byte(h)) == 1 {
			matched = n
		}
	}
	return matched, matched != ""
}

// IssueKey binds a name to a fresh key for this room and returns it once.
//
// Issued by whoever already holds the room, which is the only sensible root:
// the same person deciding that someone may be in the room is deciding what to
// call them. Reissuing for a name replaces the old key rather than adding a
// second, so handing one out twice cannot leave two people answering to it.
func (m *Manager) IssueKey(id, name string) (key, hash string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", fmt.Errorf("a key has to be for somebody: name is required")
	}
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return "", "", ErrNoSession
	}
	key, hash = newSecret()

	s.mu.Lock()
	if s.keys == nil {
		s.keys = map[string]string{}
	}
	s.keys[name] = hash
	s.mu.Unlock()
	return key, hash, nil
}

// Authorize reports whether a secret opens a session.
//
// Compared in constant time. The comparison is on hashes, which are fixed
// length, so a byte-wise compare would leak only the length of a hash — but the
// habit is worth more than the analysis, and the analysis changes if the storage
// ever does.
func (m *Manager) Authorize(id, secret string) bool {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	s.mu.Lock()
	want := s.secretHash
	s.mu.Unlock()

	// A session with no hash predates scoping — it was created by an older
	// build and restored from that log. Refusing it outright would make an
	// upgrade lose every room; opening it to anyone would make the upgrade a
	// silent downgrade. It is closed, and the operator is told why.
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(want)) == 1
}

// Unscoped reports sessions that cannot be opened by anyone because they were
// created before rooms had secrets. Named at startup rather than discovered one
// confused user at a time.
func (m *Manager) Unscoped() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for id, s := range m.sessions {
		s.mu.Lock()
		missing := s.secretHash == ""
		s.mu.Unlock()
		if missing {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// UsedBy returns the open sessions that have this agent in them, sorted.
//
// Exists because removing an agent is not a local act: a room holding one has a
// lane for it, and taking the connector away leaves that lane unable to run a
// turn for as long as the room lives. Closed rooms are excluded — they are
// history, and history is allowed to name an agent that no longer exists.
func (m *Manager) UsedBy(agent string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for id, s := range m.sessions {
		s.mu.Lock()
		closed := s.state == StateClosed
		agents := append([]string(nil), s.agents...)
		s.mu.Unlock()
		if closed {
			continue
		}
		for _, a := range agents {
			if a == agent {
				out = append(out, id)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Participants returns everyone who has spoken in a session, sorted.
//
// Derived from the same set the wake ladder already keeps: a name belongs to a
// person because a person used it, which needs no accounts and no configuration.
// Reusing it rather than keeping a second list means the room cannot disagree
// with itself about who is in it.
func (m *Manager) Participants(id string) []string {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	s.imu.Lock()
	defer s.imu.Unlock()
	out := make([]string, 0, len(s.speakers))
	for name := range s.speakers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// RecordInvite writes the binding to the log so it survives a restart.
//
// Only the hash. A room's own members can read its events, so the key itself
// there would mean anyone who was ever in the room keeps a way in under someone
// else's name.
func (m *Manager) RecordInvite(id, by, author, hash string) {
	m.emit(id, "participant.invited", by, "", map[string]any{
		"author": author, "key_sha256": hash,
	})
}
