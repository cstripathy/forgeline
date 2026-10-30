package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeK8s is a Kubernetes API with one messy application in it. It records every request.
type fakeK8s struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []string // "METHOD path"
	token string
	deny  map[string]int // path prefix -> status to answer with
	fixed bool           // the problems are fixed
}

func newFakeK8s(t *testing.T) *fakeK8s {
	f := &fakeK8s{token: "good-token", deny: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeK8s) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if strings.Contains(r, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeK8s) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
	token, fixed := f.token, f.fixed
	deny := map[string]int{}
	for k, v := range f.deny {
		deny[k] = v
	}
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	for prefix, code := range deny {
		if strings.HasPrefix(r.URL.Path, prefix) {
			http.Error(w, `{"message":"no"}`, code)
			return
		}
	}
	pr := func(v bool) string { return fmt.Sprint(v) }
	container := func(name, image, extra string) string {
		return `{"name":"` + name + `","image":"` + image + `"` + extra + `}`
	}
	list := func(items ...string) string { return `{"items":[` + strings.Join(items, ",") + `]}` }
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/version":
		fmt.Fprint(w, `{"gitVersion":"v1.31.4"}`)
	case "/api/v1/nodes":
		bad := "False"
		if fixed {
			bad = "True"
		}
		fmt.Fprint(w, list(
			`{"metadata":{"name":"node-a"},"status":{"conditions":[{"type":"Ready","status":"True"}],"nodeInfo":{"kubeletVersion":"v1.31.4","osImage":"Ubuntu"},"capacity":{"cpu":"4","memory":"16Gi"}}}`,
			`{"metadata":{"name":"node-b"},"status":{"conditions":[{"type":"Ready","status":"`+bad+`"}],"nodeInfo":{"kubeletVersion":"v1.31.4"}}}`))
	case "/api/v1/namespaces":
		fmt.Fprint(w, list(`{"metadata":{"name":"shop"},"status":{"phase":"Active"}}`, `{"metadata":{"name":"kube-system"},"status":{"phase":"Active"}}`))
	case "/api/v1/pods":
		wait := `"state":{"waiting":{"reason":"CrashLoopBackOff"}}`
		if fixed {
			wait = `"state":{"running":{}}`
		}
		fmt.Fprint(w, list(
			`{"metadata":{"name":"api-1","namespace":"shop"},"spec":{"nodeName":"node-a","containers":[`+container("api", "shop/api:1.2", "")+`]},"status":{"phase":"Running","containerStatuses":[{"name":"api","ready":false,"restartCount":9,`+wait+`}]}}`,
			`{"metadata":{"name":"coredns-1","namespace":"kube-system"},"spec":{"containers":[`+container("coredns", "coredns:1.11", "")+`]},"status":{"phase":"Running","containerStatuses":[{"name":"coredns","state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`))
	case "/api/v1/services":
		fmt.Fprint(w, list(
			`{"metadata":{"name":"web","namespace":"shop"},"spec":{"type":"LoadBalancer","clusterIP":"10.0.0.5","ports":[{"port":443,"protocol":"TCP"}]},"status":{"loadBalancer":{"ingress":[{"hostname":"lb.example.com"}]}}}`,
			`{"metadata":{"name":"internal","namespace":"shop"},"spec":{"type":"ClusterIP","clusterIP":"10.0.0.6","ports":[{"port":80,"protocol":"TCP"}]}}`))
	case "/api/v1/events":
		fmt.Fprint(w, list(`{"metadata":{"name":"ev1","namespace":"shop"},"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container","count":12,"lastTimestamp":"2026-10-30T10:00:00Z","involvedObject":{"kind":"Pod","name":"api-1","namespace":"shop"}}`))
	case "/apis/apps/v1/deployments":
		priv, hostNet, user, limits, image := `,"securityContext":{"privileged":true}`, "true", "", "", "shop/web:latest"
		ready := 0
		if fixed {
			priv, hostNet, user, limits, image, ready = "", "false", `,"securityContext":{"runAsNonRoot":true}`, `,"resources":{"limits":{"memory":"256Mi"}}`, "shop/web:1.4", 3
		}
		fmt.Fprint(w, list(
			`{"metadata":{"name":"web","namespace":"shop"},"spec":{"replicas":3,"template":{"spec":{"hostNetwork":`+hostNet+`,"containers":[`+container("web", image, priv+limits+user)+`]}}},"status":{"replicas":3,"readyReplicas":`+fmt.Sprint(ready)+`}}`,
			`{"metadata":{"name":"worker","namespace":"default"},"spec":{"replicas":2,"template":{"spec":{"containers":[`+container("worker", "shop/worker@sha256:abc", `,"securityContext":{"runAsNonRoot":true},"resources":{"limits":{"memory":"1Gi"}}`)+`]}}},"status":{"replicas":2,"readyReplicas":1}}`,
			`{"metadata":{"name":"coredns","namespace":"kube-system"},"spec":{"replicas":2,"template":{"spec":{"hostNetwork":true,"containers":[`+container("coredns", "coredns", `,"securityContext":{"privileged":true}`)+`]}}},"status":{"readyReplicas":0}}`))
	case "/apis/apps/v1/statefulsets":
		fmt.Fprint(w, list(`{"metadata":{"name":"db","namespace":"shop"},"spec":{"replicas":1,"template":{"spec":{"containers":[`+container("db", "postgres:16", `,"securityContext":{"runAsUser":999},"resources":{"limits":{"memory":"2Gi"}}`)+`]}}},"status":{"readyReplicas":1}}`))
	case "/apis/apps/v1/daemonsets":
		fmt.Fprint(w, list(`{"metadata":{"name":"agent","namespace":"shop"},"spec":{"template":{"spec":{"containers":[`+container("agent", "shop/agent:2", `,"securityContext":{"runAsNonRoot":true},"resources":{"limits":{"memory":"64Mi"}}`)+`]}}},"status":{"desiredNumberScheduled":2,"numberReady":2}}`))
	default:
		http.NotFound(w, r)
	}
	_ = pr
}

func k8sHarness(t *testing.T) (*mcpHarness, *fakeK8s) {
	m := newMCPHarness(t)
	f := newFakeK8s(t)
	return m, f
}

func (m *mcpHarness) addCluster(c *browser, want int, name string, f *fakeK8s) K8sClusterJSON {
	m.t.Helper()
	var out K8sClusterJSON
	json.Unmarshal(c.expect(want, "POST", "/v1/k8s/clusters", map[string]any{"name": name, "kind": "token", "server": f.srv.URL, "token": "good-token", "env": "staging"}), &out)
	return out
}

func (m *mcpHarness) k8sView(id, query string) AWSInventoryJSON {
	m.t.Helper()
	var v AWSInventoryJSON
	json.Unmarshal(m.v.expect(200, "GET", "/v1/k8s/clusters/"+id+"/resources"+query, nil), &v)
	return v
}

func TestK8sServerAddressRules(t *testing.T) {
	for addr, ok := range map[string]bool{
		"https://abc.eks.amazonaws.com": true, "https://10.0.0.1:6443": true, "http://127.0.0.1:6443": true, "http://localhost:8001": true,
		"http://10.0.0.1:6443": false, "http://example.com": false, "https://169.254.169.254": false, "https://[fd00:ec2::254]": false,
		"https://metadata.google.internal": false, "https://user:pw@host": false, "https://host/path": false, "ftp://host": false, "host:6443": false, "": false,
		"http://0.0.0.0:80": false,
	} {
		if err := validK8sServer(addr); (err == nil) != ok {
			t.Errorf("validK8sServer(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestK8sAddClusterProvesItAndKeepsTheTokenInTheVault(t *testing.T) {
	m, f := k8sHarness(t)
	c := m.addCluster(m.admin, 201, "Shop staging", f)
	if c.ID != "shop-staging" || c.Version != "v1.31.4" || c.Kind != "token" {
		t.Fatalf("cluster = %+v", c)
	}
	list := string(m.v.expect(200, "GET", "/v1/k8s/clusters", nil))
	if strings.Contains(list, "good-token") || !strings.Contains(list, "shop-staging") {
		t.Errorf("the list must show the cluster and never its token: %s", list)
	}
	if tok, ok := m.srv.internalSecretValue(context.Background(), k8sTokenSecret(c.ID)); !ok || tok != "good-token" {
		t.Errorf("token in the vault = %q, %v", tok, ok)
	}
	if strings.Contains(string(m.admin.expect(200, "GET", "/v1/secrets", nil)), "good-token") || strings.Contains(string(m.admin.expect(200, "GET", "/v1/secrets", nil)), "k8s") {
		t.Error("the cluster token appeared in the secrets list that steps can use")
	}
	m.addCluster(m.dev, 403, "x", f)
	m.addCluster(m.v, 403, "x", f)
	m.addCluster(m.admin, 409, "shop STAGING", f) // same name
}

func TestK8sAddClusterRefusesWhatDoesNotWork(t *testing.T) {
	m, f := k8sHarness(t)
	gone := httptest.NewServer(http.NotFoundHandler()) // an address that refuses connections
	closed := gone.URL
	gone.Close()
	post := func(want int, mut func(map[string]any)) string {
		b := map[string]any{"name": "C", "kind": "token", "server": f.srv.URL, "token": "good-token"}
		mut(b)
		return string(m.admin.expect(want, "POST", "/v1/k8s/clusters", b))
	}
	for name, c := range map[string]struct {
		mut  func(map[string]any)
		code int
		want string
	}{
		"bad token":      {func(b map[string]any) { b["token"] = "wrong" }, 422, "did not accept"},
		"no token":       {func(b map[string]any) { b["token"] = " " }, 400, "token"},
		"plain http":     {func(b map[string]any) { b["server"] = "http://example.com" }, 400, "https"},
		"metadata":       {func(b map[string]any) { b["server"] = "https://169.254.169.254" }, 400, "not a Kubernetes"},
		"bad kind":       {func(b map[string]any) { b["kind"] = "ssh" }, 400, "eks or token"},
		"bad env":        {func(b map[string]any) { b["env"] = "prod!" }, 400, "environment"},
		"no name":        {func(b map[string]any) { b["name"] = "" }, 400, "name"},
		"bad CA":         {func(b map[string]any) { b["ca"] = "not pem" }, 400, "PEM"},
		"unreachable":    {func(b map[string]any) { b["server"] = closed }, 422, "could not reach"},
		"eks no cluster": {func(b map[string]any) { b["kind"] = "eks" }, 400, "EKS cluster's name"},
	} {
		if msg := post(c.code, c.mut); !strings.Contains(msg, c.want) {
			t.Errorf("%s: %s", name, msg)
		}
	}
	var out struct{ Clusters []K8sClusterJSON }
	json.Unmarshal(m.v.expect(200, "GET", "/v1/k8s/clusters", nil), &out)
	if len(out.Clusters) != 0 {
		t.Errorf("a refused cluster was saved: %+v", out.Clusters)
	}
	if _, ok := m.srv.internalSecretValue(context.Background(), k8sTokenSecret("c")); ok {
		t.Error("a refused cluster left its token in the vault")
	}
}

func TestK8sExplorerShowsWhatRuns(t *testing.T) {
	m, f := k8sHarness(t)
	c := m.addCluster(m.admin, 201, "Shop", f)
	v := m.k8sView(c.ID, "")
	if v.Error != "" || len(v.Services) != 8 || v.Region != "Shop" {
		t.Fatalf("view = %+v", v)
	}
	by := map[string]AWSServiceJSON{}
	for _, s := range v.Services {
		by[s.ID] = s
	}
	nodes := by["k8s-nodes"]
	if nodes.Count != 2 || nodes.Resources[0].State != "Ready" || nodes.Resources[1].State != "NotReady" {
		t.Errorf("nodes = %+v", nodes.Resources)
	}
	dep := map[string]AWSResourceJSON{}
	for _, r := range by["k8s-deployments"].Resources {
		dep[r.Name] = r
	}
	if dep["shop/web"].State != "Down" || dep["default/worker"].State != "Degraded" || fieldOf(dep["shop/web"], "Ready") != "0/3" || fieldOf(dep["shop/web"], "Images") != "shop/web:latest" {
		t.Errorf("deployments = %+v", dep)
	}
	if ds := by["k8s-daemonsets"].Resources; len(ds) != 1 || ds[0].State != "Available" || fieldOf(ds[0], "Ready") != "2/2" {
		t.Errorf("daemonsets = %+v", ds)
	}
	var pod AWSResourceJSON
	for _, r := range by["k8s-pods"].Resources {
		if r.Name == "shop/api-1" {
			pod = r
		}
	}
	if pod.State != "CrashLoopBackOff" || fieldOf(pod, "Restarts") != "9" || fieldOf(pod, "Node") != "node-a" {
		t.Errorf("pod = %+v", pod)
	}
	var lb AWSResourceJSON
	for _, r := range by["k8s-services"].Resources {
		if r.Name == "shop/web" {
			lb = r
		}
	}
	if fieldOf(lb, "External") != "lb.example.com" || fieldOf(lb, "Type") != "LoadBalancer" || fieldOf(lb, "Ports") != "443/tcp" {
		t.Errorf("service = %+v", lb)
	}
	if ev := by["k8s-events"].Resources; len(ev) != 1 || ev[0].State != "Warning" || fieldOf(ev[0], "Message") == "" {
		t.Errorf("events = %+v", ev)
	}
	m.v.expect(404, "GET", "/v1/k8s/clusters/ghost/resources", nil)
}

func TestK8sOnlyEverReads(t *testing.T) {
	m, f := k8sHarness(t)
	c := m.addCluster(m.admin, 201, "Shop", f)
	m.k8sView(c.ID, "?refresh=1")
	m.scan(map[string]any{"account": "k8s:" + c.ID})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) < 9 {
		t.Fatalf("only %d requests", len(f.reqs))
	}
	for _, r := range f.reqs {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("the cluster was sent %q", r)
		}
	}
}

func TestK8sPartialAndTotalFailure(t *testing.T) {
	m, f := k8sHarness(t)
	c := m.addCluster(m.admin, 201, "Shop", f)
	f.mu.Lock()
	f.deny["/apis/apps/v1/deployments"] = 403
	f.mu.Unlock()
	v := m.k8sView(c.ID, "?refresh=1")
	var dep AWSServiceJSON
	for _, s := range v.Services {
		if s.ID == "k8s-deployments" {
			dep = s
		}
	}
	if !strings.Contains(dep.Error, "not allowed") || v.Error != "" || len(v.Services) != 8 {
		t.Errorf("one denied section should be named, not hide the rest: err=%q view.Error=%q", dep.Error, v.Error)
	}
	f.mu.Lock()
	f.deny = map[string]int{"/api": 403, "/apis": 403}
	f.mu.Unlock()
	if v := m.k8sView(c.ID, "?refresh=1"); !strings.Contains(v.Error, "not allowed") || len(v.Services) != 0 {
		t.Errorf("nothing readable should be one clear error: %+v", v)
	}
	f.mu.Lock()
	f.deny = map[string]int{}
	f.token = "rotated"
	f.mu.Unlock()
	if v := m.k8sView(c.ID, "?refresh=1"); !strings.Contains(v.Error, "did not accept") {
		t.Errorf("a revoked token: %+v", v)
	}
}

func TestK8sReadsAreCached(t *testing.T) {
	m, f := k8sHarness(t)
	c := m.addCluster(m.admin, 201, "Shop", f)
	m.k8sView(c.ID, "?refresh=1")
	n := f.count("/api/v1/pods")
	m.k8sView(c.ID, "")
	m.k8sView(c.ID, "")
	if got := f.count("/api/v1/pods"); got != n {
		t.Errorf("cached reads called the cluster again (%d -> %d)", n, got)
	}
	m.k8sView(c.ID, "?refresh=1")
	if got := f.count("/api/v1/pods"); got != n+1 {
		t.Errorf("refresh should read once more, got %d", got)
	}
}

func TestK8sRemoveClusterRemovesItsToken(t *testing.T) {
	m, f := k8sHarness(t)
	c := m.addCluster(m.admin, 201, "Shop", f)
	m.dev.expect(403, "DELETE", "/v1/k8s/clusters/"+c.ID, nil)
	m.admin.expect(204, "DELETE", "/v1/k8s/clusters/"+c.ID, nil)
	m.admin.expect(404, "DELETE", "/v1/k8s/clusters/"+c.ID, nil)
	if _, ok := m.srv.internalSecretValue(context.Background(), k8sTokenSecret(c.ID)); ok {
		t.Error("the token outlived its cluster")
	}
	m.v.expect(404, "GET", "/v1/k8s/clusters/"+c.ID+"/resources", nil)
}

func TestK8sEKSUsesTheAWSAccountAndCachesItsToken(t *testing.T) {
	m, f := k8sHarness(t)
	var tokenCalls int
	var mu sync.Mutex
	expires := time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)
	m.srv.run = func(_ context.Context, name string, args ...string) ([]byte, string, error) {
		key := strings.Join(args, " ")
		switch {
		case strings.Contains(key, "get-caller-identity"):
			return []byte(`{"Account":"111122223333","Arn":"arn:aws:sts::111122223333:assumed-role/Admin/ada","UserId":"x"}`), "", nil
		case strings.Contains(key, "eks describe-cluster --name prod-eks"):
			return []byte(`{"cluster":{"endpoint":"` + f.srv.URL + `","certificateAuthority":{"data":""}}}`), "", nil
		case strings.Contains(key, "eks get-token --cluster-name prod-eks"):
			mu.Lock()
			tokenCalls++
			mu.Unlock()
			return []byte(`{"kind":"ExecCredential","status":{"token":"good-token","expirationTimestamp":"` + expires + `"}}`), "", nil
		}
		return []byte(`{}`), "", nil
	}
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	var c K8sClusterJSON
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/k8s/clusters", map[string]any{"name": "Prod EKS", "kind": "eks", "cluster_name": "prod-eks", "region": "ap-south-1"}), &c)
	if c.Kind != "eks" || c.Server != f.srv.URL || c.Version != "v1.31.4" || c.Account != "primary" {
		t.Fatalf("cluster = %+v", c)
	}
	m.k8sView(c.ID, "?refresh=1")
	m.k8sView(c.ID, "?refresh=1")
	mu.Lock()
	got := tokenCalls
	mu.Unlock()
	if got != 1 {
		t.Errorf("a token good for 15 minutes was fetched %d times", got)
	}
	// An expired token is fetched again.
	m.srv.k8s.mu.Lock()
	m.srv.k8s.tokens[c.ID] = eksToken{token: "good-token", expires: time.Now().Add(10 * time.Second)}
	m.srv.k8s.mu.Unlock()
	m.k8sView(c.ID, "?refresh=1")
	mu.Lock()
	got = tokenCalls
	mu.Unlock()
	if got != 2 {
		t.Errorf("a token about to expire should be renewed, got %d fetches", got)
	}
	// Without the AWS account, say so.
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": false, "profile": "", "region": ""})
	m.srv.k8s.mu.Lock()
	delete(m.srv.k8s.tokens, c.ID)
	m.srv.k8s.mu.Unlock()
	if v := m.k8sView(c.ID, "?refresh=1"); !strings.Contains(v.Error, "not connected") {
		t.Errorf("view = %+v", v)
	}
}

func TestK8sGuardRules(t *testing.T) {
	m, f := k8sHarness(t)
	c := m.addCluster(m.admin, 201, "Shop", f)
	m.scan(map[string]any{"account": "k8s:" + c.ID})
	got := ruleSet(m.findings("?account=k8s:" + c.ID).Findings)
	want := map[string]string{
		"k8s-privileged|shop/Deployment/web/web": sevHigh, "k8s-host-namespace|shop/Deployment/web": sevHigh, "k8s-run-as-root|shop/Deployment/web/web": sevMedium,
		"k8s-unpinned-image|shop/Deployment/web/web": sevLow, "k8s-no-memory-limit|shop/Deployment/web/web": sevLow, "k8s-workload-down|shop/Deployment/web": sevHigh,
		"k8s-workload-degraded|default/Deployment/worker": sevMedium, "k8s-default-namespace|default/Deployment/worker": sevLow,
		"k8s-pod-failing|shop/api-1/api": sevMedium, "k8s-node-not-ready|node-b": sevHigh, "k8s-public-service|shop/web": sevInfo,
		"k8s-run-as-root|shop/StatefulSet/db/db": "", // runAsUser 999 is not root: must NOT be reported
	}
	for k, sev := range want {
		f, ok := got[k]
		if sev == "" {
			if ok {
				t.Errorf("reported %s but it is not a problem", k)
			}
			continue
		}
		if !ok || f.Severity != sev || f.Fix == "" || f.AccountName != "Shop" || f.Account != "k8s:"+c.ID {
			t.Errorf("%s = %+v (found %v)", k, f.GuardFinding, ok)
		}
	}
	for k := range got {
		if strings.Contains(k, "kube-system") || strings.Contains(k, "coredns") {
			t.Errorf("a system namespace was reported: %s", k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected finding %s", k)
		}
	}
	for _, clean := range []string{"agent", "worker/worker", "db/db"} {
		for k := range got {
			if strings.Contains(k, "k8s-privileged") && strings.Contains(k, clean) || strings.Contains(k, "k8s-unpinned") && strings.Contains(k, clean) {
				t.Errorf("clean workload reported: %s", k)
			}
		}
	}
	// Fix everything: findings resolve.
	f.mu.Lock()
	f.fixed = true
	f.mu.Unlock()
	m.scan(map[string]any{"account": "k8s:" + c.ID})
	open := ruleSet(m.findings("?account=k8s:" + c.ID).Findings)
	for _, gone := range []string{"k8s-privileged|shop/Deployment/web/web", "k8s-workload-down|shop/Deployment/web", "k8s-pod-failing|shop/api-1/api", "k8s-node-not-ready|node-b", "k8s-host-namespace|shop/Deployment/web"} {
		if _, still := open[gone]; still {
			t.Errorf("%s is fixed but still open", gone)
		}
	}
	if len(m.findings("?status=resolved&account=k8s:"+c.ID).Findings) == 0 {
		t.Error("nothing was resolved")
	}
}

func TestK8sGuardKeepsFindingsItCouldNotRead(t *testing.T) {
	m, f := k8sHarness(t)
	c := m.addCluster(m.admin, 201, "Shop", f)
	m.scan(map[string]any{"account": "k8s:" + c.ID})
	before := len(m.findings("?account=k8s:" + c.ID).Findings)
	f.mu.Lock()
	f.fixed = true
	f.deny["/apis/apps/v1/deployments"] = 403
	f.deny["/api/v1/nodes"] = 403
	f.mu.Unlock()
	sc := m.scan(map[string]any{"account": "k8s:" + c.ID})
	open := ruleSet(m.findings("?account=k8s:" + c.ID).Findings)
	if _, ok := open["k8s-privileged|shop/Deployment/web/web"]; !ok {
		t.Error("a deployment finding was resolved although deployments could not be read")
	}
	if _, ok := open["k8s-node-not-ready|node-b"]; !ok {
		t.Error("a node finding was resolved although nodes could not be read")
	}
	if _, ok := open["k8s-pod-failing|shop/api-1/api"]; ok {
		t.Error("pods could be read and the failing pod is fixed, so that finding should resolve")
	}
	if len(sc.Accounts[0].Skipped) != 2 || len(m.findings("?account=k8s:"+c.ID).Findings) >= before+1 {
		t.Errorf("skipped = %v", sc.Accounts[0].Skipped)
	}
}

func TestK8sLookups(t *testing.T) {
	m, f := k8sHarness(t)
	c := m.addCluster(m.admin, 201, "Shop", f)
	m.ask(m.dev, lookupReply("k8s_clusters", map[string]any{}))
	if res := m.llm.lastUser(1); !strings.Contains(res, `id "shop"`) || !strings.Contains(res, "v1.31.4") || strings.Contains(res, "good-token") {
		t.Errorf("k8s_clusters: %s", res)
	}
	m.ask(m.dev, lookupReply("k8s_resources", map[string]any{"cluster": c.ID, "kind": "deployments"}))
	res := m.llm.lastUser(1)
	if !strings.Contains(res, "- shop/web [Down]") || !strings.Contains(res, "Ready: 0/3") || strings.Contains(res, "Pods") {
		t.Errorf("k8s_resources: %s", res)
	}
	m.ask(m.dev, lookupReply("k8s_resources", map[string]any{"cluster": "ghost"}))
	if !strings.Contains(m.llm.lastUser(1), "there is no cluster") {
		t.Errorf("unknown cluster: %s", m.llm.lastUser(1))
	}
	m.ask(m.dev, lookupReply("k8s_resources", map[string]any{}))
	if !strings.Contains(m.llm.lastUser(1), "say which cluster") {
		t.Errorf("no cluster: %s", m.llm.lastUser(1))
	}
}

func TestImageUnpinnedAndRootRules(t *testing.T) {
	for image, want := range map[string]bool{"nginx": true, "nginx:latest": true, "nginx:1.27": false, "reg.io:5000/app": true, "reg.io:5000/app:2": false, "app@sha256:abc": false, "reg.io:5000/app:latest": true} {
		if imageUnpinned(image) != want {
			t.Errorf("imageUnpinned(%q) = %v", image, !want)
		}
	}
	yes, no, zero := true, false, int64(0)
	nonzero := int64(1000)
	cases := []struct {
		c    kContainer
		pod  kPodSpec
		root bool
	}{
		{kContainer{}, kPodSpec{}, true},
		{kContainer{SecurityContext: &kSecCtx{RunAsNonRoot: &yes}}, kPodSpec{}, false},
		{kContainer{}, kPodSpec{SecurityContext: &kSecCtx{RunAsNonRoot: &yes}}, false},
		{kContainer{SecurityContext: &kSecCtx{RunAsUser: &nonzero}}, kPodSpec{}, false},
		{kContainer{SecurityContext: &kSecCtx{RunAsUser: &zero}}, kPodSpec{}, true},
		{kContainer{SecurityContext: &kSecCtx{RunAsNonRoot: &no}}, kPodSpec{}, true},
	}
	for i, c := range cases {
		if isRoot(c.c, c.pod) != c.root {
			t.Errorf("case %d: isRoot = %v", i, !c.root)
		}
	}
}
