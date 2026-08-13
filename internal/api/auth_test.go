package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Oryxa-Vault/oryxa/internal/connector"
	"github.com/Oryxa-Vault/oryxa/internal/events"
	"github.com/Oryxa-Vault/oryxa/internal/session"
)

func guarded(t *testing.T, token string) *httptest.Server {
	t.Helper()
	reg := connector.NewRegistry()
	log := events.NewMemory()
	exec := connector.NewExecutor()
	mgr := session.NewManager(reg, exec, log)
	srv := httptest.NewServer(New(reg, exec, mgr, log).WithToken(token).Routes())
	t.Cleanup(srv.Close)
	return srv
}

func status(t *testing.T, srv *httptest.Server, path, bearer string, jar http.CookieJar) int {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	c := &http.Client{}
	if jar != nil {
		c.Jar = jar
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestTokenGuardsTheAPI(t *testing.T) {
	srv := guarded(t, "s3cret")

	for _, c := range []struct {
		name, path, bearer string
		want               int
	}{
		{"no credential", "/v1/sessions", "", 401},
		{"wrong token", "/v1/sessions", "nope", 401},
		{"right token", "/v1/sessions", "s3cret", 200},
		{"agents guarded too", "/v1/agents", "", 401},
		// The stream must be guarded: it is the whole conversation.
		{"stream guarded", "/v1/sessions/x/stream", "", 401},
		// The viewer stays reachable so it can render a sign-in prompt. It holds
		// no data until the API answers.
		{"viewer reachable", "/", "", 200},
		{"health reachable", "/health", "", 200},
		{"auth status reachable", "/v1/auth/status", "", 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := status(t, srv, c.path, c.bearer, nil); got != c.want {
				t.Fatalf("%s %s = %d, want %d", c.bearer, c.path, got, c.want)
			}
		})
	}
}

func TestOpenModeLetsEverythingThrough(t *testing.T) {
	srv := guarded(t, "")
	if got := status(t, srv, "/v1/sessions", "", nil); got != 200 {
		t.Fatalf("open mode returned %d, want 200", got)
	}
}

// The cookie exists because EventSource cannot send an Authorization header.
// If login stops setting a usable cookie, the viewer's live stream dies while
// every other request keeps working — a failure that would look like a
// streaming bug rather than an auth one.
func TestLoginCookieAuthenticatesStreams(t *testing.T) {
	srv := guarded(t, "s3cret")

	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json",
		strings.NewReader(`{"token":"s3cret"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login returned %d", resp.StatusCode)
	}

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == tokenCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login set no cookie; the viewer's stream cannot authenticate")
	}
	if !cookie.HttpOnly {
		t.Error("cookie is readable by JavaScript; it should be HttpOnly")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Error("cookie should be SameSite=Strict")
	}

	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions", nil)
	req.AddCookie(cookie)
	got, err := http.DefaultClient.Do(withRoom(req))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if got.StatusCode != 200 {
		t.Fatalf("cookie auth returned %d, want 200", got.StatusCode)
	}
}

func TestLoginRejectsWrongToken(t *testing.T) {
	srv := guarded(t, "s3cret")
	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json",
		strings.NewReader(`{"token":"wrong"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("bad login returned %d, want 401", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == tokenCookie && c.Value != "" {
			t.Fatal("a rejected login handed out a cookie")
		}
	}
}

func TestUnauthorizedAdvertisesBearer(t *testing.T) {
	srv := guarded(t, "s3cret")
	resp, err := http.Get(srv.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if h := resp.Header.Get("WWW-Authenticate"); !strings.Contains(h, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", h)
	}
}

// Identity comes from whatever runs in front of Oryxa. These tests pin the two
// properties that make that safe: a spoofed body author cannot override the
// proxy, and a request that skipped the proxy is refused rather than treated as
// anonymous.
func trusted(t *testing.T, header string) *httptest.Server {
	t.Helper()
	reg := connector.NewRegistry()
	log := events.NewMemory()
	exec := connector.NewExecutor()
	mgr := session.NewManager(reg, exec, log)
	srv := httptest.NewServer(
		New(reg, exec, mgr, log).WithTrustedHeader(header).Routes())
	t.Cleanup(srv.Close)
	return srv
}

func TestTrustedHeaderOverridesClaimedAuthor(t *testing.T) {
	srv := trusted(t, "X-Forwarded-User")

	// A registered agent and session, so input can be submitted.
	do(t, "POST", srv.URL+"/v1/agents", map[string]any{
		"name": "a", "base": "http://127.0.0.1:1",
		"turn": map[string]any{"method": "POST", "path": "/x"},
	})
	_, s := do(t, "POST", srv.URL+"/v1/sessions", map[string]any{"agent": "a"})
	sid := s["id"].(string)

	body, _ := json.Marshal(map[string]string{"text": "hi", "author": "impostor"})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/sessions/"+sid+"/input",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-User", "alice@example.com")

	resp, err := http.DefaultClient.Do(withRoom(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("submit returned %d", resp.StatusCode)
	}
	var turn map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&turn)
	if turn["author"] != "alice@example.com" {
		t.Fatalf("author = %v; the body must not be able to override the proxy", turn["author"])
	}
}

func TestMissingTrustedHeaderIsRefused(t *testing.T) {
	srv := trusted(t, "X-Forwarded-User")
	do(t, "POST", srv.URL+"/v1/agents", map[string]any{
		"name": "a", "base": "http://127.0.0.1:1",
		"turn": map[string]any{"method": "POST", "path": "/x"},
	})
	_, s := do(t, "POST", srv.URL+"/v1/sessions", map[string]any{"agent": "a"})
	sid := s["id"].(string)

	// No header: the request did not come through the proxy. Treating that as
	// anonymous would silently accept exactly what the proxy exists to prevent.
	code, _ := do(t, "POST", srv.URL+"/v1/sessions/"+sid+"/input",
		map[string]string{"text": "hi", "author": "alice"})
	if code != 401 {
		t.Fatalf("submit without the trusted header returned %d, want 401", code)
	}
}

func TestWithoutTrustHeaderAuthorsStaySelfDeclared(t *testing.T) {
	srv := trusted(t, "")
	var st map[string]any
	_, st = do(t, "GET", srv.URL+"/v1/auth/status", nil)
	if st["identity"] != "claimed" {
		t.Fatalf("identity = %v, want claimed", st["identity"])
	}
}

func TestAuthStatusReportsTrustedIdentity(t *testing.T) {
	srv := trusted(t, "X-Forwarded-User")
	req, _ := http.NewRequest("GET", srv.URL+"/v1/auth/status", nil)
	req.Header.Set("X-Forwarded-User", "bob@example.com")
	resp, err := http.DefaultClient.Do(withRoom(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if st["identity"] != "trusted" || st["author"] != "bob@example.com" {
		t.Fatalf("status = %v; the viewer needs this to stop asking for a name", st)
	}
}
