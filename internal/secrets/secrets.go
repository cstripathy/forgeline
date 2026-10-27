// Package secrets reads secret values from the environment of the process that runs steps.
//
// A secret named NAME is set with FORGELINE_SECRET_NAME. Pipelines only ever name secrets;
// values never appear in a pipeline file, a run record or a log.
package secrets

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Prefix marks an environment variable as a secret.
const Prefix = "FORGELINE_SECRET_"

// MinLength is the shortest accepted value. Output masking replaces values wherever they
// appear, and a one-character value would garble every log it touches.
const MinLength = 8

var nameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// FromEnviron collects secrets from environ, a list of NAME=value strings as returned by
// os.Environ. Errors name the offending variable and never include a value.
func FromEnviron(environ []string) (map[string]string, error) {
	out := map[string]string{}
	var problems []string
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		name, ok := strings.CutPrefix(key, Prefix)
		if !ok {
			continue
		}
		switch {
		case !nameRE.MatchString(name):
			problems = append(problems, fmt.Sprintf("%s: %q is not a valid secret name", key, name))
		case len(value) < MinLength:
			problems = append(problems, fmt.Sprintf("%s: must be at least %d characters so it can be masked in logs", key, MinLength))
		case strings.ContainsAny(value, "\n\r\x00"):
			problems = append(problems, fmt.Sprintf("%s: must not contain a newline or NUL byte", key))
		default:
			out[name] = value
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("invalid secrets: %s", strings.Join(problems, "; "))
	}
	return out, nil
}

// Names returns the secret names in sorted order, for messages that must not show values.
func Names(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Values returns the secret values, for masking.
func Values(m map[string]string) []string {
	vals := make([]string, 0, len(m))
	for _, v := range m {
		vals = append(vals, v)
	}
	return vals
}
