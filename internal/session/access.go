package session

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"sort"
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
