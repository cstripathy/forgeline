package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Kubernetes (note 017, slice 5). Clusters are registered by an administrator and read through the
// Kubernetes API with a client that can only send GET requests to paths fixed in this file. An EKS
// cluster uses a connected AWS account (aws eks get-token, nothing stored); any other cluster uses a
// service-account token kept in the vault. The explorer reuses the AWS explorer's view, and Guard
// reads the same data for security findings. Nothing here changes a cluster.

const (
	kindK8sCluster = "k8scluster"
	maxK8sClusters = 30
	k8sTTL         = 60 * time.Second
	k8sTimeout     = 40 * time.Second
	k8sPageLimit   = 500
	maxK8sBody     = 32 << 20
)

// K8sCluster is a registered cluster. The token, when there is one, is in the vault.
type K8sCluster struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Env         string    `json:"env,omitempty"`
	Kind        string    `json:"kind"`                   // eks or token
	Account     string    `json:"account,omitempty"`      // eks: the AWS account (console id) holding the cluster
	ClusterName string    `json:"cluster_name,omitempty"` // eks
	Region      string    `json:"region,omitempty"`       // eks
	Server      string    `json:"server"`
	CA          string    `json:"ca,omitempty"` // PEM
	Version     string    `json:"version,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
}

func k8sTokenSecret(id string) string { return "forgeline.k8s." + id + ".token" }

// validK8sServer keeps the server address to something that is plainly a cluster: https, or http
// only on this machine (for a local cluster), and never a link-local or metadata address.
func validK8sServer(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New("the API server address must look like https://host:6443")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) || strings.HasPrefix(strings.ToLower(host), "fd00:ec2") || host == "metadata.google.internal" {
		return errors.New("that address is not a Kubernetes API server")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("plain http is only allowed for a cluster on this machine; use https")
		}
	default:
		return errors.New("the API server address must start with https://")
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" {
		return errors.New("give the address without a path, such as https://host:6443")
	}
	return nil
}

// k8sConn is a way to read one cluster. It has no method that writes.
type k8sConn struct {
	server string
	http   *http.Client
	token  func(ctx context.Context) (string, error)
	reads  *int64 // test hook: counts requests; nil in production
}

func newK8sConn(server, ca string, token func(context.Context) (string, error)) (k8sConn, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 20 * time.Second,
		DialContext: (&net.Dialer{Timeout: 8 * time.Second}).DialContext} // a mistyped address should fail in seconds, not at the overall timeout
	if strings.TrimSpace(ca) != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return k8sConn{}, errors.New("the certificate authority is not valid PEM")
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return k8sConn{server: strings.TrimRight(server, "/"), token: token, http: &http.Client{Transport: tr, Timeout: k8sTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

var errK8sNone = errors.New("this cluster does not offer it")

// get reads one fixed API path into out.
func (c k8sConn) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server+path, nil)
	if err != nil {
		return err
	}
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the cluster: %w", errors.Unwrap(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxK8sBody))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("the cluster did not accept the credentials")
	case resp.StatusCode == http.StatusForbidden:
		return errors.New("the credentials are not allowed to read this")
	case resp.StatusCode == http.StatusNotFound:
		return errK8sNone
	case resp.StatusCode >= 300:
		return fmt.Errorf("the cluster answered %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

type eksToken struct {
	token   string
	expires time.Time
}

type k8sEngine struct {
	tokenMu sync.Mutex // one token fetch at a time, so parallel reads share it
	mu      sync.Mutex
	tokens  map[string]eksToken
	cache   map[string]k8sEntry
}

type k8sEntry struct {
	at   time.Time
	raw  k8sRaw
	view AWSInventoryJSON
}

// connFor builds the connection for a registered cluster.
func (s *Server) connFor(ctx context.Context, c K8sCluster) (k8sConn, error) {
	switch c.Kind {
	case "token":
		tok, ok := s.internalSecretValue(ctx, k8sTokenSecret(c.ID))
		if !ok {
			return k8sConn{}, errors.New("the cluster's token is missing from the vault")
		}
		return newK8sConn(c.Server, c.CA, func(context.Context) (string, error) { return tok, nil })
	case "eks":
		return newK8sConn(c.Server, c.CA, func(ctx context.Context) (string, error) { return s.eksToken(ctx, c) })
	}
	return k8sConn{}, errors.New("unknown cluster kind")
}

// eksToken asks the AWS CLI for a short-lived token and keeps it until a minute before it expires.
func (s *Server) eksToken(ctx context.Context, c K8sCluster) (string, error) {
	s.k8s.tokenMu.Lock()
	defer s.k8s.tokenMu.Unlock()
	s.k8s.mu.Lock()
	if t, ok := s.k8s.tokens[c.ID]; ok && time.Until(t.expires) > time.Minute {
		s.k8s.mu.Unlock()
		return t.token, nil
	}
	s.k8s.mu.Unlock()
	st, err := s.awsFor(ctx, c.Account)
	if err != nil || !st.Enabled {
		return "", errors.New("the AWS account for this cluster is not connected")
	}
	st.Region = c.Region
	out, err := s.aws(ctx, st, "eks", "get-token", "--cluster-name", c.ClusterName)
	if err != nil {
		return "", err
	}
	var cred struct {
		Status struct {
			Token               string
			ExpirationTimestamp string
		}
	}
	if json.Unmarshal(out, &cred) != nil || cred.Status.Token == "" {
		return "", errors.New("AWS did not return a cluster token")
	}
	exp, err := time.Parse(time.RFC3339, cred.Status.ExpirationTimestamp)
	if err != nil {
		exp = time.Now().Add(10 * time.Minute)
	}
	s.k8s.mu.Lock()
	if s.k8s.tokens == nil {
		s.k8s.tokens = map[string]eksToken{}
	}
	s.k8s.tokens[c.ID] = eksToken{cred.Status.Token, exp}
	s.k8s.mu.Unlock()
	return cred.Status.Token, nil
}

// --- the cluster list ---

func (s *Server) listK8sClusters(ctx context.Context) ([]K8sCluster, error) {
	docs, err := s.docs.List(ctx, kindK8sCluster, docstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	cs, err := docstore.Decode[K8sCluster](docs)
	sort.Slice(cs, func(i, j int) bool { return strings.ToLower(cs[i].Name) < strings.ToLower(cs[j].Name) })
	return cs, err
}

func (s *Server) loadK8sCluster(ctx context.Context, id string) (K8sCluster, error) {
	var c K8sCluster
	err := s.docs.Get(ctx, kindK8sCluster, id, &c)
	return c, err
}

// K8sClusterJSON is what the console sees: never the token or the CA.
type K8sClusterJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Env         string `json:"env,omitempty"`
	Kind        string `json:"kind"`
	Server      string `json:"server"`
	ClusterName string `json:"cluster_name,omitempty"`
	Region      string `json:"region,omitempty"`
	Account     string `json:"account,omitempty"`
	Version     string `json:"version,omitempty"`
}

func clusterJSON(c K8sCluster) K8sClusterJSON {
	return K8sClusterJSON{c.ID, c.Name, c.Env, c.Kind, c.Server, c.ClusterName, c.Region, c.Account, c.Version}
}

func (s *Server) handleListK8sClusters(w http.ResponseWriter, r *http.Request) {
	cs, err := s.listK8sClusters(r.Context())
	if err != nil {
		s.internalError(w, "reading the clusters", err)
		return
	}
	out := make([]K8sClusterJSON, 0, len(cs))
	for _, c := range cs {
		out = append(out, clusterJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out})
}

type addClusterRequest struct {
	Name        string `json:"name"`
	Env         string `json:"env"`
	Kind        string `json:"kind"`
	Account     string `json:"account"`
	ClusterName string `json:"cluster_name"`
	Region      string `json:"region"`
	Server      string `json:"server"`
	CA          string `json:"ca"`
	Token       string `json:"token"`
}

func (s *Server) handleAddK8sCluster(w http.ResponseWriter, r *http.Request) {
	var req addClusterRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Name, req.Server, req.ClusterName, req.Region = strings.TrimSpace(req.Name), strings.TrimSpace(req.Server), strings.TrimSpace(req.ClusterName), strings.TrimSpace(req.Region)
	switch {
	case req.Name == "" || len(req.Name) > accountNameMax:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("give the cluster a name of up to %d characters", accountNameMax))
		return
	case !accountEnvs[req.Env]:
		writeError(w, http.StatusBadRequest, "the environment must be production, staging, development or sandbox")
		return
	case req.Kind != "eks" && req.Kind != "token":
		writeError(w, http.StatusBadRequest, "choose how to connect: eks or token")
		return
	case len(req.CA) > 16<<10 || len(req.Token) > 16<<10:
		writeError(w, http.StatusBadRequest, "the certificate or token is too long")
		return
	}
	ctx := r.Context()
	existing, err := s.listK8sClusters(ctx)
	if err != nil {
		s.internalError(w, "reading the clusters", err)
		return
	}
	if len(existing) >= maxK8sClusters {
		writeError(w, http.StatusConflict, fmt.Sprintf("at most %d clusters can be added", maxK8sClusters))
		return
	}
	for _, c := range existing {
		if strings.EqualFold(c.Name, req.Name) {
			writeError(w, http.StatusConflict, fmt.Sprintf("there is already a cluster named %q", c.Name))
			return
		}
	}
	id, err := s.newDefID(ctx, kindK8sCluster, req.Name)
	if err != nil {
		s.internalError(w, "saving the cluster", err)
		return
	}
	c := K8sCluster{ID: id, Name: req.Name, Env: req.Env, Kind: req.Kind, CreatedAt: time.Now().UTC(), CreatedBy: principalFrom(ctx).Name}

	switch req.Kind {
	case "eks":
		if req.ClusterName == "" || !awsRegionRE.MatchString(req.Region) {
			writeError(w, http.StatusBadRequest, "give the EKS cluster's name and its region, such as ap-south-1")
			return
		}
		acct := req.Account
		if acct == "" {
			acct = primaryAccount
		}
		st, aerr := s.awsFor(ctx, acct)
		if aerr != nil || !st.Enabled {
			writeError(w, http.StatusBadRequest, "that AWS account is not connected")
			return
		}
		st.Region = req.Region
		out, derr := s.aws(ctx, st, "eks", "describe-cluster", "--name", req.ClusterName)
		if derr != nil {
			writeError(w, http.StatusUnprocessableEntity, derr.Error())
			return
		}
		var d struct {
			Cluster struct {
				Endpoint             string
				CertificateAuthority struct{ Data string }
			}
		}
		if json.Unmarshal(out, &d) != nil || d.Cluster.Endpoint == "" {
			writeError(w, http.StatusUnprocessableEntity, "AWS did not describe that cluster")
			return
		}
		ca, _ := base64.StdEncoding.DecodeString(d.Cluster.CertificateAuthority.Data)
		c.Account, c.ClusterName, c.Region, c.Server, c.CA = acct, req.ClusterName, req.Region, d.Cluster.Endpoint, string(ca)
	case "token":
		if strings.TrimSpace(req.Token) == "" {
			writeError(w, http.StatusBadRequest, "paste a service-account token that can read the cluster")
			return
		}
		c.Server, c.CA = req.Server, req.CA
	}
	if err := validK8sServer(c.Server); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Prove it works before saving anything.
	var conn k8sConn
	if req.Kind == "token" {
		tok := strings.TrimSpace(req.Token)
		conn, err = newK8sConn(c.Server, c.CA, func(context.Context) (string, error) { return tok, nil })
	} else {
		conn, err = newK8sConn(c.Server, c.CA, func(ctx context.Context) (string, error) { return s.eksToken(ctx, c) })
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var ver struct{ GitVersion string }
	if err := conn.get(ctx, "/version", &ver); err != nil {
		s.auditReq(r, "k8s.cluster.add", "k8s/"+id, c.Name+" refused: "+err.Error(), false)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	c.Version = ver.GitVersion
	if req.Kind == "token" {
		if _, _, err := s.putSecret(ctx, principalFrom(ctx).Name, k8sTokenSecret(id), strings.TrimSpace(req.Token), "Kubernetes token for "+c.Name); err != nil {
			s.internalError(w, "saving the token", err)
			return
		}
	}
	if err := s.docs.Put(ctx, kindK8sCluster, id, c); err != nil {
		s.internalError(w, "saving the cluster", err)
		return
	}
	s.auditReq(r, "k8s.cluster.add", "k8s/"+id, fmt.Sprintf("%s (%s) %s %s", c.Name, c.Kind, c.Server, c.Version), true)
	writeJSON(w, http.StatusCreated, clusterJSON(c))
}

func (s *Server) handleDeleteK8sCluster(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	c, err := s.loadK8sCluster(ctx, id)
	if err != nil {
		s.defError(w, "cluster", err)
		return
	}
	if err := s.docs.Delete(ctx, kindK8sCluster, id); err != nil {
		s.internalError(w, "removing the cluster", err)
		return
	}
	_ = s.docs.Delete(ctx, kindSecret, k8sTokenSecret(id))
	s.k8s.mu.Lock()
	delete(s.k8s.cache, id)
	delete(s.k8s.tokens, id)
	s.k8s.mu.Unlock()
	s.auditReq(r, "k8s.cluster.remove", "k8s/"+id, c.Name, true)
	w.WriteHeader(http.StatusNoContent)
}

// --- reading a cluster ---

type kMeta struct {
	Name              string
	Namespace         string
	CreationTimestamp string
}

type kSecCtx struct {
	Privileged   *bool
	RunAsNonRoot *bool
	RunAsUser    *int64
}

type kContainer struct {
	Name            string
	Image           string
	SecurityContext *kSecCtx
	Resources       struct{ Limits map[string]string }
}

type kPodSpec struct {
	Containers      []kContainer
	InitContainers  []kContainer
	HostNetwork     bool
	HostPID         bool
	HostIPC         bool
	SecurityContext *kSecCtx
	NodeName        string
}

type kWorkload struct {
	Kind     string `json:"-"`
	Metadata kMeta
	Spec     struct {
		Replicas *int
		Template struct{ Spec kPodSpec }
	}
	Status struct {
		Replicas, ReadyReplicas, NumberReady, DesiredNumberScheduled int
	}
}

type kPod struct {
	Metadata kMeta
	Spec     kPodSpec
	Status   struct {
		Phase             string
		ContainerStatuses []struct {
			Name         string
			Ready        bool
			RestartCount int
			State        struct{ Waiting *struct{ Reason string } }
		}
	}
}

type kNode struct {
	Metadata kMeta
	Status   struct {
		Conditions []struct{ Type, Status string }
		NodeInfo   struct{ KubeletVersion, OsImage string }
		Capacity   map[string]string
	}
}

type kService struct {
	Metadata kMeta
	Spec     struct {
		Type      string
		ClusterIP string
		Ports     []struct {
			Port     int
			Protocol string
		}
	}
	Status struct {
		LoadBalancer struct {
			Ingress []struct{ Hostname, IP string }
		}
	}
}

type kEvent struct {
	Metadata       kMeta
	Type, Reason   string
	Message        string
	Count          int
	LastTimestamp  string
	InvolvedObject struct{ Kind, Name, Namespace string }
}

type kNamespace struct {
	Metadata kMeta
	Status   struct{ Phase string }
}

// k8sRaw is what one read of a cluster returned, with the sections that could not be read.
type k8sRaw struct {
	Nodes        []kNode
	Namespaces   []kNamespace
	Pods         []kPod
	Services     []kService
	Events       []kEvent
	Deployments  []kWorkload
	StatefulSets []kWorkload
	DaemonSets   []kWorkload
	Errors       map[string]string // section id -> why it could not be read
	Version      string
}

func readList[T any](ctx context.Context, c k8sConn, path string, label string, errs map[string]string, mu *sync.Mutex) []T {
	var l struct{ Items []T }
	if err := c.get(ctx, path, &l); err != nil {
		mu.Lock()
		errs[label] = inventoryError(err)
		mu.Unlock()
		return nil
	}
	return l.Items
}

func (s *Server) readK8s(ctx context.Context, c k8sConn) k8sRaw {
	ctx, cancel := context.WithTimeout(ctx, k8sTimeout)
	defer cancel()
	raw := k8sRaw{Errors: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	lim := "?limit=" + strconv.Itoa(k8sPageLimit)
	do := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	do(func() { raw.Nodes = readList[kNode](ctx, c, "/api/v1/nodes"+lim, "k8s-nodes", raw.Errors, &mu) })
	do(func() {
		raw.Namespaces = readList[kNamespace](ctx, c, "/api/v1/namespaces"+lim, "k8s-namespaces", raw.Errors, &mu)
	})
	do(func() { raw.Pods = readList[kPod](ctx, c, "/api/v1/pods"+lim, "k8s-pods", raw.Errors, &mu) })
	do(func() {
		raw.Services = readList[kService](ctx, c, "/api/v1/services"+lim, "k8s-services", raw.Errors, &mu)
	})
	do(func() {
		raw.Events = readList[kEvent](ctx, c, "/api/v1/events?fieldSelector=type%3DWarning&limit=200", "k8s-events", raw.Errors, &mu)
	})
	do(func() {
		raw.Deployments = readList[kWorkload](ctx, c, "/apis/apps/v1/deployments"+lim, "k8s-deployments", raw.Errors, &mu)
	})
	do(func() {
		raw.StatefulSets = readList[kWorkload](ctx, c, "/apis/apps/v1/statefulsets"+lim, "k8s-statefulsets", raw.Errors, &mu)
	})
	do(func() {
		raw.DaemonSets = readList[kWorkload](ctx, c, "/apis/apps/v1/daemonsets"+lim, "k8s-daemonsets", raw.Errors, &mu)
	})
	var ver struct{ GitVersion string }
	_ = c.get(ctx, "/version", &ver)
	raw.Version = ver.GitVersion
	wg.Wait()
	return raw
}

func qn(m kMeta) string {
	if m.Namespace == "" {
		return m.Name
	}
	return m.Namespace + "/" + m.Name
}

func (w kWorkload) ready() (int, int) {
	if w.Spec.Replicas == nil && w.Status.DesiredNumberScheduled > 0 { // a daemon set
		return w.Status.NumberReady, w.Status.DesiredNumberScheduled
	}
	want := 1
	if w.Spec.Replicas != nil {
		want = *w.Spec.Replicas
	}
	return w.Status.ReadyReplicas, want
}

func images(spec kPodSpec) string {
	var out []string
	for _, c := range spec.Containers {
		out = append(out, c.Image)
	}
	return strings.Join(out, ", ")
}

// view turns a raw read into the explorer's shape.
func (raw k8sRaw) view(cluster K8sCluster) AWSInventoryJSON {
	d := AWSInventoryJSON{Enabled: true, Region: cluster.Name, Account: cluster.Version, FetchedAt: time.Now().UTC().Format(time.RFC3339), Services: []AWSServiceJSON{}}
	add := func(id, label, cat string, res []AWSResourceJSON) {
		svc := AWSServiceJSON{ID: id, Label: label, Category: cat, Resources: []AWSResourceJSON{}}
		if e, bad := raw.Errors[id]; bad {
			svc.Error = e
		} else {
			fillService(&svc, res)
		}
		d.Services = append(d.Services, svc)
	}
	var nodes, nss, pods, svcs, evs []AWSResourceJSON
	for _, n := range raw.Nodes {
		state := "NotReady"
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				state = "Ready"
			}
		}
		nodes = append(nodes, AWSResourceJSON{ID: n.Metadata.Name, Name: n.Metadata.Name, State: state, Created: n.Metadata.CreationTimestamp, Fields: []AWSFieldJSON{
			{"Kubelet", n.Status.NodeInfo.KubeletVersion}, {"OS", n.Status.NodeInfo.OsImage}, {"CPU", n.Status.Capacity["cpu"]}, {"Memory", n.Status.Capacity["memory"]}}})
	}
	for _, n := range raw.Namespaces {
		nss = append(nss, AWSResourceJSON{ID: n.Metadata.Name, Name: n.Metadata.Name, State: n.Status.Phase, Created: n.Metadata.CreationTimestamp})
	}
	for _, p := range raw.Pods {
		state, restarts := p.Status.Phase, 0
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				state = cs.State.Waiting.Reason
			}
		}
		pods = append(pods, AWSResourceJSON{ID: qn(p.Metadata), Name: qn(p.Metadata), State: state, Created: p.Metadata.CreationTimestamp, Fields: []AWSFieldJSON{
			{"Node", p.Spec.NodeName}, {"Restarts", strconv.Itoa(restarts)}, {"Images", images(p.Spec)}}})
	}
	for _, s := range raw.Services {
		ports := []string{}
		for _, p := range s.Spec.Ports {
			ports = append(ports, fmt.Sprintf("%d/%s", p.Port, strings.ToLower(p.Protocol)))
		}
		f := []AWSFieldJSON{{"Type", s.Spec.Type}, {"Cluster IP", s.Spec.ClusterIP}, {"Ports", strings.Join(ports, ", ")}}
		for _, ing := range s.Status.LoadBalancer.Ingress {
			f = append(f, AWSFieldJSON{"External", firstNonEmpty(ing.Hostname, ing.IP)})
		}
		svcs = append(svcs, AWSResourceJSON{ID: qn(s.Metadata), Name: qn(s.Metadata), Created: s.Metadata.CreationTimestamp, Fields: f})
	}
	for _, e := range raw.Events {
		evs = append(evs, AWSResourceJSON{ID: qn(e.Metadata), Name: e.Reason + ": " + e.InvolvedObject.Name, State: e.Type, Created: e.LastTimestamp, Fields: []AWSFieldJSON{
			{"Namespace", e.InvolvedObject.Namespace}, {"Object", e.InvolvedObject.Kind + " " + e.InvolvedObject.Name}, {"Message", e.Message}, {"Count", strconv.Itoa(e.Count)}}})
	}
	workloads := func(ws []kWorkload) []AWSResourceJSON {
		var out []AWSResourceJSON
		for _, w := range ws {
			ready, want := w.ready()
			state := "Available"
			switch {
			case want > 0 && ready == 0:
				state = "Down"
			case ready < want:
				state = "Degraded"
			}
			out = append(out, AWSResourceJSON{ID: qn(w.Metadata), Name: qn(w.Metadata), State: state, Created: w.Metadata.CreationTimestamp, Fields: []AWSFieldJSON{
				{"Ready", fmt.Sprintf("%d/%d", ready, want)}, {"Images", images(w.Spec.Template.Spec)}}})
		}
		return out
	}
	add("k8s-nodes", "Nodes", "Compute", nodes)
	add("k8s-deployments", "Deployments", "Containers", workloads(raw.Deployments))
	add("k8s-statefulsets", "StatefulSets", "Containers", workloads(raw.StatefulSets))
	add("k8s-daemonsets", "DaemonSets", "Containers", workloads(raw.DaemonSets))
	add("k8s-pods", "Pods", "Containers", pods)
	add("k8s-services", "Services", "Network", svcs)
	add("k8s-namespaces", "Namespaces", "Management", nss)
	add("k8s-events", "Warning events", "Management", evs)
	return d
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// k8sRead returns the last read of a cluster, or reads it again when it is a minute old.
func (s *Server) k8sRead(ctx context.Context, c K8sCluster, refresh bool) (k8sRaw, AWSInventoryJSON, error) {
	s.k8s.mu.Lock()
	if e, ok := s.k8s.cache[c.ID]; ok && !refresh && time.Since(e.at) < k8sTTL {
		s.k8s.mu.Unlock()
		return e.raw, e.view, nil
	}
	s.k8s.mu.Unlock()
	conn, err := s.connFor(ctx, c)
	if err != nil {
		return k8sRaw{}, AWSInventoryJSON{}, err
	}
	// If nothing can be read at all, say why instead of showing an empty cluster.
	raw := s.readK8s(ctx, conn)
	if len(raw.Errors) == 8 {
		var first string
		for _, e := range raw.Errors {
			first = e
			break
		}
		return k8sRaw{}, AWSInventoryJSON{}, errors.New(first)
	}
	view := raw.view(c)
	s.k8s.mu.Lock()
	if s.k8s.cache == nil || len(s.k8s.cache) > maxK8sClusters*2 {
		s.k8s.cache = map[string]k8sEntry{}
	}
	s.k8s.cache[c.ID] = k8sEntry{at: time.Now(), raw: raw, view: view}
	s.k8s.mu.Unlock()
	return raw, view, nil
}

func (s *Server) handleK8sResources(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadK8sCluster(r.Context(), r.PathValue("id"))
	if err != nil {
		s.defError(w, "cluster", err)
		return
	}
	_, view, err := s.k8sRead(r.Context(), c, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeJSON(w, http.StatusOK, AWSInventoryJSON{Enabled: true, Error: err.Error(), Services: []AWSServiceJSON{}})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// --- the assistant's view ---

func (s *Server) lookupK8sClusters(ctx context.Context, _ json.RawMessage) (string, error) {
	cs, err := s.listK8sClusters(ctx)
	if err != nil {
		return "", errors.New("could not read the clusters")
	}
	if len(cs) == 0 {
		return "No Kubernetes cluster is registered.", nil
	}
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "- id %q: %s", c.ID, c.Name)
		if c.Env != "" {
			fmt.Fprintf(&b, ", %s", c.Env)
		}
		fmt.Fprintf(&b, ", %s, Kubernetes %s\n", c.Kind, c.Version)
	}
	return b.String(), nil
}

func (s *Server) lookupK8sResources(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Cluster string `json:"cluster"`
		Kind    string `json:"kind"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Cluster == "" {
		return "", errors.New("say which cluster; k8s_clusters lists them")
	}
	c, err := s.loadK8sCluster(ctx, a.Cluster)
	if err != nil {
		return "", fmt.Errorf("there is no cluster %q; k8s_clusters lists them", a.Cluster)
	}
	_, view, err := s.k8sRead(ctx, c, false)
	if err != nil {
		return "", err
	}
	return formatInventory(fmt.Sprintf("Cluster %s (id %q), Kubernetes %s, read %s.", c.Name, c.ID, c.Version, view.FetchedAt), view, a.Kind)
}
