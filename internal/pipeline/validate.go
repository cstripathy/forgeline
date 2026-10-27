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

		if strings.TrimSpace(s.Image) == "" || strings.ContainsAny(s.Image, " \t\n") {
			add(path+".image", "is required and must not contain whitespace, got %q", s.Image)
		}

		hasRun := strings.TrimSpace(s.Run) != ""
		hasCommand := len(s.Command) > 0
		switch {
		case hasRun && hasCommand:
			add(path, "set exactly one of run or command, not both")
		case !hasRun && !hasCommand:
			add(path, "set exactly one of run or command")
		case hasCommand && strings.TrimSpace(s.Command[0]) == "":
			add(path+".command[0]", "the program name must not be empty")
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
