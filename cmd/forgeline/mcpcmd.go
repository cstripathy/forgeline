package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const mcpHelp = `usage: forgeline mcp [--server URL]

A stdio MCP server for clients that cannot speak HTTP. It reads one JSON-RPC message per line on
standard input, sends each to the Forgeline server's /mcp endpoint, and writes the answers to standard
output. Create the token on the Account page (API tokens).

Environment:
  FORGELINE_MCP_TOKEN  a personal API token (required; the shared server token does not work)
  FORGELINE_SERVER     server URL, if --server is not given
`

const mcpLineLimit = 1 << 20

// runMCP implements `forgeline mcp`.
func runMCP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, mcpHelp) }
	server := fs.String("server", getenv("FORGELINE_SERVER"), "server URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token := getenv("FORGELINE_MCP_TOKEN")
	if *server == "" || token == "" {
		fmt.Fprintln(stderr, "forgeline: set --server (or FORGELINE_SERVER) and FORGELINE_MCP_TOKEN")
		return 2
	}
	u, err := url.Parse(strings.TrimRight(*server, "/") + "/mcp")
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintln(stderr, "forgeline: the server URL must be http:// or https://")
		return 2
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		fmt.Fprintln(stderr, "forgeline: the token would cross the network in clear text; use an https:// server URL (http:// is only for this machine)")
		return 2
	}
	client := &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	rd := bufio.NewReaderSize(stdin, 64<<10)
	for {
		line, tooLong, err := readLine(rd)
		if tooLong {
			// The id is unknowable, so answer with a null id like a parse error, and carry on.
			stdout.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"message larger than 1 MiB"}}` + "\n"))
		} else if line = bytes.TrimSpace(line); len(line) > 0 {
			if out := forwardMCP(ctx, client, u.String(), token, line); out != nil {
				stdout.Write(append(out, '\n'))
			}
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(stderr, "forgeline: reading standard input: %v\n", err)
				return 1
			}
			return 0
		}
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readLine reads one line, keeping at most mcpLineLimit bytes. A longer line is read to its end and
// reported as tooLong, so one oversized message does not end the session.
func readLine(rd *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		part, isPrefix, e := rd.ReadLine()
		if e != nil {
			return line, tooLong, e
		}
		if !tooLong {
			if len(line)+len(part) > mcpLineLimit {
				line, tooLong = nil, true
			} else {
				line = append(line, part...)
			}
		}
		if !isPrefix {
			return line, tooLong, nil
		}
	}
}

// forwardMCP sends one message and returns the line to print, or nil when there is nothing to say
// (a notification the server accepted).
func forwardMCP(ctx context.Context, c *http.Client, endpoint, token string, msg []byte) []byte {
	var head struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(msg, &head)
	fail := func(text string) []byte {
		if len(head.ID) == 0 {
			return nil
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": head.ID, "error": map[string]any{"code": -32000, "message": text}})
		return b
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(msg))
	if err != nil {
		return fail("bad server address")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return fail("could not reach the Forgeline server")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusAccepted {
		return nil
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) == nil && probe["jsonrpc"] != nil {
		return bytes.TrimSpace(body)
	}
	var e struct{ Error string }
	_ = json.Unmarshal(body, &e)
	if e.Error == "" {
		e.Error = fmt.Sprintf("the server answered %d", resp.StatusCode)
	}
	return fail(e.Error)
}
