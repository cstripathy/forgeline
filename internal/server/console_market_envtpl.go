package server

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Environment templates that arrive in marketplace packages (note 019). They join the catalog under
// the id pkg.<package>.<key>, can never be on the self-service list, and carry no capabilities.

const (
	kindEnvTemplate = "envtemplate"
	maxTemplateBody = 50 << 10
	maxTplParams    = 12
	tplIDPrefix     = "pkg."
)

// MarketParam is one input of a packaged environment template.
type MarketParam struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Help     string   `json:"help,omitempty"`
	Required bool     `json:"required,omitempty"`
	Default  string   `json:"default,omitempty"`
	Options  []string `json:"options,omitempty"`
	Pattern  string   `json:"pattern,omitempty"` // RE2, matched against the whole value
}

// MarketEnvTemplate is an environment template inside a package.
type MarketEnvTemplate struct {
	Key             string        `json:"key"`
	Title           string        `json:"title"`
	Description     string        `json:"description"`
	DefaultStack    string        `json:"default_stack"`
	DefaultTTLHours int           `json:"default_ttl_hours"`
	MonthlyUSD      float64       `json:"monthly_usd"` // the publisher's estimate
	Parameters      []MarketParam `json:"parameters"`
	Body            string        `json:"body"` // CloudFormation, YAML or JSON
}

// pkgTemplate is an installed packaged template.
type pkgTemplate struct {
	ID      string `json:"id"`
	Package string `json:"package"`
	Version int    `json:"version"`
	MarketEnvTemplate
}

var (
	tplKeyRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	tplParamRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,40}$`)
	tplForbidRE = regexp.MustCompile(`(?i)AWS::IAM::|AWS::CloudFormation::(?:Stack|CustomResource|Macro|WaitCondition)|\bCustom::|Transform\b|AWS::Serverless`)
)

func tplID(pkg, key string) string { return tplIDPrefix + pkg + "." + key }

// check validates a packaged template the way the built-in ones were written: bounded, no way to
// create identities or run code, and the parameters the environment engine sets.
func (t MarketEnvTemplate) check() error {
	if !tplKeyRE.MatchString(t.Key) {
		return fmt.Errorf("template key %q is not a lowercase slug", t.Key)
	}
	if m := checkNameDesc(t.Title, t.Description); m != "" {
		return fmt.Errorf("template %q: %s", t.Key, m)
	}
	switch {
	case !stackNameRE.MatchString(t.DefaultStack) || len(t.DefaultStack) > 40:
		return fmt.Errorf("template %q: the default stack name is not valid", t.Key)
	case t.DefaultTTLHours < 1 || t.DefaultTTLHours > 168:
		return fmt.Errorf("template %q: the default lifetime is 1 to 168 hours", t.Key)
	case t.MonthlyUSD < 0 || t.MonthlyUSD > 500:
		return fmt.Errorf("template %q: the cost estimate is $0 to $500 a month", t.Key)
	case strings.TrimSpace(t.Body) == "" || len(t.Body) > maxTemplateBody:
		return fmt.Errorf("template %q: the body is empty or over %d KB", t.Key, maxTemplateBody>>10)
	case tplForbidRE.MatchString(t.Body):
		return fmt.Errorf("template %q: a packaged template may not create IAM resources, nested stacks, custom resources or use macros or Transform", t.Key)
	case len(t.Parameters) > maxTplParams:
		return fmt.Errorf("template %q: at most %d parameters", t.Key, maxTplParams)
	}
	seen := map[string]MarketParam{}
	for _, p := range t.Parameters {
		if !tplParamRE.MatchString(p.Name) || p.Label == "" || len(p.Label) > 60 || len(p.Help) > 200 || seen[p.Name].Name != "" {
			return fmt.Errorf("template %q: parameter %q is not valid or repeated", t.Key, p.Name)
		}
		seen[p.Name] = p
		if len(p.Options) > 20 || len(p.Pattern) > 200 || len(p.Default) > 200 {
			return fmt.Errorf("template %q: parameter %q is too large", t.Key, p.Name)
		}
		if _, err := p.compile(); err != nil {
			return fmt.Errorf("template %q: parameter %q: %w", t.Key, p.Name, err)
		}
	}
	for _, need := range []string{"Owner", "ExpiresAt"} {
		if !seen[need].Required {
			return fmt.Errorf("template %q must declare a required parameter %s, which the server fills in", t.Key, need)
		}
	}
	// A default must satisfy its own rules.
	for _, p := range t.Parameters {
		cp, _ := p.compile()
		if p.Default != "" {
			if _, err := checkParams(CatalogTemplate{Parameters: []CatalogParam{cp}}, map[string]string{p.Name: p.Default}); err != nil {
				return fmt.Errorf("template %q: the default of %s does not pass its own rules", t.Key, p.Name)
			}
		}
	}
	return nil
}

func (p MarketParam) compile() (CatalogParam, error) {
	cp := CatalogParam{Name: p.Name, Label: p.Label, Help: p.Help, Required: p.Required, Default: p.Default, Options: p.Options}
	if p.Pattern != "" {
		re, err := regexp.Compile("^(?:" + p.Pattern + ")$")
		if err != nil {
			return cp, fmt.Errorf("the pattern is not valid: %w", err)
		}
		cp.pattern = re
	}
	return cp, nil
}

// catalogTemplate turns an installed packaged template into what the catalog deploys.
func (t pkgTemplate) catalogTemplate() CatalogTemplate {
	ct := CatalogTemplate{ID: t.ID, Title: t.Title, Description: t.Description, DefaultStack: t.DefaultStack, body: t.Body}
	for _, p := range t.Parameters {
		cp, _ := p.compile()
		ct.Parameters = append(ct.Parameters, cp)
	}
	return ct
}

// spec says what the template is as an environment. A packaged one asks for the default network
// when it has a VPC and a subnet parameter, and lets people set everything the server does not.
func (t pkgTemplate) spec() envSpec {
	names := map[string]bool{}
	for _, p := range t.Parameters {
		names[p.Name] = true
	}
	auto := names["VpcId"] && names["SubnetId"]
	var user []string
	for _, p := range t.Parameters {
		switch p.Name {
		case "Owner", "ExpiresAt":
		case "VpcId", "SubnetId":
			if !auto {
				user = append(user, p.Name)
			}
		default:
			user = append(user, p.Name)
		}
	}
	monthly := t.MonthlyUSD
	return envSpec{Kind: "Packaged template", DefaultTTL: t.DefaultTTLHours, AutoNet: auto, UserParams: user,
		Monthly: func(map[string]string) float64 { return monthly }}
}

func (s *Server) pkgTemplates(ctx context.Context) []pkgTemplate {
	docs, err := s.docs.List(ctx, kindEnvTemplate, docstore.ListOptions{})
	if err != nil {
		return nil
	}
	out, _ := docstore.Decode[pkgTemplate](docs)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// allTemplates is the built-in catalog followed by installed packaged templates.
func (s *Server) allTemplates(ctx context.Context) []CatalogTemplate {
	out := append([]CatalogTemplate{}, catalog...)
	for _, t := range s.pkgTemplates(ctx) {
		out = append(out, t.catalogTemplate())
	}
	return out
}

func (s *Server) findTemplate(ctx context.Context, id string) (CatalogTemplate, bool) {
	if t, ok := findTemplate(id); ok {
		return t, true
	}
	var t pkgTemplate
	if strings.HasPrefix(id, tplIDPrefix) && s.docs.Get(ctx, kindEnvTemplate, id, &t) == nil {
		return t.catalogTemplate(), true
	}
	return CatalogTemplate{}, false
}

// envSpecFor finds the environment spec of a built-in or packaged template.
func (s *Server) envSpecFor(ctx context.Context, id string) (envSpec, bool) {
	if sp, ok := envSpecs[id]; ok {
		return sp, true
	}
	var t pkgTemplate
	if strings.HasPrefix(id, tplIDPrefix) && s.docs.Get(ctx, kindEnvTemplate, id, &t) == nil {
		return t.spec(), true
	}
	return envSpec{}, false
}
