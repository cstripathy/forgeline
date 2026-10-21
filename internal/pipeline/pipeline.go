// Package pipeline defines the Forgeline pipeline model: the typed description of
// what to run, in what order, and in which containers.
//
// The Go types in this file are the source of truth. The JSON Schema in
// schema/pipeline.v1alpha1.schema.json is generated from them (see schema.go), and the
// YAML form compiles to the same types.
package pipeline

// VersionV1Alpha1 is the only spec version so far. It is alpha: fields can change.
const VersionV1Alpha1 = "v1alpha1"

// Pipeline is a named set of steps. Order comes from each step's Needs, which together
// form a directed acyclic graph. There are no fixed stages.
type Pipeline struct {
	Version string `json:"version" doc:"Spec version. Must be v1alpha1." enum:"v1alpha1"`
	Name    string `json:"name" doc:"Pipeline name: lowercase letters, digits and hyphens." pattern:"^[a-z][a-z0-9-]{0,62}$"`
	Steps   []Step `json:"steps" doc:"The steps to run. At least one is required." minItems:"1"`
}

// Step is one unit of work that runs in a container.
//
// Exactly one of Run or Command must be set.
type Step struct {
	ID      string            `json:"id" doc:"Unique step identifier: lowercase letters, digits and hyphens." pattern:"^[a-z][a-z0-9-]{0,62}$"`
	Image   string            `json:"image" doc:"Container image the step runs in." minLength:"1"`
	Run     string            `json:"run,omitempty" doc:"Shell script to run. Set this or command, not both." minLength:"1"`
	Command []string          `json:"command,omitempty" doc:"Command and arguments, run without a shell. Set this or run, not both." minItems:"1"`
	Needs   []string          `json:"needs,omitempty" doc:"IDs of steps that must succeed before this one starts." itemPattern:"^[a-z][a-z0-9-]{0,62}$"`
	Env     map[string]string `json:"env,omitempty" doc:"Environment variables for the step. Names must be valid identifiers."`
	WorkDir string            `json:"workdir,omitempty" doc:"Absolute working directory inside the container." pattern:"^/"`
	Timeout string            `json:"timeout,omitempty" doc:"Maximum run time as a duration such as 30s or 10m. Must be positive."`
	Inputs  []string          `json:"inputs,omitempty" doc:"Workspace-relative file globs that make up the step's cache key. A step without inputs is never cached."`
}
