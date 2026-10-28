// Package pipeline defines the Forgeline pipeline model: the typed description of
// what to run, in what order, and in which containers.
//
// The Go types in this file are the source of truth. The JSON Schema in
// schema/pipeline.v1alpha1.schema.json is generated from them (see schema.go), and the
// YAML form compiles to the same types.
package pipeline

import (
	"strconv"
	"strings"
)

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
// Exactly one of Run, Command or Agent must be set.
type Step struct {
	ID       string            `json:"id" doc:"Unique step identifier: lowercase letters, digits and hyphens." pattern:"^[a-z][a-z0-9-]{0,62}$"`
	Image    string            `json:"image" doc:"Container image the step runs in." minLength:"1"`
	Run      string            `json:"run,omitempty" doc:"Shell script to run. Set this or command, not both." minLength:"1"`
	Command  []string          `json:"command,omitempty" doc:"Command and arguments, run without a shell. Set this or run, not both." minItems:"1"`
	Agent    *Agent            `json:"agent,omitempty" doc:"Run a coding agent with a prompt instead of a script. The image must contain the agent's command line. Set this or run or command."`
	Workflow *Workflow         `json:"workflow,omitempty" doc:"Start a GitHub Actions workflow and wait for it, instead of running a container. Only the Forgeline server can do this (it holds the GitHub credentials); the step has no image, script, environment or permissions. Set this or run or command or agent."`
	Needs    []string          `json:"needs,omitempty" doc:"IDs of steps that must succeed before this one starts." itemPattern:"^[a-z][a-z0-9-]{0,62}$"`
	Env      map[string]string `json:"env,omitempty" doc:"Environment variables for the step. Names must be valid identifiers."`
	WorkDir  string            `json:"workdir,omitempty" doc:"Absolute working directory inside the container." pattern:"^/"`
	Timeout  string            `json:"timeout,omitempty" doc:"Maximum run time as a duration such as 30s or 10m. Must be positive."`
	// Permissions limits what the step may do. Network and secrets are enforced so far; the rest of
	// the reserved set (untrusted input) is described in docs/design/005-threat-model.md.
	Permissions Permissions `json:"permissions,omitempty" doc:"What the step is allowed to do. See docs/design/005-threat-model.md."`
	Inputs      []string    `json:"inputs,omitempty" doc:"Workspace-relative file globs that make up the step's cache key. A step without inputs is never cached."`
}

// Permissions is what a step is allowed to do beyond running in its container.
type Permissions struct {
	Network string `json:"network,omitempty" doc:"Outbound network access: none cuts the step off from every network, egress allows it. Empty means egress for now." enum:"none,egress"`
	// Secrets names the secrets the step receives, as environment variables of the same name.
	// Values come from the server's configuration, never from the pipeline file.
	Secrets []string `json:"secrets,omitempty" doc:"Names of the secrets this step receives as environment variables. Values come from the server or the local environment (FORGELINE_SECRET_<NAME>), never from the pipeline file. A step gets only what it lists." itemPattern:"^[A-Za-z_][A-Za-z0-9_]*$"`
}

// Network values.
const (
	NetworkNone   = "none"
	NetworkEgress = "egress"
)

// Workflow describes a GitHub Actions workflow started by a step (design note 011, slice 2). The
// Forgeline server starts it with its own GitHub credentials, which never enter a container, and
// only for repositories and branches on its GitHub allow list. Runs started by GitHub cannot use it.
type Workflow struct {
	Repo     string            `json:"repo" doc:"Repository as owner/name. It must be on the server's GitHub allow list." pattern:"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$"`
	Workflow string            `json:"workflow" doc:"The workflow's file name such as ci.yml, or its number. The file needs on: workflow_dispatch."`
	Ref      string            `json:"ref" doc:"Branch to run it on. It must be a branch the allow list permits for the repository."`
	Inputs   map[string]string `json:"inputs,omitempty" doc:"Workflow inputs, at most 25, each value at most 1000 bytes."`
	Detach   bool              `json:"detach,omitempty" doc:"Do not wait: the step succeeds as soon as GitHub accepts the request. By default the step waits and fails if the workflow does not succeed."`
}

// Agent describes a coding agent run as a step. The agent's own command line runs inside the
// step's container; Forgeline never reads or refreshes the vendor's login. The step must list the
// credential in permissions.secrets (see AgentCredentials).
type Agent struct {
	Provider string `json:"provider" doc:"Which agent to run. Only claude-code so far." enum:"claude-code"`
	Prompt   string `json:"prompt" doc:"What to ask the agent. It is passed as a single argument, never through a shell." minLength:"1"`
	Model    string `json:"model,omitempty" doc:"Model name to pass to the agent. Empty uses the agent's default."`
	MaxTurns int    `json:"maxTurns,omitempty" doc:"Most turns the agent may take. Empty means 10." minimum:"1" maximum:"100"`
	// Tools is deliberately read-only by default: editing files or running commands must be asked for.
	Tools []string `json:"tools,omitempty" doc:"Tools the agent may use, for example Read, Grep, Edit, Bash(go test:*). Empty means read-only: Read, Grep, Glob. A tool not listed is refused, because the agent cannot ask for approval in a pipeline."`
}

// Agent providers.
const ProviderClaudeCode = "claude-code"

// Defaults for agent steps.
const (
	defaultAgentMaxTurns = 10
)

// DefaultAgentTools is what an agent may use when the pipeline lists none.
var DefaultAgentTools = []string{"Read", "Grep", "Glob"}

// AgentCredentials lists the secret names that can authenticate each provider. A step must request
// at least one of them.
var AgentCredentials = map[string][]string{
	ProviderClaudeCode: {"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
}

// Argv is the command line that runs the agent. The prompt is one argument after "--", so a prompt
// that starts with "-" or holds shell characters cannot change the command.
func (a *Agent) Argv() []string {
	turns := a.MaxTurns
	if turns == 0 {
		turns = defaultAgentMaxTurns
	}
	tools := a.Tools
	if len(tools) == 0 {
		tools = DefaultAgentTools
	}
	argv := []string{"claude", "-p", "--output-format", "text",
		"--max-turns", strconv.Itoa(turns),
		"--allowedTools=" + strings.Join(tools, ",")}
	if a.Model != "" {
		argv = append(argv, "--model", a.Model)
	}
	return append(argv, "--", a.Prompt)
}
