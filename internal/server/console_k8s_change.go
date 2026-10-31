package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/forgelinehq/forgeline/internal/authn"
)

// Kubernetes changes (note 017, slice 5, second half). A change is a proposal like any cloud change:
// a developer proposes it, the preview is the cluster's own server-side dry run, and a different
// administrator approves it. Three operations exist and no others: scale, restart and set an image,
// on a Deployment or StatefulSet in a namespace that is not the cluster's own. The client's patch
// method accepts only those workload paths.

const (
	kindK8sScale   = "k8s-scale"
	kindK8sRestart = "k8s-restart"
	kindK8sImage   = "k8s-image"

	maxK8sReplicas = 100
)

var (
	k8sWorkloadPath = regexp.MustCompile(`^/apis/apps/v1/namespaces/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/(deployments|statefulsets)/[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	k8sNameRE       = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	k8sNamespaceRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	k8sImageRE      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,254}$`)
	k8sContainerRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// protectedNamespace is where the cluster keeps itself. Changes there are done by hand.
func protectedNamespace(ns string) bool {
	return ns == "kube-system" || ns == "kube-public" || ns == "kube-node-lease"
}

func (c CloudChange) isK8s() bool { return strings.HasPrefix(c.Kind, "k8s-") }

// patch sends one merge patch to a workload. With dryRun the API server runs the request through
// admission and validation and stores nothing.
func (c k8sConn) patch(ctx context.Context, path, contentType string, body []byte, dryRun bool, out any) error {
	if !k8sWorkloadPath.MatchString(path) {
		return errors.New("refusing to change anything but a Deployment or StatefulSet")
	}
	u := c.server + path
	if dryRun {
		u += "?dryRun=All"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the cluster: %w", errors.Unwrap(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxK8sBody))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("the cluster did not accept the credentials")
	case resp.StatusCode == http.StatusForbidden:
		return errors.New("the cluster's credentials are not allowed to change this workload; the service account needs the patch verb on deployments and statefulsets")
	case resp.StatusCode == http.StatusNotFound:
		return errors.New("the workload was not found")
	case resp.StatusCode == http.StatusConflict:
		return errors.New("the workload changed while this was being applied; propose it again")
	case resp.StatusCode >= 300:
		var st struct{ Message string }
		json.Unmarshal(data, &st)
		return fmt.Errorf("the cluster refused the change (%d): %s", resp.StatusCode, truncate(st.Message, 300))
	}
	return json.Unmarshal(data, out)
}

type kWorkloadDoc struct {
	Metadata struct {
		Name            string
		Namespace       string
		ResourceVersion string
		Generation      int64
	}
	Spec struct {
		Replicas *int
		Template struct {
			Metadata struct{ Annotations map[string]string }
			Spec     kPodSpec
		}
	}
	Status struct{ ReadyReplicas int }
}

func (d kWorkloadDoc) replicas() int {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

// splitWorkload turns "deployment/web" into the API collection and name.
func splitWorkload(w string) (collection, name string, ok bool) {
	kind, name, found := strings.Cut(w, "/")
	if !found || !k8sNameRE.MatchString(name) {
		return "", "", false
	}
	switch kind {
	case "deployment":
		return "deployments", name, true
	case "statefulset":
		return "statefulsets", name, true
	}
	return "", "", false
}

func workloadPath(ns, w string) (string, bool) {
	coll, name, ok := splitWorkload(w)
	if !ok || !k8sNamespaceRE.MatchString(ns) {
		return "", false
	}
	return "/apis/apps/v1/namespaces/" + ns + "/" + coll + "/" + name, true
}

const restartAnnotation = "kubectl.kubernetes.io/restartedAt"

// k8sPatch builds the patch for a change against the workload as it is now.
func k8sPatch(c CloudChange, now time.Time) (contentType string, body []byte, err error) {
	switch c.Kind {
	case kindK8sScale:
		body, err = json.Marshal(map[string]any{"spec": map[string]any{"replicas": c.Replicas}})
		return "application/merge-patch+json", body, err
	case kindK8sRestart:
		body, err = json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]string{restartAnnotation: now.UTC().Format(time.RFC3339)}}}}})
		return "application/merge-patch+json", body, err
	case kindK8sImage:
		body, err = json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []map[string]string{{"name": c.Container, "image": c.Image}}}}}})
		return "application/strategic-merge-patch+json", body, err
	}
	return "", nil, errors.New("unknown change")
}

func (s *Server) k8sConnFor(ctx context.Context, id string) (K8sCluster, k8sConn, error) {
	cl, err := s.loadK8sCluster(ctx, id)
	if err != nil {
		return K8sCluster{}, k8sConn{}, errors.New("that cluster is no longer registered")
	}
	conn, err := s.connFor(ctx, cl)
	return cl, conn, err
}

// proposeK8s validates a Kubernetes change and prepares its preview in the background.
func (s *Server) proposeK8s(ctx context.Context, p Principal, ip string, req proposeRequest) (CloudChange, *httpErr) {
	req.Reason = strings.TrimSpace(req.Reason)
	if len(req.Reason) < 5 || len(req.Reason) > 500 {
		return CloudChange{}, refusal(http.StatusBadRequest, "say why in 5 to 500 characters; the approver reads it")
	}
	cl, err := s.loadK8sCluster(ctx, req.Cluster)
	if err != nil {
		return CloudChange{}, refusal(http.StatusNotFound, "there is no such cluster")
	}
	if _, ok := workloadPath(req.Namespace, req.Workload); !ok {
		return CloudChange{}, refusal(http.StatusBadRequest, "give a namespace and a workload such as deployment/web or statefulset/db")
	}
	c := CloudChange{ID: authn.NewID(), Kind: req.Kind, Status: stPreparing, Reason: req.Reason, Preview: []ChangePreview{},
		Cluster: cl.ID, ClusterName: cl.Name, Namespace: req.Namespace, Workload: req.Workload,
		Params: map[string]string{"server": cl.Server}, ProposedBy: p.Name, ProposedAt: time.Now().UTC()}
	if protectedNamespace(req.Namespace) {
		_ = s.audit(ctx, p, ip, "cloud.propose", c.target(), "refused: protected namespace", false)
		return CloudChange{}, refusal(http.StatusForbidden, "the cluster's own namespaces can't be changed from the console")
	}
	switch req.Kind {
	case kindK8sScale:
		if req.Replicas == nil || *req.Replicas < 0 || *req.Replicas > maxK8sReplicas {
			return CloudChange{}, refusal(http.StatusBadRequest, fmt.Sprintf("replicas is a number from 0 to %d", maxK8sReplicas))
		}
		n := *req.Replicas
		c.Replicas = &n
		c.Title = fmt.Sprintf("Scale %s in %s (%s) to %d", req.Workload, req.Namespace, cl.Name, n)
	case kindK8sRestart:
		c.Title = fmt.Sprintf("Restart %s in %s (%s)", req.Workload, req.Namespace, cl.Name)
	case kindK8sImage:
		if !k8sContainerRE.MatchString(req.Container) {
			return CloudChange{}, refusal(http.StatusBadRequest, "give the container name")
		}
		if !k8sImageRE.MatchString(req.Image) {
			return CloudChange{}, refusal(http.StatusBadRequest, "that is not an image reference such as registry/app:1.4 or registry/app@sha256:…")
		}
		c.Container, c.Image = req.Container, req.Image
		c.Title = fmt.Sprintf("Set %s of %s in %s (%s) to %s", req.Container, req.Workload, req.Namespace, cl.Name, req.Image)
	default:
		return CloudChange{}, refusal(http.StatusBadRequest, "kind must be k8s-scale, k8s-restart or k8s-image")
	}
	return s.storeAndPrepare(ctx, p, ip, c, s.prepareK8s)
}

// prepareK8s reads the workload, asks the API server to dry-run the change and records the difference.
func (s *Server) prepareK8s(ctx context.Context, c *CloudChange) error {
	_, conn, err := s.k8sConnFor(ctx, c.Cluster)
	if err != nil {
		return err
	}
	path, _ := workloadPath(c.Namespace, c.Workload)
	var cur kWorkloadDoc
	if err := conn.get(ctx, path, &cur); errors.Is(err, errK8sNone) {
		return fmt.Errorf("there is no %s in the namespace %s", c.Workload, c.Namespace)
	} else if err != nil {
		return err
	}
	if c.Kind == kindK8sImage {
		found := false
		for _, k := range cur.Spec.Template.Spec.Containers {
			found = found || k.Name == c.Container
		}
		if !found {
			return fmt.Errorf("%s has no container named %q", c.Workload, c.Container)
		}
	}
	ct, body, err := k8sPatch(*c, time.Now())
	if err != nil {
		return err
	}
	var after kWorkloadDoc
	if err := conn.patch(ctx, path, ct, body, true, &after); err != nil {
		return err
	}
	res := c.Workload + " in " + c.Namespace
	typ := "apps/v1 " + map[string]string{"deployment": "Deployment", "statefulset": "StatefulSet"}[strings.SplitN(c.Workload, "/", 2)[0]]
	if after.replicas() != cur.replicas() {
		detail := fmt.Sprintf("replicas %d → %d (%d ready now)", cur.replicas(), after.replicas(), cur.Status.ReadyReplicas)
		if after.replicas() == 0 {
			detail += ". Nothing will be serving"
		}
		c.Preview = append(c.Preview, ChangePreview{Action: "Modify", Resource: res, Type: typ, Detail: detail})
	}
	was := map[string]string{}
	for _, k := range cur.Spec.Template.Spec.Containers {
		was[k.Name] = k.Image
	}
	for _, k := range after.Spec.Template.Spec.Containers {
		if was[k.Name] != k.Image {
			detail := fmt.Sprintf("container %s: %s → %s", k.Name, was[k.Name], k.Image)
			if !strings.Contains(k.Image, "@sha256:") {
				detail += " (not pinned by digest)"
			}
			c.Preview = append(c.Preview, ChangePreview{Action: "Modify", Resource: res, Type: typ, Detail: detail})
		}
	}
	if after.Spec.Template.Metadata.Annotations[restartAnnotation] != cur.Spec.Template.Metadata.Annotations[restartAnnotation] {
		c.Preview = append(c.Preview, ChangePreview{Action: "Restart", Resource: res, Type: typ, Detail: fmt.Sprintf("every pod is replaced, %d now", cur.replicas())})
	}
	if len(c.Preview) == 0 {
		return errors.New("the workload already looks like this; nothing would change")
	}
	c.PreviewNote = "The cluster's own dry run accepted this. Nothing has been stored in the cluster."
	c.Params["generation"] = fmt.Sprint(cur.Metadata.Generation)
	return nil
}

// executeK8s applies an approved change. The workload must be the one the approver saw: if its spec
// changed since the preview, nothing is sent.
func (s *Server) executeK8s(ctx context.Context, c CloudChange) (string, error) {
	cl, conn, err := s.k8sConnFor(ctx, c.Cluster)
	if err != nil {
		return "", err
	}
	if cl.Server != c.Params["server"] {
		return "", errors.New("the cluster's address changed since this was proposed; propose it again")
	}
	path, _ := workloadPath(c.Namespace, c.Workload)
	var cur kWorkloadDoc
	if err := conn.get(ctx, path, &cur); err != nil {
		return "", err
	}
	if fmt.Sprint(cur.Metadata.Generation) != c.Params["generation"] {
		return "", errors.New("the workload's spec changed after the preview was made; nothing was applied. Propose it again")
	}
	ct, body, err := k8sPatch(c, time.Now())
	if err != nil {
		return "", err
	}
	// Carry the version just read so a change slipping in between is refused by the cluster.
	var m map[string]any
	json.Unmarshal(body, &m)
	m["metadata"] = map[string]any{"resourceVersion": cur.Metadata.ResourceVersion}
	body, _ = json.Marshal(m)
	var after kWorkloadDoc
	if err := conn.patch(ctx, path, ct, body, false, &after); err != nil {
		return "", err
	}
	return fmt.Sprintf("Applied to %s in %s (%s). It now wants %d replicas.", c.Workload, c.Namespace, c.ClusterName, after.replicas()), nil
}
