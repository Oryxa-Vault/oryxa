package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// openRoom creates a room and returns its id and secret, deliberately without
// going through the helpers that remember secrets — these tests are about what
// happens to a caller who does not have one.
func openRoom(t *testing.T, base string) (id, secret string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"agents": []string{"a"}})
	resp, err := http.Post(base+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 201 {
		t.Fatalf("create returned %d: %v", resp.StatusCode, out)
	}
	id, _ = out["id"].(string)
	secret, _ = out["secret"].(string)
	if id == "" || secret == "" {
		t.Fatalf("create returned no id or no secret: %v", out)
	}
	return id, secret
}

func registerStub(t *testing.T, base string) {
	t.Helper()
	do(t, "POST", base+"/v1/agents", map[string]any{
		"name": "a", "base": "http://127.0.0.1:1",
		"turn": map[string]any{"method": "POST", "path": "/x"},
	})
}

// The gap this closes: one token opened every room, including rooms its holder
// was never part of. The token is unchanged here — both callers have it — and
// only the secret differs.
func TestARoomIsClosedToWhoeverLacksItsSecret(t *testing.T) {
	srv := newOryxa(t)
	registerStub(t, srv.URL)
	id, secret := openRoom(t, srv.URL)

	paths := []string{
		"/v1/sessions/" + id,
		"/v1/sessions/" + id + "/context",
		"/v1/sessions/" + id + "/events",
		"/v1/sessions/" + id + "/stream?since=0",
	}
	for _, p := range paths {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s without a secret returned %d, want 404", p, resp.StatusCode)
		}
	}

	// And the same paths open with it.
	for _, p := range paths {
		req, _ := http.NewRequest("GET", srv.URL+p, nil)
		req.Header.Set(SessionHeader, secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			t.Errorf("GET %s with the secret returned %d", p, resp.StatusCode)
		}
	}
}

// Writing is guarded as well as reading. Scoping that let a stranger talk to
// your agents while merely not seeing the replies would be worse than none.
func TestWritingToARoomNeedsItsSecretToo(t *testing.T) {
	srv := newOryxa(t)
	registerStub(t, srv.URL)
	id, _ := openRoom(t, srv.URL)

	body, _ := json.Marshal(map[string]string{"text": "hello", "author": "stranger"})
	for _, p := range []string{
		"/v1/sessions/" + id + "/input",
		"/v1/sessions/" + id + "/cancel",
		"/v1/sessions/" + id + "/close",
		"/v1/sessions/" + id + "/context/plan",
	} {
		resp, err := http.Post(srv.URL+p, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s without a secret returned %d, want 404", p, resp.StatusCode)
		}
	}
}

// A wrong secret and a missing room answer identically. Distinguishing them
// turns the endpoint into an oracle for which rooms exist, which is most of what
// the scoping is protecting in the first place.
func TestAWrongSecretIsIndistinguishableFromAMissingRoom(t *testing.T) {
	srv := newOryxa(t)
	registerStub(t, srv.URL)
	id, _ := openRoom(t, srv.URL)

	get := func(path, secret string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if secret != "" {
			req.Header.Set(SessionHeader, secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		msg, _ := out["error"].(string)
		return resp.StatusCode, msg
	}

	realRoomWrongSecret, msg1 := get("/v1/sessions/"+id, "not-the-secret")
	noSuchRoom, msg2 := get("/v1/sessions/s_doesnotexist", "not-the-secret")

	if realRoomWrongSecret != noSuchRoom {
		t.Errorf("status differs: real room %d, missing room %d", realRoomWrongSecret, noSuchRoom)
	}
	if msg1 != msg2 {
		t.Errorf("message differs:\n  real room:    %q\n  missing room: %q", msg1, msg2)
	}
}

// The secret appears exactly once, in the create response. Anywhere else and it
// would leak to whoever could already list rooms.
func TestTheSecretIsReturnedOnceAndNeverAgain(t *testing.T) {
	srv := newOryxa(t)
	registerStub(t, srv.URL)
	id, secret := openRoom(t, srv.URL)

	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/"+id, nil)
	req.Header.Set(SessionHeader, secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var view bytes.Buffer
	_, _ = view.ReadFrom(resp.Body)
	if strings.Contains(view.String(), secret) {
		t.Error("the room view carried the secret back")
	}

	list, err := http.Get(srv.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer list.Body.Close()
	var all bytes.Buffer
	_, _ = all.ReadFrom(list.Body)
	if strings.Contains(all.String(), secret) {
		t.Error("the session list carried the secret")
	}
	// The hash must not travel either — it is not the secret, but it is the only
	// thing a brute-force attempt would need.
	if strings.Contains(all.String(), "secret_sha256") {
		t.Error("the session list carried the stored hash")
	}
}

// The viewer's stream cannot send a header, so join swaps the secret for a
// cookie. It is the one route under a session that the secret does not guard.
func TestJoinExchangesTheSecretForACookie(t *testing.T) {
	srv := newOryxa(t)
	registerStub(t, srv.URL)
	id, secret := openRoom(t, srv.URL)

	body, _ := json.Marshal(map[string]string{"secret": "wrong"})
	bad, err := http.Post(srv.URL+"/v1/sessions/"+id+"/join", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusNotFound {
		t.Fatalf("join with a wrong secret returned %d, want 404", bad.StatusCode)
	}

	body, _ = json.Marshal(map[string]string{"secret": secret})
	ok, err := http.Post(srv.URL+"/v1/sessions/"+id+"/join", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("join returned %d", ok.StatusCode)
	}

	var cookie *http.Cookie
	for _, c := range ok.Cookies() {
		if c.Name == sessionCookieName(id) {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("join set no cookie")
	}
	// Scoped to this room alone, so a browser in several rooms never sends one
	// room's secret to another.
	if cookie.Path != "/v1/sessions/"+id {
		t.Errorf("cookie path = %q, want it scoped to the room", cookie.Path)
	}
	if !cookie.HttpOnly {
		t.Error("cookie is readable from JavaScript")
	}

	// And the cookie opens the room.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/"+id, nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the cookie did not open the room: %d", resp.StatusCode)
	}
}

// Two rooms, two secrets, and neither opens the other.
func TestOneRoomsSecretDoesNotOpenAnother(t *testing.T) {
	srv := newOryxa(t)
	registerStub(t, srv.URL)
	idA, secretA := openRoom(t, srv.URL)
	idB, _ := openRoom(t, srv.URL)

	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/"+idB, nil)
	req.Header.Set(SessionHeader, secretA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("room A's secret opened room B: %d", resp.StatusCode)
	}
	if idA == idB {
		t.Fatal("two creates returned the same room")
	}
}

func TestRoomIDFromPath(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/sessions/s_abc":           "s_abc",
		"/v1/sessions/s_abc/input":     "s_abc",
		"/v1/sessions/s_abc/context/k": "s_abc",
		"/v1/sessions/s_abc/stream":    "s_abc",
		"/v1/sessions":                 "",
		"/v1/sessions/":                "",
		"/v1/agents":                   "",
		"/v1/agents/claude-code/check": "",
		"/health":                      "",
		"/":                            "",
	} {
		if got := roomIDFromPath(path); got != want {
			t.Errorf("roomIDFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}
