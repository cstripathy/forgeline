package server

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The AWS MCP Server runs Python in a sandbox and the script calls AWS with
// call_boto3(service_name=..., operation_name=..., params=...). Forgeline never forwards model
// text unchecked: it reads the script first. Rules, in short: only a small set of imports, none
// of the names that make code dynamic, and every call_boto3 must name its service and operation
// as string literals, so the exact list of AWS calls is known before anything runs. For reads
// each operation must be Describe, List or Get; for proposed changes the same list is shown to the
// approver and certain operations are never allowed.

type scriptCall struct {
	Service   string `json:"service"`
	Operation string `json:"operation"`
	Region    string `json:"region,omitempty"`
	Args      string `json:"args,omitempty"` // the source text of the call's arguments, for the approver
}

const maxScriptBytes = 6000

var (
	scriptImportOK = map[string]bool{"asyncio": true, "json": true, "datetime": true, "re": true, "collections": true, "itertools": true, "math": true, "statistics": true, "typing": true}
	// Names that let code build or run other code, reach files, or hide a call.
	scriptBadWord = regexp.MustCompile(`\b(eval|exec|compile|open|globals|locals|getattr|setattr|delattr|vars|input|breakpoint|help|dir|type|super|memoryview|__\w+__|aws_mcp|os|sys|subprocess|socket|importlib|builtins|pickle|ctypes)\b`)
	scriptFString = regexp.MustCompile(`(?i)\b(f|rf|fr)["']`)
	scriptImport  = regexp.MustCompile(`(?m)^\s*(?:import\s+([\w., ]+)|from\s+([\w.]+)\s+import\s+.+)$`)
	scriptLiteral = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`\b` + name + `\s*=\s*(?:"([^"\\]*)"|'([^'\\]*)')`)
	}
	reService, reOperation, reRegion = scriptLiteral("service_name"), scriptLiteral("operation_name"), scriptLiteral("region_name")
	scriptReadOp                     = regexp.MustCompile(`^(Describe|List|Get)[A-Za-z0-9]+$`)
	scriptNeverOp                    = regexp.MustCompile(`(?i)(SecretValue|Password|Credential|Token|Authorization|Session|Federation|Decrypt|GetObject|LogEvents|LogRecord|QueryResults|ConsoleOutput|ConsoleScreenshot|GetFunction|GetLayer|Download|Export|Presign|Generate|GetParameter|KeyMaterial|Login|Presigned)`)
	scriptNeverSvc                   = map[string]bool{"sso": true, "sso-oidc": true, "secretsmanager": true, "kms": true, "s3": true}
	// For proposed changes only: services whose changes can hand out access or remove the guard rails.
	scriptWriteNeverSvc = map[string]bool{"iam": true, "organizations": true, "sts": true, "account": true, "cloudtrail": true, "guardduty": true, "securityhub": true, "config": true, "ssm": true}
	scriptWriteBadOp    = regexp.MustCompile(`^(Delete|Terminate|Remove|Deregister|Disable|Detach|Revoke|Purge|Destroy|Disassociate|Reset|Cancel|Release)`)
)

// checkScript reads a script and returns the AWS calls it makes. With write false every call must
// be a read; with write true changes are allowed except the ones in scriptWriteBadOp and the
// services in scriptWriteNeverSvc, and the script must make at least one change.
func checkScript(code string, write bool) ([]scriptCall, error) {
	switch {
	case strings.TrimSpace(code) == "":
		return nil, errors.New("the script is empty")
	case len(code) > maxScriptBytes:
		return nil, fmt.Errorf("the script is longer than %d characters", maxScriptBytes)
	case strings.ContainsAny(code, "\x00"):
		return nil, errors.New("the script contains a null character")
	}
	// Strip comments and string contents before looking for names, so a word inside a string or a
	// comment cannot trip or hide a check. call_boto3's literals are read from the original text.
	bare := stripStringsAndComments(code)
	if scriptFString.MatchString(bare) || strings.Contains(bare, ".format") || strings.Contains(bare, "format_map") {
		return nil, errors.New("f-strings and format() are not allowed; build text with + or a % format")
	}
	if m := scriptBadWord.FindString(bare); m != "" {
		return nil, fmt.Errorf("the script uses %q, which is not allowed", m)
	}
	for _, m := range scriptImport.FindAllStringSubmatch(bare, -1) {
		mods := m[1]
		if m[2] != "" {
			mods = m[2]
		}
		for _, mod := range strings.Split(mods, ",") {
			mod = strings.TrimSpace(strings.SplitN(strings.TrimSpace(mod), " as ", 2)[0])
			if !scriptImportOK[mod] {
				return nil, fmt.Errorf("importing %q is not allowed", mod)
			}
		}
	}
	if strings.Contains(bare, "import") && !scriptImport.MatchString(bare) {
		return nil, errors.New("write imports on their own line")
	}

	total := strings.Count(bare, "call_boto3")
	var calls []scriptCall
	rest := code
	for {
		i := strings.Index(rest, "call_boto3")
		if i < 0 {
			break
		}
		after := strings.TrimLeft(rest[i+len("call_boto3"):], " \t")
		if !strings.HasPrefix(after, "(") {
			return nil, errors.New("call_boto3 can only be called directly, not stored or passed around")
		}
		args, end, ok := matchParen(after)
		if !ok {
			return nil, errors.New("a call_boto3 call is not closed")
		}
		c := scriptCall{Args: strings.Join(strings.Fields(args), " ")}
		if m := reService.FindStringSubmatch(args); m != nil {
			c.Service = m[1] + m[2]
		}
		if m := reOperation.FindStringSubmatch(args); m != nil {
			c.Operation = m[1] + m[2]
		}
		if m := reRegion.FindStringSubmatch(args); m != nil {
			c.Region = m[1] + m[2]
		}
		if bad := badCallKeyword(args); bad != "" {
			return nil, fmt.Errorf("call_boto3 takes only service_name, operation_name, region_name and params; %q is not one of them. Put the API's own arguments inside params={...}", bad)
		}
		if c.Service == "" || c.Operation == "" {
			return nil, errors.New("every call_boto3 must give service_name and operation_name as plain text in quotes, so the call can be checked")
		}
		if c.Region != "" && !awsRegionRE.MatchString(c.Region) {
			return nil, fmt.Errorf("%q is not a region name", c.Region)
		}
		if err := checkScriptOp(c, write); err != nil {
			return nil, err
		}
		calls = append(calls, c)
		rest = after[end:]
	}
	if len(calls) != total {
		return nil, errors.New("call_boto3 appears somewhere other than a direct call")
	}
	if len(calls) == 0 {
		return nil, errors.New("the script makes no AWS calls")
	}
	if write {
		changes := 0
		for _, c := range calls {
			if !scriptReadOp.MatchString(c.Operation) {
				changes++
			}
		}
		if changes == 0 {
			return nil, errors.New("this script only reads; ask the assistant to look it up instead of proposing it")
		}
	}
	return calls, nil
}

func checkScriptOp(c scriptCall, write bool) error {
	switch {
	case scriptNeverSvc[c.Service]:
		return fmt.Errorf("the %s service is not available to the assistant", c.Service)
	case scriptNeverOp.MatchString(c.Operation):
		return fmt.Errorf("%s %s can return secrets, credentials, code or data and is not available", c.Service, c.Operation)
	case !write && !scriptReadOp.MatchString(c.Operation):
		return fmt.Errorf("%s %s is not a read operation (Describe, List or Get). To change something, propose a change", c.Service, c.Operation)
	case !write && c.Service == "sts" && c.Operation != "GetCallerIdentity":
		return errors.New("for sts only GetCallerIdentity is available")
	case write && scriptWriteNeverSvc[c.Service] && !scriptReadOp.MatchString(c.Operation):
		return fmt.Errorf("changes to %s cannot be proposed by the assistant; make them by hand", c.Service)
	case write && scriptWriteBadOp.MatchString(c.Operation):
		return fmt.Errorf("%s %s removes or disables something; the assistant can advise on it but cannot propose it", c.Service, c.Operation)
	}
	return nil
}

// matchParen takes text that starts with "(" and returns what is inside the matching ")" and the
// offset just past it. Quotes are skipped so a bracket inside a string does not count.
func matchParen(s string) (inner string, end int, ok bool) {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case quote != 0:
			if ch == '\\' {
				i++
			} else if ch == quote {
				quote = 0
			}
		case ch == '"' || ch == '\'':
			quote = ch
		case ch == '(':
			depth++
		case ch == ')':
			depth--
			if depth == 0 {
				return s[1:i], i + 1, true
			}
		}
	}
	return "", 0, false
}

// stripStringsAndComments blanks the inside of string literals and drops comments.
func stripStringsAndComments(s string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case quote != 0:
			if ch == '\\' {
				i++
			} else if ch == quote {
				quote = 0
				b.WriteByte(ch)
			}
		case ch == '"' || ch == '\'':
			quote = ch
			b.WriteByte(ch)
		case ch == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// badCallKeyword returns the first top-level argument of a call_boto3 call that is not one of the
// four the sandbox accepts ("" if all are fine). Nested brackets and strings are skipped.
func badCallKeyword(args string) string {
	depth := 0
	var quote byte
	start := 0
	check := func(seg string) string {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return ""
		}
		name, _, ok := strings.Cut(seg, "=")
		name = strings.TrimSpace(name)
		switch name {
		case "service_name", "operation_name", "region_name", "params":
			if ok {
				return ""
			}
		}
		if len(name) > 40 {
			name = name[:40]
		}
		return name
	}
	for i := 0; i < len(args); i++ {
		ch := args[i]
		switch {
		case quote != 0:
			if ch == '\\' {
				i++
			} else if ch == quote {
				quote = 0
			}
		case ch == '"' || ch == '\'':
			quote = ch
		case ch == '(' || ch == '[' || ch == '{':
			depth++
		case ch == ')' || ch == ']' || ch == '}':
			depth--
		case ch == ',' && depth == 0:
			if bad := check(args[start:i]); bad != "" {
				return bad
			}
			start = i + 1
		}
	}
	return check(args[start:])
}
