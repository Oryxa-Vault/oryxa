package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Where the CLI keeps room secrets.
//
// `oryxa open` and `oryxa send` are two processes, so a secret held in memory
// would be a secret the next command does not have. Writing it down is what
// makes the shell usable at all — the same trade every CLI with a credential
// makes, and the same mitigations: the user's own config directory, 0600, and
// one file rather than a secret per shell history line.
//
// A flag and an environment variable come first, so nothing has to be written
// down in CI or in a container that should hold no state.

const roomsEnv = "ORYXA_SESSION_SECRET"

var roomsOnce struct {
	sync.Once
	path string
}

// roomsPath is ~/.config/oryxa/rooms.json, honouring XDG_CONFIG_HOME.
func roomsPath() string {
	roomsOnce.Do(func() {
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return
			}
			dir = filepath.Join(home, ".config")
		}
		roomsOnce.path = filepath.Join(dir, "oryxa", "rooms.json")
	})
	return roomsOnce.path
}

func loadRooms() map[string]string {
	out := map[string]string{}
	p := roomsPath()
	if p == "" {
		return out
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

// rememberRoom writes one secret down. Failures are silent on purpose: not being
// able to save it makes the next command ask for it, which is inconvenient, and
// refusing to open a room over it would be worse.
func rememberRoom(id, secret string) {
	if id == "" || secret == "" {
		return
	}
	p := roomsPath()
	if p == "" {
		return
	}
	rooms := loadRooms()
	rooms[id] = secret
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	b, err := json.MarshalIndent(rooms, "", "  ")
	if err != nil {
		return
	}
	// 0600 before anything is in it: writing world-readable and then chmod-ing
	// leaves a window, and the window is the whole file.
	_ = os.WriteFile(p, b, 0o600)
}

// roomSecret finds a room's secret: the flag, then the environment, then what
// was written down when it was opened.
func roomSecret(id, flag string) string {
	if s := strings.TrimSpace(flag); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv(roomsEnv)); s != "" {
		return s
	}
	return loadRooms()[id]
}

// roomIDFromPath pulls a session id out of /v1/sessions/{id}/..., so every
// request carries its own room's secret without each command remembering to.
func roomIDFromPath(path string) string {
	const prefix = "/v1/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	// Trim a query string, so /stream?since=0 does not become part of the id.
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
