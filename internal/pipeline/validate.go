package pipeline

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// idPattern is the shape of pipeline names and step IDs. It must match the pattern tags
// in pipeline.go; a test checks that they agree.
const idPattern = `^[a-z][a-z0-9-]{0,62}$`

var (
	idRE      = regexp.MustCompile(idPattern)
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// FieldError describes one problem, located by a path such as steps[2].needs[0].
type FieldError struct {
	Path string
	Msg  string
}

func (e FieldError) Error() string { return e.Path + ": " + e.Msg }

// ValidationError holds every problem found in a pipeline, so a user can fix them all
// in one pass instead of one at a time.
type ValidationError struct {
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	lines := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		lines[i] = fe.Error()
	}
	return fmt.Sprintf("invalid pipeline (%d problem(s)):\n  %s", len(e.Errors), strings.Join(lines, "\n  "))
}

// Validate checks the pipeline and returns a *ValidationError listing every problem, or
// nil when the pipeline is valid. It does not detect dependency cycles; the planner does.
func (p *Pipeline) Validate() error {
	var errs []FieldError
	add := func(path, format string, args ...any) {
		errs = append(errs, FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
	}

	if p.Version != VersionV1Alpha1 {
		add("version", "must be %q, got %q", VersionV1Alpha1, p.Version)
	}
	if !idRE.MatchString(p.Name) {
		add("name", "must match %s, got %q", idPattern, p.Name)
	}
	if len(p.Steps) == 0 {
		add("steps", "at least one step is required")
	}

	// First pass: collect IDs so needs can point forward as well as backward.
	known := make(map[string]bool, len(p.Steps))
	for i, s := range p.Steps {
		path := fmt.Sprintf("steps[%d]", i)
		switch {
		case !idRE.MatchString(s.ID):
			add(path+".id", "must match %s, got %q", idPattern, s.ID)
		case known[s.ID]:
			add(path+".id", "duplicate step id %q", s.ID)
		default:
			known[s.ID] = true
		}
	}

	for i, s := range p.Steps {
		path := fmt.Sprintf("steps[%d]", i)

		if s.Workflow == nil && (strings.TrimSpace(s.Image) == "" || strings.ContainsAny(s.Image, " \t\n")) {
			add(path+".image", "is required and must not contain whitespace, got %q", s.Image)
		}

		hasRun := strings.TrimSpace(s.Run) != ""
		hasCommand := len(s.Command) > 0
		switch {
		case countTrue(hasRun, hasCommand, s.Agent != nil, s.Workflow != nil) > 1:
			add(path, "set exactly one of run, command, agent or workflow, not several")
		case !hasRun && !hasCommand && s.Agent == nil && s.Workflow == nil:
			add(path, "set exactly one of run, command, agent or workflow")
		case hasCommand && strings.TrimSpace(s.Command[0]) == "":
			add(path+".command[0]", "the program name must not be empty")
		}

		if s.Agent != nil {
			validateAgent(s, path, add)
		}
		if s.Workflow != nil {
			validateWorkflow(s, path, add)
		}

		seenNeeds := make(map[string]bool, len(s.Needs))
		for j, need := range s.Needs {
			npath := fmt.Sprintf("%s.needs[%d]", path, j)
			switch {
			case need == s.ID:
				add(npath, "a step cannot need itself")
			case !known[need]:
				add(npath, "unknown step %q", need)
			case seenNeeds[need]:
				add(npath, "duplicate dependency %q", need)
			}
			seenNeeds[need] = true
		}

		envNames := make([]string, 0, len(s.Env))
		for name := range s.Env {
			envNames = append(envNames, name)
		}
		sort.Strings(envNames) // map order is random; keep error output stable
		for _, name := range envNames {
			if !envNameRE.MatchString(name) {
				add(path+".env", "invalid variable name %q", name)
			}
		}

		if s.WorkDir != "" && !strings.HasPrefix(s.WorkDir, "/") {
			add(path+".workdir", "must be an absolute path, got %q", s.WorkDir)
		}

		for j, pat := range s.Inputs {
			if msg := checkGlob(pat); msg != "" {
				add(fmt.Sprintf("%s.inputs[%d]", path, j), "%s, got %q", msg, pat)
			}
		}

		switch s.Permissions.Network {
		case "", NetworkNone, NetworkEgress:
		default:
			add(path+".permissions.network", "must be %q or %q, got %q", NetworkNone, NetworkEgress, s.Permissions.Network)
		}

		seenSecrets := make(map[string]bool, len(s.Permissions.Secrets))
		for j, name := range s.Permissions.Secrets {
			spath := fmt.Sprintf("%s.permissions.secrets[%d]", path, j)
			switch {
			case !envNameRE.MatchString(name):
				add(spath, "invalid secret name %q", name)
			case seenSecrets[name]:
				add(spath, "duplicate secret %q", name)
			default:
				if _, clash := s.Env[name]; clash {
					add(spath, "secret %q is also set in env; a name can only come from one place", name)
				}
			}
			seenSecrets[name] = true
		}

		if s.Timeout != "" {
			d, err := time.ParseDuration(s.Timeout)
			switch {
			case err != nil:
				add(path+".timeout", "not a valid duration: %q", s.Timeout)
			case d <= 0:
				add(path+".timeout", "must be positive, got %q", s.Timeout)
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: errs}
}

// checkGlob returns why an inputs pattern is invalid, or "". The syntax is described in
// docs/design/002-spec-extensions.md.
func checkGlob(pat string) string {
	switch {
	case strings.TrimSpace(pat) == "":
		return "must not be empty"
	case strings.HasPrefix(pat, "/"):
		return "must be relative to the workspace"
	case strings.Contains(pat, `\`):
		return `must use "/" separators, not backslashes`
	}
	for _, seg := range strings.Split(pat, "/") {
		if seg == ".." {
			return `must not contain a ".." segment`
		}
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return "is not a valid glob"
		}
	}
	return ""
}

func countTrue(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

var (
	workflowRepoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	workflowFileRE = regexp.MustCompile(`^([A-Za-z0-9_.-]{1,100}\.ya?ml|[0-9]{1,18})$`)
	workflowInput  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// validateWorkflow checks a workflow step. The server checks the same things again, against its
// allow list, when the step runs.
func validateWorkflow(s Step, path string, add func(path, format string, args ...any)) {
	w := s.Workflow
	wpath := path + ".workflow"
	if !workflowRepoRE.MatchString(w.Repo) {
		add(wpath+".repo", "must look like owner/name, got %q", w.Repo)
	}
	if !workflowFileRE.MatchString(w.Workflow) {
		add(wpath+".workflow", "must be a file name such as ci.yml or the workflow's number, got %q", w.Workflow)
	}
	if w.Ref == "" || len(w.Ref) > 200 || strings.HasPrefix(w.Ref, "refs/") || strings.ContainsAny(w.Ref, " \t\n") {
		add(wpath+".ref", "must be a branch name such as main (not a full ref), got %q", w.Ref)
	}
	if len(w.Inputs) > 25 {
		add(wpath+".inputs", "at most 25 inputs, got %d", len(w.Inputs))
	}
	names := make([]string, 0, len(w.Inputs))
	for k := range w.Inputs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if !workflowInput.MatchString(k) || len(w.Inputs[k]) > 1000 {
			add(wpath+".inputs", "input %q has a bad name or a value over 1000 bytes", k)
		}
	}
	if s.Image != "" || len(s.Env) > 0 || s.WorkDir != "" || len(s.Inputs) > 0 || s.Permissions.Network != "" || len(s.Permissions.Secrets) > 0 {
		add(path, "a workflow step runs on GitHub: it takes no image, env, workdir, inputs or permissions")
	}
}

// validateAgent checks an agent step: a known provider, a prompt, sane tools, a network to reach
// the vendor and a credential requested through permissions.secrets.
func validateAgent(s Step, path string, add func(path, format string, args ...any)) {
	a := s.Agent
	apath := path + ".agent"
	creds, ok := AgentCredentials[a.Provider]
	if !ok {
		add(apath+".provider", "unknown provider %q; supported: %s", a.Provider, ProviderClaudeCode)
		return
	}
	if strings.TrimSpace(a.Prompt) == "" {
		add(apath+".prompt", "is required")
	}
	if a.MaxTurns < 0 || a.MaxTurns > 100 {
		add(apath+".maxTurns", "must be between 1 and 100, got %d", a.MaxTurns)
	}
	for j, t := range a.Tools {
		if strings.TrimSpace(t) == "" || strings.ContainsAny(t, ",\n") {
			add(fmt.Sprintf("%s.tools[%d]", apath, j), "must be a single tool name or pattern without commas, got %q", t)
		}
	}
	if s.Permissions.Network == NetworkNone {
		add(path+".permissions.network", "an agent step needs network access to reach its provider")
	}
	has := false
	for _, name := range s.Permissions.Secrets {
		for _, c := range creds {
			has = has || name == c
		}
	}
	if !has {
		add(path+".permissions.secrets", "an agent step must request one of: %s", strings.Join(creds, ", "))
	}
}
