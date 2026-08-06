package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oryxa/oryxa/internal/connector"
)

func connectorsFlag(fs *flag.FlagSet) *string {
	return fs.String("connectors", envOr("ORYXA_CONNECTORS", "./connectors"),
		"directory of connector files (or ORYXA_CONNECTORS)")
}

// loadRegistry reads connector files without needing a server. That split is
// deliberate: diagnosing a connector should not require the thing you are
// trying to diagnose to already be working.
func loadRegistry(dir string) *connector.Registry {
	defaultAgentHost()
	reg := connector.NewRegistry()
	if _, err := reg.LoadDir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "\n  loading connectors: %v\n\n", err)
		os.Exit(1)
	}
	return reg
}

func mustGet(reg *connector.Registry, dir, name string) *connector.Spec {
	spec, ok := reg.Get(name)
	if ok {
		return spec
	}
	fmt.Fprintf(os.Stderr, "\n  no connector named %q in %s\n", name, dir)
	if list := reg.List(); len(list) > 0 {
		var names []string
		for _, s := range list {
			names = append(names, s.Name)
		}
		fmt.Fprintf(os.Stderr, "  available: %s\n", strings.Join(names, ", "))
	}
	fmt.Fprintln(os.Stderr)
	os.Exit(1)
	return nil
}

// ---- agents ----

func cmdAgents(args []string) {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	dir := connectorsFlag(fs)
	asJSON := fs.Bool("json", false, "print raw JSON")
	_ = fs.Parse(args)

	reg := loadRegistry(*dir)
	list := reg.List()

	if *asJSON {
		printJSON(list)
		return
	}
	if len(list) == 0 {
		fmt.Printf("\n  no connectors in %s\n  see connectors/templates/ for starting points\n\n", *dir)
		return
	}
	fmt.Println()
	fmt.Printf("  %-14s %-40s %s\n", "AGENT", "BASE", "CAPABILITIES")
	for _, s := range list {
		caps := strings.Join(s.Capabilities, ",")
		if caps == "" {
			caps = "—"
		}
		fmt.Printf("  %-14s %-40s %s\n", s.Name, resolveBase(s), caps)
	}
	fmt.Printf("\n  oryxa check <agent>   probe one with a real turn\n\n")
}

// ---- which ----

// cmdWhich answers "where does this actually point, and which file said so".
//
// Worth its own command because base is templated: the same connector resolves
// to different hosts on a machine and in a container, and "it works in my shell
// but not in the server" is otherwise a confusing afternoon.
func cmdWhich(args []string) {
	fs := flag.NewFlagSet("which", flag.ExitOnError)
	dir := connectorsFlag(fs)
	asJSON := fs.Bool("json", false, "print raw JSON")

	pos := parseArgs(fs, args)
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "usage: oryxa which <agent>")
		os.Exit(2)
	}
	reg := loadRegistry(*dir)
	spec := mustGet(reg, *dir, pos[0])

	file := findConnectorFile(*dir, spec.Name)
	resolved := resolveBase(spec)

	if *asJSON {
		printJSON(map[string]any{
			"name": spec.Name, "file": file,
			"base": spec.Base, "resolved": resolved,
			"vars": spec.Vars, "capabilities": spec.Capabilities,
		})
		return
	}

	fmt.Printf("\n  %s\n\n", spec.Name)
	fmt.Printf("  %-12s %s\n", "file", file)
	fmt.Printf("  %-12s %s\n", "base", spec.Base)
	if resolved != spec.Base {
		fmt.Printf("  %-12s %s\n", "resolves to", resolved)
	}
	fmt.Printf("  %-12s %s\n", "turn", strings.ToUpper(methodOr(spec.Turn))+" "+spec.Turn.Path)
	if spec.Open != nil {
		fmt.Printf("  %-12s %s\n", "open", strings.ToUpper(methodOr(spec.Open))+" "+spec.Open.Path)
	}
	if len(spec.Capabilities) > 0 {
		fmt.Printf("  %-12s %s\n", "capabilities", strings.Join(spec.Capabilities, ", "))
	}
	if len(spec.Vars) > 0 {
		var kv []string
		for k, v := range spec.Vars {
			kv = append(kv, k+"="+v)
		}
		fmt.Printf("  %-12s %s\n", "vars", strings.Join(kv, " "))
	}

	// The host/container split is the common surprise, so name it explicitly.
	if strings.Contains(spec.Base, "ORYXA_AGENT_HOST") {
		fmt.Printf("\n  ORYXA_AGENT_HOST=%s — a containerised server resolves this differently\n",
			envOr("ORYXA_AGENT_HOST", "localhost"))
	}
	fmt.Println()
}

func findConnectorFile(dir, name string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			continue
		}
		var s *connector.Spec
		if ext == ".json" {
			s, err = connector.ParseJSON(b)
		} else {
			s, err = connector.ParseYAML(b)
		}
		if err == nil && s.Name == name {
			return path
		}
	}
	return "(registered over the API, not from a file)"
}

func resolveBase(s *connector.Spec) string {
	return connector.Ctx{Vars: s.Vars}.RenderString(s.Base)
}

func methodOr(step *connector.Step) string {
	if step == nil || step.Method == "" {
		return "post"
	}
	return step.Method
}

// defaultAgentHost keeps a plain run working with no environment at all.
func defaultAgentHost() {
	if os.Getenv("ORYXA_AGENT_HOST") == "" {
		os.Setenv("ORYXA_AGENT_HOST", "localhost")
	}
}
