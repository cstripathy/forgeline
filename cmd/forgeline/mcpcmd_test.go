package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPProxyForwardsMessagesWithTheTokenAndNeverPrintsIt(t *testing.T) {
	var seen []string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/mcp" || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		seen = append(seen, string(b))
		switch {
		case strings.Contains(string(b), "notifications/"):
			w.WriteHeader(http.StatusAccepted)
		case strings.Contains(string(b), "boom"):
			http.Error(w, `{"error":"sign in required"}`, http.StatusUnauthorized)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
		}
	}))
	defer srv.Close()
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		``,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":7,"method":"boom"}`,
	}, "\n") + "\n"
	var out, errb bytes.Buffer
	env := map[string]string{"FORGELINE_MCP_TOKEN": "flt_secretsecretsecret", "FORGELINE_SERVER": srv.URL}
	code := runMCP(context.Background(), nil, strings.NewReader(in), &out, &errb, func(k string) string { return env[k] })
	if code != 0 || len(seen) != 3 || auth != "Bearer flt_secretsecretsecret" {
		t.Fatalf("code %d, seen %v, auth %q, stderr %s", code, seen, auth, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"result"`) || !strings.Contains(lines[1], `"id":7`) || !strings.Contains(lines[1], "sign in required") {
		t.Errorf("stdout = %q", out.String())
	}
	if strings.Contains(out.String()+errb.String(), "flt_secret") {
		t.Error("the token was printed")
	}
}

func TestMCPProxyRefusesToStartWithoutATokenOrWithABadAddress(t *testing.T) {
	run := func(env map[string]string, args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := runMCP(context.Background(), args, strings.NewReader(""), &out, &errb, func(k string) string { return env[k] })
		return code, errb.String()
	}
	if code, msg := run(map[string]string{"FORGELINE_SERVER": "http://x"}); code != 2 || !strings.Contains(msg, "FORGELINE_MCP_TOKEN") {
		t.Errorf("no token: %d %s", code, msg)
	}
	if code, _ := run(map[string]string{"FORGELINE_MCP_TOKEN": "flt_x"}); code != 2 {
		t.Errorf("no server: %d", code)
	}
	if code, msg := run(map[string]string{"FORGELINE_MCP_TOKEN": "flt_x"}, "--server", "file:///etc/passwd"); code != 2 || !strings.Contains(msg, "http") {
		t.Errorf("a file URL: %d %s", code, msg)
	}
	// A server that cannot be reached becomes a JSON-RPC error for a request, silence for a notification.
	var out bytes.Buffer
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	env := map[string]string{"FORGELINE_MCP_TOKEN": "flt_x", "FORGELINE_SERVER": dead.URL}
	runMCP(context.Background(), nil, strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"ping"}`+"\n"+`{"jsonrpc":"2.0","method":"notifications/x"}`+"\n"), &out, io.Discard, func(k string) string { return env[k] })
	if !strings.Contains(out.String(), `"id":4`) || strings.Count(out.String(), "\n") != 1 {
		t.Errorf("stdout = %q", out.String())
	}
}
