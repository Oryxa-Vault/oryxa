package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// client talks to a running Oryxa. Commands that only read connector files
// (check, which, agents) do not need one — keeping that split is why `check`
// works before anything is running.
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base, token string) *client {
	if base == "" {
		base = envOr("ORYXA_URL", "http://localhost:8080")
	}
	if token == "" {
		token = os.Getenv("ORYXA_TOKEN")
	}
	return &client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *client) do(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w\n  is the server running? set -server or ORYXA_URL", c.base, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == 401 {
		return fmt.Errorf("unauthorized — pass -token or set ORYXA_TOKEN")
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// stream follows an SSE endpoint, calling fn for each event payload. It returns
// when the server closes or fn returns false.
func (c *client) stream(path string, fn func(json.RawMessage) bool) error {
	req, err := http.NewRequest("GET", c.base+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// No timeout: a stream stays open for the life of a session.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return fmt.Errorf("unauthorized — pass -token or set ORYXA_TOKEN")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("stream returned %s", resp.Status)
	}

	dec := newSSEReader(resp.Body)
	for {
		payload, err := dec.next()
		if err != nil {
			return nil // server closed
		}
		if len(payload) == 0 {
			continue
		}
		if !fn(payload) {
			return nil
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
