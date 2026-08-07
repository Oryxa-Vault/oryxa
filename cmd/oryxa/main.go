// Command oryxa runs the Oryxa server and its tooling.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/oryxa/oryxa/internal/api"
	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/events"
	"github.com/oryxa/oryxa/internal/session"
)

// version is set at build time:
//
//	go build -ldflags "-X main.version=v0.2.0"
//
// Left as "dev" it falls back to the VCS stamp Go records automatically, so a
// `go install` from source still reports something specific.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	// -v / --version work in the leading position too, which is where people
	// reach for them.
	switch cmd {
	case "-v", "--version", "-version", "version":
		fmt.Println(versionString())
		return
	case "-h", "--help", "help":
		usage()
		return
	}

	switch cmd {
	// server
	case "serve":
		serve(args, false)
	case "launch":
		launch(args)

	// connectors — these read files and need no running server
	case "agents":
		cmdAgents(args)
	case "which":
		cmdWhich(args)
	case "check":
		check(args)

	// rooms — these talk to a running server
	case "sessions":
		cmdSessions(args)
	case "open":
		cmdOpen(args)
	case "send":
		cmdSend(args)
	case "tail":
		cmdTail(args)
	case "replay":
		cmdReplay(args)
	case "context":
		cmdContext(args)

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func versionString() string {
	v := version
	if v == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok {
			var rev, dirty string
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if len(s.Value) >= 12 {
						rev = s.Value[:12]
					}
				case "vcs.modified":
					if s.Value == "true" {
						dirty = "-dirty"
					}
				}
			}
			if rev != "" {
				v = "dev+" + rev + dirty
			}
		}
	}
	return fmt.Sprintf("oryxa %s (%s/%s, %s)", v, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func usage() {
	fmt.Print(`oryxa — many people, one agent

  Puts your existing agents in a shared session several people can watch and
  talk to. Nothing about the agents changes.

Usage:
  oryxa <command> [flags]

Server
  serve                 run the API and viewer
  launch window         run and open the viewer in a browser

Connectors                                    (read files; no server needed)
  agents                list configured connectors
  which <agent>         where a connector points, and which file said so
  check <agent>         probe an agent with a real turn

Rooms                                         (talk to a running server)
  open <agent>...       start a session with one or more agents
  send <session> TEXT   say something to the room
  tail <session>        follow the live stream
  replay <session>      print the history
  sessions              list sessions
  context <session>     read or write shared context

Other
  version               print the version
  help                  this

Common flags
  -connectors DIR       connector directory        (ORYXA_CONNECTORS)
  -server URL           a running Oryxa            (ORYXA_URL)
  -token TOKEN          API token                  (ORYXA_TOKEN)
  -json                 machine-readable output

Serve flags
  -addr :8080           listen address
  -db DSN               postgres; in-memory if unset  (ORYXA_DATABASE_URL)
  -token TOKEN          require this token            (ORYXA_TOKEN)
  -trust-header HEADER  identity from your proxy      (ORYXA_TRUST_HEADER)
  -reset                erase every session on start  (ORYXA_RESET) — development

Run  oryxa <command> -h  for a command's own flags.

  https://github.com/oryxa/oryxa
`)
}

// ---- serve / launch ----

func serve(args []string, openWindow bool) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dir := connectorsFlag(fs)
	dsn := fs.String("db", os.Getenv("ORYXA_DATABASE_URL"),
		"postgres DSN; in-memory if empty (nothing survives a restart)")
	token := fs.String("token", os.Getenv("ORYXA_TOKEN"),
		"shared token guarding the API; open to anyone who can reach the port if empty")
	trustHeader := fs.String("trust-header", os.Getenv("ORYXA_TRUST_HEADER"),
		"take the acting user from this header, set by your proxy (e.g. X-Forwarded-User); "+
			"only safe when nothing can reach this port except that proxy")
	reset := fs.Bool("reset", os.Getenv("ORYXA_RESET") != "",
		"erase every session before starting (ORYXA_RESET); for development, where "+
			"a durable log means each restart brings back every room you were done with")
	_ = fs.Parse(args)

	defaultAgentHost()

	reg := connector.NewRegistry()
	n, err := reg.LoadDir(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading connectors: %v\n", err)
		os.Exit(1)
	}

	log, storeKind, err := openStore(*dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store: %v\n", err)
		os.Exit(1)
	}
	defer log.Close()

	exec := connector.NewExecutor()
	mgr := session.NewManager(reg, exec, log)

	// Reset before rehydrate, so nothing is restored only to be deleted.
	var wiped int
	if *reset {
		if wiped, err = log.Reset(); err != nil {
			fmt.Fprintf(os.Stderr, "reset: %v\n", err)
			os.Exit(1)
		}
	}

	recovered, err := mgr.Rehydrate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rehydrate: %v\n", err)
		os.Exit(1)
	}

	srv := api.New(reg, exec, mgr, log).
		WithToken(*token).
		WithTrustedHeader(*trustHeader)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: SSE streams stay open for the life of a session.
	}

	url := browserURL(*addr)
	fmt.Printf("\n  oryxa %s\n", version)
	fmt.Printf("  ├─ viewer      %s\n", url)
	fmt.Printf("  ├─ api         %s/v1\n", url)
	fmt.Printf("  ├─ store       %s\n", storeKind)
	if *reset {
		// Said plainly and every time. Erasing the log is the one startup option
		// that destroys something, and a flag left set in an .env is exactly how
		// it gets used somewhere it should not be.
		fmt.Printf("  ├─ reset       ON — erased %d session(s); this runs on every start\n", wiped)
	}
	if *token == "" {
		fmt.Printf("  ├─ auth        none — anyone who can reach %s has full access\n", *addr)
	} else {
		fmt.Printf("  ├─ auth        shared token\n")
	}
	if *trustHeader == "" {
		fmt.Printf("  ├─ identity    self-declared — the log records claims, not people\n")
	} else {
		fmt.Printf("  ├─ identity    from %s (bind privately; this header is spoofable)\n", *trustHeader)
	}
	fmt.Printf("  └─ connectors  %d loaded from %s\n\n", n, *dir)
	if recovered > 0 {
		fmt.Printf("     recovered %d session(s) from the log\n\n", recovered)
	}
	for _, s := range reg.List() {
		fmt.Printf("     • %-14s %s\n", s.Name,
			connector.Ctx{Vars: s.Vars}.RenderString(s.Base))
	}
	if n == 0 {
		fmt.Printf("     (none — drop a .yaml in %s, see connectors/templates/)\n", *dir)
	}
	fmt.Println()

	if openWindow {
		go func() {
			if waitReady(url, 8*time.Second) {
				openBrowser(url)
			} else {
				fmt.Fprintln(os.Stderr, "server did not become ready; open", url, "manually")
			}
		}()
	}

	// Shut down on a signal rather than being killed mid-turn. An abrupt exit
	// leaves running turns recorded as "outcome unknown" on the next start,
	// which is correct but avoidable — draining first means the common case of
	// a deploy or restart does not manufacture uncertainty.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errc := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	case sig := <-stop:
		fmt.Printf("\n  %v — draining; in-flight turns finish, new input is refused\n", sig)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "  shutdown timed out: %v\n", err)
	}
	if n := mgr.AppendFailures(); n > 0 {
		fmt.Fprintf(os.Stderr, "  warning: %d event append(s) failed; the log is incomplete\n", n)
	}
	fmt.Println("  stopped")
}

// openStore picks the durable store when a DSN is given and says so plainly
// when it does not: an in-memory log looks identical until the process dies.
func openStore(dsn string) (events.Store, string, error) {
	if strings.TrimSpace(dsn) == "" {
		return events.NewMemory(), "in-memory (not durable — set -db for postgres)", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := events.NewPostgres(ctx, dsn)
	if err != nil {
		return nil, "", err
	}
	return store, "postgres — " + redactDSN(dsn), nil
}

// redactDSN keeps the host and database visible and the password out of logs.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "configured"
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "****")
		}
	}
	u.RawQuery = ""
	return u.String()
}

func launch(args []string) {
	// `launch window` reads better out loud than a flag, and window is the only
	// target today — accept it, and tolerate a bare `launch`.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if args[0] != "window" {
			fmt.Fprintf(os.Stderr, "unknown launch target %q (did you mean: oryxa launch window)\n", args[0])
			os.Exit(2)
		}
		args = args[1:]
	}
	serve(args, true)
}

// ---- check ----

func check(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	dir := connectorsFlag(fs)
	probe := fs.String("probe", "ping from oryxa check", "probe text")

	pos := parseArgs(fs, args)
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "usage: oryxa check <agent> [-probe text] [-connectors DIR]")
		os.Exit(2)
	}
	name := pos[0]

	defaultAgentHost()
	reg := connector.NewRegistry()
	if _, err := reg.LoadDir(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "loading connectors: %v\n", err)
		os.Exit(1)
	}
	spec, ok := reg.Get(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "no connector named %q in %s\n", name, *dir)
		if list := reg.List(); len(list) > 0 {
			fmt.Fprint(os.Stderr, "available: ")
			for i, s := range list {
				if i > 0 {
					fmt.Fprint(os.Stderr, ", ")
				}
				fmt.Fprint(os.Stderr, s.Name)
			}
			fmt.Fprintln(os.Stderr)
		}
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), spec.TimeoutDuration())
	defer cancel()

	base := connector.Ctx{Vars: spec.Vars}.RenderString(spec.Base)
	fmt.Printf("\n  checking %s → %s\n\n", name, base)
	res := connector.NewExecutor().Check(ctx, spec, *probe)

	line := func(label, status, detail string) {
		fmt.Printf("  %-12s %-5s %s\n", label, status, detail)
	}
	mark := func(ok bool) string {
		if ok {
			return "ok"
		}
		return "FAIL"
	}

	line("reachable", mark(res.Reachable), base)
	if res.Open != nil {
		d := res.Open.Handle
		if !res.Open.OK {
			d = res.Open.Error
		}
		line("open", mark(res.Open.OK), d)
	}
	if res.Turn != nil {
		d := fmt.Sprintf("%dms · %d parts · %d chars", res.Turn.Ms, res.Turn.Parts, res.Turn.TextLen)
		if !res.Turn.OK {
			d = res.Turn.Error
		}
		line("turn", mark(res.Turn.OK), d)
		if res.Turn.Sample != "" {
			line("sample", "", trunc(res.Turn.Sample, 90))
		}
	}
	if len(res.Capabilities) > 0 {
		line("capabilities", "", strings.Join(res.Capabilities, ", "))
	}
	for _, w := range res.Warnings {
		fmt.Printf("  %-12s %-5s %s\n", "warning", "!", w)
	}
	if res.Error != "" && res.Turn == nil && res.Open == nil {
		line("error", "FAIL", res.Error)
	}

	fmt.Println()
	if !res.OK {
		os.Exit(1)
	}
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- browser ----

func browserURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://localhost" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func waitReady(url string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url + "/health"); err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "could not open a browser (%v) — open %s manually\n", err, url)
	}
}
