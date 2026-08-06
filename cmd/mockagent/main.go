// Command mockagent is a stand-in agent for exercising Oryxa without a real
// framework. It deliberately offers two very different response shapes, because
// the thing worth testing is that the core does not care which one it gets.
//
//	mockagent -addr :9000
//
//	POST /apps/{app}/users/{user}/sessions/{id}   open   -> {"id": "..."}
//	POST /run_sse                                 turn   -> SSE, nested parts
//	POST /invoke                                  turn   -> one JSON object
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	delay := flag.Duration("delay", 120*time.Millisecond, "per-chunk delay")
	flag.Parse()

	mux := http.NewServeMux()

	mux.HandleFunc("POST /apps/{app}/users/{user}/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := "remote-" + r.PathValue("id")
		fmt.Printf("open  app=%s user=%s -> %s\n", r.PathValue("app"), r.PathValue("user"), id)
		writeJSON(w, map[string]string{"id": id})
	})

	// Streaming shape: nested content.parts[].text, like several frameworks use.
	mux.HandleFunc("POST /run_sse", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		text := extractText(body)
		fmt.Printf("turn  sse  session=%v input=%q\n", body["session_id"], text)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)

		for _, word := range strings.Fields("you said: " + text) {
			chunk, _ := json.Marshal(map[string]any{
				"content": map[string]any{
					"parts": []any{map[string]string{"text": word + " "}},
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(*delay)
		}
		fmt.Fprint(w, "data: {\"turn_complete\":true}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	// Non-streaming shape: one flat JSON object.
	mux.HandleFunc("POST /invoke", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		text := extractText(body)
		fmt.Printf("turn  json input=%q\n", text)
		time.Sleep(*delay)
		writeJSON(w, map[string]string{"output": "you said: " + text})
	})

	fmt.Printf("mockagent listening on %s\n", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Println(err)
	}
}

func readBody(r *http.Request) map[string]any {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}

// extractText finds the prompt wherever the caller happened to put it, so the
// same mock serves several connector shapes.
func extractText(body map[string]any) string {
	for _, key := range []string{"input", "prompt", "text"} {
		if v, ok := body[key].(string); ok {
			return v
		}
	}
	if nm, ok := body["new_message"].(map[string]any); ok {
		if parts, ok := nm["parts"].([]any); ok {
			var b strings.Builder
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						b.WriteString(t)
					}
				}
			}
			return b.String()
		}
	}
	if s, ok := body["new_message"].(string); ok {
		return s
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
