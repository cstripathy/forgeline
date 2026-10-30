package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// call_aws lets the assistant read the connected AWS account with the AWS CLI, the way the AWS
// MCP servers' call_aws tool does, but only for read operations and only through the credentials
// the console already holds. It is not an MCP client: the server parses one command, checks it
// against the rules below and runs it without a shell.
const (
	maxAWSCommand = 600
	awsCallTime   = 30 * time.Second
)

// awsReadOp is the only shape of operation allowed: describe-*, list-* and get-*.
var awsReadOp = regexp.MustCompile(`^(describe|list|get)-[a-z0-9-]+$`)

// awsNeverOp names operations that read secrets, credentials, tokens, code, data or logs even
// though they look like reads. Matched against "service operation".
var awsNeverOp = regexp.MustCompile(`(?i)(secret-value|password|credential|token|authorization|session|federation|decrypt|get-object|get-log-events|get-log-record|get-query-results|console-output|console-screenshot|get-function|get-layer|download|export|presign|generate|get-parameter|get-key-material|public-access-key|login)`)

// awsBlockedService are services the assistant never calls.
var awsBlockedService = map[string]bool{"configure": true, "sso": true, "sso-oidc": true, "secretsmanager": true, "kms": true, "s3": true}

// awsBlockedFlag are options that change where or as whom the call is made, read local files or
// fetch URLs, or reveal secrets.
var awsBlockedFlag = map[string]bool{
	"--profile": true, "--endpoint-url": true, "--cli-input-json": true, "--cli-input-yaml": true,
	"--generate-cli-skeleton": true, "--ca-bundle": true, "--no-verify-ssl": true, "--debug": true,
	"--no-sign-request": true, "--with-decryption": true, "--cli-read-timeout": true,
	"--cli-connect-timeout": true, "--output": true, "--color": true,
}

// tokenize splits a command line on spaces, honouring simple '...' and "..." quotes. There is no
// expansion of any kind.
func tokenize(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var quote rune
	have := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, have = r, true
		case r == ' ' || r == '\t':
			if have || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, errors.New("an opening quote is never closed")
	}
	if have || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}

// parseAWSCommand checks a command and returns the CLI arguments after "aws" (service,
// operation, options) and the region the person asked for, if any.
func parseAWSCommand(cmd string) (args []string, region string, err error) {
	cmd = strings.TrimSpace(cmd)
	switch {
	case cmd == "":
		return nil, "", errors.New("give a cli_command such as: aws ecs list-clusters")
	case len(cmd) > maxAWSCommand:
		return nil, "", fmt.Errorf("the command is longer than %d characters", maxAWSCommand)
	case strings.ContainsAny(cmd, "\n\r\x00`$;|&<>"):
		return nil, "", errors.New("one plain AWS CLI command only: no pipes, redirects, substitution or several commands")
	}
	toks, err := tokenize(cmd)
	if err != nil {
		return nil, "", err
	}
	if len(toks) > 0 && toks[0] == "aws" {
		toks = toks[1:]
	}
	if len(toks) < 2 || strings.HasPrefix(toks[0], "-") || strings.HasPrefix(toks[1], "-") {
		return nil, "", errors.New("the command must be: aws <service> <operation> [options]")
	}
	service, op := toks[0], toks[1]
	switch {
	case awsBlockedService[service]:
		return nil, "", fmt.Errorf("the %s service is not available to the assistant", service)
	case service == "sts" && op != "get-caller-identity":
		return nil, "", errors.New("for sts only get-caller-identity is available")
	case !awsReadOp.MatchString(op):
		return nil, "", fmt.Errorf("%s is not a read operation; only describe-*, list-* and get-* are allowed. To change something, propose a change in the console", op)
	case awsNeverOp.MatchString(service + " " + op):
		return nil, "", fmt.Errorf("%s %s can return secrets, credentials, code or data and is not available", service, op)
	}
	rest := toks[2:]
	out := []string{service, op}
	for i := 0; i < len(rest); i++ {
		t := rest[i]
		name, value, hasValue := strings.Cut(t, "=")
		if strings.HasPrefix(name, "--") {
			if awsBlockedFlag[name] {
				return nil, "", fmt.Errorf("the option %s is not allowed", name)
			}
			if name == "--region" {
				if !hasValue {
					if i+1 >= len(rest) {
						return nil, "", errors.New("--region needs a value")
					}
					i++
					value = rest[i]
				}
				if !awsRegionRE.MatchString(value) {
					return nil, "", fmt.Errorf("%q is not a region name", value)
				}
				region = value
				continue
			}
			if hasValue && badAWSValue(value) {
				return nil, "", errors.New("option values cannot read local files or fetch addresses")
			}
			out = append(out, t)
			continue
		}
		if badAWSValue(t) {
			return nil, "", errors.New("option values cannot read local files or fetch addresses")
		}
		out = append(out, t)
	}
	return out, region, nil
}

func badAWSValue(v string) bool {
	l := strings.ToLower(v)
	for _, p := range []string{"file://", "fileb://", "http://", "https://", "s3://", "env:", "ssm:", "secretsmanager:"} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

func (s *Server) lookupCallAWS(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"cli_command"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	args, region, err := parseAWSCommand(a.Command)
	if err != nil {
		return "", err
	}
	st := s.loadAWS(ctx)
	if !st.Enabled {
		return "", errors.New("AWS is not connected to this Forgeline server")
	}
	if region != "" {
		st.Region = region
	}
	ctx, cancel := context.WithTimeout(ctx, awsCallTime)
	defer cancel()
	out, err := s.aws(ctx, st, args...)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "The call succeeded and returned nothing.", nil
	}
	if len(text) > maxLookupResult {
		text = text[:maxLookupResult] + "\n[output cut; narrow it with --query or --max-items]"
	}
	return text, nil
}

// lookupDetail is what the audit log records for a lookup: the tool, and for call_aws the command.
func lookupDetail(c lookupCall) string {
	if c.Tool != "call_aws" {
		return c.Tool
	}
	var a struct {
		Command string `json:"cli_command"`
	}
	_ = json.Unmarshal(c.Args, &a)
	return "call_aws: " + truncate(strings.TrimSpace(a.Command), 200)
}
