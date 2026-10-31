package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
	"github.com/forgelinehq/forgeline/internal/pipeline"
	"github.com/forgelinehq/forgeline/internal/store"
)

// Guard rules for pipelines. They read the saved pipelines and the tail of recent run logs, nothing
// else, so they need no cloud account. A finding never repeats a secret: it names the variable or
// the kind of credential and where it was seen.

const (
	pipelineScanID   = "pipelines"
	guardLogRuns     = 25      // newest runs whose logs are read
	guardLogBytes    = 2 << 20 // per step
	guardLogOverlap  = 256     // carried across pages so a credential on a boundary is still seen
	guardLogPageSize = 200
)

// credentialPatterns are credential formats that should never appear in a pipeline file or a log.
var credentialPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"an AWS access key id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"a GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{50,})\b`)},
	{"an Anthropic API key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"a Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"a live Stripe key", regexp.MustCompile(`\b[sr]k_live_[A-Za-z0-9]{16,}`)},
}

var (
	secretLikeNameRE = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|CREDENTIAL)`)
	notSecretNameRE  = regexp.MustCompile(`(?i)_(FILE|PATH|DIR|NAME|URL|URI|ID|SOURCE|ENV)$`)
	pipeToShell      = regexp.MustCompile(`(?:curl|wget)\b[^\n|]*\|\s*(?:sudo\s+)?(?:ba|z|da)?sh\b`)
	traceOrDump      = regexp.MustCompile(`(?m)^\s*(?:set\s+-[a-z]*x|set\s+-o\s+xtrace|env|printenv|export\s+-p)\s*(?:\||;|$)`)
)

var printCmdRE = regexp.MustCompile(`^\s*(?:echo|printf|cat|print)\b`)

// printsSecret says whether a line of the script writes $name to the log. A line that pipes or
// redirects after the variable (echo "$T" | docker login --password-stdin) sends it elsewhere.
func printsSecret(script, name string) bool {
	ref := regexp.MustCompile(`\$\{?` + regexp.QuoteMeta(name) + `\b\}?`)
	for _, line := range strings.Split(script, "\n") {
		if !printCmdRE.MatchString(line) {
			continue
		}
		loc := ref.FindStringIndex(line)
		if loc != nil && !strings.ContainsAny(line[loc[1]:], "|>") {
			return true
		}
	}
	return false
}

// credentialKind names the first credential format found in s, or "".
func credentialKind(s string) string {
	for _, p := range credentialPatterns {
		if p.re.MatchString(s) {
			return p.kind
		}
	}
	return ""
}

func pipelineRules(sp SavedPipeline) []rawFinding {
	var out []rawFinding
	add := func(rule, title, sev, resource, name, detail, fix string) {
		out = append(out, rawFinding{rule, title, sev, "pipe-pipelines", resource, name, detail, fix})
	}
	p, err := pipeline.Load(strings.NewReader(sp.YAML))
	if err != nil {
		add("pipe-invalid", "A saved pipeline no longer validates", sevLow, sp.ID, sp.Name, "The stored YAML is refused by the current pipeline schema, so it can't run.",
			"Open the pipeline in the editor and fix what the validator reports, or delete it.")
		return out
	}
	for _, st := range p.Steps {
		res, name := sp.ID+"/"+st.ID, sp.Name+" › "+st.ID
		hasSecrets := len(st.Permissions.Secrets) > 0
		egress := st.Permissions.Network != pipeline.NetworkNone

		if st.Workflow == nil && st.Image != "" {
			switch {
			case imageUnpinned(st.Image) && hasSecrets:
				add("pipe-unpinned-image-secrets", "A step with secrets runs an image that can change under it", sevHigh, res, name,
					fmt.Sprintf("The image %s has no tag or the moving tag latest, and the step receives %s.", st.Image, strings.Join(st.Permissions.Secrets, ", ")),
					"Pin the image by digest (image@sha256:…). A moved tag then can't hand your secrets to different code.")
			case imageUnpinned(st.Image):
				add("pipe-unpinned-image", "A step runs an image with no fixed version", sevMedium, res, name,
					fmt.Sprintf("The image %s has no tag or the moving tag latest, so two runs can use different code.", st.Image),
					"Pin the image by digest, or at least to a version tag.")
			case !strings.Contains(st.Image, "@sha256:") && hasSecrets:
				add("pipe-tag-only-image-secrets", "A step with secrets trusts a tag, not a digest", sevLow, res, name,
					fmt.Sprintf("The image %s is pinned to a tag, which its publisher can move, and the step receives secrets.", st.Image),
					"Pin the image by digest (image@sha256:…).")
			}
		}
		if hasSecrets && egress && st.Workflow == nil {
			sev, why := sevLow, "Anything that runs in the step can send them out."
			if st.Agent != nil {
				sev, why = sevMedium, "An agent that reads untrusted text (an issue, a pull request, a web page) could be talked into sending them out."
			}
			add("pipe-secrets-with-network", "A step has secrets and unrestricted network access", sev, res, name,
				fmt.Sprintf("The step receives %s and may reach any address. %s", strings.Join(st.Permissions.Secrets, ", "), why),
				"If the step doesn't need the network, set permissions.network to none. If it does, keep its secrets to the ones it needs.")
		}
		if st.Agent != nil {
			own := map[string]bool{}
			for _, c := range pipeline.AgentCredentials[st.Agent.Provider] {
				own[c] = true
			}
			var extra []string
			for _, s := range st.Permissions.Secrets {
				if !own[s] {
					extra = append(extra, s)
				}
			}
			if len(extra) > 0 {
				add("pipe-agent-extra-secrets", "An agent step receives secrets beyond its own login", sevHigh, res, name,
					fmt.Sprintf("The agent's prompt and tools run alongside %s. A prompt injection can read what the step can read.", strings.Join(extra, ", ")),
					"Give the agent only its own credential. Do the work that needs other secrets in a separate step that has no agent.")
			}
		}
		for k, v := range st.Env {
			if kind := credentialKind(v); kind != "" {
				add("pipe-literal-credential", "A pipeline file contains a credential", sevCritical, res+"/env/"+k, name+" › "+k,
					fmt.Sprintf("The variable %s holds what looks like %s, written into the pipeline file.", k, kind),
					"Revoke that credential now, since it is in the saved history. Then list a secret in permissions.secrets instead of writing the value.")
			} else if secretLikeNameRE.MatchString(k) && !notSecretNameRE.MatchString(k) && len(v) >= 8 && !strings.HasPrefix(v, "$") && !strings.HasPrefix(v, "/") {
				add("pipe-literal-secret-env", "A secret-looking variable is written into the pipeline file", sevHigh, res+"/env/"+k, name+" › "+k,
					fmt.Sprintf("The variable %s has a fixed value in the file, which everyone who can read pipelines can see.", k),
					"Move it to a secret and list it in permissions.secrets.")
			}
		}
		if kind := credentialKind(st.Run + "\n" + strings.Join(st.Command, " ")); kind != "" {
			add("pipe-literal-credential", "A pipeline file contains a credential", sevCritical, res+"/script", name,
				"The step's script contains what looks like "+kind+".", "Revoke that credential now, since it is in the saved history. Then pass it as a secret.")
		}
		script := st.Run + "\n" + strings.Join(st.Command, " ")
		if pipeToShell.MatchString(script) {
			sev := sevMedium
			if hasSecrets {
				sev = sevHigh
			}
			add("pipe-pipe-to-shell", "A step runs a downloaded script without looking at it", sev, res, name,
				"The script pipes curl or wget straight into a shell. Whoever controls that address controls the step"+map[bool]string{true: ", including its secrets", false: ""}[hasSecrets]+".",
				"Download to a file, check its checksum or signature, then run it. Better, bake the tool into a pinned image.")
		}
		if hasSecrets {
			for _, s := range st.Permissions.Secrets {
				if printsSecret(script, s) {
					add("pipe-prints-secret", "A step prints a secret", sevHigh, res+"/"+s, name+" › "+s,
						fmt.Sprintf("The script writes $%s to the log. Forgeline masks the exact value, but anything that transforms it (base64, splitting) gets through.", s),
						"Don't print it. Pass it to the program that needs it and nothing else.")
				}
			}
			if traceOrDump.MatchString(script) {
				add("pipe-dumps-environment", "A step with secrets traces or dumps its environment", sevMedium, res+"/trace", name,
					"The script uses set -x, env or printenv while the step holds secrets. The log can end up with transformed values the masker can't catch.",
					"Remove the trace or dump, or move it to a step that has no secrets.")
			}
		}
	}
	return out
}

// logRules reads the first guardLogBytes of each step's log in the newest runs for credential
// formats. A page's last guardLogOverlap bytes are scanned again with the next page, so a credential
// that straddles two pages is still seen.
func (s *Server) logRules(ctx context.Context) []rawFinding {
	runs, err := s.cfg.Store.ListRuns(ctx, store.ListOptions{Limit: guardLogRuns})
	if err != nil {
		return nil
	}
	var out []rawFinding
	for _, r := range runs {
		_, steps, err := s.cfg.Store.GetRun(ctx, r.ID)
		if err != nil {
			continue
		}
		for _, st := range steps {
			var after int64
			var seen int
			var carry string
			kinds := map[string]bool{}
			for seen < guardLogBytes {
				chunks, err := s.cfg.Store.ReadLog(ctx, r.ID, st.StepID, after, guardLogPageSize)
				if err != nil || len(chunks) == 0 {
					break
				}
				var text strings.Builder
				text.WriteString(carry)
				for _, c := range chunks {
					after = c.Seq
					seen += len(c.Data)
					text.Write(c.Data)
				}
				page := text.String()
				for _, p := range credentialPatterns {
					if p.re.MatchString(page) {
						kinds[p.kind] = true
					}
				}
				carry = page[max(0, len(page)-guardLogOverlap):]
			}
			for kind := range kinds {
				out = append(out, rawFinding{"pipe-log-credential", "A run log contains a credential", sevCritical, "pipe-logs", r.ID + "/" + st.StepID + "/" + kind,
					r.PipelineName + " › " + st.StepID, fmt.Sprintf("The log of step %s in run %s contains what looks like %s.", st.StepID, r.ID, kind),
					"Revoke that credential, then find what printed it. Anyone who can read run logs can read it. This finding stays open until an administrator accepts it with a note that it was revoked."})
			}
		}
	}
	return out
}

// scanPipelines checks every saved pipeline and the recent logs. It never touches an account.
func (s *Server) scanPipelines(ctx context.Context, now time.Time) GuardScanAccount {
	res := GuardScanAccount{Account: pipelineScanID, Name: "Pipelines"}
	docs, err := s.docs.List(ctx, kindPipeline, docstore.ListOptions{})
	if err != nil {
		res.Error = "could not read the saved pipelines"
		return res
	}
	saved, _ := docstore.Decode[SavedPipeline](docs)
	var raws []rawFinding
	for _, sp := range saved {
		raws = append(raws, pipelineRules(sp)...)
	}
	// Log findings are never resolved by a scan: a leaked credential is not safe because its run
	// scrolled out of the window or a read failed. An administrator accepts one once it is revoked.
	known := map[string]bool{"pipe-pipelines": true}
	raws = append(raws, s.logRules(ctx)...)
	res.Findings = s.reconcile(ctx, pipelineScanID, "Pipelines", "", "", raws, known, now)
	return res
}
