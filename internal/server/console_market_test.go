package server

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

// publish swaps the catalogue for the given packages (hash filled in) for one test.
func publish(t *testing.T, pkgs ...MarketPackage) {
	t.Helper()
	fsys := fstest.MapFS{}
	for _, p := range pkgs {
		p.SHA256 = p.contentHash()
		b, _ := json.Marshal(p)
		fsys["market/"+p.ID+".json"] = &fstest.MapFile{Data: b}
	}
	old := marketFiles
	marketFiles = fsys
	t.Cleanup(func() { marketFiles = old })
}

func samplePkg(version int, body string) MarketPackage {
	return MarketPackage{
		ID: "sample", Version: version, Title: "Sample", Description: "d", Publisher: "Forgeline",
		Skills: []MarketSkill{{Key: "s", Name: "Sample skill", Description: "d", Body: body}},
		Agents: []MarketAgent{{Key: "a", Name: "Sample agent", Description: "d", Instructions: "Help.", Skills: []string{"s"},
			Tools: []string{"aws_summary", "propose_aws_change"}, Tier: "T1"}},
	}
}

func listMarket(t *testing.T, c *browser) []marketEntry {
	t.Helper()
	var out struct{ Packages []marketEntry }
	json.Unmarshal(c.expect(200, "GET", "/v1/market/packages", nil), &out)
	return out.Packages
}

func TestShippedCatalogueIsValidAndHashed(t *testing.T) {
	cat := marketCatalog()
	if len(cat) < 4 {
		t.Fatalf("only %d packages shipped", len(cat))
	}
	for _, e := range cat {
		if e.Problem != "" {
			t.Errorf("%s: %s (after an edit run FORGELINE_UPDATE_MARKET=1 go test -run RegenerateMarketHashes)", e.ID, e.Problem)
		}
	}
}

func TestPackageChecks(t *testing.T) {
	bad := map[string]func(*MarketPackage){
		"id":                 func(p *MarketPackage) { p.ID = "Bad Id" },
		"version":            func(p *MarketPackage) { p.Version = 0 },
		"empty":              func(p *MarketPackage) { p.Skills, p.Agents = nil, nil },
		"unknown tool":       func(p *MarketPackage) { p.Agents[0].Tools = []string{"rm_rf"} },
		"agent calls agents": func(p *MarketPackage) { p.Agents[0].Tools = []string{"ask_agent"} },
		"missing skill":      func(p *MarketPackage) { p.Agents[0].Skills = []string{"nope"} },
		"propose at T0":      func(p *MarketPackage) { p.Agents[0].Tier = "T0" },
		"empty body":         func(p *MarketPackage) { p.Skills[0].Body = " " },
	}
	for name, mut := range bad {
		p := samplePkg(1, "Steps.")
		mut(&p)
		p.SHA256 = p.contentHash()
		if p.check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p := samplePkg(1, "Steps.")
	p.SHA256 = p.contentHash()
	if err := p.check(); err != nil {
		t.Fatalf("good package refused: %v", err)
	}
	p.Skills[0].Body = "Changed after hashing."
	if p.check() == nil || !strings.Contains(p.check().Error(), "sha256") {
		t.Error("an edited package must fail its hash")
	}
}

func TestInstallCreatesSkillsAndAgentsAndIsBoundToTheHash(t *testing.T) {
	m := newMCPHarness(t)
	publish(t, samplePkg(1, "Steps."))
	e := listMarket(t, m.admin)[0]
	// Developers can browse; only administrators install.
	listMarket(t, m.dev)
	m.dev.expect(403, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e.SHA256})
	// A stale or wrong hash is refused: what is installed is what was shown.
	m.admin.expect(409, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": strings.Repeat("0", 64)})
	m.admin.expect(404, "POST", "/v1/market/install", map[string]any{"id": "nope", "hash": e.SHA256})

	// Default tier is T0: the agent is capped and loses the proposal tool.
	var in MarketInstall
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e.SHA256}), &in)
	if in.Tier != "T0" || in.Version != 1 {
		t.Fatalf("install = %+v", in)
	}
	var a AIAgent
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/agents/"+in.Agents["a"], nil), &a)
	v := a.Versions[0]
	if v.Tier != "T0" || strings.Join(v.Tools, ",") != "aws_summary" || len(v.Skills) != 1 || v.Skills[0].ID != in.Skills["s"] || !strings.Contains(v.Note, "sha256:"+e.SHA256[:12]) {
		t.Errorf("agent version = %+v", v)
	}
	m.admin.expect(409, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e.SHA256}) // already at v1
	if got := listMarket(t, m.admin)[0]; got.Installed == nil || got.Update {
		t.Errorf("listing = %+v", got)
	}
}

func TestInstallAtT1KeepsTheProposalToolAndRefusesUnknownTiers(t *testing.T) {
	m := newMCPHarness(t)
	publish(t, samplePkg(1, "Steps."))
	e := listMarket(t, m.admin)[0]
	m.admin.expect(400, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e.SHA256, "tier": "T9"})
	var in MarketInstall
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e.SHA256, "tier": "T1"}), &in)
	var a AIAgent
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/agents/"+in.Agents["a"], nil), &a)
	if a.Versions[0].Tier != "T1" || len(a.Versions[0].Tools) != 2 {
		t.Fatalf("T1 install = %+v", a.Versions[0])
	}
}

func TestUpgradeAddsVersionsKeepsTierAndSkipsUnchanged(t *testing.T) {
	m := newMCPHarness(t)
	publish(t, samplePkg(1, "Steps."))
	e1 := listMarket(t, m.admin)[0]
	var in MarketInstall
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e1.SHA256}), &in) // T0

	// v2 changes the skill body and asks for T1 in the agent; the installed tier stays T0.
	publish(t, samplePkg(2, "Steps, improved."))
	e2 := listMarket(t, m.admin)[0]
	if !e2.Update || e2.Installed.Version != 1 {
		t.Fatalf("listing = %+v", e2)
	}
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e2.SHA256, "tier": "T1"}), &in)
	if in.Tier != "T0" || in.Version != 2 {
		t.Errorf("upgrade changed the tier: %+v", in)
	}
	var sk AISkill
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/skills/"+in.Skills["s"], nil), &sk)
	if sk.Current != 2 || sk.Versions[0].Body != "Steps." || sk.Versions[1].Body != "Steps, improved." {
		t.Errorf("skill = %+v", sk)
	}
	var a AIAgent
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/agents/"+in.Agents["a"], nil), &a)
	if a.Current != 2 || a.Versions[1].Tier != "T0" || a.Versions[1].Skills[0].N != 2 {
		t.Errorf("agent = %+v", a.Versions)
	}
	// Downgrading to an older package is refused.
	publish(t, samplePkg(1, "Steps."))
	e0 := listMarket(t, m.admin)[0]
	m.admin.expect(409, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e0.SHA256})

	// A newer package whose content is the same adds no versions.
	p3 := samplePkg(3, "Steps, improved.")
	publish(t, p3)
	e3 := listMarket(t, m.admin)[0]
	m.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e3.SHA256})
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/skills/"+in.Skills["s"], nil), &sk)
	if len(sk.Versions) != 2 {
		t.Errorf("unchanged skill got %d versions", len(sk.Versions))
	}
}

func TestUninstallDisablesAgentsAndKeepsTheirHistory(t *testing.T) {
	m := newMCPHarness(t)
	publish(t, samplePkg(1, "Steps."))
	e := listMarket(t, m.admin)[0]
	var in MarketInstall
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e.SHA256}), &in)
	m.dev.expect(403, "DELETE", "/v1/market/install/sample", nil)
	m.admin.expect(200, "DELETE", "/v1/market/install/sample", nil)
	m.admin.expect(404, "DELETE", "/v1/market/install/sample", nil)
	var a AIAgent
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/agents/"+in.Agents["a"], nil), &a)
	if !a.Disabled || len(a.Versions) != 1 {
		t.Errorf("agent after uninstall = %+v", a)
	}
	m.admin.expect(200, "GET", "/v1/ai/skills/"+in.Skills["s"], nil)
	if listMarket(t, m.admin)[0].Installed != nil {
		t.Error("still listed as installed")
	}
}

func TestDamagedPackageIsListedButCannotBeInstalled(t *testing.T) {
	m := newMCPHarness(t)
	p := samplePkg(1, "Steps.")
	publish(t, p)
	p.SHA256 = "deadbeef"
	b, _ := json.Marshal(p)
	marketFiles = overlay(marketFiles, "market/sample.json", b)
	e := listMarket(t, m.admin)[0]
	if e.Problem == "" {
		t.Fatal("damage not reported")
	}
	m.admin.expect(409, "POST", "/v1/market/install", map[string]any{"id": "sample", "hash": e.SHA256})
}

func overlay(base interface{}, name string, data []byte) fstest.MapFS {
	return fstest.MapFS{name: &fstest.MapFile{Data: data}}
}
