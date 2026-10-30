// Package mcp is a small client for MCP servers that speak JSON-RPC over a child process's
// standard input and output. It is enough to list tools and call them; it does not implement
// resources, prompts or sampling.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Tool is a tool a server offers.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations struct {
		ReadOnly    bool `json:"readOnlyHint"`
		Destructive bool `json:"destructiveHint"`
	} `json:"annotations"`
}

// Result is what a tool call returned: its text, whether the server flagged an error, and the
// structured content if there was any.
type Result struct {
	Text       string
	IsError    bool
	Structured json.RawMessage
}

// Client talks to one server process. It is not safe for concurrent calls.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte
	nextID int
	mu     sync.Mutex
	stderr *capture
}

type capture struct {
	mu sync.Mutex
	b  []byte
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.b) < 4096 {
		c.b = append(c.b, p...)
	}
	return len(p), nil
}

func (c *capture) String() string { c.mu.Lock(); defer c.mu.Unlock(); return string(c.b) }

// Start launches the command and completes the MCP handshake.
func Start(ctx context.Context, cmd *exec.Cmd) (*Client, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &Client{cmd: cmd, stdin: stdin, lines: make(chan []byte, 16), stderr: &capture{}}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			c.lines <- line
		}
		close(c.lines)
	}()
	var init struct {
		ServerInfo struct{ Name string } `json:"serverInfo"`
	}
	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "forgeline", "version": "1"},
	}, &init); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.notify("notifications/initialized"); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) send(m map[string]any) error {
	b, _ := json.Marshal(m)
	_, err := c.stdin.Write(append(b, '\n'))
	return err
}

func (c *Client) notify(method string) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method})
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return c.fail(err)
	}
	for {
		select {
		case <-ctx.Done():
			return errors.New("the MCP server took too long")
		case line, ok := <-c.lines:
			if !ok {
				return c.fail(io.EOF)
			}
			var m struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(line, &m) != nil || m.ID == nil || *m.ID != id {
				continue // a notification or log line
			}
			if m.Error != nil {
				return fmt.Errorf("the MCP server refused %s: %s", method, m.Error.Message)
			}
			if out == nil {
				return nil
			}
			return json.Unmarshal(m.Result, out)
		}
	}
}

func (c *Client) fail(err error) error {
	msg := firstLine(c.stderr.String())
	if msg != "" {
		return fmt.Errorf("the MCP server stopped: %s", msg)
	}
	return fmt.Errorf("the MCP server stopped: %v", err)
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// Tools lists the server's tools.
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// Call runs one tool.
func (c *Client) Call(ctx context.Context, name string, args map[string]any) (Result, error) {
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &out); err != nil {
		return Result{}, err
	}
	var r Result
	for _, p := range out.Content {
		if p.Type == "text" {
			r.Text += p.Text
		}
	}
	r.IsError, r.Structured = out.IsError, out.StructuredContent
	return r, nil
}

// Close stops the process.
func (c *Client) Close() {
	c.stdin.Close()
	done := make(chan struct{})
	go func() { c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		c.cmd.Process.Kill()
		<-done
	}
}
