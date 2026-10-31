// Command sample-plugin is a tiny MCP server used to try the plugin runner for real. It offers two
// tools: shout (upper-cases text) and probe (reports what the container can see: whether it has a
// granted secret, the network, and a writable root).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     *int `json:"id"`
			Method string
			Params struct {
				Name      string
				Arguments map[string]any
			}
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil {
			continue
		}
		reply := func(result any) {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": result})
			fmt.Println(string(b))
		}
		switch m.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "sample", "version": "1"}})
		case "tools/list":
			reply(map[string]any{"tools": []map[string]any{
				{"name": "shout", "description": "upper-case some text", "inputSchema": map[string]any{"type": "object"}},
				{"name": "probe", "description": "report what the container can see", "inputSchema": map[string]any{"type": "object"}},
			}})
		case "tools/call":
			text := ""
			switch m.Params.Name {
			case "shout":
				text = strings.ToUpper(fmt.Sprint(m.Params.Arguments["text"]))
			case "probe":
				_, werr := os.Create("/rootfile")
				_, nerr := net.DialTimeout("tcp", "1.1.1.1:53", 2*time.Second)
				text = fmt.Sprintf("secret=%t network=%t writable_root=%t uid=%d", os.Getenv("SAMPLE_KEY") != "", nerr == nil, werr == nil, os.Getuid())
			}
			reply(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
		}
	}
}
