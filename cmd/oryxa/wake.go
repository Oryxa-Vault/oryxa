package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/oryxa/oryxa/internal/session"
)

// cmdWake answers "why did my agent not say anything".
//
// It is the counterpart to `check`: check tells you an agent can be reached,
// this tells you whether it would ever be asked. Both read files and need no
// running server, because the moment you need them is usually before you have
// one working.
func cmdWake(args []string) {
	fs := flag.NewFlagSet("wake", flag.ExitOnError)
	dir := connectorsFlag(fs)
	people := fs.String("people", "", "comma-separated names of people in the room; "+
		"a message naming one of them is for them, and no agent answers it")
	only := fs.String("agents", "", "comma-separated agents in the room (default: all configured)")
	to := fs.String("to", "", "comma-separated agents addressed explicitly")
	asJSON := fs.Bool("json", false, "print raw JSON")

	pos := parseArgs(fs, args)
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, `usage: oryxa wake "the message someone would type"`)
		os.Exit(2)
	}
	text := strings.Join(pos, " ")

	reg := loadRegistry(*dir)
	agents := *only
	if agents == "" {
		var names []string
		for _, s := range reg.List() {
			names = append(names, s.Name)
		}
		agents = strings.Join(names, ",")
	}
	room := split(agents)
	woken, why := session.WhoWakes(text, split(*to), room, split(*people), reg)

	if *asJSON {
		printJSON(map[string]any{"text": text, "wake": woken, "why": why, "room": room})
		return
	}

	fmt.Printf("\n  %q\n\n", text)
	fmt.Printf("  %-10s %s\n", "why", why)
	switch {
	case len(woken) == 0:
		fmt.Printf("  %-10s nobody answers this\n", "wake")
	case len(woken) == len(room):
		fmt.Printf("  %-10s everyone (%d agents)\n", "wake", len(woken))
	default:
		fmt.Printf("  %-10s %s\n", "wake", strings.Join(woken, ", "))
	}

	// The silent ones are the point of the command, so say what would have
	// reached each of them instead of leaving it to be guessed at.
	if len(woken) < len(room) {
		fmt.Printf("\n  staying quiet, and what would wake them:\n")
		sort.Strings(room)
		for _, a := range room {
			if containsStr(woken, a) {
				continue
			}
			hint := "@" + a
			if spec, ok := reg.Get(a); ok && len(spec.Interests) > 0 {
				hint += "  ·  interests: " + strings.Join(spec.Interests, ", ")
			}
			fmt.Printf("    %-16s %s\n", a, hint)
		}
	}
	fmt.Println()
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
