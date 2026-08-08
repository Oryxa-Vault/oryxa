package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The contract has to stay true, and the only thing that keeps a hand-written
// one true is a test that fails when it drifts.
//
// A published spec that is almost right is worse than none: someone builds a
// client against it, and the endpoint they were told about is the one that was
// renamed. This compares what the router registers with what the file promises,
// in both directions — an undocumented route and a documented one that does not
// exist are both wrong.

var routeLine = regexp.MustCompile(`HandleFunc\("(GET|POST|DELETE|PUT|PATCH) (/[^"]*)"`)

// placeholders differ by name between the two (`{id}` vs `{sessionId}`) and that
// is not a difference worth failing over — the shape is what matters.
var placeholder = regexp.MustCompile(`\{[^}]+\}`)

func normalise(method, path string) string {
	return strings.ToLower(method) + " " + placeholder.ReplaceAllString(path, "{}")
}

func TestOpenAPIMatchesTheRoutes(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, m := range routeLine.FindAllStringSubmatch(string(src), -1) {
		registered[normalise(m[1], m[2])] = true
	}
	if len(registered) == 0 {
		t.Fatal("found no routes in server.go; this test has stopped checking anything")
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatalf("the contract is missing: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("openapi.yaml does not parse: %v", err)
	}

	documented := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			switch method {
			case "get", "post", "delete", "put", "patch":
				documented[normalise(method, path)] = true
			}
		}
	}

	var undocumented, phantom []string
	for r := range registered {
		if !documented[r] {
			undocumented = append(undocumented, r)
		}
	}
	for d := range documented {
		if !registered[d] {
			phantom = append(phantom, d)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(phantom)

	if len(undocumented) > 0 {
		t.Errorf("routes with no entry in openapi.yaml:\n  %s", strings.Join(undocumented, "\n  "))
	}
	if len(phantom) > 0 {
		t.Errorf("openapi.yaml promises routes that do not exist:\n  %s", strings.Join(phantom, "\n  "))
	}
}

// Every schema the spec references must be defined, or a generated client
// breaks on a dangling $ref rather than on anything real.
func TestOpenAPIRefsResolve(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	defined := map[string]bool{}
	comps, _ := doc["components"].(map[string]any)
	for group, v := range comps {
		if m, ok := v.(map[string]any); ok {
			for name := range m {
				defined["#/components/"+group+"/"+name] = true
			}
		}
	}

	var missing []string
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			for k, v := range t {
				if k == "$ref" {
					if s, ok := v.(string); ok && !defined[s] {
						missing = append(missing, s)
					}
					continue
				}
				walk(v)
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		}
	}
	walk(doc)

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("dangling $ref:\n  %s", strings.Join(missing, "\n  "))
	}
}
