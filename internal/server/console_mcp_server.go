package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/store"
)

// Forgeline as an MCP server (design note 020). The same read-only lookups the assistant has, and
// two propose-only tools, for an external AI client holding a personal API token. Stateless
// streamable HTTP: one JSON-RPC message in, one JSON answer out.

const (
	mcpMaxBody      = 1 << 20
	mcpCallsPerMin  = 60
	mcpMaxInflight  = 4
	mcpCallTimeout  = 60 * time.Second
	mcpDeniedEvery  = time.Minute
	mcpMaxTracked   = 2000
	credentialMask  = "[credential removed]"
	mcpServerName   = "forgeline"
	mcpDefaultProto = "2025-06-18"
)

var mcpProtocols = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

// mcpReadTools are the assistant lookups an MCP client may use. Anything not listed is not exposed:
// no raw AWS access, no agents, no plugin tools, nothing that applies or approves.
var mcpReadTools = map[string]bool{
	"list_runs": true, "get_run": true, "get_step_log": true, "run_failure": true, "list_pipelines": true, "get_pipeline": true,
	"aws_accounts": true, "aws_resources": true, "aws_summary": true, "k8s_clusters": true, "k8s_resources": true,
	"guard_findings": true, "list_agents": true, "search_audit": true, "environment_templates": true,
	"request_environment": true, // propose-only: it never creates one without a different administrator
}

const (
	toolProposeAWS   = "propose_aws_change"
	toolProposeCloud = "propose_cloud_change"
)

type mcpState struct {
	mu       sync.Mutex
	window   map[string]*mcpBucket
	deniedAt map[string]time.Time
}

type mcpBucket struct {
	start    time.Time
	calls    int
	inflight int
}

// admit counts one call against the token's minute and its concurrency. The returned function must
// be called when the call ends.
func (m *mcpState) admit(id string, now time.Time) (func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.window == nil {
		m.window = map[string]*mcpBucket{}
	}
	if len(m.window) > mcpMaxTracked {
		for id, b := range m.window { // forget only what is idle and a minute old
			if b.inflight == 0 && now.Sub(b.start) >= time.Minute {
				delete(m.window, id)
			}
		}
	}
	b := m.window[id]
	if b == nil {
		b = &mcpBucket{start: now}
		m.window[id] = b
	}
	if now.Sub(b.start) >= time.Minute {
		b.start, b.calls = now, 0
	}
	if b.calls >= mcpCallsPerMin || b.inflight >= mcpMaxInflight {
		return nil, false
	}
	b.calls++
	b.inflight++
	return func() { m.mu.Lock(); b.inflight--; m.mu.Unlock() }, true
}

// shouldAuditDenied lets one refused request per client address a minute into the audit log, so a
// stream of bad tokens cannot fill it.
func (m *mcpState) shouldAuditDenied(ip string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deniedAt == nil {
		m.deniedAt = map[string]time.Time{}
	}
	if len(m.deniedAt) > mcpMaxTracked {
		for ip, t := range m.deniedAt {
			if now.Sub(t) >= mcpDeniedEvery {
				delete(m.deniedAt, ip)
			}
		}
	}
	if t, ok := m.deniedAt[ip]; ok && now.Sub(t) < mcpDeniedEvery {
		return false
	}
	m.deniedAt[ip] = now
	return true
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpCallResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

var anyObject = map[string]any{"type": "object", "additionalProperties": true}

// mcpTools lists what a caller with this role may use.
func (s *Server) mcpTools(ctx context.Context, role authn.Role) []mcpToolDef {
	var out []mcpToolDef
	mcpOn := s.loadAWSMCP(ctx).Enabled
	for _, t := range assistantLookups() {
		if !mcpReadTools[t.name] && t.name != toolProposeAWS || !role.AtLeast(t.min) {
			continue
		}
		if needsMCP(t.name) && !mcpOn {
			continue
		}
		out = append(out, mcpToolDef{Name: t.name, Description: t.help, InputSchema: anyObject})
	}
	if role.AtLeast(authn.Developer) {
		out = append(out, mcpToolDef{Name: toolProposeCloud, Description: "files a change proposal for a stack deploy, stack deletion or instance start/stop/reboot; nothing runs until a different administrator approves it in the console",
			InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "reason"}, "properties": map[string]any{
				"kind":        map[string]any{"type": "string", "enum": []string{kindStackDeploy, kindStackDelete, kindInstanceAction}},
				"reason":      map[string]any{"type": "string", "description": "why, 5 to 500 characters; the approver reads it"},
				"template":    map[string]any{"type": "string", "description": "stack-deploy: a catalog template id"},
				"stack":       map[string]any{"type": "string"},
				"params":      map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"instance_id": map[string]any{"type": "string"},
				"action":      map[string]any{"type": "string", "enum": []string{"start", "stop", "reboot"}},
			}}})
	}
	return out
}

func (s *Server) lookupProposeCloud(ctx context.Context, raw json.RawMessage) (string, error) {
	var req proposeRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return "", errors.New("the arguments do not fit this tool: " + err.Error())
	}
	if req.Kind != kindStackDeploy && req.Kind != kindStackDelete && req.Kind != kindInstanceAction {
		return "", errors.New("kind must be stack-deploy, stack-delete or instance-action")
	}
	ip, _ := ctx.Value(mcpIPKey{}).(string)
	c, herr := s.proposeChange(ctx, principalFrom(ctx), ip, req)
	if herr != nil {
		return "", errors.New(herr.msg)
	}
	return fmt.Sprintf("Change %s proposed: %s. Nothing has run. Its preview is being prepared; a different administrator must review and approve it in the console under Infrastructure → Cloud changes. Tell the person that.", c.ID, c.Title), nil
}

type mcpIPKey struct{}

func (s *Server) lookupRunFailure(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	run, steps, err := s.cfg.Store.GetRun(ctx, a.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("there is no run %q; list_runs lists them", a.ID)
	} else if err != nil {
		return "", errors.New("could not read the run")
	}
	f := s.explainFailure(ctx, run, steps)
	if !f.Failed {
		return fmt.Sprintf("Run %s did not fail (status %s).", run.ID, run.Status), nil
	}
	var b strings.Builder
	b.WriteString(f.Headline + "\n")
	for _, c := range f.Causes {
		b.WriteString("- likely: " + c + "\n")
	}
	if len(f.Skipped) > 0 {
		b.WriteString("Did not run because of it: " + strings.Join(f.Skipped, ", ") + "\n")
	}
	if f.Tail != "" {
		b.WriteString("\nEnd of the output of step " + f.Step + ":\n" + f.Tail + "\n")
	}
	return maskCredentials(b.String()), nil
}

// mcpOutput masks credentials and then cuts the text, in that order: a credential that straddles the
// cut would no longer match its pattern.
func mcpOutput(s string) string { return truncate(maskCredentials(s), maxLookupResult) }

var privateKeyBlock = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)

// maskCredentials replaces credential formats in tool output. The lookups never return secret
// values; this is the second line of defence for a value that turns up in data (a log, a tag).
func maskCredentials(s string) string {
	s = privateKeyBlock.ReplaceAllString(s, credentialMask)
	for _, p := range credentialPatterns {
		s = p.re.ReplaceAllString(s, credentialMask)
	}
	return s
}

func rpcFail(id json.RawMessage, code int, msg string) rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
		s.audit(r.Context(), Principal{Name: "unknown"}, ip, "mcp.denied", "mcp", "origin "+truncate(o, 80), false)
		writeError(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	h := r.Header.Get("Authorization")
	p, tok, ok := Principal{}, APIToken{}, false
	if strings.HasPrefix(h, "Bearer ") {
		p, tok, ok = s.authenticateToken(r.Context(), strings.TrimPrefix(h, "Bearer "))
	}
	if !ok {
		if s.mcp.shouldAuditDenied(ip, time.Now()) {
			s.audit(r.Context(), Principal{Name: "unknown"}, ip, "mcp.denied", "mcp", "missing, unknown, expired or unusable token", false)
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "send a Forgeline API token as a bearer token (Account → API tokens)")
		return
	}
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "send Content-Type: application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the request is larger than 1 MiB")
		return
	}
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
		writeJSON(w, http.StatusOK, rpcFail(nil, -32600, "send one message per request"))
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, rpcFail(nil, -32700, "not a JSON-RPC 2.0 message"))
		return
	}
	if len(req.ID) == 0 { // a notification: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	ctx := context.WithValue(r.Context(), principalKey{}, p)
	ctx = context.WithValue(ctx, mcpIPKey{}, ip)
	switch req.Method {
	case "initialize":
		var in struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &in)
		proto := mcpDefaultProto
		if mcpProtocols[in.ProtocolVersion] {
			proto = in.ProtocolVersion
		}
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": "1"},
			"instructions":    "Forgeline: read what runs and what Guard found, and propose changes. A proposal runs only after a different administrator approves it in the console.",
		}})
	case "ping":
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
	case "tools/list":
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": s.mcpTools(ctx, p.Role)}})
	case "tools/call":
		s.mcpCall(ctx, w, req, p, tok, ip)
	default:
		writeJSON(w, http.StatusOK, rpcFail(req.ID, -32601, "method not found"))
	}
}

func (s *Server) mcpCall(ctx context.Context, w http.ResponseWriter, req rpcRequest, p Principal, tok APIToken, ip string) {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &in); err != nil || in.Name == "" {
		writeJSON(w, http.StatusOK, rpcFail(req.ID, -32602, "tools/call needs a name"))
		return
	}
	reply := func(text string, isErr bool) {
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: mcpCallResult{Content: []mcpContent{{Type: "text", Text: text}}, IsError: isErr}})
	}
	detail := "[token " + truncate(tok.Name, 40) + "] " + in.Name
	if len(in.Arguments) > 0 {
		detail += " " + truncate(maskCredentials(string(bytes.TrimSpace(in.Arguments))), 200)
	}
	done, admitted := s.mcp.admit(tok.ID, time.Now())
	if !admitted {
		s.audit(ctx, p, ip, "mcp.denied", in.Name, "rate limit", false)
		reply("Too many calls: at most 60 a minute and 4 at once for one token. Wait a moment and try again.", true)
		return
	}
	defer done()

	allowed := false
	for _, t := range s.mcpTools(ctx, p.Role) {
		allowed = allowed || t.Name == in.Name
	}
	if !allowed {
		s.audit(ctx, p, ip, "mcp.denied", in.Name, "not available to role "+string(p.Role)+" or not exposed over MCP", false)
		reply(fmt.Sprintf("%s is not available to this token.", in.Name), true)
		return
	}
	args := in.Arguments
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = json.RawMessage(`{}`)
	}
	cctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	var out string
	var err error
	switch in.Name {
	case toolProposeCloud:
		out, err = s.lookupProposeCloud(cctx, args)
	case "run_failure":
		out, err = s.lookupRunFailure(cctx, args)
	default:
		out, err = s.runLookup(cctx, p, lookupCall{Tool: in.Name, Args: args})
	}
	s.audit(ctx, p, ip, "mcp.call", in.Name, detail, err == nil)
	if err != nil {
		reply(maskCredentials(err.Error()), true)
		return
	}
	reply(mcpOutput(out), false)
}
