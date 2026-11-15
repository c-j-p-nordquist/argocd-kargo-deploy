// Package preflight inspects a cluster before anything is installed.
//
// Preflight is read-only. The one request that is not a read is a
// SelfSubjectAccessReview, which asks the API server what the caller may do
// and creates nothing.
package preflight

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// MinKubernetesMinor is the oldest supported Kubernetes 1.x minor version.
const MinKubernetesMinor = 30

// Status is the outcome of one check.
type Status string

const (
	// Pass means the cluster meets the requirement.
	Pass Status = "pass"
	// Warn means installation can continue, but the user should know.
	Warn Status = "warn"
	// Fail means installation cannot continue.
	Fail Status = "fail"
	// Unknown means the check could not be decided from the API.
	Unknown Status = "unknown"
)

// Distros that preflight recognises.
const (
	DistroGKE     = "gke"
	DistroEKS     = "eks"
	DistroAKS     = "aks"
	DistroK3s     = "k3s"
	DistroRKE2    = "rke2"
	DistroTalos   = "talos"
	DistroKind    = "kind"
	DistroGeneric = "generic"
)

// Check is one finding about the cluster.
type Check struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Cluster describes the cluster that was inspected. KubernetesVersion comes
// from the cluster and must be treated as data.
type Cluster struct {
	Context           string `json:"context"`
	KubernetesVersion string `json:"kubernetesVersion"`
	Distro            string `json:"distro"`
	// Nodes is the node count, or -1 if nodes could not be listed.
	Nodes int `json:"nodes"`
}

// Report is the result of a preflight run.
type Report struct {
	// Ready is false when any check failed.
	Ready   bool    `json:"ready"`
	Cluster Cluster `json:"cluster"`
	Checks  []Check `json:"checks"`
	// Suggestions maps a platform.yaml field to the answer that fits this
	// cluster. They are recommendations for the user, not decisions.
	Suggestions map[string]string `json:"suggestions"`
}

// Run inspects the cluster. It returns an error only when the API server
// cannot be reached; everything else is reported as checks.
func Run(ctx context.Context, client kubernetes.Interface, contextName string) (Report, error) {
	version, err := client.Discovery().ServerVersion()
	if err != nil {
		return Report{}, err
	}

	r := Report{
		Cluster: Cluster{
			Context:           contextName,
			KubernetesVersion: clean(version.GitVersion),
			Nodes:             -1,
		},
		Suggestions: map[string]string{},
	}

	var nodes []corev1.Node
	if list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		nodes = list.Items
		r.Cluster.Nodes = len(nodes)
	}
	r.Cluster.Distro = detectDistro(version.GitVersion, nodes)

	groups := map[string]bool{}
	groupList, groupsErr := client.Discovery().ServerGroups()
	if groupsErr == nil {
		for _, g := range groupList.Groups {
			groups[g.Name] = true
		}
	}

	r.Checks = []Check{
		checkVersion(version.Minor, r.Cluster.KubernetesVersion),
		checkPermissions(ctx, client),
		checkNetworkPolicy(ctx, client, r.Cluster.Distro),
		checkExistingInstall(groups, groupsErr),
		checkExposure(ctx, client, groups),
		checkSecretsAtRest(r.Cluster.Distro),
	}

	r.Ready = true
	for _, c := range r.Checks {
		if c.Status == Fail {
			r.Ready = false
		}
	}

	switch r.Cluster.Distro {
	case DistroGKE:
		r.Suggestions["secrets.backend"] = "gcp-secret-manager"
	case DistroEKS:
		r.Suggestions["secrets.backend"] = "aws-secrets-manager"
	case DistroAKS:
		r.Suggestions["secrets.backend"] = "azure-key-vault"
	}
	return r, nil
}

// clean makes a string that came from the cluster safe to show: printable
// ASCII only, and short. Cluster content is untrusted and must not be able to
// pose as instructions or break the output format.
func clean(s string) string {
	const max = 80
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= max {
			b.WriteString("...")
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

func detectDistro(gitVersion string, nodes []corev1.Node) string {
	switch {
	case strings.Contains(gitVersion, "+k3s"):
		return DistroK3s
	case strings.Contains(gitVersion, "+rke2"):
		return DistroRKE2
	case strings.Contains(gitVersion, "-gke."):
		return DistroGKE
	case strings.Contains(gitVersion, "-eks-"):
		return DistroEKS
	}
	for _, n := range nodes {
		switch {
		case strings.Contains(n.Status.NodeInfo.OSImage, "Talos"):
			return DistroTalos
		case n.Labels["kubernetes.azure.com/cluster"] != "":
			return DistroAKS
		case strings.HasPrefix(n.Spec.ProviderID, "kind://"):
			return DistroKind
		}
	}
	return DistroGeneric
}

func checkVersion(minor, shown string) Check {
	c := Check{ID: "kubernetes-version", Title: "Kubernetes version is supported"}
	// Managed distros report a minor such as "33+".
	digits := strings.TrimRightFunc(minor, func(r rune) bool { return r < '0' || r > '9' })
	n, err := strconv.Atoi(digits)
	switch {
	case err != nil:
		c.Status = Unknown
		c.Message = fmt.Sprintf("Could not read the minor version from %q.", shown)
	case n < MinKubernetesMinor:
		c.Status = Fail
		c.Message = fmt.Sprintf("The cluster runs %s. The minimum is 1.%d.", shown, MinKubernetesMinor)
		c.Hint = "Upgrade the cluster before installing."
	default:
		c.Status = Pass
		c.Message = fmt.Sprintf("The cluster runs %s.", shown)
	}
	return c
}

func checkPermissions(ctx context.Context, client kubernetes.Interface) Check {
	c := Check{ID: "permissions", Title: "The kubeconfig can install cluster-wide resources"}
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: "*", Group: "*", Resource: "*"},
		},
	}
	res, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	switch {
	case err != nil:
		c.Status = Unknown
		c.Message = "Could not ask the API server what this kubeconfig may do."
	case res.Status.Allowed:
		c.Status = Pass
		c.Message = "This kubeconfig has cluster-admin rights, which the first install needs. They can be revoked afterwards."
	default:
		c.Status = Fail
		c.Message = "This kubeconfig does not have cluster-admin rights. The first install creates CRDs, namespaces and cluster roles."
		c.Hint = "Use a kubeconfig with cluster-admin for the install, then revoke it."
	}
	return c
}

// enforcers are DaemonSets whose presence means NetworkPolicy is enforced.
var enforcers = map[string]string{
	"cilium":       "Cilium",
	"anetd":        "GKE Dataplane V2",
	"calico-node":  "Calico",
	"antrea-agent": "Antrea",
	"weave-net":    "Weave Net",
	"kube-router":  "kube-router",
}

func checkNetworkPolicy(ctx context.Context, client kubernetes.Interface, distro string) Check {
	c := Check{ID: "network-policy", Title: "NetworkPolicy is enforced"}
	const hint = "Without enforcement the default-deny policies this platform relies on do nothing. Install a CNI that enforces NetworkPolicy, such as Cilium or Calico."

	list, err := client.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.Status = Unknown
		c.Message = "Could not list DaemonSets to identify the network plugin."
		c.Hint = hint
		return c
	}

	found := map[string]bool{}
	flannel := false
	for _, ds := range list.Items {
		if name, ok := enforcers[ds.Name]; ok {
			found[name] = true
		}
		if strings.Contains(ds.Name, "flannel") {
			flannel = true
		}
	}

	switch {
	case len(found) > 0:
		names := make([]string, 0, len(found))
		for name := range found {
			names = append(names, name)
		}
		sort.Strings(names)
		c.Status = Pass
		c.Message = fmt.Sprintf("Found %s, which enforces NetworkPolicy.", strings.Join(names, " and "))
	case distro == DistroK3s:
		c.Status = Warn
		c.Message = "k3s enforces NetworkPolicy with a built-in controller unless it was started with --disable-network-policy. That flag cannot be seen from the API, so enforcement is not confirmed."
		c.Hint = "Check the k3s server flags on the nodes."
	case flannel:
		c.Status = Warn
		c.Message = "The cluster uses Flannel, which does not enforce NetworkPolicy on its own, and no enforcing plugin was found."
		c.Hint = hint
	default:
		c.Status = Warn
		c.Message = "No known network plugin that enforces NetworkPolicy was found. Enforcement is not confirmed."
		c.Hint = hint
	}
	return c
}

// platformGroups are the API groups of the components this toolkit installs.
var platformGroups = map[string]string{
	"argoproj.io":         "Argo CD or Argo Rollouts",
	"kargo.akuity.io":     "Kargo",
	"cert-manager.io":     "cert-manager",
	"kyverno.io":          "Kyverno",
	"external-secrets.io": "External Secrets Operator",
}

func checkExistingInstall(groups map[string]bool, err error) Check {
	c := Check{ID: "existing-install", Title: "No conflicting installation exists"}
	if err != nil {
		c.Status = Unknown
		c.Message = "Could not list the API groups the cluster serves."
		return c
	}
	var found []string
	for group, name := range platformGroups {
		if groups[group] {
			found = append(found, name)
		}
	}
	if len(found) == 0 {
		c.Status = Pass
		c.Message = "None of the platform components are installed yet."
		return c
	}
	sort.Strings(found)
	c.Status = Warn
	c.Message = fmt.Sprintf("Already installed: %s. This toolkit does not take over an existing installation.", strings.Join(found, ", "))
	c.Hint = "Remove the existing components, or use a cluster without them."
	return c
}

func checkExposure(ctx context.Context, client kubernetes.Interface, groups map[string]bool) Check {
	c := Check{ID: "exposure", Title: "Ways to reach the UIs", Status: Pass}
	var ways []string
	if groups["gateway.networking.k8s.io"] {
		ways = append(ways, "Gateway API")
	}
	if classes, err := client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{}); err == nil {
		for _, class := range classes.Items {
			ways = append(ways, fmt.Sprintf("IngressClass %q", clean(class.Name)))
		}
	}
	if len(ways) == 0 {
		c.Message = "No Gateway API or IngressClass was found, so only port-forward access is possible. That is the secure default."
		return c
	}
	c.Message = fmt.Sprintf("Port-forward always works. Also available: %s.", strings.Join(ways, ", "))
	return c
}

func checkSecretsAtRest(distro string) Check {
	c := Check{
		ID:      "secrets-at-rest",
		Title:   "Secrets are encrypted at rest",
		Status:  Unknown,
		Message: "Encryption of Secrets in etcd cannot be seen from the Kubernetes API.",
	}
	switch distro {
	case DistroK3s:
		c.Hint = "k3s encrypts Secrets only when the server runs with --secrets-encryption. Check the server flags."
	case DistroGKE:
		c.Hint = "GKE encrypts storage by default. Application-layer secrets encryption with a KMS key is a separate cluster setting; check it in the cluster's security settings."
	case DistroTalos:
		c.Hint = "Check the cluster's machine configuration for secretbox or aescbc encryption keys."
	default:
		c.Hint = "Check the API server's encryption configuration. This matters most with the in-cluster secrets backend."
	}
	return c
}
