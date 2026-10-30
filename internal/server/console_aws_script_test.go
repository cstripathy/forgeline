package server

import (
	"strings"
	"testing"
)

func TestCheckScriptReadsOnly(t *testing.T) {
	good := map[string]string{
		"one":    `r = await call_boto3(service_name="ecs", operation_name="ListClusters", region_name="ap-south-1")` + "\nresult = r\nresult",
		"loop":   "import asyncio\nimport json\nrs = await asyncio.gather(*[call_boto3(service_name='ec2', operation_name='DescribeInstances', params={'Filters': []}), call_boto3(service_name='ecs', operation_name='ListClusters')])\nresult = rs\nresult",
		"sts":    `r = await call_boto3(service_name="sts", operation_name="GetCallerIdentity")` + "\nresult = r\nresult",
		"string": `# eval open( __import__ in a comment` + "\n" + `x = "os.system exec getattr"` + "\n" + `r = await call_boto3(service_name="ecs", operation_name="ListClusters")` + "\nresult = [x, r]\nresult",
	}
	for name, code := range good {
		if _, err := checkScript(code, false); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	calls, err := checkScript(good["loop"], false)
	if err != nil || len(calls) != 2 || calls[0].Operation != "DescribeInstances" || calls[1].Service != "ecs" {
		t.Errorf("calls = %+v, %v", calls, err)
	}

	bad := map[string]string{
		"write":                               `await call_boto3(service_name="ec2", operation_name="TerminateInstances", params={})`,
		"write ecs":                           `await call_boto3(service_name="ecs", operation_name="UpdateService", params={})`,
		"dynamic op":                          `op = "ListClusters"` + "\n" + `await call_boto3(service_name="ecs", operation_name=op)`,
		"dynamic service":                     `await call_boto3(service_name=svc, operation_name="ListClusters")`,
		"alias":                               "f = call_boto3\nawait f(service_name='ecs', operation_name='ListClusters')",
		"passed":                              "await asyncio.gather(call_boto3)",
		"unclosed":                            `await call_boto3(service_name="ecs", operation_name="ListClusters"`,
		"no calls":                            "result = 1\nresult",
		"secret":                              `await call_boto3(service_name="secretsmanager", operation_name="ListSecrets")`,
		"secret value":                        `await call_boto3(service_name="ssm", operation_name="GetParameter", params={})`,
		"s3 data":                             `await call_boto3(service_name="s3", operation_name="GetObject", params={})`,
		"sts assume":                          `await call_boto3(service_name="sts", operation_name="GetSessionToken")`,
		"sts other":                           `await call_boto3(service_name="sts", operation_name="GetAccessKeyInfo")`,
		"import os":                           "import os\n" + `await call_boto3(service_name="ecs", operation_name="ListClusters")`,
		"from import":                         "from os import system\n" + `await call_boto3(service_name="ecs", operation_name="ListClusters")`,
		"aws_mcp":                             "import aws_mcp\n" + `await call_boto3(service_name="ecs", operation_name="ListClusters")`,
		"eval":                                `eval("1")` + "\n" + `await call_boto3(service_name="ecs", operation_name="ListClusters")`,
		"open":                                `open("/etc/passwd")` + "\n" + `await call_boto3(service_name="ecs", operation_name="ListClusters")`,
		"dunder":                              `x = ().__class__` + "\n" + `await call_boto3(service_name="ecs", operation_name="ListClusters")`,
		"fstring":                             `x = f"{eval('1')}"` + "\n" + `await call_boto3(service_name="ecs", operation_name="ListClusters")`,
		"format":                              `x = "{0.__class__}".format(1)` + "\n" + `await call_boto3(service_name="ecs", operation_name="ListClusters")`,
		"kwargs":                              `await call_boto3(service_name="ecs", operation_name="ListServices", cluster="payments")`,
		"positional":                          `await call_boto3("ecs", "ListClusters")`,
		"bad region":                          `await call_boto3(service_name="ecs", operation_name="ListClusters", region_name="x y")`,
		"empty":                               "  ",
		"too long":                            strings.Repeat("x = 1\n", maxScriptBytes),
		"call in string ok but second hidden": `x = "call_boto3"` + "\n" + `await call_boto3(service_name="ecs", operation_name="ListClusters")`,
	}
	for name, code := range bad {
		if _, err := checkScript(code, false); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestCheckScriptForProposedChanges(t *testing.T) {
	update := `r = await call_boto3(service_name="ecs", operation_name="UpdateService", params={"cluster": "payments", "service": "web", "desiredCount": 2})` + "\nresult = r\nresult"
	calls, err := checkScript(update, true)
	if err != nil || len(calls) != 1 || calls[0].Operation != "UpdateService" || !strings.Contains(calls[0].Args, "desiredCount") {
		t.Fatalf("calls = %+v, %v", calls, err)
	}
	// The sandbox takes only four keywords; the API's own arguments go inside params.
	if _, err := checkScript(`await call_boto3(service_name="ecs", operation_name="UpdateService", cluster="payments", desiredCount=2)`, true); err == nil || !strings.Contains(err.Error(), "params") {
		t.Errorf("kwargs err = %v", err)
	}
	// A change with a read before it is fine; only reads is not a change.
	mixed := `d = await call_boto3(service_name="ecs", operation_name="DescribeServices", params={})` + "\n" + `await call_boto3(service_name="ecs", operation_name="UpdateService", params={})`
	if _, err := checkScript(mixed, true); err != nil {
		t.Errorf("mixed refused: %v", err)
	}
	if _, err := checkScript(`await call_boto3(service_name="ecs", operation_name="ListClusters")`, true); err == nil {
		t.Error("a read-only script was accepted as a change")
	}
	for name, op := range map[string][2]string{
		"delete": {"ecs", "DeleteCluster"}, "terminate": {"ec2", "TerminateInstances"}, "deregister": {"ecs", "DeregisterTaskDefinition"},
		"iam": {"iam", "CreateUser"}, "iam policy": {"iam", "PutRolePolicy"}, "organizations": {"organizations", "CreateAccount"},
		"cloudtrail": {"cloudtrail", "StopLogging"}, "kms": {"kms", "CreateKey"}, "secrets": {"secretsmanager", "PutSecretValue"},
		"s3 object": {"s3", "PutObject"}, "token": {"ecr", "GetAuthorizationToken"}, "ssm": {"ssm", "SendCommand"},
	} {
		code := `await call_boto3(service_name="` + op[0] + `", operation_name="` + op[1] + `", params={})`
		if _, err := checkScript(code, true); err == nil {
			t.Errorf("%s change was accepted", name)
		}
	}
	// The same code rules apply to changes.
	if _, err := checkScript("import os\n"+update, true); err == nil {
		t.Error("a change script with a forbidden import was accepted")
	}
}
