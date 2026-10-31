package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The assistant, agents and workflows can ask for a temporary environment, never create one. A
// request is the same environment and cloud change a person's request makes, owned by the person the
// assistant is working for, with two differences: it never starts under the self-service policy (no
// person has looked at it yet), and a scheduled workflow, which has no person, cannot make one.

const requestEnvTool = "request_environment"

func (s *Server) lookupEnvTemplates(ctx context.Context, _ json.RawMessage) (string, error) {
	pol := s.loadEnvPolicy(ctx)
	var ids []string
	for id := range envSpecs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	fmt.Fprintf(&b, "Environments live between 1 and %d hours; each person may have %d at once. A request from the assistant is always approved by a person.\n", pol.MaxLifetimeHours, pol.MaxActivePerUser)
	for _, id := range ids {
		tpl, ok := findTemplate(id)
		if !ok {
			continue
		}
		spec := envSpecs[id]
		defaults, _ := checkParams(tpl, map[string]string{"VpcId": "vpc-00000000", "SubnetId": "subnet-00000000", "Owner": "x", "ExpiresAt": "2026-01-01T00:00:00Z"})
		fmt.Fprintf(&b, "- %s: %s. %s About $%.2f a month while it lives; usual lifetime %d hours.", id, spec.Kind, tpl.Description, spec.Monthly(defaults), spec.DefaultTTL)
		var opts []string
		for _, p := range tpl.Parameters {
			if contains(spec.UserParams, p.Name) {
				o := p.Name
				if len(p.Options) > 0 {
					o += " (one of " + strings.Join(p.Options, ", ") + ")"
				}
				if p.Default != "" {
					o += " default " + p.Default
				}
				opts = append(opts, o)
			}
		}
		if len(opts) > 0 {
			fmt.Fprintf(&b, " Settable: %s.", strings.Join(opts, "; "))
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func (s *Server) lookupRequestEnv(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Template string            `json:"template"`
		Name     string            `json:"name"`
		Purpose  string            `json:"purpose"`
		TTLHours int               `json:"ttl_hours"`
		Params   map[string]string `json:"params"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	p := principalFrom(ctx)
	if strings.HasPrefix(p.Name, "workflow:") || strings.HasPrefix(p.Name, "policy:") {
		return "", errors.New("a scheduled workflow has no person to own an environment; ask for it in the console")
	}
	spec, ok := envSpecs[a.Template]
	if !ok {
		return "", errors.New("choose one of the templates environment_templates lists")
	}
	if a.TTLHours == 0 {
		a.TTLHours = spec.DefaultTTL
	}
	e, herr := s.requestEnv(ctx, p, "", envRequest{Template: a.Template, Name: a.Name, Purpose: a.Purpose, TTLHours: a.TTLHours, Params: a.Params}, true)
	if herr != nil {
		return "", errors.New(herr.msg)
	}
	if sink, ok := ctx.Value(lookupSinkKey{}).(*lookupSink); ok {
		sink.environments = append(sink.environments, e.ID)
	}
	return fmt.Sprintf("Environment %q (id %s) requested: %s for %d hours, about $%.2f a month while it lives. Nothing exists yet. A different administrator must approve the request in the console under Environments (or Infrastructure → Cloud changes); tell the person that.", e.Name, e.ID, spec.Kind, e.TTLHours, e.MonthlyUSD), nil
}
