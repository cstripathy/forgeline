package server

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Guard rules for Kubernetes. They read the same data as the explorer. System namespaces are left
// out because they run privileged and host-networked workloads on purpose.

var k8sSystemNamespaces = map[string]bool{"kube-system": true, "kube-public": true, "kube-node-lease": true}

// imageUnpinned says whether an image has no tag, or the moving tag "latest", and no digest.
func imageUnpinned(image string) bool {
	if strings.Contains(image, "@sha256:") {
		return false
	}
	last := image[strings.LastIndex(image, "/")+1:]
	i := strings.LastIndex(last, ":")
	return i < 0 || last[i+1:] == "latest"
}

func isRoot(c kContainer, pod kPodSpec) bool {
	secs := []*kSecCtx{c.SecurityContext, pod.SecurityContext}
	for _, sc := range secs {
		if sc == nil {
			continue
		}
		if sc.RunAsNonRoot != nil && *sc.RunAsNonRoot {
			return false
		}
		if sc.RunAsUser != nil && *sc.RunAsUser != 0 {
			return false
		}
	}
	return true
}

func k8sRules(raw k8sRaw) []rawFinding {
	var out []rawFinding
	add := func(f rawFinding) { out = append(out, f) }

	kinds := []struct {
		kind, service string
		list          []kWorkload
	}{{"Deployment", "k8s-deployments", raw.Deployments}, {"StatefulSet", "k8s-statefulsets", raw.StatefulSets}, {"DaemonSet", "k8s-daemonsets", raw.DaemonSets}}
	for _, k := range kinds {
		for _, w := range k.list {
			ns := w.Metadata.Namespace
			if k8sSystemNamespaces[ns] {
				continue
			}
			id := ns + "/" + k.kind + "/" + w.Metadata.Name
			name := qn(w.Metadata)
			spec := w.Spec.Template.Spec
			if ns == "default" {
				add(rawFinding{"k8s-default-namespace", k.kind + " runs in the default namespace", sevLow, k.service, id, name, "Namespace default",
					"Move workloads into their own namespaces so access, quotas and network policies can be set per team or application."})
			}
			if spec.HostNetwork || spec.HostPID || spec.HostIPC {
				var which []string
				for n, on := range map[string]bool{"hostNetwork": spec.HostNetwork, "hostPID": spec.HostPID, "hostIPC": spec.HostIPC} {
					if on {
						which = append(which, n)
					}
				}
				add(rawFinding{"k8s-host-namespace", k.kind + " shares the node's network or process space", sevHigh, k.service, id, name, "Sets " + strings.Join(sortedStrings(which), ", "),
					"Remove hostNetwork, hostPID and hostIPC unless this workload truly needs them. They let a container see the node's traffic and processes."})
			}
			ctrs := append(append([]kContainer{}, spec.InitContainers...), spec.Containers...)
			for _, c := range ctrs {
				cid := id + "/" + c.Name
				if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
					add(rawFinding{"k8s-privileged", "Container runs privileged", sevHigh, k.service, cid, name + " / " + c.Name, "securityContext.privileged is true in " + c.Image,
						"Drop privileged mode and add only the specific capabilities needed. A privileged container can take over its node."})
				}
				if isRoot(c, spec) {
					add(rawFinding{"k8s-run-as-root", "Container may run as root", sevMedium, k.service, cid, name + " / " + c.Name, "No runAsNonRoot or non-zero runAsUser is set for " + c.Image,
						"Set runAsNonRoot: true (and a numeric runAsUser) in the container or pod securityContext."})
				}
				if imageUnpinned(c.Image) {
					add(rawFinding{"k8s-unpinned-image", "Image has no fixed version", sevLow, k.service, cid, name + " / " + c.Name, c.Image,
						"Pin the image to a version tag or, better, a digest, so a deploy always means the same code."})
				}
				if c.Resources.Limits["memory"] == "" {
					add(rawFinding{"k8s-no-memory-limit", "Container has no memory limit", sevLow, k.service, cid, name + " / " + c.Name, c.Image,
						"Set a memory limit so one container cannot starve the node and its neighbours."})
				}
			}
			ready, want := w.ready()
			switch {
			case want > 0 && ready == 0:
				add(rawFinding{"k8s-workload-down", k.kind + " has no ready replicas", sevHigh, k.service, id, name, fmt.Sprintf("0 of %d replicas ready", want),
					"Look at the pods' events and logs: a failing image pull, a crash, or a scheduling problem is usually named there."})
			case ready < want:
				add(rawFinding{"k8s-workload-degraded", k.kind + " is not fully available", sevMedium, k.service, id, name, fmt.Sprintf("%d of %d replicas ready", ready, want),
					"Check whether a rollout is in progress; if it is stuck, look at the pods' events."})
			}
		}
	}
	for _, p := range raw.Pods {
		if k8sSystemNamespaces[p.Metadata.Namespace] {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting == nil {
				continue
			}
			switch cs.State.Waiting.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError":
				add(rawFinding{"k8s-pod-failing", "Pod is failing to start", sevMedium, "k8s-pods", qn(p.Metadata) + "/" + cs.Name, qn(p.Metadata), fmt.Sprintf("%s: %s, %d restarts", cs.Name, cs.State.Waiting.Reason, cs.RestartCount),
					"Read the pod's events and logs: CrashLoopBackOff means the process keeps exiting, ImagePullBackOff means the image or its credentials are wrong."})
			}
		}
	}
	for _, n := range raw.Nodes {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = true
			}
		}
		if !ready {
			add(rawFinding{"k8s-node-not-ready", "Node is not ready", sevHigh, "k8s-nodes", n.Metadata.Name, n.Metadata.Name, "The kubelet is not reporting Ready",
				"Check the node's conditions and its kubelet; if it cannot recover, cordon, drain and replace it."})
		}
	}
	for _, s := range raw.Services {
		if s.Spec.Type == "LoadBalancer" && !k8sSystemNamespaces[s.Metadata.Namespace] {
			add(rawFinding{"k8s-public-service", "Service is exposed through a load balancer", sevInfo, "k8s-services", qn(s.Metadata), qn(s.Metadata), "type LoadBalancer",
				"Confirm it is meant to be reachable from outside, and restrict it with loadBalancerSourceRanges or an internal load balancer if not."})
		}
	}
	return out
}

func sortedStrings(in []string) []string {
	out := append([]string{}, in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// scanCluster reads one cluster and reconciles its findings. A section that could not be read leaves
// its findings alone.
func (s *Server) scanCluster(ctx context.Context, c K8sCluster, now time.Time) GuardScanAccount {
	id := "k8s:" + c.ID
	res := GuardScanAccount{Account: id, Name: c.Name}
	raw, _, err := s.k8sRead(ctx, c, true)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	known := map[string]bool{}
	for _, svc := range []string{"k8s-nodes", "k8s-deployments", "k8s-statefulsets", "k8s-daemonsets", "k8s-pods", "k8s-services"} {
		if e, bad := raw.Errors[svc]; bad {
			res.Skipped = append(res.Skipped, svc[4:]+" ("+e+")")
		} else {
			known[svc] = true
		}
	}
	res.Findings = s.reconcile(ctx, id, c.Name, "", c.Name, k8sRules(raw), known, now)
	return res
}
