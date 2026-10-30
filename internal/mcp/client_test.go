package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as a tiny MCP server when run with FAKE_MCP=1.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_MCP") == "1" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

func fakeServer() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		json.Unmarshal(sc.Bytes(), &m)
		if m.ID == nil {
			continue
		}
		fmt.Println(`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`) // noise first
		switch m.Method {
		case "initialize":
			fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"serverInfo":{"name":"fake"}}}`+"\n", *m.ID)
		case "tools/list":
			fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"echo","description":"d","inputSchema":{},"annotations":{"readOnlyHint":true}}]}}`+"\n", *m.ID)
		case "tools/call":
			args, _ := m.Params["arguments"].(map[string]any)
			if args["fail"] == true {
				fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"bad"}],"isError":true}}`+"\n", *m.ID)
			} else {
				fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%q}],"structuredContent":{"n":1}}}`+"\n", *m.ID, "echo:"+fmt.Sprint(args["x"]))
			}
		default:
			fmt.Printf(`{"jsonrpc":"2.0","id":%d,"error":{"message":"no such method"}}`+"\n", *m.ID)
		}
	}
}

func start(t *testing.T) *Client {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FAKE_MCP=1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c, err := Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestClientListsAndCallsTools(t *testing.T) {
	c := start(t)
	ctx := context.Background()
	tools, err := c.Tools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" || !tools[0].Annotations.ReadOnly {
		t.Fatalf("tools = %+v, %v", tools, err)
	}
	r, err := c.Call(ctx, "echo", map[string]any{"x": "hi"})
	if err != nil || r.Text != "echo:hi" || r.IsError || !strings.Contains(string(r.Structured), `"n":1`) {
		t.Fatalf("result = %+v, %v", r, err)
	}
	r, err = c.Call(ctx, "echo", map[string]any{"fail": true})
	if err != nil || !r.IsError || r.Text != "bad" {
		t.Fatalf("error result = %+v, %v", r, err)
	}
	if err := c.call(ctx, "nope", nil, nil); err == nil || !strings.Contains(err.Error(), "no such method") {
		t.Errorf("unknown method error = %v", err)
	}
}

func TestClientReportsAServerThatDies(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo 'boom: no credentials' >&2; exit 1")
	_, err := Start(context.Background(), cmd)
	if err == nil || !strings.Contains(err.Error(), "boom: no credentials") {
		t.Fatalf("err = %v", err)
	}
}

func TestClientStopsWaitingWhenTheContextEnds(t *testing.T) {
	cmd := exec.Command("sh", "-c", "cat >/dev/null")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Start(ctx, cmd); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("err = %v", err)
	}
}
