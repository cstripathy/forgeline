package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeWorkload is the one Deployment the fake API server lets a test change: shop/web.
type fakeWorkload struct {
	replicas   int
	image      string
	restarted  string
	generation int
	version    int
	patchDeny  int // answer every PATCH with this status
	patches    []string
}

const fakeWebPath = "/apis/apps/v1/namespaces/shop/deployments/web"

func (f *fakeK8s) setWorkload(replicas int, image string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wl = fakeWorkload{replicas: replicas, image: image, generation: 7, version: 100}
}

func (f *fakeK8s) workloadNow() fakeWorkload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wl
}

// touch is a change made behind Forgeline's back: it bumps the generation.
func (f *fakeK8s) touch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wl.generation++
	f.wl.version++
}

func (f *fakeK8s) realPatches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.wl.patches {
		if strings.HasPrefix(p, "dry=false") {
			n++
		}
	}
	return n
}

// workload answers GET and PATCH for shop/web and reports whether the path was its own.
func (f *fakeK8s) workload(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != fakeWebPath {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.wl.generation == 0 {
		f.wl = fakeWorkload{replicas: 3, image: "shop/web:1.2", generation: 7, version: 100}
	}
	wl := f.wl
	if r.Method == http.MethodPatch {
		dry := r.URL.Query().Get("dryRun") == "All"
		raw, _ := io.ReadAll(r.Body)
		f.wl.patches = append(f.wl.patches, fmt.Sprintf("dry=%v %s %s", dry, r.Header.Get("Content-Type"), raw))
		if f.wl.patchDeny != 0 {
			http.Error(w, `{"message":"forbidden"}`, f.wl.patchDeny)
			return true
		}
		var p struct {
			Metadata struct{ ResourceVersion string }
			Spec     struct {
				Replicas *int
				Template struct {
					Metadata struct{ Annotations map[string]string }
					Spec     struct {
						Containers []struct{ Name, Image string }
					}
				}
			}
		}
		json.Unmarshal(raw, &p)
		if !dry && p.Metadata.ResourceVersion != "" && p.Metadata.ResourceVersion != fmt.Sprint(wl.version) {
			http.Error(w, `{"message":"conflict"}`, http.StatusConflict)
			return true
		}
		if p.Spec.Replicas != nil {
			wl.replicas = *p.Spec.Replicas
		}
		for _, c := range p.Spec.Template.Spec.Containers {
			if c.Name == "web" {
				wl.image = c.Image
			}
		}
		if v, ok := p.Spec.Template.Metadata.Annotations[restartAnnotation]; ok {
			wl.restarted = v
		}
		if !dry {
			wl.generation++
			wl.version++
			wl.patches, wl.patchDeny = f.wl.patches, f.wl.patchDeny
			f.wl = wl
		}
	}
	ann := ""
	if wl.restarted != "" {
		ann = fmt.Sprintf(`"annotations":{%q:%q}`, restartAnnotation, wl.restarted)
	}
	fmt.Fprintf(w, `{"metadata":{"name":"web","namespace":"shop","generation":%d,"resourceVersion":"%d"},"spec":{"replicas":%d,"template":{"metadata":{%s},"spec":{"containers":[{"name":"web","image":%q}]}}},"status":{"readyReplicas":3}}`,
		wl.generation, wl.version, wl.replicas, ann, wl.image)
	return true
}

func (m *mcpHarness) k8sChange(b *browser, want int, cluster string, req map[string]any) CloudChange {
	m.t.Helper()
	body := map[string]any{"cluster": cluster, "namespace": "shop", "workload": "deployment/web", "reason": "The shop is busy this week"}
	for k, v := range req {
		body[k] = v
	}
	var ch CloudChange
	json.Unmarshal(b.expect(want, "POST", "/v1/cloud/changes", body), &ch)
	return ch
}

func (m *mcpHarness) waitK8sChange(id string, notIn ...string) CloudChange {
	m.t.Helper()
	var ch CloudChange
	m.waitFor("change "+id+" to settle", func() bool {
		_, b := m.admin.do("GET", "/v1/cloud/changes/"+id, nil)
		json.Unmarshal(b, &ch)
		for _, s := range notIn {
			if ch.Status == s {
				return false
			}
		}
		return true
	})
	return ch
}

func TestK8sScaleFlowDryRunsFirstAndNeedsAnotherApprover(t *testing.T) {
	m, f := k8sHarness(t)
	f.setWorkload(3, "shop/web:1.2")
	c := m.addCluster(m.admin, 201, "Shop", f) // note: AWS is not connected at all
	ch := m.k8sChange(m.dev, 202, c.ID, map[string]any{"kind": "k8s-scale", "replicas": 5})
	ch = m.waitK8sChange(ch.ID, "preparing")
	if ch.Status != "proposed" || len(ch.Preview) != 1 || !strings.Contains(ch.Preview[0].Detail, "replicas 3 → 5") || ch.ClusterName != "Shop" {
		t.Fatalf("change = %+v", ch)
	}
	if f.realPatches() != 0 || f.workloadNow().replicas != 3 {
		t.Fatalf("the preview must not change the cluster: %+v", f.workloadNow())
	}
	if !strings.Contains(f.workloadNow().patches[0], "dry=true") {
		t.Errorf("patches = %v", f.workloadNow().patches)
	}
	// The proposer, even an administrator, cannot approve their own change.
	own := m.k8sChange(m.admin, 202, c.ID, map[string]any{"kind": "k8s-scale", "replicas": 4, "workload": "deployment/web"})
	m.waitK8sChange(own.ID, "preparing")
	m.admin.expect(403, "POST", "/v1/cloud/changes/"+own.ID+"/approve", map[string]any{})
	m.dev.expect(403, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]any{})
	m.admin.expect(202, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]any{"note": "ok for the sale"})
	done := m.waitK8sChange(ch.ID, "proposed", "executing")
	if done.Status != "succeeded" || !strings.Contains(done.Result, "5 replicas") || f.workloadNow().replicas != 5 || f.realPatches() != 1 {
		t.Fatalf("done = %+v, cluster = %+v", done, f.workloadNow())
	}
	// The real patch carries the version just read, so a change in between is refused by the cluster.
	if last := f.workloadNow().patches; !strings.Contains(last[len(last)-1], `"resourceVersion":"100"`) || !strings.Contains(last[len(last)-1], "application/merge-patch+json") {
		t.Errorf("patches = %v", last)
	}
	raw := m.admin.expect(200, "GET", "/v1/cloud/changes/"+ch.ID, nil)
	if strings.Contains(string(raw), "good-token") {
		t.Errorf("the cluster token is in the change: %s", raw)
	}
	if log := string(m.admin.expect(200, "GET", "/v1/audit?limit=100", nil)); !strings.Contains(log, "cloud.execute") || !strings.Contains(log, "k8s/"+c.ID) || strings.Contains(log, "good-token") {
		t.Errorf("audit log: %s", log)
	}
}

func TestK8sProposalRules(t *testing.T) {
	m, f := k8sHarness(t)
	c := m.addCluster(m.admin, 201, "Shop", f)
	bad := func(want int, req map[string]any) {
		t.Helper()
		m.k8sChange(m.dev, want, c.ID, req)
	}
	bad(400, map[string]any{"kind": "k8s-scale"})                                       // no replicas
	bad(400, map[string]any{"kind": "k8s-scale", "replicas": -1})                       //
	bad(400, map[string]any{"kind": "k8s-scale", "replicas": 101})                      //
	bad(400, map[string]any{"kind": "k8s-scale", "replicas": 2, "reason": "no"})        // reason too short
	bad(400, map[string]any{"kind": "k8s-scale", "replicas": 2, "workload": "pod/web"}) // only deployments and statefulsets
	bad(400, map[string]any{"kind": "k8s-scale", "replicas": 2, "workload": "deployment/../x"})
	bad(400, map[string]any{"kind": "k8s-scale", "replicas": 2, "namespace": "Shop"})
	bad(400, map[string]any{"kind": "k8s-image", "container": "web", "image": "bad image"})
	bad(400, map[string]any{"kind": "k8s-image", "container": "", "image": "shop/web:2"})
	bad(400, map[string]any{"kind": "k8s-delete"})
	bad(403, map[string]any{"kind": "k8s-scale", "replicas": 2, "namespace": "kube-system"})
	bad(403, map[string]any{"kind": "k8s-restart", "namespace": "kube-public"})
	bad(403, map[string]any{"kind": "k8s-restart", "namespace": "kube-node-lease"})
	m.k8sChange(m.dev, 404, "ghost", map[string]any{"kind": "k8s-restart"})
	m.k8sChange(m.v, 403, c.ID, map[string]any{"kind": "k8s-restart"}) // a viewer cannot propose
	if f.count("PATCH") != 0 {
		t.Errorf("a refused proposal reached the cluster: %v", f.reqs)
	}
}

func TestK8sRestartAndImagePreviews(t *testing.T) {
	m, f := k8sHarness(t)
	f.setWorkload(3, "shop/web:1.2")
	c := m.addCluster(m.admin, 201, "Shop", f)

	r := m.waitK8sChange(m.k8sChange(m.dev, 202, c.ID, map[string]any{"kind": "k8s-restart"}).ID, "preparing")
	if r.Status != "proposed" || len(r.Preview) != 1 || r.Preview[0].Action != "Restart" {
		t.Fatalf("restart = %+v", r)
	}
	m.admin.expect(202, "POST", "/v1/cloud/changes/"+r.ID+"/approve", map[string]any{})
	if d := m.waitK8sChange(r.ID, "proposed", "executing"); d.Status != "succeeded" || f.workloadNow().restarted == "" {
		t.Fatalf("restart done = %+v / %+v", d, f.workloadNow())
	}

	img := m.waitK8sChange(m.k8sChange(m.dev, 202, c.ID, map[string]any{"kind": "k8s-image", "container": "web", "image": "shop/web:1.5"}).ID, "preparing")
	if img.Status != "proposed" || !strings.Contains(img.Preview[0].Detail, "shop/web:1.2 → shop/web:1.5") || !strings.Contains(img.Preview[0].Detail, "not pinned by digest") {
		t.Fatalf("image = %+v", img)
	}
	pinned := m.waitK8sChange(m.k8sChange(m.dev, 202, c.ID, map[string]any{"kind": "k8s-image", "container": "web", "image": "shop/web@sha256:" + strings.Repeat("a", 64)}).ID, "preparing")
	if pinned.Status != "proposed" || strings.Contains(pinned.Preview[0].Detail, "not pinned") {
		t.Fatalf("pinned = %+v", pinned)
	}
	m.admin.expect(202, "POST", "/v1/cloud/changes/"+img.ID+"/approve", map[string]any{})
	if d := m.waitK8sChange(img.ID, "proposed", "executing"); d.Status != "succeeded" || f.workloadNow().image != "shop/web:1.5" {
		t.Fatalf("image done = %+v / %+v", d, f.workloadNow())
	}
}

func TestK8sPreviewFailuresSaySo(t *testing.T) {
	m, f := k8sHarness(t)
	f.setWorkload(3, "shop/web:1.2")
	c := m.addCluster(m.admin, 201, "Shop", f)
	fail := func(want string, req map[string]any) {
		t.Helper()
		ch := m.waitK8sChange(m.k8sChange(m.dev, 202, c.ID, req).ID, "preparing")
		if ch.Status != "failed" || !strings.Contains(ch.Result, want) {
			t.Errorf("%v: %+v, want %q", req, ch, want)
		}
	}
	fail("nothing would change", map[string]any{"kind": "k8s-scale", "replicas": 3})
	fail(`no container named "sidecar"`, map[string]any{"kind": "k8s-image", "container": "sidecar", "image": "x/y:1"})
	fail("no deployment/ghost", map[string]any{"kind": "k8s-restart", "workload": "deployment/ghost"})
	f.mu.Lock()
	f.wl.patchDeny = http.StatusForbidden
	f.mu.Unlock()
	fail("patch verb", map[string]any{"kind": "k8s-restart"})
	if f.realPatches() != 0 {
		t.Error("a failed preview changed the cluster")
	}
}

func TestK8sChangeIsRefusedIfTheWorkloadMovedAfterThePreview(t *testing.T) {
	m, f := k8sHarness(t)
	f.setWorkload(3, "shop/web:1.2")
	c := m.addCluster(m.admin, 201, "Shop", f)
	ch := m.waitK8sChange(m.k8sChange(m.dev, 202, c.ID, map[string]any{"kind": "k8s-scale", "replicas": 6}).ID, "preparing")
	f.touch() // someone edits the deployment by hand
	m.admin.expect(202, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]any{})
	d := m.waitK8sChange(ch.ID, "proposed", "executing")
	if d.Status != "failed" || !strings.Contains(d.Result, "changed after the preview") || f.realPatches() != 0 {
		t.Fatalf("done = %+v, patches = %v", d, f.workloadNow().patches)
	}
}

func TestK8sApprovalNeedsTheSameClusterAndOneChangeAtATime(t *testing.T) {
	m, f := k8sHarness(t)
	f.setWorkload(3, "shop/web:1.2")
	c := m.addCluster(m.admin, 201, "Shop", f)
	ch := m.waitK8sChange(m.k8sChange(m.dev, 202, c.ID, map[string]any{"kind": "k8s-scale", "replicas": 6}).ID, "preparing")
	m.admin.expect(204, "DELETE", "/v1/k8s/clusters/"+c.ID, nil)
	m.admin.expect(409, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]any{})
	// A viewer may read the proposal but never decide it.
	m.v.expect(200, "GET", "/v1/cloud/changes/"+ch.ID, nil)
	m.v.expect(403, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]any{})
}

func TestK8sClientPatchesOnlyWorkloadPaths(t *testing.T) {
	f := newFakeK8s(t)
	conn, err := newK8sConn(f.srv.URL, "", func(context.Context) (string, error) { return "good-token", nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/namespaces/shop/secrets/x", "/api/v1/namespaces/shop/pods/api-1", "/apis/apps/v1/namespaces/shop/daemonsets/agent",
		"/apis/apps/v1/namespaces/shop/deployments/web/scale", "/apis/apps/v1/namespaces/shop/deployments/../secrets", "/apis/apps/v1/deployments",
		"/apis/apps/v1/namespaces/shop/deployments/web?x=1", "/apis/rbac.authorization.k8s.io/v1/clusterroles/admin",
	} {
		var out map[string]any
		if err := conn.patch(context.Background(), path, "application/merge-patch+json", []byte(`{}`), false, &out); err == nil {
			t.Errorf("%s was accepted", path)
		}
	}
	if f.count("PATCH") != 0 {
		t.Errorf("a refused path reached the server: %v", f.reqs)
	}
	var out map[string]any
	if err := conn.patch(context.Background(), fakeWebPath, "application/merge-patch+json", []byte(`{}`), true, &out); err != nil {
		t.Errorf("a workload path was refused: %v", err)
	}
}

func TestK8sExecutionRefusesAChangedClusterAddress(t *testing.T) {
	m, f := k8sHarness(t)
	f.setWorkload(3, "shop/web:1.2")
	c := m.addCluster(m.admin, 201, "Shop", f)
	ch := m.waitK8sChange(m.k8sChange(m.dev, 202, c.ID, map[string]any{"kind": "k8s-scale", "replicas": 6}).ID, "preparing")
	ch.Params["server"] = "https://elsewhere.example.com"
	if _, err := m.srv.executeK8s(context.Background(), ch); err == nil || !strings.Contains(err.Error(), "address changed") || f.realPatches() != 0 {
		t.Fatalf("err = %v, patches = %v", err, f.workloadNow().patches)
	}
}
