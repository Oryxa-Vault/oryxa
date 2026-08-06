package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oryxa/oryxa/internal/connector"
	"github.com/oryxa/oryxa/internal/events"
	"github.com/oryxa/oryxa/internal/session"
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
	got, err := http.DefaultClient.Do(req)
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
