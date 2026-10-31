package server

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Marketplace (note 017, slice 7). A package is a bundle of skills and agents in a manifest that
// carries its own sha256. The catalogue shipped in the binary is checked against those hashes when
// the server starts, and an install is bound to the hash the administrator was shown, so what is
// installed is exactly what was reviewed. Installing only creates ordinary skills and agents (with
// versions, like anything else); it adds no new way to write. The tier is decided at install and
// kept on the install record, so a package update can never raise it.

//go:embed market/*.json
var marketFS embed.FS

// marketFiles is where the catalogue is read from; tests swap it to publish a newer version.
var marketFiles fs.FS = marketFS

const kindMarketInstall = "marketinstall"

var marketIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}$`)

// MarketSkill is a skill inside a package. Key names it within the package only.
type MarketSkill struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// MarketAgent is an agent inside a package; Skills are keys of the package's own skills.
type MarketAgent struct {
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Instructions string   `json:"instructions"`
	Skills       []string `json:"skills"`
	Tools        []string `json:"tools"`
	Tier         string   `json:"tier"`
}

// MarketWorkflow is a workflow inside a package. Its steps name agents by the key of an agent in the
// same package; it cannot be scheduled.
type MarketWorkflow struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Stages      []WFStage `json:"stages"`
	RunAs       string    `json:"run_as"`
}

// MarketPackage is one entry in the catalogue.
type MarketPackage struct {
	ID           string              `json:"id"`
	Version      int                 `json:"version"`
	Title        string              `json:"title"`
	Description  string              `json:"description"`
	Publisher    string              `json:"publisher"`
	Tags         []string            `json:"tags"`
	Skills       []MarketSkill       `json:"skills"`
	Agents       []MarketAgent       `json:"agents"`
	Plugins      []MarketPlugin      `json:"plugins,omitempty"`
	Workflows    []MarketWorkflow    `json:"workflows,omitempty"`
	EnvTemplates []MarketEnvTemplate `json:"env_templates,omitempty"`
	SHA256       string              `json:"sha256"`
}

// contentHash is the fingerprint of everything but the hash field itself.
func (p MarketPackage) contentHash() string {
	p.SHA256 = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// check validates a package the way an administrator would create the same things by hand.
func (p MarketPackage) check() error {
	switch {
	case !marketIDRE.MatchString(p.ID):
		return fmt.Errorf("package id %q is not a lowercase slug", p.ID)
	case p.Version < 1:
		return errors.New("the version must be 1 or more")
	case p.Title == "" || p.Publisher == "":
		return errors.New("a package needs a title and a publisher")
	case len(p.Skills)+len(p.Agents)+len(p.Plugins)+len(p.Workflows)+len(p.EnvTemplates) == 0:
		return errors.New("a package must hold something to install")
	case p.SHA256 != p.contentHash():
		return errors.New("the content does not match its sha256")
	}
	keys := map[string]bool{}
	for _, sk := range p.Skills {
		r := skillRequest{Name: sk.Name, Description: sk.Description, Body: sk.Body}
		if m := r.check(); m != "" || sk.Key == "" || keys["s:"+sk.Key] {
			return fmt.Errorf("skill %q: %s", sk.Key, orText(m, "its key is empty or repeated"))
		}
		keys["s:"+sk.Key] = true
	}
	extra, err := p.checkPlugins()
	if err != nil {
		return err
	}
	for _, a := range p.Agents {
		if m := checkNameDesc(a.Name, a.Description); m != "" || a.Key == "" || keys["a:"+a.Key] {
			return fmt.Errorf("agent %q: %s", a.Key, orText(m, "its key is empty or repeated"))
		}
		keys["a:"+a.Key] = true
		switch {
		case strings.TrimSpace(a.Instructions) == "" || len(a.Instructions) > maxInstructions:
			return fmt.Errorf("agent %q: instructions are empty or too long", a.Key)
		case len(a.Skills) > maxAgentSkills:
			return fmt.Errorf("agent %q: too many skills", a.Key)
		case !validTier(a.Tier):
			return fmt.Errorf("agent %q: unknown tier %q", a.Key, a.Tier)
		}
		if err := validTools(a.Tools, a.Tier, extra...); err != nil {
			return fmt.Errorf("agent %q: %w", a.Key, err)
		}
		for _, k := range a.Skills {
			if !keys["s:"+k] {
				return fmt.Errorf("agent %q uses skill %q that the package does not hold", a.Key, k)
			}
		}
	}
	for _, w := range p.Workflows {
		if err := w.check(keys); err != nil {
			return err
		}
		keys["w:"+w.Key] = true
	}
	for _, t := range p.EnvTemplates {
		if keys["t:"+t.Key] {
			return fmt.Errorf("template key %q is repeated", t.Key)
		}
		keys["t:"+t.Key] = true
		if err := t.check(); err != nil {
			return err
		}
	}
	return nil
}

// check validates a packaged workflow. keys holds the package's own skills ("s:"), agents ("a:") and
// workflows ("w:") already seen.
func (w MarketWorkflow) check(keys map[string]bool) error {
	if m := checkNameDesc(w.Name, w.Description); m != "" || w.Key == "" || keys["w:"+w.Key] {
		return fmt.Errorf("workflow %q: %s", w.Key, orText(m, "its key is empty or repeated"))
	}
	switch {
	case w.RunAs != "viewer" && w.RunAs != "developer":
		return fmt.Errorf("workflow %q: run_as is viewer or developer", w.Key)
	case len(w.Stages) == 0 || len(w.Stages) > maxWFStages:
		return fmt.Errorf("workflow %q: it needs 1 to %d stages", w.Key, maxWFStages)
	}
	for _, st := range w.Stages {
		if len(st.Name) > maxDefName || len(st.Steps) == 0 || len(st.Steps) > maxWFStepsPerStage {
			return fmt.Errorf("workflow %q: a stage needs a short name and 1 to %d steps", w.Key, maxWFStepsPerStage)
		}
		for _, sp := range st.Steps {
			switch {
			case sp.ID != "" || sp.AgentN != 0:
				return fmt.Errorf("workflow %q: steps carry only an agent key and a task", w.Key)
			case strings.TrimSpace(sp.Task) == "" || len(sp.Task) > maxWFTask:
				return fmt.Errorf("workflow %q: each step needs a task of up to %d characters", w.Key, maxWFTask)
			case !keys["a:"+sp.Agent]:
				return fmt.Errorf("workflow %q uses agent %q that the package does not hold", w.Key, sp.Agent)
			}
		}
	}
	return nil
}

func orText(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// marketEntry is a package as the catalogue returns it: the package, why it cannot be installed if
// it cannot, and what is installed of it.
type marketEntry struct {
	MarketPackage
	Problem   string         `json:"problem,omitempty"`
	Source    string         `json:"source"`           // built-in, local (FORGELINE_MARKET_DIR) or git (signed)
	Signer    string         `json:"signer,omitempty"` // the trusted key that signed it, for git
	Installed *MarketInstall `json:"installed,omitempty"`
	Update    bool           `json:"update,omitempty"`
	// Writes is true when an agent in the package may file change proposals at T1.
	Writes bool `json:"writes"`
}

// MarketInstall records what an install created, so an upgrade and an uninstall touch only that.
type MarketInstall struct {
	ID      string                 `json:"id"`
	Version int                    `json:"version"`
	Hash    string                 `json:"hash"`
	Tier    string                 `json:"tier"` // the most any agent from this package may be
	By      string                 `json:"by"`
	At      time.Time              `json:"at"`
	Skills  map[string]string      `json:"skills"` // package key to skill id
	Agents  map[string]string      `json:"agents"`
	Plugins map[string]PluginGrant `json:"plugins,omitempty"`
	// Workflows maps a package workflow key to the workflow it made; Templates lists packaged
	// environment template ids.
	Workflows map[string]string `json:"workflows,omitempty"`
	Templates []string          `json:"templates,omitempty"`
	// Source and Signer say where the package came from; an upgrade must come from the same.
	Source string `json:"source,omitempty"`
	Signer string `json:"signer,omitempty"`
}

// marketCatalog reads the shipped manifests. A package that fails its check stays in the list with
// the reason, so a damaged catalogue is visible, but it cannot be installed.
func (s *Server) marketCatalog(ctx context.Context) []marketEntry {
	var out []marketEntry
	seen := map[string]bool{}
	add := func(fsys fs.FS, dir, source string) {
		files, _ := fs.ReadDir(fsys, dir)
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			b, err := fs.ReadFile(fsys, path.Join(dir, f.Name()))
			var e marketEntry
			if err == nil {
				err = json.Unmarshal(b, &e.MarketPackage)
			}
			if err == nil {
				err = e.MarketPackage.check()
			}
			if err == nil && seen[e.ID] {
				// Shown, but under another id so it can never stand in for the first one.
				err = errors.New("another package already has the id " + e.ID)
				e.ID += " (duplicate from " + source + ")"
			}
			if err != nil {
				e.Problem = err.Error()
				if e.ID == "" {
					e.ID = strings.TrimSuffix(f.Name(), ".json")
				}
			}
			seen[e.ID] = true
			e.Source = source
			for _, a := range e.Agents {
				if a.Tier == tierReview {
					e.Writes = true
				}
			}
			out = append(out, e)
		}
	}
	add(marketFiles, "market", "built-in")
	// A directory of manifests set by whoever runs the server (never by the console). They are
	// checked exactly like the built-in ones, hash included.
	if dir := os.Getenv("FORGELINE_MARKET_DIR"); dir != "" {
		add(os.DirFS(dir), ".", "local")
	}
	// Packages from the signed git source come last, so a built-in or local package with the same id
	// always wins. A verified one that clashes is shown as a duplicate; a damaged one never takes an id
	// that is in use (its id comes from a file nobody vouched for), and never makes a good one a duplicate.
	var git []marketEntry
	all := s.gitEntries(ctx)
	for _, e := range all {
		if e.Problem == "" {
			git = append(git, e)
		}
	}
	for _, e := range all {
		if e.Problem != "" {
			git = append(git, e)
		}
	}
	for _, e := range git {
		if seen[e.ID] {
			if e.Problem == "" {
				e.Problem = "another package already has the id " + e.ID
			}
			e.ID += " (duplicate from git)"
		}
		if e.Problem == "" {
			seen[e.ID] = true
		}
		out = append(out, e)
	}
	// A nil list would reach the console as null.
	for i := range out {
		if out[i].Skills == nil {
			out[i].Skills = []MarketSkill{}
		}
		if out[i].Agents == nil {
			out[i].Agents = []MarketAgent{}
		}
		if out[i].Tags == nil {
			out[i].Tags = []string{}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out
}

func (s *Server) loadInstall(ctx context.Context, id string) (MarketInstall, error) {
	var in MarketInstall
	err := s.docs.Get(ctx, kindMarketInstall, id, &in)
	return in, err
}

func (s *Server) handleMarketList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	docs, err := s.docs.List(ctx, kindMarketInstall, docstore.ListOptions{})
	var ins []MarketInstall
	if err == nil {
		ins, err = docstore.Decode[MarketInstall](docs)
	}
	if err != nil {
		s.internalError(w, "reading what is installed", err)
		return
	}
	byID := map[string]MarketInstall{}
	for _, in := range ins {
		byID[in.ID] = in
	}
	cat := s.marketCatalog(ctx)
	for i := range cat {
		if in, ok := byID[cat[i].ID]; ok {
			cp := in
			cat[i].Installed = &cp
			cat[i].Update = cat[i].Problem == "" && cat[i].Version > in.Version
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"packages": cat, "git": s.marketGitStatus()})
}

type marketInstallRequest struct {
	ID   string `json:"id"`
	Hash string `json:"hash"` // the sha256 the administrator reviewed
	Tier string `json:"tier"` // T0 or T1; ignored on an upgrade
	// Grants are what the administrator allows each plugin, by plugin key. Nothing is on by default.
	Grants map[string]PluginGrantRequest `json:"grants"`
}

var tierRank = map[string]int{tierAdvisory: 0, tierReview: 1}

func (s *Server) handleMarketInstall(w http.ResponseWriter, r *http.Request) {
	var req marketInstallRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	var pkg *marketEntry
	for _, e := range s.marketCatalog(ctx) {
		if e.ID == req.ID && pkg == nil {
			e := e
			pkg = &e
		}
	}
	switch {
	case pkg == nil:
		writeError(w, http.StatusNotFound, "no such package")
		return
	case pkg.Problem != "":
		writeError(w, http.StatusConflict, "this package cannot be installed: "+pkg.Problem)
		return
	case req.Hash != pkg.SHA256:
		writeError(w, http.StatusConflict, "this package changed since you looked at it; review it again")
		return
	}
	cur, err := s.loadInstall(ctx, pkg.ID)
	upgrade := err == nil
	if err != nil && !errors.Is(err, docstore.ErrNotFound) {
		s.internalError(w, "reading what is installed", err)
		return
	}
	tier := req.Tier
	if upgrade {
		tier = cur.Tier // an update never raises it
		// An install made before sources were recorded (Source empty) came from the binary or the
		// local directory; it may be updated from there but not taken over by a git package.
		if (cur.Source == "" && pkg.Source == "git") || (cur.Source != "" && (cur.Source != pkg.Source || cur.Signer != pkg.Signer)) {
			writeError(w, http.StatusConflict, fmt.Sprintf("this package was installed from %s; an update must come from the same source and signer", sourceLabel(orText(cur.Source, "the built-in or local catalogue"), cur.Signer)))
			return
		}
		if pkg.Version <= cur.Version {
			writeError(w, http.StatusConflict, fmt.Sprintf("version %d is already installed", cur.Version))
			return
		}
	} else if tier == "" {
		tier = tierAdvisory
	}
	if !validTier(tier) {
		writeError(w, http.StatusBadRequest, "the tier must be T0 (advisory) or T1 (may file proposals)")
		return
	}
	in := cur
	if !upgrade {
		in = MarketInstall{ID: pkg.ID, Tier: tier, Skills: map[string]string{}, Agents: map[string]string{}}
	}
	p := principalFrom(ctx)
	now := time.Now().UTC()
	plugins, status, msg := s.approvePlugins(ctx, pkg.MarketPackage, in.Plugins, req.Grants, p.Name, now)
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	in.Plugins = plugins
	note := fmt.Sprintf("Marketplace: %s v%d sha256:%s", pkg.ID, pkg.Version, pkg.SHA256[:12])
	for _, sk := range pkg.Skills {
		if err := s.installSkill(ctx, &in, sk, note, p.Name, now); err != nil {
			s.internalError(w, "installing "+sk.Name, err)
			return
		}
	}
	for _, a := range pkg.Agents {
		if err := s.installAgent(ctx, &in, a, note, p.Name, now); err != nil {
			s.internalError(w, "installing "+a.Name, err)
			return
		}
	}
	if in.Workflows == nil {
		in.Workflows = map[string]string{}
	}
	for _, wf := range pkg.Workflows {
		if err := s.installWorkflow(ctx, &in, wf, note, p.Name, now); err != nil {
			s.internalError(w, "installing "+wf.Name, err)
			return
		}
	}
	if err := s.installEnvTemplates(ctx, &in, pkg.MarketPackage); err != nil {
		s.internalError(w, "installing the environment templates", err)
		return
	}
	in.Version, in.Hash, in.By, in.At = pkg.Version, pkg.SHA256, p.Name, now
	in.Source, in.Signer = pkg.Source, pkg.Signer
	if err := s.docs.Put(ctx, kindMarketInstall, in.ID, in); err != nil {
		s.internalError(w, "recording the install", err)
		return
	}
	verb := "market.install"
	if upgrade {
		verb = "market.upgrade"
	}
	s.auditReq(r, verb, "market/"+pkg.ID, fmt.Sprintf("v%d sha256:%s tier %s", pkg.Version, pkg.SHA256[:12], in.Tier), true)
	writeJSON(w, http.StatusCreated, in)
}

// installSkill creates the skill, or adds a version to the one an earlier install made.
func sourceLabel(source, signer string) string {
	if signer != "" {
		return source + ", signed by " + signer
	}
	return source
}

func (s *Server) installSkill(ctx context.Context, in *MarketInstall, m MarketSkill, note, by string, now time.Time) error {
	if id, ok := in.Skills[m.Key]; ok {
		sk, err := s.loadSkill(ctx, id)
		if errors.Is(err, docstore.ErrNotFound) {
			delete(in.Skills, m.Key) // removed by hand since; make it again below
		} else if err != nil {
			return err
		} else {
			last := sk.Versions[len(sk.Versions)-1]
			if last.Body == m.Body && sk.Name == m.Name && sk.Description == m.Description {
				return nil
			}
			if len(sk.Versions) >= maxVersions {
				return fmt.Errorf("skill %q already has %d versions", sk.Name, maxVersions)
			}
			sv := SkillVersion{N: last.N + 1, Body: m.Body, Note: note, By: by, At: now}
			sv.Hash = sv.computeHash(sk.ID)
			sk.Name, sk.Description = m.Name, m.Description
			sk.Versions, sk.Current = append(sk.Versions, sv), sv.N
			return s.docs.Put(ctx, kindAISkill, sk.ID, sk)
		}
	}
	id, err := s.newDefID(ctx, kindAISkill, m.Name)
	if err != nil {
		return err
	}
	sv := SkillVersion{N: 1, Body: m.Body, Note: note, By: by, At: now}
	sv.Hash = sv.computeHash(id)
	if err := s.docs.Put(ctx, kindAISkill, id, AISkill{ID: id, Name: m.Name, Description: m.Description, Current: 1, Versions: []SkillVersion{sv}}); err != nil {
		return err
	}
	in.Skills[m.Key] = id
	return nil
}

// installAgent creates the agent, or adds a version. The agent never gets more than the install's
// tier: a tool that needs T1 is left out at T0, and the agent's own tier is capped.
func (s *Server) installAgent(ctx context.Context, in *MarketInstall, m MarketAgent, note, by string, now time.Time) error {
	v := AgentVersion{Instructions: m.Instructions, Tier: m.Tier, Note: note, By: by, At: now}
	if tierRank[v.Tier] > tierRank[in.Tier] {
		v.Tier = in.Tier
	}
	for _, t := range m.Tools {
		if isProposalTool(t) && v.Tier != tierReview {
			continue
		}
		v.Tools = append(v.Tools, t)
	}
	sort.Strings(v.Tools)
	if v.Tools == nil {
		v.Tools = []string{}
	}
	v.Skills = []SkillPin{}
	for _, k := range m.Skills {
		id, ok := in.Skills[k]
		if !ok {
			return fmt.Errorf("skill %q is missing", k)
		}
		sk, err := s.loadSkill(ctx, id)
		if err != nil {
			return err
		}
		v.Skills = append(v.Skills, SkillPin{ID: id, N: sk.Current})
	}
	if id, ok := in.Agents[m.Key]; ok {
		a, err := s.loadAgent(ctx, id)
		if errors.Is(err, docstore.ErrNotFound) {
			delete(in.Agents, m.Key)
		} else if err != nil {
			return err
		} else {
			last := a.Versions[len(a.Versions)-1]
			if sameVersionContent(last, v) && a.Name == m.Name && a.Description == m.Description {
				return nil
			}
			if len(a.Versions) >= maxVersions {
				return fmt.Errorf("agent %q already has %d versions", a.Name, maxVersions)
			}
			v.N = last.N + 1
			v.Hash = v.computeHash(a.ID)
			a.Name, a.Description = m.Name, m.Description
			a.Versions, a.Current = append(a.Versions, v), v.N
			return s.docs.Put(ctx, kindAIAgent, a.ID, a)
		}
	}
	id, err := s.newDefID(ctx, kindAIAgent, m.Name)
	if err != nil {
		return err
	}
	v.N = 1
	v.Hash = v.computeHash(id)
	if err := s.docs.Put(ctx, kindAIAgent, id, AIAgent{ID: id, Name: m.Name, Description: m.Description, Current: 1, Versions: []AgentVersion{v}}); err != nil {
		return err
	}
	in.Agents[m.Key] = id
	return nil
}

// installWorkflow creates the workflow from the package form: agent keys become the ids of the agents
// this install made, it never has a schedule, and at tier T0 it runs as a viewer at most.
func (s *Server) installWorkflow(ctx context.Context, in *MarketInstall, m MarketWorkflow, note, by string, now time.Time) error {
	stages := make([]WFStage, 0, len(m.Stages))
	for _, st := range m.Stages {
		steps := make([]WFStep, 0, len(st.Steps))
		for _, sp := range st.Steps {
			id, ok := in.Agents[sp.Agent]
			if !ok {
				return fmt.Errorf("agent %q is missing", sp.Agent)
			}
			steps = append(steps, WFStep{Agent: id, Task: sp.Task})
		}
		stages = append(stages, WFStage{Name: st.Name, Gate: st.Gate, Steps: steps})
	}
	runAs := m.RunAs
	if in.Tier != tierReview {
		runAs = "viewer"
	}
	v, msg := s.resolveWorkflow(ctx, wfRequest{Name: m.Name, Description: m.Description, Stages: stages, RunAs: runAs, Note: note})
	if msg != "" {
		return errors.New(msg)
	}
	v.By, v.At = by, now
	if id, ok := in.Workflows[m.Key]; ok {
		wf, err := s.loadWorkflow(ctx, id)
		if errors.Is(err, docstore.ErrNotFound) {
			delete(in.Workflows, m.Key)
		} else if err != nil {
			return err
		} else {
			last := wf.Versions[len(wf.Versions)-1]
			v.N = last.N + 1
			v.Hash = v.computeHash(wf.ID)
			if fmt.Sprint(wfContent(last)) == fmt.Sprint(wfContent(v)) && wf.Name == m.Name && wf.Description == m.Description {
				return nil
			}
			if len(wf.Versions) >= maxVersions {
				return fmt.Errorf("workflow %q already has %d versions", wf.Name, maxVersions)
			}
			wf.Name, wf.Description, wf.Disabled = m.Name, m.Description, false
			wf.Versions, wf.Current = append(wf.Versions, v), v.N
			return s.docs.Put(ctx, kindWorkflow, wf.ID, wf)
		}
	}
	id, err := s.newDefID(ctx, kindWorkflow, m.Name)
	if err != nil {
		return err
	}
	v.N = 1
	v.Hash = v.computeHash(id)
	if err := s.docs.Put(ctx, kindWorkflow, id, Workflow{ID: id, Name: m.Name, Description: m.Description, Current: 1, Versions: []WFVersion{v}}); err != nil {
		return err
	}
	in.Workflows[m.Key] = id
	return nil
}

// installEnvTemplates makes the package's templates available to environment requests, replacing the
// earlier version's. Removed ones go away.
func (s *Server) installEnvTemplates(ctx context.Context, in *MarketInstall, pkg MarketPackage) error {
	keep := map[string]bool{}
	for _, t := range pkg.EnvTemplates {
		id := tplID(pkg.ID, t.Key)
		keep[id] = true
		if err := s.docs.Put(ctx, kindEnvTemplate, id, pkgTemplate{ID: id, Package: pkg.ID, Version: pkg.Version, MarketEnvTemplate: t}); err != nil {
			return err
		}
	}
	for _, id := range in.Templates {
		if !keep[id] {
			if err := s.docs.Delete(ctx, kindEnvTemplate, id); err != nil && !errors.Is(err, docstore.ErrNotFound) {
				return err
			}
		}
	}
	in.Templates = nil
	for id := range keep {
		in.Templates = append(in.Templates, id)
	}
	sort.Strings(in.Templates)
	return nil
}

// handleMarketUninstall forgets the install and disables the agents it made. The skills and agents
// stay, with their history, because people may have used them in workflows; they are the
// administrator's to delete.
func (s *Server) handleMarketUninstall(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, err := s.loadInstall(ctx, r.PathValue("id"))
	if err != nil {
		s.defError(w, "install", err)
		return
	}
	for _, id := range in.Agents {
		a, err := s.loadAgent(ctx, id)
		if errors.Is(err, docstore.ErrNotFound) {
			continue
		} else if err != nil {
			s.internalError(w, "disabling an agent", err)
			return
		}
		a.Disabled = true
		if err := s.docs.Put(ctx, kindAIAgent, a.ID, a); err != nil {
			s.internalError(w, "disabling an agent", err)
			return
		}
	}
	for _, id := range in.Workflows {
		wf, err := s.loadWorkflow(ctx, id)
		if errors.Is(err, docstore.ErrNotFound) {
			continue
		} else if err != nil {
			s.internalError(w, "turning off a workflow", err)
			return
		}
		wf.Disabled = true
		if err := s.docs.Put(ctx, kindWorkflow, wf.ID, wf); err != nil {
			s.internalError(w, "turning off a workflow", err)
			return
		}
	}
	for _, id := range in.Templates {
		if err := s.docs.Delete(ctx, kindEnvTemplate, id); err != nil && !errors.Is(err, docstore.ErrNotFound) {
			s.internalError(w, "removing a template", err)
			return
		}
	}
	if err := s.docs.Delete(ctx, kindMarketInstall, in.ID); err != nil {
		s.internalError(w, "removing the install", err)
		return
	}
	s.auditReq(r, "market.uninstall", "market/"+in.ID, fmt.Sprintf("v%d", in.Version), true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
