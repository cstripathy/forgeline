package pipeline

import (
	"errors"
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
		{"run and command", func(p *Pipeline) { p.Steps[0].Command = []string{"x"} }, "steps[0]", "not both"},
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
		{"zero timeout", func(p *Pipeline) { p.Steps[0].Timeout = "0s" }, "steps[0].timeout", "must be positive"},
		{"negative timeout", func(p *Pipeline) { p.Steps[0].Timeout = "-5m" }, "steps[0].timeout", "must be positive"},
		{"empty input", func(p *Pipeline) { p.Steps[0].Inputs = []string{""} }, "steps[0].inputs[0]", "must not be empty"},
		{"absolute input", func(p *Pipeline) { p.Steps[0].Inputs = []string{"/etc/passwd"} }, "steps[0].inputs[0]", "relative"},
		{"parent input", func(p *Pipeline) { p.Steps[0].Inputs = []string{"ok", "../secret"} }, "steps[0].inputs[1]", ".."},
		{"backslash input", func(p *Pipeline) { p.Steps[0].Inputs = []string{`a\b`} }, "steps[0].inputs[0]", "backslash"},
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
