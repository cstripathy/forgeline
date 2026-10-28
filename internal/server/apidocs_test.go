package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The API reference in docs/api/reference.md is written by hand (the descriptions and request
// fields need a person), but its list of endpoints and the role each needs must not drift from
// the route table. This test reads both and fails with the line to add, fix or remove.
func TestAPIReferenceMatchesTheRoutes(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	routeRE := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+ /[^"]*)",\s*(s\.requireRole\(authn\.(\w+))?`)
	code := map[string]string{} // "METHOD /path" -> role
	for _, m := range routeRE.FindAllStringSubmatch(string(src), -1) {
		route := m[1]
		if strings.HasSuffix(route, "/{$}") || route == "GET /ui/" {
			continue // the console's own pages, not API
		}
		role := "public"
		if m[3] != "" {
			role = strings.ToLower(m[3])
		}
		code[route] = role
	}
	if len(code) < 50 {
		t.Fatalf("found only %d routes in api.go; the pattern in this test needs updating", len(code))
	}

	doc, err := os.ReadFile("../../docs/api/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	rowRE := regexp.MustCompile("(?m)^\\| `([A-Z]+ /[^`]*)` \\| (\\w+)")
	docs := map[string]string{}
	for _, m := range rowRE.FindAllStringSubmatch(string(doc), -1) {
		docs[m[1]] = m[2]
	}

	for route, role := range code {
		got, ok := docs[route]
		switch {
		case !ok:
			t.Errorf("docs/api/reference.md has no row for `%s` (role %s)", route, role)
		case got != role:
			t.Errorf("docs/api/reference.md says `%s` needs %q; the code says %q", route, got, role)
		}
	}
	for route := range docs {
		if _, ok := code[route]; !ok {
			t.Errorf("docs/api/reference.md documents `%s`, which is not a route any more", route)
		}
	}
}
