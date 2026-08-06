// Command oryxa runs the Oryxa server and its tooling.
//
//	oryxa serve                 start the server (API + viewer)
//	oryxa check <agent>         probe a connector with a real turn
//	oryxa launch window         start the server and open the viewer
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/oryxa/oryxa/internal/api"
	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/events"
	"github.com/oryxa/oryxa/internal/session"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:], false)
	case "launch":
		launch(os.Args[2:])
	case "check":
		check(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`oryxa — many people, one agent

Usage:
  oryxa serve [-addr :8080] [-connectors ./connectors]
      Start the server. API and viewer on the same port.

  oryxa launch window [-addr :8080] [-connectors ./connectors]
      Start the server and open the viewer in a browser.

  oryxa check <agent> [-connectors ./connectors]
      Probe a connector with a real turn and report what came back.
      Needs no running server.
`)
}

// ---- serve / launch ----

func serve(args []string, openWindow bool) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dir := fs.String("connectors", "./connectors", "directory of connector files")
	_ = fs.Parse(args)

	reg := connector.NewRegistry()
	n, err := reg.LoadDir(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading connectors: %v\n", err)
		os.Exit(1)
	}

	log := events.NewMemory()
	exec := connector.NewExecutor()
	mgr := session.NewManager(reg, exec, log)
	srv := api.New(reg, exec, mgr, log)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: SSE streams stay open for the life of a session.
	}

	url := browserURL(*addr)
	fmt.Printf("\n  oryxa\n")
	fmt.Printf("  ├─ viewer      %s\n", url)
	fmt.Printf("  ├─ api         %s/v1\n", url)
	fmt.Printf("  └─ connectors  %d loaded from %s\n\n", n, *dir)
	for _, s := range reg.List() {
		fmt.Printf("     • %-14s %s\n", s.Name, s.Base)
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

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	}
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
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: oryxa check <agent> [-connectors ./connectors]")
		os.Exit(2)
	}
	name := args[0]

	fs := flag.NewFlagSet("check", flag.ExitOnError)
	dir := fs.String("connectors", "./connectors", "directory of connector files")
	probe := fs.String("probe", "ping from oryxa check", "probe text")
	_ = fs.Parse(args[1:])

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

	fmt.Printf("\n  checking %s → %s\n\n", name, spec.Base)
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

	line("reachable", mark(res.Reachable), spec.Base)
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
