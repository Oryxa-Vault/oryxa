package api

import (
	"encoding/json"
	"fmt"

	"github.com/Oryxa-Vault/oryxa/internal/connector"
	"github.com/Oryxa-Vault/oryxa/internal/events"
)

// Connectors registered over the API are durable.
//
// They were not, and the way that failed was worse than losing them. Sessions
// replay from the log on restart, so a room whose agent had been registered over
// the API came back intact and then failed every turn with "agent no longer
// registered" — a transcript sitting there next to an agent that had evaporated.
// A platform whose agents come from a UI rather than a file lost every one of
// them on a deploy.
//
// A registration is an event, folded at startup like everything else. That buys
// durability, ordering, who-changed-what, and a history of edits to a connector
// — all from the mechanism already carrying sessions and shared context.

const (
	kindAgentRegistered = "agent.registered"
	kindAgentRemoved    = "agent.removed"
)

// recordAgent persists a registration. Called after the registry accepts it, so
// a spec that failed validation never reaches the log.
func (s *Server) recordAgent(actor string, spec *connector.Spec) {
	if _, err := s.log.Append(events.SystemStream, kindAgentRegistered, actor, "",
		map[string]any{"spec": spec}); err != nil {
		// The agent is live but will not survive a restart. Worth saying out
		// loud for the same reason a failed session append is: what is on disk
		// no longer matches what is running.
		fmt.Printf("oryxa: could not persist agent %q: %v\n", spec.Name, err)
	}
}

func (s *Server) recordAgentRemoved(actor, name string) {
	if _, err := s.log.Append(events.SystemStream, kindAgentRemoved, actor, "",
		map[string]any{"name": name}); err != nil {
		fmt.Printf("oryxa: could not persist removal of agent %q: %v\n", name, err)
	}
}

// RestoreAgents replays registrations into a registry.
//
// Run it after LoadDir, so the API wins a name collision with a file. That is
// the order that matches what people expect: registering over the API is the
// more recent deliberate act, and it is the only one available to someone who
// cannot reach the filesystem. A collision is reported rather than silent,
// because a file that looks authoritative and is being overridden is exactly
// the kind of thing that costs an afternoon.
func RestoreAgents(log events.Store, reg *connector.Registry, origin connector.Origin) (restored int, shadowed []string, err error) {
	evs, err := log.Since(events.SystemStream, 0)
	if err != nil {
		return 0, nil, fmt.Errorf("read %s: %w", events.SystemStream, err)
	}

	for _, ev := range evs {
		switch ev.Kind {
		case kindAgentRegistered:
			var d struct {
				Spec json.RawMessage `json:"spec"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			spec, perr := connector.ParseJSON(d.Spec)
			if perr != nil {
				// A spec that no longer validates is skipped, not fatal. The
				// rules can tighten between versions, and one stale connector
				// must not stop a server from starting.
				fmt.Printf("oryxa: skipping stored agent (seq %d): %v\n", ev.Seq, perr)
				continue
			}
			if _, existed := reg.Get(spec.Name); existed {
				shadowed = append(shadowed, spec.Name)
			}
			// Everything in this log arrived over the API, and a restart is not
			// a promotion. Source is deliberately not serialised, so it has to
			// be re-established here rather than read back.
			spec.Source = origin
			if perr := reg.Put(spec); perr != nil {
				fmt.Printf("oryxa: skipping stored agent %q: %v\n", spec.Name, perr)
				continue
			}
			restored++

		case kindAgentRemoved:
			var d struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			// A file connector deleted over the API stays deleted across a
			// restart: the file reloads, then this removes it again.
			if reg.Delete(d.Name) && restored > 0 {
				restored--
			}
		}
	}
	return restored, dedupe(shadowed), nil
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
