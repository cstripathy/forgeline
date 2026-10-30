package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The resource explorer lists what is in the connected account, one read-only AWS CLI call per
// service. It is the "what is actually there" view that the stacks and instances cards cannot be.

const (
	inventoryTTL     = 60 * time.Second
	inventoryTimeout = 45 * time.Second
	inventoryWorkers = 6
	inventoryMax     = 200 // resources kept per service; the true count is still reported
)

// AWSFieldJSON is one labelled detail of a resource.
type AWSFieldJSON struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// AWSResourceJSON is one thing in the account.
type AWSResourceJSON struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	State   string            `json:"state,omitempty"`
	Created string            `json:"created,omitempty"`
	ARN     string            `json:"arn,omitempty"`
	Fields  []AWSFieldJSON    `json:"fields,omitempty"`
	Tags    map[string]string `json:"tags,omitempty"`
}

// AWSServiceJSON is every resource of one kind. Error is set when the service could not be listed
// (no permission, or not offered in the region) so the console can say so instead of showing zero.
type AWSServiceJSON struct {
	ID        string            `json:"id"`
	Label     string            `json:"label"`
	Category  string            `json:"category"`
	Global    bool              `json:"global,omitempty"`
	Count     int               `json:"count"`
	Truncated bool              `json:"truncated,omitempty"`
	Error     string            `json:"error,omitempty"`
	Resources []AWSResourceJSON `json:"resources"`
}

// AWSInventoryJSON is the whole explorer view.
type AWSInventoryJSON struct {
	Enabled   bool             `json:"enabled"`
	Profile   string           `json:"profile,omitempty"`
	Region    string           `json:"region,omitempty"`
	Account   string           `json:"account,omitempty"`
	FetchedAt string           `json:"fetched_at,omitempty"`
	Error     string           `json:"error,omitempty"`
	Services  []AWSServiceJSON `json:"services"`
}

type invCache struct {
	mu   sync.Mutex
	at   time.Time
	key  string
	data AWSInventoryJSON
}

// invSpec describes how to list one service with a single CLI call and read the answer. A
// collector that needs several calls sets custom instead.
type invSpec struct {
	id, label, category string
	global              bool
	args                []string
	list                string      // key holding the array; "" when the answer is the array
	sub                 string      // key of an array nested in each item (EC2 reservations)
	strs                bool        // the array holds plain strings
	idPath, namePath    string      // dotted paths into an item
	statePath, created  string      //
	arnPath             string      //
	nameFromID          bool        // name is the last segment of the id/ARN
	nameTag             bool        // name comes from the Name tag when namePath is empty
	fields              [][2]string // label, dotted path
	custom              func(ctx context.Context, s *Server, st awsSettings) ([]AWSServiceJSON, error)
}

func inventorySpecs() []invSpec {
	return []invSpec{
		{id: "ecs", label: "ECS clusters", category: "Containers", custom: collectECS},
		{id: "ec2", label: "EC2 instances", category: "Compute", args: []string{"ec2", "describe-instances"}, list: "Reservations", sub: "Instances",
			idPath: "InstanceId", statePath: "State.Name", created: "LaunchTime", nameTag: true,
			fields: [][2]string{{"Type", "InstanceType"}, {"Architecture", "Architecture"}, {"Private IP", "PrivateIpAddress"}, {"Public IP", "PublicIpAddress"}, {"VPC", "VpcId"}, {"Subnet", "SubnetId"}, {"Image", "ImageId"}}},
		{id: "lambda", label: "Lambda functions", category: "Compute", args: []string{"lambda", "list-functions"}, list: "Functions",
			idPath: "FunctionName", namePath: "FunctionName", arnPath: "FunctionArn", created: "LastModified", statePath: "State",
			fields: [][2]string{{"Runtime", "Runtime"}, {"Memory (MB)", "MemorySize"}, {"Timeout (s)", "Timeout"}, {"Handler", "Handler"}, {"Code size (bytes)", "CodeSize"}}},
		{id: "cfn", label: "CloudFormation stacks", category: "Management", args: []string{"cloudformation", "describe-stacks"}, list: "Stacks",
			idPath: "StackName", namePath: "StackName", arnPath: "StackId", statePath: "StackStatus", created: "CreationTime",
			fields: [][2]string{{"Description", "Description"}, {"Last updated", "LastUpdatedTime"}, {"Reason", "StackStatusReason"}}},
		{id: "s3", label: "S3 buckets", category: "Storage", global: true, args: []string{"s3api", "list-buckets"}, list: "Buckets",
			idPath: "Name", namePath: "Name", created: "CreationDate", fields: [][2]string{{"Region", "BucketRegion"}}},
		{id: "ebs", label: "EBS volumes", category: "Storage", args: []string{"ec2", "describe-volumes"}, list: "Volumes",
			idPath: "VolumeId", statePath: "State", created: "CreateTime", nameTag: true,
			fields: [][2]string{{"Size (GiB)", "Size"}, {"Type", "VolumeType"}, {"Encrypted", "Encrypted"}, {"Zone", "AvailabilityZone"}}},
		{id: "ecr", label: "ECR repositories", category: "Containers", args: []string{"ecr", "describe-repositories"}, list: "repositories",
			idPath: "repositoryName", namePath: "repositoryName", arnPath: "repositoryArn", created: "createdAt",
			fields: [][2]string{{"URI", "repositoryUri"}, {"Tag mutability", "imageTagMutability"}, {"Scan on push", "imageScanningConfiguration.scanOnPush"}}},
		{id: "rds", label: "RDS databases", category: "Databases", args: []string{"rds", "describe-db-instances"}, list: "DBInstances",
			idPath: "DBInstanceIdentifier", namePath: "DBInstanceIdentifier", arnPath: "DBInstanceArn", statePath: "DBInstanceStatus", created: "InstanceCreateTime",
			fields: [][2]string{{"Engine", "Engine"}, {"Version", "EngineVersion"}, {"Class", "DBInstanceClass"}, {"Storage (GiB)", "AllocatedStorage"}, {"Multi-AZ", "MultiAZ"}, {"Publicly accessible", "PubliclyAccessible"}}},
		{id: "dynamodb", label: "DynamoDB tables", category: "Databases", args: []string{"dynamodb", "list-tables"}, list: "TableNames", strs: true},
		{id: "vpc", label: "VPCs", category: "Network", args: []string{"ec2", "describe-vpcs"}, list: "Vpcs",
			idPath: "VpcId", statePath: "State", nameTag: true, fields: [][2]string{{"CIDR", "CidrBlock"}, {"Default", "IsDefault"}}},
		{id: "sg", label: "Security groups", category: "Network", args: []string{"ec2", "describe-security-groups"}, list: "SecurityGroups",
			idPath: "GroupId", namePath: "GroupName", fields: [][2]string{{"VPC", "VpcId"}, {"Description", "Description"}}},
		{id: "elb", label: "Load balancers", category: "Network", args: []string{"elbv2", "describe-load-balancers"}, list: "LoadBalancers",
			idPath: "LoadBalancerName", namePath: "LoadBalancerName", arnPath: "LoadBalancerArn", statePath: "State.Code", created: "CreatedTime",
			fields: [][2]string{{"Type", "Type"}, {"Scheme", "Scheme"}, {"DNS", "DNSName"}, {"VPC", "VpcId"}}},
		{id: "sqs", label: "SQS queues", category: "Messaging", args: []string{"sqs", "list-queues"}, list: "QueueUrls", strs: true},
		{id: "sns", label: "SNS topics", category: "Messaging", args: []string{"sns", "list-topics"}, list: "Topics",
			idPath: "TopicArn", arnPath: "TopicArn", nameFromID: true},
		{id: "logs", label: "CloudWatch log groups", category: "Management", args: []string{"logs", "describe-log-groups"}, list: "logGroups",
			idPath: "logGroupName", namePath: "logGroupName", arnPath: "arn", created: "creationTime",
			fields: [][2]string{{"Retention (days)", "retentionInDays"}, {"Stored (bytes)", "storedBytes"}}},
	}
}

func (s *Server) handleAWSInventory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := s.loadAWS(ctx)
	if !st.Enabled {
		writeJSON(w, http.StatusOK, AWSInventoryJSON{Services: []AWSServiceJSON{}})
		return
	}
	key := st.Profile + "|" + st.Region
	s.awsInv.mu.Lock()
	if r.URL.Query().Get("refresh") != "1" && s.awsInv.key == key && time.Since(s.awsInv.at) < inventoryTTL {
		d := s.awsInv.data
		s.awsInv.mu.Unlock()
		writeJSON(w, http.StatusOK, d)
		return
	}
	s.awsInv.mu.Unlock()

	d := s.fetchInventory(ctx, st)
	if d.Error == "" {
		s.awsInv.mu.Lock()
		s.awsInv.key, s.awsInv.at, s.awsInv.data = key, time.Now(), d
		s.awsInv.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) fetchInventory(ctx context.Context, st awsSettings) AWSInventoryJSON {
	d := AWSInventoryJSON{Enabled: true, Profile: st.Profile, Region: st.Region, FetchedAt: time.Now().UTC().Format(time.RFC3339), Services: []AWSServiceJSON{}}
	ctx, cancel := context.WithTimeout(ctx, inventoryTimeout)
	defer cancel()

	idOut, err := s.aws(ctx, st, "sts", "get-caller-identity")
	if err != nil {
		d.Error = err.Error()
		return d
	}
	var id struct{ Account string }
	_ = json.Unmarshal(idOut, &id)
	d.Account = id.Account

	specs := inventorySpecs()
	results := make([][]AWSServiceJSON, len(specs))
	sem := make(chan struct{}, inventoryWorkers)
	var wg sync.WaitGroup
	for i, sp := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.collect(ctx, st, sp)
		}()
	}
	wg.Wait()
	for _, r := range results {
		d.Services = append(d.Services, r...)
	}
	return d
}

// collect lists one service. A failure becomes the service's Error, never the whole view's.
func (s *Server) collect(ctx context.Context, st awsSettings, sp invSpec) []AWSServiceJSON {
	svc := AWSServiceJSON{ID: sp.id, Label: sp.label, Category: sp.category, Global: sp.global, Resources: []AWSResourceJSON{}}
	if sp.custom != nil {
		out, err := sp.custom(ctx, s, st)
		if err != nil {
			svc.Error = inventoryError(err)
			return []AWSServiceJSON{svc}
		}
		return out
	}
	raw, err := s.aws(ctx, st, sp.args...)
	if err != nil {
		svc.Error = inventoryError(err)
		return []AWSServiceJSON{svc}
	}
	res, err := parseInventory(sp, raw)
	if err != nil {
		svc.Error = "could not read the answer: " + err.Error()
		return []AWSServiceJSON{svc}
	}
	fillService(&svc, res)
	return []AWSServiceJSON{svc}
}

// fillService sorts, caps and counts. The cap keeps the response small for a big account while the
// count stays honest.
func fillService(svc *AWSServiceJSON, res []AWSResourceJSON) {
	sort.Slice(res, func(i, j int) bool { return strings.ToLower(res[i].Name) < strings.ToLower(res[j].Name) })
	svc.Count = len(res)
	if len(res) > inventoryMax {
		res, svc.Truncated = res[:inventoryMax], true
	}
	if res == nil {
		res = []AWSResourceJSON{} // JSON null would break the console
	}
	svc.Resources = res
}

func inventoryError(err error) string {
	m := err.Error()
	low := strings.ToLower(m)
	if strings.Contains(low, "accessdenied") || strings.Contains(low, "not authorized") || strings.Contains(low, "unauthorized") || strings.Contains(low, "denied") {
		return "this role is not allowed to list it"
	}
	return truncate(m, 200)
}

// parseInventory reads one service's answer. It works on generic JSON so one table of specs covers
// every service instead of a struct per service.
func parseInventory(sp invSpec, raw []byte) ([]AWSResourceJSON, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return []AWSResourceJSON{}, nil // some commands, like sqs list-queues, print nothing when there is nothing
	}
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	if sp.list != "" {
		m, ok := top.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expected an object")
		}
		top = m[sp.list]
	}
	items, _ := top.([]any)
	if sp.sub != "" {
		var flat []any
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				sub, _ := m[sp.sub].([]any)
				flat = append(flat, sub...)
			}
		}
		items = flat
	}
	res := []AWSResourceJSON{}
	for _, it := range items {
		if sp.strs {
			v, _ := it.(string)
			if v != "" {
				res = append(res, AWSResourceJSON{ID: v, Name: lastSegment(v)})
			}
			continue
		}
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		r := AWSResourceJSON{ID: pathString(m, sp.idPath), State: pathString(m, sp.statePath), ARN: pathString(m, sp.arnPath), Created: timeString(pathValue(m, sp.created))}
		r.Tags = tagMap(m)
		switch {
		case sp.namePath != "":
			r.Name = pathString(m, sp.namePath)
		case sp.nameFromID:
			r.Name = lastSegment(r.ID)
		case sp.nameTag:
			r.Name = r.Tags["Name"]
		}
		if r.Name == "" {
			r.Name = r.ID
		}
		for _, f := range sp.fields {
			if v := pathString(m, f[1]); v != "" {
				r.Fields = append(r.Fields, AWSFieldJSON{Label: f[0], Value: v})
			}
		}
		if r.ID == "" {
			continue
		}
		res = append(res, r)
	}
	return res, nil
}

func pathValue(m map[string]any, path string) any {
	if path == "" {
		return nil
	}
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func pathString(m map[string]any, path string) string {
	switch v := pathValue(m, path).(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// timeString turns the epoch numbers some services return into RFC 3339 and leaves strings alone.
func timeString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t > 1e11 { // milliseconds
			t /= 1000
		}
		if t > 1e9 {
			return time.Unix(int64(t), 0).UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// tagMap reads tags in either shape the CLI uses: a list of Key/Value pairs or a plain map.
func tagMap(m map[string]any) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"Tags", "tags"} {
		switch t := m[k].(type) {
		case []any:
			for _, e := range t {
				if kv, ok := e.(map[string]any); ok {
					key, _ := kv["Key"].(string)
					if key == "" {
						key, _ = kv["key"].(string)
					}
					val, _ := kv["Value"].(string)
					if val == "" {
						val, _ = kv["value"].(string)
					}
					if key != "" {
						out[key] = val
					}
				}
			}
		case map[string]any:
			for key, v := range t {
				if s, ok := v.(string); ok {
					out[key] = s
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func lastSegment(s string) string {
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// collectECS lists clusters and, for each, its services. Clusters need a describe call after the
// list, and services a list then a describe in batches of ten, which is why it is not a plain spec.
func collectECS(ctx context.Context, s *Server, st awsSettings) ([]AWSServiceJSON, error) {
	clusters := AWSServiceJSON{ID: "ecs", Label: "ECS clusters", Category: "Containers", Resources: []AWSResourceJSON{}}
	services := AWSServiceJSON{ID: "ecs-services", Label: "ECS services", Category: "Containers", Resources: []AWSResourceJSON{}}

	out, err := s.aws(ctx, st, "ecs", "list-clusters")
	if err != nil {
		return nil, err
	}
	var lc struct{ ClusterArns []string }
	if err := json.Unmarshal(out, &lc); err != nil {
		return nil, err
	}
	if len(lc.ClusterArns) == 0 {
		return []AWSServiceJSON{clusters, services}, nil
	}
	if len(lc.ClusterArns) > inventoryMax {
		lc.ClusterArns = lc.ClusterArns[:inventoryMax]
	}
	out, err = s.aws(ctx, st, append([]string{"ecs", "describe-clusters", "--include", "TAGS", "SETTINGS", "--clusters"}, lc.ClusterArns...)...)
	if err != nil {
		return nil, err
	}
	spec := invSpec{list: "clusters", idPath: "clusterName", namePath: "clusterName", arnPath: "clusterArn", statePath: "status",
		fields: [][2]string{{"Services", "activeServicesCount"}, {"Running tasks", "runningTasksCount"}, {"Pending tasks", "pendingTasksCount"},
			{"Container instances", "registeredContainerInstancesCount"}}}
	res, err := parseInventory(spec, out)
	if err != nil {
		return nil, err
	}
	// Settings are a list the generic reader skips; say whether Container Insights is on in plain words.
	var dc struct {
		Clusters []struct {
			ClusterName string
			Settings    []struct{ Name, Value string }
		}
	}
	_ = json.Unmarshal(out, &dc)
	insights := map[string]string{}
	for _, c := range dc.Clusters {
		for _, set := range c.Settings {
			if set.Name == "containerInsights" {
				insights[c.ClusterName] = set.Value
			}
		}
	}
	for i := range res {
		if v := insights[res[i].Name]; v != "" {
			res[i].Fields = append(res[i].Fields, AWSFieldJSON{Label: "Container Insights", Value: v})
		}
	}
	fillService(&clusters, res)

	// Services are best effort: a role that can list clusters may not be allowed to list services.
	var svcRes []AWSResourceJSON
	for _, arn := range lc.ClusterArns {
		cname := lastSegment(arn)
		lo, err := s.aws(ctx, st, "ecs", "list-services", "--cluster", cname)
		if err != nil {
			services.Error = inventoryError(err)
			continue
		}
		var ls struct{ ServiceArns []string }
		if json.Unmarshal(lo, &ls) != nil {
			continue
		}
		for i := 0; i < len(ls.ServiceArns); i += 10 {
			end := min(i+10, len(ls.ServiceArns))
			do, err := s.aws(ctx, st, append([]string{"ecs", "describe-services", "--cluster", cname, "--services"}, ls.ServiceArns[i:end]...)...)
			if err != nil {
				services.Error = inventoryError(err)
				break
			}
			sres, err := parseInventory(invSpec{list: "services", idPath: "serviceName", namePath: "serviceName", arnPath: "serviceArn", statePath: "status", created: "createdAt",
				fields: [][2]string{{"Desired", "desiredCount"}, {"Running", "runningCount"}, {"Pending", "pendingCount"}, {"Launch type", "launchType"}, {"Task definition", "taskDefinition"}}}, do)
			if err != nil {
				continue
			}
			for k := range sres {
				sres[k].Fields = append([]AWSFieldJSON{{Label: "Cluster", Value: cname}}, sres[k].Fields...)
				sres[k].Fields = shortTaskDef(sres[k].Fields)
			}
			svcRes = append(svcRes, sres...)
		}
	}
	fillService(&services, svcRes)
	return []AWSServiceJSON{clusters, services}, nil
}

// shortTaskDef shows family:revision instead of the whole ARN.
func shortTaskDef(f []AWSFieldJSON) []AWSFieldJSON {
	for i := range f {
		if f[i].Label == "Task definition" {
			f[i].Value = f[i].Value[strings.LastIndex(f[i].Value, "/")+1:]
		}
	}
	return f
}
