package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// parseArgs parses flags that appear anywhere, not only before the first
// positional. The stdlib stops at the first non-flag argument, so
//
//	oryxa send s_123 "hello" -f
//
// would silently ignore -f — which is exactly how people type it. Parsing
// repeatedly, peeling off one positional each round, accepts both orders.
func parseArgs(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// Commands that talk to a running server. Every one takes -server and -token,
// falling back to ORYXA_URL and ORYXA_TOKEN.
func serverFlags(fs *flag.FlagSet) (*string, *string, *bool) {
	return fs.String("server", "", "Oryxa URL (or ORYXA_URL; default http://localhost:8080)"),
		fs.String("token", "", "API token (or ORYXA_TOKEN)"),
		fs.Bool("json", false, "print raw JSON instead of a table")
}

// ---- sessions ----

func cmdSessions(args []string) {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	server, token, asJSON := serverFlags(fs)
	_ = fs.Parse(args)

	var out struct {
		Sessions []struct {
			ID      string    `json:"id"`
			Agents  []string  `json:"agents"`
			State   string    `json:"state"`
			Created time.Time `json:"created"`
		} `json:"sessions"`
	}
	c := newClient(*server, *token)
	fail(c.do("GET", "/v1/sessions", nil, &out))

	if *asJSON {
		printJSON(out.Sessions)
		return
	}
	if len(out.Sessions) == 0 {
		fmt.Printf("\n  no sessions yet — oryxa open <agent>\n\n")
		return
	}
	fmt.Println()
	fmt.Printf("  %-22s %-9s %-8s %s\n", "SESSION", "STATE", "AGE", "AGENTS")
	for _, s := range out.Sessions {
		fmt.Printf("  %-22s %-9s %-8s %s\n", s.ID, s.State,
			shortAge(time.Since(s.Created)), strings.Join(s.Agents, ", "))
	}
	fmt.Println()
}

// ---- open ----

func cmdOpen(args []string) {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	server, token, asJSON := serverFlags(fs)
	agents := parseArgs(fs, args)
	if len(agents) == 0 {
		fmt.Fprintln(os.Stderr, "usage: oryxa open <agent> [agent...]")
		os.Exit(2)
	}

	var out map[string]any
	c := newClient(*server, *token)
	fail(c.do("POST", "/v1/sessions", map[string]any{"agents": agents}, &out))

	if *asJSON {
		printJSON(out)
		return
	}
	fmt.Printf("\n  %v\n  agents: %s\n\n", out["id"], strings.Join(agents, ", "))
	fmt.Printf("  oryxa send %v \"your question\"\n", out["id"])
	fmt.Printf("  oryxa tail %v\n\n", out["id"])
}

// ---- send ----

func cmdSend(args []string) {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	server, token, asJSON := serverFlags(fs)
	author := fs.String("as", envOr("USER", "cli"), "who is speaking")
	follow := fs.Bool("f", false, "follow the answers instead of returning immediately")

	rest := parseArgs(fs, args)
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, `usage: oryxa send <session> "text" [-as name] [-f]`)
		os.Exit(2)
	}
	sid, text := rest[0], strings.Join(rest[1:], " ")

	c := newClient(*server, *token)
	var out map[string]any
	fail(c.do("POST", "/v1/sessions/"+sid+"/input",
		map[string]string{"text": text, "author": *author}, &out))

	if *asJSON {
		printJSON(out)
		return
	}
	if !*follow {
		fmt.Printf("\n  queued as %v\n  oryxa tail %s\n\n", out["id"], sid)
		return
	}
	followSession(c, sid, true)
}

// ---- tail ----

func cmdTail(args []string) {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	server, token, _ := serverFlags(fs)
	since := fs.Int64("since", -1, "start from this sequence number; -1 means only what happens next")
	raw := fs.Bool("raw", false, "print every event, including the agent's opaque activity")

	pos := parseArgs(fs, args)
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "usage: oryxa tail <session> [-since 0] [-raw]")
		os.Exit(2)
	}
	sid := pos[0]
	c := newClient(*server, *token)

	from := *since
	if from < 0 {
		// Default to the current end, so tail behaves like tail rather than
		// replaying an entire history nobody asked for.
		from = currentSeq(c, sid)
	}
	fmt.Printf("\n  following %s from seq %d — ctrl-c to stop\n\n", sid, from)
	streamEvents(c, sid, from, *raw, false)
}

// ---- replay ----

func cmdReplay(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	server, token, asJSON := serverFlags(fs)
	raw := fs.Bool("raw", false, "print every event, including the agent's opaque activity")

	pos := parseArgs(fs, args)
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "usage: oryxa replay <session> [-raw] [-json]")
		os.Exit(2)
	}
	c := newClient(*server, *token)

	var out struct {
		Events []event `json:"events"`
	}
	fail(c.do("GET", "/v1/sessions/"+pos[0]+"/events", nil, &out))

	if *asJSON {
		printJSON(out.Events)
		return
	}
	fmt.Println()
	p := newPrinter(*raw)
	for _, ev := range out.Events {
		p.print(ev)
	}
	fmt.Println()
}

// ---- context ----

func cmdContext(args []string) {
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	server, token, asJSON := serverFlags(fs)
	appendTo := fs.String("append", "", "append to this key")
	setKey := fs.String("set", "", "set this key")
	value := fs.String("value", "", "value for -append or -set")
	author := fs.String("as", envOr("USER", "cli"), "who is writing")

	pos := parseArgs(fs, args)
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "usage: oryxa context <session> [-append key -value text] [-set key -value text]")
		os.Exit(2)
	}
	sid := pos[0]
	c := newClient(*server, *token)

	switch {
	case *appendTo != "":
		var e map[string]any
		fail(c.do("POST", "/v1/sessions/"+sid+"/context/"+*appendTo,
			map[string]string{"append": *value, "author": *author}, &e))
		fmt.Printf("\n  appended to %s\n\n", *appendTo)
		return
	case *setKey != "":
		var e map[string]any
		fail(c.do("POST", "/v1/sessions/"+sid+"/context/"+*setKey,
			map[string]string{"value": *value, "author": *author}, &e))
		fmt.Printf("\n  %s = %v (version %v)\n\n", *setKey, e["value"], e["version"])
		return
	}

	var out struct {
		Context []struct {
			Key     string `json:"key"`
			Kind    string `json:"kind"`
			Pinned  bool   `json:"pinned"`
			Value   string `json:"value"`
			Version int64  `json:"version"`
			By      string `json:"by"`
			Items   []struct {
				By   string `json:"by"`
				Text string `json:"text"`
			} `json:"items"`
		} `json:"context"`
	}
	fail(c.do("GET", "/v1/sessions/"+sid+"/context", nil, &out))

	if *asJSON {
		printJSON(out.Context)
		return
	}
	if len(out.Context) == 0 {
		fmt.Printf("\n  no shared context yet\n\n")
		return
	}
	fmt.Println()
	for _, e := range out.Context {
		pin := "  "
		if e.Pinned {
			pin = "📌"
		}
		if e.Kind == "append" {
			fmt.Printf("  %s %-16s %d entries\n", pin, e.Key, len(e.Items))
			for _, it := range e.Items {
				fmt.Printf("       %-8s %s\n", it.By, it.Text)
			}
			continue
		}
		fmt.Printf("  %s %-16s v%-4d %-8s %s\n", pin, e.Key, e.Version, e.By, e.Value)
	}
	fmt.Println()
}

// ---- shared plumbing ----

type event struct {
	Seq   int64           `json:"seq"`
	TS    time.Time       `json:"ts"`
	Kind  string          `json:"kind"`
	Actor string          `json:"actor"`
	Turn  string          `json:"turn"`
	Data  json.RawMessage `json:"data"`
}

func currentSeq(c *client, sid string) int64 {
	var out struct {
		Events []event `json:"events"`
	}
	if err := c.do("GET", "/v1/sessions/"+sid+"/events", nil, &out); err != nil {
		fail(err)
	}
	if len(out.Events) == 0 {
		return 0
	}
	return out.Events[len(out.Events)-1].Seq
}

func followSession(c *client, sid string, untilIdle bool) {
	fmt.Println()
	streamEvents(c, sid, currentSeq(c, sid)-1, false, untilIdle)
	fmt.Println()
}

// streamEvents prints a session's stream. Text parts are printed as they
// arrive so a streaming agent reads like one, rather than appearing all at once
// when the turn ends.
func streamEvents(c *client, sid string, since int64, raw, untilIdle bool) {
	running := map[string]bool{}
	started := false
	p := newPrinter(raw)

	err := c.stream(fmt.Sprintf("/v1/sessions/%s/stream?since=%d", sid, since),
		func(payload json.RawMessage) bool {
			var ev event
			if json.Unmarshal(payload, &ev) != nil {
				return true
			}
			p.print(ev)

			switch ev.Kind {
			case "turn.started":
				running[ev.Turn] = true
				started = true
			case "turn.finished", "turn.failed", "turn.cancelled":
				delete(running, ev.Turn)
				if untilIdle && started && len(running) == 0 {
					return false
				}
			}
			return true
		})
	if err != nil {
		fail(err)
	}
}

// printer renders a stream. It keeps the little state needed to read well:
// one input fans out to a turn per agent, so the question is printed once per
// group rather than once per agent.
//
// The other piece of state is who is currently speaking, and that one exists
// because lanes run in parallel. Announcing an agent when its turn starts reads
// correctly only if turns take it in turns, which is exactly what this project
// went out of its way not to do: two agents starting together printed both
// headers back to back and then concatenated both answers into one paragraph.
//
// So the header is printed when an agent first says something, and again
// whenever the speaker changes. A single agent is unaffected — one header, one
// continuous stream, still live. Several agents read as blocks, attributed,
// in the order the room actually heard them.
type printer struct {
	raw    bool
	w      io.Writer
	groups map[string]bool

	speaking string          // whose text is mid-line right now
	spoke    map[string]bool // agents that have said something this turn
}

func newPrinter(raw bool) *printer {
	return &printer{raw: raw, w: os.Stdout, groups: map[string]bool{}, spoke: map[string]bool{}}
}

// say prints text as coming from actor, opening a new block if the speaker
// changed since the last thing printed.
func (p *printer) say(actor, text string) {
	if p.speaking != actor {
		p.endLine()
		fmt.Fprintf(p.w, "  %s ‹ ", actor)
		p.speaking = actor
	}
	fmt.Fprint(p.w, text) // no newline: deltas concatenate into one answer
}

func (p *printer) endLine() {
	if p.speaking != "" {
		fmt.Fprintln(p.w)
		p.speaking = ""
	}
}

func (p *printer) print(ev event) {
	raw := p.raw
	var d struct {
		Text  string `json:"text"`
		Kind  string `json:"kind"`
		Agent string `json:"agent"`
		Error string `json:"error"`
		Key   string `json:"key"`
		Value string `json:"value"`
		Group string `json:"group"`
	}
	if len(ev.Data) > 0 {
		_ = json.Unmarshal(ev.Data, &d)
	}

	switch ev.Kind {
	case "output.part":
		if d.Kind == "text" {
			p.spoke[ev.Actor] = true
			p.say(ev.Actor, d.Text)
		} else if raw {
			p.endLine()
			fmt.Fprintf(p.w, "  · %s %s\n", ev.Actor, strings.TrimSpace(string(ev.Data)))
		}
	case "input.submitted":
		if d.Group != "" {
			if p.groups[d.Group] {
				return // already shown; this copy belongs to another agent
			}
			p.groups[d.Group] = true
		}
		p.endLine()
		fmt.Fprintf(p.w, "\n  %s › %s\n", ev.Actor, d.Text)
	case "turn.started":
		// Nothing printed here. An agent is announced by speaking.
		delete(p.spoke, d.Agent)
	case "turn.finished":
		if p.speaking == ev.Actor {
			p.endLine()
		}
		delete(p.spoke, ev.Actor)

	// A turn that worked and said nothing. The server already worked out which
	// half it came from and put the selectors on the event, so the terminal
	// says what the viewer says rather than inventing a thinner version of it —
	// a silent agent is the failure people blame the framework for.
	//
	// Decoded separately because `text` is a string on every other event and a
	// list of selectors on this one. Sharing a struct means the whole decode
	// fails on the type mismatch and every field silently reads as empty.
	case "turn.empty":
		var e struct {
			Reason string   `json:"reason"`
			Text   []string `json:"text"`
			When   string   `json:"when"`
		}
		_ = json.Unmarshal(ev.Data, &e)
		p.endLine()
		fmt.Fprintf(p.w, "  %s ‹ finished without saying anything — %s\n", ev.Actor, e.Reason)
		var sel []string
		if len(e.Text) > 0 {
			sel = append(sel, "text: "+strings.Join(e.Text, " | "))
		}
		if e.When != "" {
			sel = append(sel, "when: "+e.When)
		}
		if len(sel) > 0 {
			fmt.Fprintf(p.w, "      [%s]\n", strings.Join(sel, "  "))
		}
	case "turn.failed":
		p.endLine()
		fmt.Fprintf(p.w, "  %s failed: %s\n", ev.Actor, d.Error)
	case "context.appended":
		p.endLine()
		fmt.Fprintf(p.w, "  · %s appended to %s\n", ev.Actor, d.Key)
	case "context.set":
		p.endLine()
		fmt.Fprintf(p.w, "  · %s set %s = %s\n", ev.Actor, d.Key, d.Value)
	case "conflict.rejected":
		p.endLine()
		fmt.Fprintf(p.w, "  · conflict on %s (rejected)\n", d.Key)
	default:
		if raw {
			p.endLine()
			fmt.Fprintf(p.w, "  · seq=%d %s %s\n", ev.Seq, ev.Kind, ev.Actor)
		}
	}
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func fail(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n  %v\n\n", err)
		os.Exit(1)
	}
}
