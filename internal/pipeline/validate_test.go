package pipeline

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func validPipeline() Pipeline {
	return Pipeline{
		Version: VersionV1Alpha1,
		Name:    "demo",
		Steps: []Step{
			{ID: "build", Image: "golang:1.27", Run: "go build ./..."},
			{ID: "test", Image: "golang:1.27", Command: []string{"go", "test", "./..."}, Needs: []string{"build"}},
		},
	}
}

func TestValidateAcceptsValidPipeline(t *testing.T) {
	p := validPipeline()
	p.Steps[1].Env = map[string]string{"CGO_ENABLED": "0", "_X": "1"}
	p.Steps[1].WorkDir = "/src"
	p.Steps[1].Timeout = "10m"
	p.Steps[1].Permissions.Network = NetworkNone
	p.Steps[1].Permissions.Secrets = []string{"NPM_TOKEN", "_deploy_key"}
	p.Steps[1].Inputs = []string{"go.mod", "**/*.go", "docs", "src/[ab]?.txt"}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateAllowsForwardNeeds(t *testing.T) {
	p := validPipeline()
	// Declare the dependent step first; needs may point forward.
	p.Steps[0], p.Steps[1] = p.Steps[1], p.Steps[0]
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(p *Pipeline)
		wantPath string
		wantMsg  string
	}{
		{"missing version", func(p *Pipeline) { p.Version = "" }, "version", "must be"},
		{"wrong version", func(p *Pipeline) { p.Version = "v2" }, "version", "must be"},
		{"empty name", func(p *Pipeline) { p.Name = "" }, "name", "must match"},
		{"uppercase name", func(p *Pipeline) { p.Name = "Demo" }, "name", "must match"},
		{"no steps", func(p *Pipeline) { p.Steps = nil }, "steps", "at least one"},
		{"bad id", func(p *Pipeline) { p.Steps[0].ID = "Build_1" }, "steps[0].id", "must match"},
		{"duplicate id", func(p *Pipeline) { p.Steps[1].ID = "build"; p.Steps[1].Needs = nil }, "steps[1].id", "duplicate step id"},
		{"missing image", func(p *Pipeline) { p.Steps[0].Image = "" }, "steps[0].image", "required"},
		{"image with space", func(p *Pipeline) { p.Steps[0].Image = "golang 1.24" }, "steps[0].image", "whitespace"},
		{"run and command", func(p *Pipeline) { p.Steps[0].Command = []string{"x"} }, "steps[0]", "not several"},
		{"neither run nor command", func(p *Pipeline) { p.Steps[0].Run = "" }, "steps[0]", "set exactly one"},
		{"blank run", func(p *Pipeline) { p.Steps[0].Run = "   " }, "steps[0]", "set exactly one"},
		{"empty program", func(p *Pipeline) { p.Steps[1].Command = []string{" "} }, "steps[1].command[0]", "must not be empty"},
		{"unknown need", func(p *Pipeline) { p.Steps[1].Needs = []string{"nope"} }, "steps[1].needs[0]", "unknown step"},
		{"self need", func(p *Pipeline) { p.Steps[1].Needs = []string{"test"} }, "steps[1].needs[0]", "cannot need itself"},
		{"duplicate need", func(p *Pipeline) { p.Steps[1].Needs = []string{"build", "build"} }, "steps[1].needs[1]", "duplicate dependency"},
		{"bad env name", func(p *Pipeline) { p.Steps[0].Env = map[string]string{"1BAD": "x"} }, "steps[0].env", "invalid variable name"},
		{"relative workdir", func(p *Pipeline) { p.Steps[0].WorkDir = "src" }, "steps[0].workdir", "absolute"},
		{"bad timeout", func(p *Pipeline) { p.Steps[0].Timeout = "soon" }, "steps[0].timeout", "not a valid duration"},
		{"unknown network", func(p *Pipeline) { p.Steps[0].Permissions.Network = "host" }, "steps[0].permissions.network", `must be "none" or "egress"`},
		{"network is case sensitive", func(p *Pipeline) { p.Steps[0].Permissions.Network = "None" }, "steps[0].permissions.network", "must be"},
		{"bad secret name", func(p *Pipeline) { p.Steps[0].Permissions.Secrets = []string{"1BAD"} }, "steps[0].permissions.secrets[0]", "invalid secret name"},
		{"secret with equals sign", func(p *Pipeline) { p.Steps[0].Permissions.Secrets = []string{"A=B"} }, "steps[0].permissions.secrets[0]", "invalid secret name"},
		{"empty secret name", func(p *Pipeline) { p.Steps[0].Permissions.Secrets = []string{""} }, "steps[0].permissions.secrets[0]", "invalid secret name"},
		{"duplicate secret", func(p *Pipeline) { p.Steps[0].Permissions.Secrets = []string{"TOKEN", "TOKEN"} }, "steps[0].permissions.secrets[1]", "duplicate secret"},
		{"secret also in env", func(p *Pipeline) {
			p.Steps[0].Env = map[string]string{"TOKEN": "x"}
			p.Steps[0].Permissions.Secrets = []string{"TOKEN"}
		}, "steps[0].permissions.secrets[0]", "also set in env"},
		{"zero timeout", func(p *Pipeline) { p.Steps[0].Timeout = "0s" }, "steps[0].timeout", "must be positive"},
		{"negative timeout", func(p *Pipeline) { p.Steps[0].Timeout = "-5m" }, "steps[0].timeout", "must be positive"},
		{"empty input", func(p *Pipeline) { p.Steps[0].Inputs = []string{""} }, "steps[0].inputs[0]", "must not be empty"},
		{"absolute input", func(p *Pipeline) { p.Steps[0].Inputs = []string{"/etc/passwd"} }, "steps[0].inputs[0]", "relative"},
		{"parent input", func(p *Pipeline) { p.Steps[0].Inputs = []string{"ok", "../secret"} }, "steps[0].inputs[1]", ".."},
		{"backslash input", func(p *Pipeline) { p.Steps[0].Inputs = []string{`a\b`} }, "steps[0].inputs[0]", "backslash"},
		{"agent and run", func(p *Pipeline) { p.Steps[0].Agent = agentStep() }, "steps[0]", "not several"},
		{"agent unknown provider", func(p *Pipeline) { setAgent(p, func(a *Agent) { a.Provider = "gpt" }) }, "steps[0].agent.provider", "unknown provider"},
		{"agent no prompt", func(p *Pipeline) { setAgent(p, func(a *Agent) { a.Prompt = "  " }) }, "steps[0].agent.prompt", "required"},
		{"agent too many turns", func(p *Pipeline) { setAgent(p, func(a *Agent) { a.MaxTurns = 101 }) }, "steps[0].agent.maxTurns", "between 1 and 100"},
		{"agent tool with comma", func(p *Pipeline) { setAgent(p, func(a *Agent) { a.Tools = []string{"Read,Bash"} }) }, "steps[0].agent.tools[0]", "without commas"},
		{"agent without network", func(p *Pipeline) {
			setAgent(p, func(a *Agent) {})
			p.Steps[0].Permissions.Network = NetworkNone
		}, "steps[0].permissions.network", "needs network"},
		{"agent without credential", func(p *Pipeline) {
			setAgent(p, func(a *Agent) {})
			p.Steps[0].Permissions.Secrets = []string{"OTHER"}
		}, "steps[0].permissions.secrets", "must request one of"},
		{"bad bracket input", func(p *Pipeline) { p.Steps[0].Inputs = []string{"src/[a-"} }, "steps[0].inputs[0]", "not a valid glob"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPipeline()
			tt.mutate(&p)
			err := p.Validate()
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate() = %v, want *ValidationError", err)
			}
			for _, fe := range ve.Errors {
				if fe.Path == tt.wantPath && strings.Contains(fe.Msg, tt.wantMsg) {
					return
				}
			}
			t.Fatalf("no error at %q containing %q; got:\n%v", tt.wantPath, tt.wantMsg, err)
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	p := Pipeline{
		Version: "nope",
		Name:    "",
		Steps: []Step{
			{ID: "a", Image: "", Run: "", Needs: []string{"ghost"}},
		},
	}
	err := p.Validate()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Validate() = %v, want *ValidationError", err)
	}
	// version, name, image, run/command, unknown need.
	if got, want := len(ve.Errors), 5; got != want {
		t.Fatalf("got %d problems, want %d:\n%v", got, want, err)
	}
	if !strings.Contains(err.Error(), "5 problem(s)") {
		t.Errorf("error text does not state the problem count: %q", err.Error())
	}
}

func TestValidateErrorOrderIsStable(t *testing.T) {
	p := validPipeline()
	p.Steps[0].Env = map[string]string{"1A": "x", "2B": "x", "3C": "x"}
	first := p.Validate().Error()
	for i := 0; i < 20; i++ {
		if got := p.Validate().Error(); got != first {
			t.Fatalf("error output changed between runs:\n%s\n---\n%s", first, got)
		}
	}
}

func TestIDPatternMatchesSchemaTags(t *testing.T) {
	// The pattern tags in pipeline.go are literals, so check they agree with idPattern.
	for _, f := range []struct{ typ, field, tag string }{
		{"Pipeline", "Name", "pattern"},
		{"Step", "ID", "pattern"},
		{"Step", "Needs", "itemPattern"},
	} {
		var got string
		switch f.typ {
		case "Pipeline":
			sf, _ := reflectField(Pipeline{}, f.field)
			got = sf.Tag.Get(f.tag)
		case "Step":
			sf, _ := reflectField(Step{}, f.field)
			got = sf.Tag.Get(f.tag)
		}
		if got != idPattern {
			t.Errorf("%s.%s %s tag = %q, want %q", f.typ, f.field, f.tag, got, idPattern)
		}
	}
}

func agentStep() *Agent {
	return &Agent{Provider: ProviderClaudeCode, Prompt: "review the diff"}
}

// setAgent turns step 0 into a valid agent step, then lets the test break one thing.
func setAgent(p *Pipeline, mutate func(*Agent)) {
	a := agentStep()
	mutate(a)
	p.Steps[0].Run = ""
	p.Steps[0].Agent = a
	p.Steps[0].Permissions = Permissions{Secrets: []string{"CLAUDE_CODE_OAUTH_TOKEN"}}
}

func TestValidateAcceptsAgentStep(t *testing.T) {
	for _, cred := range AgentCredentials[ProviderClaudeCode] {
		p := validPipeline()
		setAgent(&p, func(*Agent) {})
		p.Steps[0].Permissions.Secrets = []string{cred}
		if err := p.Validate(); err != nil {
			t.Errorf("credential %s: Validate() = %v", cred, err)
		}
	}
}

func TestAgentArgv(t *testing.T) {
	got := (&Agent{Provider: ProviderClaudeCode, Prompt: "--dangerously-skip-permissions; rm -rf /"}).Argv()
	want := []string{"claude", "-p", "--output-format", "text", "--max-turns", "10",
		"--allowedTools=Read,Grep,Glob", "--", "--dangerously-skip-permissions; rm -rf /"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Argv() =\n%q\nwant\n%q", got, want)
	}

	got = (&Agent{Provider: ProviderClaudeCode, Prompt: "fix it", Model: "m", MaxTurns: 3, Tools: []string{"Edit", "Bash(go test:*)"}}).Argv()
	want = []string{"claude", "-p", "--output-format", "text", "--max-turns", "3",
		"--allowedTools=Edit,Bash(go test:*)", "--model", "m", "--", "fix it"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Argv() =\n%q\nwant\n%q", got, want)
	}
}

func workflowStep(mut func(*Step)) *Pipeline {
	p := validPipeline()
	p.Steps = []Step{{ID: "ci", Workflow: &Workflow{Repo: "acme/widgets", Workflow: "ci.yml", Ref: "main", Inputs: map[string]string{"env": "staging"}}}}
	if mut != nil {
		mut(&p.Steps[0])
	}
	return &p
}

func TestValidateWorkflowStep(t *testing.T) {
	if err := workflowStep(nil).Validate(); err != nil {
		t.Fatalf("a good workflow step: %v", err)
	}
	if err := workflowStep(func(s *Step) { s.Workflow.Workflow = "123456"; s.Timeout = "20m"; s.Workflow.Detach = true }).Validate(); err != nil {
		t.Fatalf("numeric id, timeout, detach: %v", err)
	}
	bad := map[string]func(*Step){
		"repo without owner":     func(s *Step) { s.Workflow.Repo = "widgets" },
		"repo with a slash path": func(s *Step) { s.Workflow.Repo = "acme/widgets/extra" },
		"workflow with a path":   func(s *Step) { s.Workflow.Workflow = "../ci.yml" },
		"workflow not yaml":      func(s *Step) { s.Workflow.Workflow = "ci.sh" },
		"full ref":               func(s *Step) { s.Workflow.Ref = "refs/heads/main" },
		"empty ref":              func(s *Step) { s.Workflow.Ref = "" },
		"ref with a space":       func(s *Step) { s.Workflow.Ref = "my branch" },
		"too many inputs": func(s *Step) {
			s.Workflow.Inputs = map[string]string{}
			for i := 0; i < 26; i++ {
				s.Workflow.Inputs[string(rune('a'+i))] = "v"
			}
		},
		"bad input name": func(s *Step) { s.Workflow.Inputs = map[string]string{"a b": "v"} },
		"long input":     func(s *Step) { s.Workflow.Inputs = map[string]string{"a": strings.Repeat("x", 1001)} },
		"with an image":  func(s *Step) { s.Image = "alpine" },
		"with a script":  func(s *Step) { s.Run = "echo hi" },
		"with an agent":  func(s *Step) { s.Agent = &Agent{Provider: ProviderClaudeCode, Prompt: "x"} },
		"with secrets":   func(s *Step) { s.Permissions.Secrets = []string{"TOKEN"} },
		"with env":       func(s *Step) { s.Env = map[string]string{"A": "b"} },
		"with a network": func(s *Step) { s.Permissions.Network = NetworkEgress },
		"bad timeout":    func(s *Step) { s.Timeout = "soon" },
	}
	for name, mut := range bad {
		if err := workflowStep(mut).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A normal step still needs an image.
	p := validPipeline()
	p.Steps[0].Image = ""
	if p.Validate() == nil {
		t.Error("a script step without an image was accepted")
	}
}
