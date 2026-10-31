package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Skills and agents (note 017, slice 2). A skill is a named procedure; an agent is instructions,
// pinned skills, a tool allowlist and a tier. Every change is a new immutable version, so what ran
// last Tuesday can be read back exactly and rolled back to. Running an agent only narrows what the
// assistant may do: it never adds a tool, and every lookup still runs as the person who asked.

const (
	kindAISkill = "aiskill"
	kindAIAgent = "aiagent"

	maxVersions       = 100
	maxSkillBody      = 8 << 10
	maxInstructions   = 4 << 10
	maxAgentSkills    = 8
	maxDefName        = 60
	maxDefDesc        = 300
	maxNote           = 200
	tierAdvisory      = "T0" // reads and answers
	tierReview        = "T1" // may also file change proposals, which a person approves
	askAgentTool      = "ask_agent"
	listAgentsTool    = "list_agents"
	proposeChangeTool = "propose_aws_change"
)

// SkillVersion is one immutable version of a skill.
type SkillVersion struct {
	N    int       `json:"n"`
	Body string    `json:"body"`
	Note string    `json:"note,omitempty"`
	By   string    `json:"by"`
	At   time.Time `json:"at"`
	Hash string    `json:"hash"`
}

// AISkill is a procedure the assistant or an agent can follow.
type AISkill struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Current     int            `json:"current"`
	Versions    []SkillVersion `json:"versions"`
}

// SkillPin names the exact version of a skill an agent version uses.
type SkillPin struct {
	ID string `json:"id"`
	N  int    `json:"n"`
}

// AgentVersion is one immutable version of an agent.
type AgentVersion struct {
	N            int        `json:"n"`
	Instructions string     `json:"instructions"`
	Skills       []SkillPin `json:"skills"`
	Tools        []string   `json:"tools"`
	Tier         string     `json:"tier"`
	Note         string     `json:"note,omitempty"`
	By           string     `json:"by"`
	At           time.Time  `json:"at"`
	Hash         string     `json:"hash"`
}

// AIAgent is a named worker with a history.
type AIAgent struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Current     int            `json:"current"`
	Disabled    bool           `json:"disabled,omitempty"`
	Versions    []AgentVersion `json:"versions"`
}

func sha(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (v SkillVersion) computeHash(id string) string { return sha("skill", id, fmt.Sprint(v.N), v.Body) }

func (v AgentVersion) computeHash(id string) string {
	pins := make([]string, len(v.Skills))
	for i, p := range v.Skills {
		pins[i] = fmt.Sprintf("%s@%d", p.ID, p.N)
	}
	return sha("agent", id, fmt.Sprint(v.N), v.Instructions, strings.Join(pins, ","), strings.Join(v.Tools, ","), v.Tier)
}

func (a AIAgent) version(n int) (AgentVersion, bool) {
	for _, v := range a.Versions {
		if v.N == n {
			return v, true
		}
	}
	return AgentVersion{}, false
}

func (s AISkill) version(n int) (SkillVersion, bool) {
	for _, v := range s.Versions {
		if v.N == n {
			return v, true
		}
	}
	return SkillVersion{}, false
}

// --- agent scope: what a running agent is allowed to do ---

type agentScopeKey struct{}

// agentScope narrows the assistant while an agent runs.
type agentScope struct {
	persona string
	tools   map[string]bool
	label   string // "agent-id@version", for the audit log
}

func scopeFrom(ctx context.Context) *agentScope {
	sc, _ := ctx.Value(agentScopeKey{}).(*agentScope)
	return sc
}

// allows says whether the running agent (if any) may use a tool.
func (sc *agentScope) allows(tool string) bool { return sc == nil || sc.tools[tool] }

// validTools checks an agent's tool list against the real lookups. ask_agent is never allowed (no
// agents calling agents), and only a review-tier agent may file proposals.
func validTools(tools []string, tier string, extra ...string) error {
	known := map[string]bool{}
	for _, n := range extra {
		known[n] = true // plugin tools, which are not in the fixed list
	}
	for _, t := range assistantLookups() {
		known[t.name] = true
	}
	known[listAgentsTool] = true
	seen := map[string]bool{}
	for _, t := range tools {
		switch {
		case t == askAgentTool:
			return errors.New("an agent cannot call other agents")
		case !known[t]:
			return fmt.Errorf("%q is not a tool the assistant has", t)
		case seen[t]:
			return fmt.Errorf("%q is listed twice", t)
		case isProposalTool(t) && tier != tierReview:
			return errors.New("only a review-tier (T1) agent may file proposals")
		}
		seen[t] = true
	}
	return nil
}

// isProposalTool says whether a tool files something a person will approve. Only T1 agents get them.
func isProposalTool(t string) bool { return t == proposeChangeTool || t == requestEnvTool }

func validTier(t string) bool { return t == tierAdvisory || t == tierReview }

// compose builds the system text for one agent version with its pinned skills.
func (s *Server) composeAgent(ctx context.Context, a AIAgent, v AgentVersion) (*agentScope, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the agent %q (version %d). Stay within this role.\n\n%s\n", a.Name, v.N, v.Instructions)
	if len(v.Skills) > 0 {
		b.WriteString("\nSkills you have. Follow them when they apply:\n")
		for _, pin := range v.Skills {
			var sk AISkill
			if err := s.docs.Get(ctx, kindAISkill, pin.ID, &sk); err != nil {
				return nil, fmt.Errorf("the skill %q is missing", pin.ID)
			}
			sv, ok := sk.version(pin.N)
			if !ok {
				return nil, fmt.Errorf("the skill %q has no version %d", pin.ID, pin.N)
			}
			fmt.Fprintf(&b, "\n### %s (version %d)\n%s\n", sk.Name, sv.N, sv.Body)
		}
	}
	if v.Tier == tierAdvisory {
		b.WriteString("\nYou may read and advise. You cannot file change proposals.\n")
	}
	tools := map[string]bool{}
	for _, t := range v.Tools {
		tools[t] = true
	}
	return &agentScope{persona: b.String(), tools: tools, label: fmt.Sprintf("%s@%d", a.ID, v.N)}, nil
}

// --- storage helpers ---

func (s *Server) loadSkill(ctx context.Context, id string) (AISkill, error) {
	var sk AISkill
	err := s.docs.Get(ctx, kindAISkill, id, &sk)
	return sk, err
}

func (s *Server) loadAgent(ctx context.Context, id string) (AIAgent, error) {
	var a AIAgent
	err := s.docs.Get(ctx, kindAIAgent, id, &a)
	return a, err
}

func (s *Server) defError(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, docstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such "+what)
		return
	}
	s.internalError(w, "reading the "+what, err)
}

func checkNameDesc(name, desc string) string {
	switch {
	case strings.TrimSpace(name) == "" || len(name) > maxDefName:
		return fmt.Sprintf("give it a name of up to %d characters", maxDefName)
	case len(desc) > maxDefDesc:
		return fmt.Sprintf("the description is longer than %d characters", maxDefDesc)
	}
	return ""
}

// newDefID makes a unique slug from a name for the given kind.
func (s *Server) newDefID(ctx context.Context, kind, name string) (string, error) {
	base := accountSlug(name)
	id := base
	for i := 0; i < 20; i++ {
		var tmp json.RawMessage
		err := s.docs.Get(ctx, kind, id, &tmp)
		if errors.Is(err, docstore.ErrNotFound) {
			return id, nil
		} else if err != nil {
			return "", err
		}
		id = base + "-" + randomID()[:4]
	}
	return "", errors.New("could not find a free id")
}

// --- skills ---

type skillRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Note        string `json:"note"`
}

func (r skillRequest) check() string {
	if m := checkNameDesc(r.Name, r.Description); m != "" {
		return m
	}
	switch {
	case strings.TrimSpace(r.Body) == "":
		return "write the steps the skill should follow"
	case len(r.Body) > maxSkillBody:
		return fmt.Sprintf("the skill is longer than %d characters", maxSkillBody)
	case len(r.Note) > maxNote:
		return fmt.Sprintf("the note is longer than %d characters", maxNote)
	}
	return ""
}

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	docs, err := s.docs.List(r.Context(), kindAISkill, docstore.ListOptions{})
	if err != nil {
		s.internalError(w, "reading the skills", err)
		return
	}
	all, err := docstore.Decode[AISkill](docs)
	if err != nil {
		s.internalError(w, "reading the skills", err)
		return
	}
	sort.Slice(all, func(i, j int) bool { return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name) })
	for i := range all {
		all[i].Versions = nil // the list shows names; a skill's history is one request away
	}
	if all == nil {
		all = []AISkill{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": all})
}

func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	sk, err := s.loadSkill(r.Context(), r.PathValue("id"))
	if err != nil {
		s.defError(w, "skill", err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (s *Server) handleCreateSkill(w http.ResponseWriter, r *http.Request) {
	var req skillRequest
	if !readJSON(w, r, &req) {
		return
	}
	if m := req.check(); m != "" {
		writeError(w, http.StatusBadRequest, m)
		return
	}
	ctx := r.Context()
	id, err := s.newDefID(ctx, kindAISkill, req.Name)
	if err != nil {
		s.internalError(w, "saving the skill", err)
		return
	}
	p := principalFrom(ctx)
	sv := SkillVersion{N: 1, Body: req.Body, Note: req.Note, By: p.Name, At: time.Now().UTC()}
	sv.Hash = sv.computeHash(id)
	sk := AISkill{ID: id, Name: strings.TrimSpace(req.Name), Description: req.Description, Current: 1, Versions: []SkillVersion{sv}}
	if err := s.docs.Put(ctx, kindAISkill, id, sk); err != nil {
		s.internalError(w, "saving the skill", err)
		return
	}
	s.auditReq(r, "ai.skill.create", "skill/"+id, fmt.Sprintf("%s v1 %s", sk.Name, sv.Hash[:12]), true)
	writeJSON(w, http.StatusCreated, sk)
}

func (s *Server) handleSkillVersion(w http.ResponseWriter, r *http.Request) {
	var req skillRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	sk, err := s.loadSkill(ctx, r.PathValue("id"))
	if err != nil {
		s.defError(w, "skill", err)
		return
	}
	if req.Name == "" {
		req.Name = sk.Name
	}
	if m := req.check(); m != "" {
		writeError(w, http.StatusBadRequest, m)
		return
	}
	if len(sk.Versions) >= maxVersions {
		writeError(w, http.StatusConflict, fmt.Sprintf("a skill keeps at most %d versions", maxVersions))
		return
	}
	last := sk.Versions[len(sk.Versions)-1]
	if last.Body == req.Body && sk.Name == strings.TrimSpace(req.Name) && sk.Description == req.Description {
		writeError(w, http.StatusConflict, "nothing changed since the latest version")
		return
	}
	p := principalFrom(ctx)
	sv := SkillVersion{N: last.N + 1, Body: req.Body, Note: req.Note, By: p.Name, At: time.Now().UTC()}
	sv.Hash = sv.computeHash(sk.ID)
	sk.Name, sk.Description = strings.TrimSpace(req.Name), req.Description
	sk.Versions = append(sk.Versions, sv)
	sk.Current = sv.N
	if err := s.docs.Put(ctx, kindAISkill, sk.ID, sk); err != nil {
		s.internalError(w, "saving the skill", err)
		return
	}
	s.auditReq(r, "ai.skill.version", "skill/"+sk.ID, fmt.Sprintf("v%d %s", sv.N, sv.Hash[:12]), true)
	writeJSON(w, http.StatusCreated, sk)
}

type rollbackRequest struct {
	To   int    `json:"to"`
	Note string `json:"note"`
}

// handleSkillRollback makes an old version current again by copying it forward as a new version, so
// the history only ever grows.
func (s *Server) handleSkillRollback(w http.ResponseWriter, r *http.Request) {
	var req rollbackRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	sk, err := s.loadSkill(ctx, r.PathValue("id"))
	if err != nil {
		s.defError(w, "skill", err)
		return
	}
	old, ok := sk.version(req.To)
	if !ok {
		writeError(w, http.StatusNotFound, "no such version")
		return
	}
	if req.To == sk.Current {
		writeError(w, http.StatusConflict, "that is already the current version")
		return
	}
	if len(sk.Versions) >= maxVersions {
		writeError(w, http.StatusConflict, fmt.Sprintf("a skill keeps at most %d versions", maxVersions))
		return
	}
	p := principalFrom(ctx)
	note := strings.TrimSpace(req.Note)
	if note == "" {
		note = fmt.Sprintf("rolled back to v%d", old.N)
	}
	sv := SkillVersion{N: sk.Versions[len(sk.Versions)-1].N + 1, Body: old.Body, Note: truncate(note, maxNote), By: p.Name, At: time.Now().UTC()}
	sv.Hash = sv.computeHash(sk.ID)
	sk.Versions = append(sk.Versions, sv)
	sk.Current = sv.N
	if err := s.docs.Put(ctx, kindAISkill, sk.ID, sk); err != nil {
		s.internalError(w, "saving the skill", err)
		return
	}
	s.auditReq(r, "ai.skill.rollback", "skill/"+sk.ID, fmt.Sprintf("to v%d as v%d", old.N, sv.N), true)
	writeJSON(w, http.StatusCreated, sk)
}

func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	sk, err := s.loadSkill(ctx, id)
	if err != nil {
		s.defError(w, "skill", err)
		return
	}
	agents, err := s.listAgents(ctx)
	if err != nil {
		s.internalError(w, "reading the agents", err)
		return
	}
	for _, a := range agents {
		for _, v := range a.Versions {
			for _, pin := range v.Skills {
				if pin.ID == id {
					writeError(w, http.StatusConflict, fmt.Sprintf("agent %q (version %d) uses this skill, and its history must stay readable", a.Name, v.N))
					return
				}
			}
		}
	}
	if err := s.docs.Delete(ctx, kindAISkill, id); err != nil {
		s.internalError(w, "removing the skill", err)
		return
	}
	s.auditReq(r, "ai.skill.delete", "skill/"+id, sk.Name, true)
	w.WriteHeader(http.StatusNoContent)
}

// --- agents ---

type agentRequest struct {
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Instructions string     `json:"instructions"`
	Skills       []SkillPin `json:"skills"` // N 0 pins the skill's current version
	Tools        []string   `json:"tools"`
	Tier         string     `json:"tier"`
	Note         string     `json:"note"`
}

// resolve checks the request and returns the version it describes, with skill pins made exact.
func (s *Server) resolveAgent(ctx context.Context, req agentRequest) (AgentVersion, string) {
	if m := checkNameDesc(req.Name, req.Description); m != "" {
		return AgentVersion{}, m
	}
	switch {
	case strings.TrimSpace(req.Instructions) == "":
		return AgentVersion{}, "say what the agent is for"
	case len(req.Instructions) > maxInstructions:
		return AgentVersion{}, fmt.Sprintf("the instructions are longer than %d characters", maxInstructions)
	case len(req.Note) > maxNote:
		return AgentVersion{}, fmt.Sprintf("the note is longer than %d characters", maxNote)
	case len(req.Skills) > maxAgentSkills:
		return AgentVersion{}, fmt.Sprintf("an agent can use at most %d skills", maxAgentSkills)
	case !validTier(req.Tier):
		return AgentVersion{}, "the tier must be T0 (advisory) or T1 (may file proposals)"
	}
	if err := validTools(req.Tools, req.Tier, s.pluginToolNames(ctx)...); err != nil {
		return AgentVersion{}, err.Error()
	}
	pins := []SkillPin{}
	seen := map[string]bool{}
	for _, pin := range req.Skills {
		if seen[pin.ID] {
			return AgentVersion{}, fmt.Sprintf("skill %q is listed twice", pin.ID)
		}
		seen[pin.ID] = true
		sk, err := s.loadSkill(ctx, pin.ID)
		if err != nil {
			return AgentVersion{}, fmt.Sprintf("there is no skill %q", pin.ID)
		}
		if pin.N == 0 {
			pin.N = sk.Current
		}
		if _, ok := sk.version(pin.N); !ok {
			return AgentVersion{}, fmt.Sprintf("skill %q has no version %d", pin.ID, pin.N)
		}
		pins = append(pins, pin)
	}
	tools := append([]string{}, req.Tools...)
	sort.Strings(tools)
	return AgentVersion{Instructions: req.Instructions, Skills: pins, Tools: tools, Tier: req.Tier, Note: req.Note}, ""
}

func (s *Server) listAgents(ctx context.Context) ([]AIAgent, error) {
	docs, err := s.docs.List(ctx, kindAIAgent, docstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	all, err := docstore.Decode[AIAgent](docs)
	sort.Slice(all, func(i, j int) bool { return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name) })
	return all, err
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	all, err := s.listAgents(r.Context())
	if err != nil {
		s.internalError(w, "reading the agents", err)
		return
	}
	out := make([]map[string]any, 0, len(all))
	for _, a := range all {
		cur, _ := a.version(a.Current)
		out = append(out, map[string]any{"id": a.ID, "name": a.Name, "description": a.Description, "current": a.Current, "disabled": a.Disabled,
			"tier": cur.Tier, "tools": cur.Tools, "skills": cur.Skills, "versions": len(a.Versions)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.loadAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		s.defError(w, "agent", err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req agentRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	v, msg := s.resolveAgent(ctx, req)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	id, err := s.newDefID(ctx, kindAIAgent, req.Name)
	if err != nil {
		s.internalError(w, "saving the agent", err)
		return
	}
	p := principalFrom(ctx)
	v.N, v.By, v.At = 1, p.Name, time.Now().UTC()
	v.Hash = v.computeHash(id)
	a := AIAgent{ID: id, Name: strings.TrimSpace(req.Name), Description: req.Description, Current: 1, Versions: []AgentVersion{v}}
	if err := s.docs.Put(ctx, kindAIAgent, id, a); err != nil {
		s.internalError(w, "saving the agent", err)
		return
	}
	s.auditReq(r, "ai.agent.create", "agent/"+id, fmt.Sprintf("%s v1 %s tier %s", a.Name, v.Hash[:12], v.Tier), true)
	writeJSON(w, http.StatusCreated, a)
}

func sameVersionContent(a, b AgentVersion) bool {
	if a.Instructions != b.Instructions || a.Tier != b.Tier || strings.Join(a.Tools, ",") != strings.Join(b.Tools, ",") || len(a.Skills) != len(b.Skills) {
		return false
	}
	for i := range a.Skills {
		if a.Skills[i] != b.Skills[i] {
			return false
		}
	}
	return true
}

func (s *Server) handleAgentVersion(w http.ResponseWriter, r *http.Request) {
	var req agentRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	a, err := s.loadAgent(ctx, r.PathValue("id"))
	if err != nil {
		s.defError(w, "agent", err)
		return
	}
	if req.Name == "" {
		req.Name = a.Name
	}
	v, msg := s.resolveAgent(ctx, req)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if len(a.Versions) >= maxVersions {
		writeError(w, http.StatusConflict, fmt.Sprintf("an agent keeps at most %d versions", maxVersions))
		return
	}
	last := a.Versions[len(a.Versions)-1]
	if sameVersionContent(last, v) && a.Name == strings.TrimSpace(req.Name) && a.Description == req.Description {
		writeError(w, http.StatusConflict, "nothing changed since the latest version")
		return
	}
	p := principalFrom(ctx)
	v.N, v.By, v.At = last.N+1, p.Name, time.Now().UTC()
	v.Hash = v.computeHash(a.ID)
	a.Name, a.Description = strings.TrimSpace(req.Name), req.Description
	a.Versions = append(a.Versions, v)
	a.Current = v.N
	if err := s.docs.Put(ctx, kindAIAgent, a.ID, a); err != nil {
		s.internalError(w, "saving the agent", err)
		return
	}
	s.auditReq(r, "ai.agent.version", "agent/"+a.ID, fmt.Sprintf("v%d %s tier %s", v.N, v.Hash[:12], v.Tier), true)
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleAgentRollback(w http.ResponseWriter, r *http.Request) {
	var req rollbackRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	a, err := s.loadAgent(ctx, r.PathValue("id"))
	if err != nil {
		s.defError(w, "agent", err)
		return
	}
	old, ok := a.version(req.To)
	if !ok {
		writeError(w, http.StatusNotFound, "no such version")
		return
	}
	if req.To == a.Current {
		writeError(w, http.StatusConflict, "that is already the current version")
		return
	}
	if len(a.Versions) >= maxVersions {
		writeError(w, http.StatusConflict, fmt.Sprintf("an agent keeps at most %d versions", maxVersions))
		return
	}
	p := principalFrom(ctx)
	note := strings.TrimSpace(req.Note)
	if note == "" {
		note = fmt.Sprintf("rolled back to v%d", old.N)
	}
	v := old
	v.N, v.Note, v.By, v.At = a.Versions[len(a.Versions)-1].N+1, truncate(note, maxNote), p.Name, time.Now().UTC()
	v.Hash = v.computeHash(a.ID)
	a.Versions = append(a.Versions, v)
	a.Current = v.N
	if err := s.docs.Put(ctx, kindAIAgent, a.ID, a); err != nil {
		s.internalError(w, "saving the agent", err)
		return
	}
	s.auditReq(r, "ai.agent.rollback", "agent/"+a.ID, fmt.Sprintf("to v%d as v%d", old.N, v.N), true)
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleAgentEnable(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := s.loadAgent(r.Context(), r.PathValue("id"))
		if err != nil {
			s.defError(w, "agent", err)
			return
		}
		a.Disabled = disabled
		if err := s.docs.Put(r.Context(), kindAIAgent, a.ID, a); err != nil {
			s.internalError(w, "saving the agent", err)
			return
		}
		verb := "ai.agent.enable"
		if disabled {
			verb = "ai.agent.disable"
		}
		s.auditReq(r, verb, "agent/"+a.ID, a.Name, true)
		writeJSON(w, http.StatusOK, a)
	}
}

// --- running an agent ---

type agentChatRequest struct {
	Message string     `json:"message"`
	History []chatTurn `json:"history"`
	Version int        `json:"version"` // 0 is the current version
}

func (s *Server) handleAgentChat(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if !s.assistLimiter.allow(p.Name) {
		writeError(w, http.StatusTooManyRequests, "slow down: the assistant allows 20 questions a minute")
		return
	}
	var req agentChatRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "ask a question")
		return
	}
	if len(req.Message) > maxAssistMessage {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the question is longer than %d characters", maxAssistMessage))
		return
	}
	if s.loadAssistant(r.Context()).Backend == assistOff {
		writeError(w, http.StatusConflict, "the assistant is not set up. An administrator can turn it on under Integrations")
		return
	}
	a, err := s.loadAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		s.defError(w, "agent", err)
		return
	}
	if a.Disabled {
		writeError(w, http.StatusConflict, "this agent is turned off")
		return
	}
	n := req.Version
	if n == 0 {
		n = a.Current
	}
	v, ok := a.version(n)
	if !ok {
		writeError(w, http.StatusNotFound, "no such version")
		return
	}
	turns := historyTurns(req.History, req.Message)
	sink := &lookupSink{}
	ctx := context.WithValue(r.Context(), lookupSinkKey{}, sink)
	reply, lookups, label, err := s.runAgent(ctx, p, a, v, turns, func(tool string, ok bool) {
		s.auditReq(r, "assistant.lookup", "agent/"+a.ID, tool, ok)
	})
	s.auditReq(r, "ai.agent.run", "agent/"+a.ID, fmt.Sprintf("v%d, %d lookups, %v", v.N, len(lookups), err == nil), err == nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reply": reply, "lookups": lookups, "proposals": sink.proposals, "environments": sink.environments, "agent": label, "hash": v.Hash})
}

// historyTurns trims a conversation the same way the assistant does.
func historyTurns(hist []chatTurn, message string) []chatTurn {
	turns := make([]chatTurn, 0, maxAssistHistory+1)
	if len(hist) > maxAssistHistory {
		hist = hist[len(hist)-maxAssistHistory:]
	}
	for _, t := range hist {
		if (t.Role != "user" && t.Role != "assistant") || strings.TrimSpace(t.Content) == "" {
			continue
		}
		turns = append(turns, chatTurn{Role: t.Role, Content: truncate(t.Content, maxAssistTurn)})
	}
	for len(turns) > 0 && turns[0].Role != "user" {
		turns = turns[1:]
	}
	return append(turns, chatTurn{Role: "user", Content: message})
}

// runAgent answers as the agent at one version. The person's own role still limits every lookup.
func (s *Server) runAgent(ctx context.Context, p Principal, a AIAgent, v AgentVersion, turns []chatTurn, audit func(string, bool)) (string, []string, string, error) {
	sc, err := s.composeAgent(ctx, a, v)
	if err != nil {
		return "", nil, "", err
	}
	ctx = context.WithValue(ctx, agentScopeKey{}, sc)
	reply, lookups, err := s.askWithLookups(ctx, p, turns, "", audit)
	return reply, lookups, sc.label, err
}

// --- the assistant's view of agents ---

func (s *Server) lookupListAgents(ctx context.Context, _ json.RawMessage) (string, error) {
	all, err := s.listAgents(ctx)
	if err != nil {
		return "", errors.New("could not read the agents")
	}
	var b strings.Builder
	for _, a := range all {
		if a.Disabled {
			continue
		}
		cur, _ := a.version(a.Current)
		fmt.Fprintf(&b, "- id %q: %s (tier %s). %s\n", a.ID, a.Name, cur.Tier, a.Description)
	}
	if b.Len() == 0 {
		return "There are no agents.", nil
	}
	return b.String(), nil
}

// lookupAskAgent hands one task to an agent and returns its answer. An agent cannot do this itself.
func (s *Server) lookupAskAgent(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Agent string `json:"agent"`
		Task  string `json:"task"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return "", err
	}
	if scopeFrom(ctx) != nil {
		return "", errors.New("an agent cannot call other agents")
	}
	task := strings.TrimSpace(args.Task)
	if task == "" {
		return "", errors.New("say what the agent should do")
	}
	a, err := s.loadAgent(ctx, args.Agent)
	if err != nil {
		return "", fmt.Errorf("there is no agent %q; list_agents shows them", args.Agent)
	}
	if a.Disabled {
		return "", errors.New("that agent is turned off")
	}
	v, _ := a.version(a.Current)
	p := principalFrom(ctx)
	reply, lookups, label, err := s.runAgent(ctx, p, a, v, []chatTurn{{Role: "user", Content: truncate(task, maxAssistMessage)}}, func(tool string, ok bool) {
		s.audit(ctx, p, "", "assistant.lookup", "agent/"+a.ID, tool, ok)
	})
	s.audit(ctx, p, "", "ai.agent.run", "agent/"+a.ID, fmt.Sprintf("v%d via the assistant, %d lookups, %v", v.N, len(lookups), err == nil), err == nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Answer from agent %s (looked up: %s):\n%s", label, strings.Join(lookups, ", "), reply), nil
}

// handleListAITools is what an agent can be given, for the form that builds one.
func (s *Server) handleListAITools(w http.ResponseWriter, r *http.Request) {
	type tool struct {
		Name  string `json:"name"`
		Help  string `json:"help"`
		Min   string `json:"min"`             // the lowest role that can run it, whoever the agent is
		Needs string `json:"needs,omitempty"` // "mcp" when it exists only with the AWS MCP Server on
	}
	out := []tool{}
	for _, t := range assistantLookups() {
		if t.name == askAgentTool {
			continue
		}
		x := tool{Name: t.name, Help: t.help, Min: string(t.min)}
		if needsMCP(t.name) {
			x.Needs = "mcp"
		}
		out = append(out, x)
	}
	for _, t := range s.pluginTools(r.Context()) {
		out = append(out, tool{Name: t.name, Help: t.help, Min: string(authn.Developer), Needs: "plugins"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}
