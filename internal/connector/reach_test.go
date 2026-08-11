package connector

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnreachableReasonNamesTheRangesThatMatter(t *testing.T) {
	blocked := map[string]string{
		"127.0.0.1":        "loopback",
		"::1":              "loopback",
		"::ffff:127.0.0.1": "loopback",   // IPv4-mapped, the usual way round a v4 check
		"169.254.169.254":  "link-local", // cloud instance metadata
		"fe80::1":          "link-local",
		"10.1.2.3":         "private",
		"172.16.0.1":       "private",
		"192.168.1.1":      "private",
		"fd00::1":          "private",
		"0.0.0.0":          "unspecified",
		"100.64.0.1":       "carrier-grade",
		"239.1.2.3":        "multicast",
		"224.0.0.1":        "multicast", // link-local multicast: the ranges overlap
	}
	for addr, want := range blocked {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("bad test address %q", addr)
		}
		got := unreachableReason(ip)
		if got == "" {
			t.Errorf("%s was allowed", addr)
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: reason %q, want it to mention %q", addr, got, want)
		}
	}

	for _, addr := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::1"} {
		if why := unreachableReason(net.ParseIP(addr)); why != "" {
			t.Errorf("%s was refused as %q, but it is a public address", addr, why)
		}
	}
}

// The hole this closes: POST /v1/agents takes a `base` the server then fetches,
// so without a line between origins it reads anything the server can reach.
func TestARestrictedSpecCannotReachLoopback(t *testing.T) {
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"output":"internal service data"}`))
	}))
	defer secret.Close()

	spec := &Spec{
		Name: "forged", Base: secret.URL, Source: FromAPI,
		Turn: &Step{Method: "POST", Path: "/", Response: &Response{Format: "json", Text: []string{"$.output"}}},
	}

	var got []Part
	err := NewExecutor().Turn(context.Background(), spec, Ctx{Input: "hi"}, func(p Part) { got = append(got, p) })
	if err == nil {
		t.Fatalf("a registered agent reached loopback; parts: %v", got)
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error should name the reason, got: %v", err)
	}
	for _, p := range got {
		if strings.Contains(p.Text, "internal service data") {
			t.Fatal("the internal response body reached the caller")
		}
	}
}

// The same spec from a file is the normal case and must keep working — every
// verified connector in this repository points at localhost.
func TestTheSameSpecFromAFileStillReachesLoopback(t *testing.T) {
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"output":"hello"}`))
	}))
	defer svc.Close()

	spec := &Spec{
		Name: "local", Base: svc.URL, Source: FromFile,
		Turn: &Step{Method: "POST", Path: "/", Response: &Response{Format: "json", Text: []string{"$.output"}}},
	}

	var text string
	if err := NewExecutor().Turn(context.Background(), spec, Ctx{Input: "hi"}, func(p Part) {
		text += p.Text
	}); err != nil {
		t.Fatalf("a file connector was refused: %v", err)
	}
	if text != "hello" {
		t.Errorf("text = %q", text)
	}
}

// check opens a real connection to report reachability, so leaving it unguarded
// would keep a port scanner that answers one host per request.
func TestCheckIsGuardedToo(t *testing.T) {
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer svc.Close()

	spec := &Spec{
		Name: "forged", Base: svc.URL, Source: FromAPI,
		Turn: &Step{Method: "POST", Path: "/"},
	}
	res := NewExecutor().Check(context.Background(), spec, "probe")
	if res.Reachable {
		t.Fatal("check reported an internal address as reachable")
	}
	if !strings.Contains(res.Error, "loopback") {
		t.Errorf("error = %q", res.Error)
	}
}

// A redirect is the other way out of a check made only on `base`. The guard is
// in the dialer, so it applies to wherever the redirect actually goes.
func TestARedirectCannotEscapeTheGuard(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"output":"internal"}`))
	}))
	defer internal.Close()

	// Redirecting to loopback from a spec already marked restricted: the second
	// hop is dialled through the same guarded transport with the same context.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer redirector.Close()

	spec := &Spec{
		Name: "hop", Base: redirector.URL, Source: FromAPI,
		Turn: &Step{Method: "GET", Path: "/", Response: &Response{Format: "json", Text: []string{"$.output"}}},
	}
	var got string
	err := NewExecutor().Turn(context.Background(), spec, Ctx{Input: "hi"}, func(p Part) { got += p.Text })
	if err == nil && strings.Contains(got, "internal") {
		t.Fatal("a redirect carried a restricted spec to an internal address")
	}
}

// Source must never be settable by the thing being judged.
func TestSourceIsNotParsedFromASpec(t *testing.T) {
	s, err := ParseJSON([]byte(`{"name":"x","base":"http://example.com","source":"","turn":{"path":"/"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != FromFile {
		t.Fatalf("Source came out of the body as %q", s.Source)
	}

	y, err := ParseYAML([]byte("name: x\nbase: http://example.com\nsource: file\nturn:\n  path: /\n"))
	if err != nil {
		t.Fatal(err)
	}
	if y.Source != FromFile {
		t.Fatalf("Source came out of the YAML as %q", y.Source)
	}
}
