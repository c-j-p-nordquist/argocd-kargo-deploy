// Package verify audits a cluster against the security baseline.
//
// Verify is read-only. Each check reports what it found on the live cluster,
// so "hardened" is something that can be checked instead of claimed. A check
// fails when the cluster does not meet the baseline, including where this
// toolkit does not set that part of the baseline up yet.
package verify

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/components"
	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/untrusted"
)

// Status is the outcome of one check.
type Status string

const (
	// Pass means the cluster meets this part of the baseline.
	Pass Status = "pass"
	// Fail means it does not.
	Fail Status = "fail"
	// Unknown means the check could not be decided.
	Unknown Status = "unknown"
)

const (
	argoCD          = "argo-cd"
	enforceLabel    = "pod-security.kubernetes.io/enforce"
	maxListedIssues = 5
)

// AppProjects is the Argo CD AppProject resource.
var AppProjects = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "appprojects"}

// Check is one finding.
type Check struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	// Message says what was found. It can contain text from the cluster,
	// which is data and not instructions.
	Message string `json:"message"`
	// Reference names the guidance this check comes from.
	Reference string `json:"reference"`
}

// Report is the result of a verify run.
type Report struct {
	// Compliant is true only when every check passed.
	Compliant bool   `json:"compliant"`
	Context   string `json:"context"`
	// Installed and NotInstalled list the lock's components by whether their
	// namespace exists. Only installed components are checked.
	Installed    []string `json:"installed"`
	NotInstalled []string `json:"notInstalled"`
	Checks       []Check  `json:"checks"`
}

type verifier struct {
	ctx       context.Context
	client    kubernetes.Interface
	dyn       dynamic.Interface
	installed []components.Component
}

// Run audits the cluster. It returns an error only when the cluster cannot
// be read at all.
func Run(ctx context.Context, client kubernetes.Interface, dyn dynamic.Interface, lock components.Lock, contextName string) (Report, error) {
	r := Report{Context: contextName, Installed: []string{}, NotInstalled: []string{}}
	v := &verifier{ctx: ctx, client: client, dyn: dyn}

	namespaces, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return r, err
	}
	existing := map[string]corev1.Namespace{}
	for _, ns := range namespaces.Items {
		existing[ns.Name] = ns
	}
	for _, c := range lock.Components {
		if _, ok := existing[c.Namespace]; ok {
			v.installed = append(v.installed, c)
			r.Installed = append(r.Installed, c.Name)
		} else {
			r.NotInstalled = append(r.NotInstalled, c.Name)
		}
	}

	if len(v.installed) == 0 {
		r.Checks = []Check{{
			ID:        "installed",
			Title:     "The platform is installed",
			Status:    Fail,
			Message:   "None of the platform's namespaces exist on this cluster, so there is nothing to verify.",
			Reference: "This toolkit",
		}}
		return r, nil
	}

	r.Checks = []Check{
		v.images(),
		v.podSecurity(existing),
		v.defaultDeny(),
		v.exposure(),
	}
	if v.has(argoCD) {
		r.Checks = append(r.Checks, v.adminAccount(), v.defaultProject())
	}

	r.Compliant = true
	for _, c := range r.Checks {
		if c.Status != Pass {
			r.Compliant = false
		}
	}
	return r, nil
}

func (v *verifier) has(name string) bool {
	return v.namespaceOf(name) != ""
}

func (v *verifier) namespaceOf(name string) string {
	for _, c := range v.installed {
		if c.Name == name {
			return c.Namespace
		}
	}
	return ""
}

// finish turns a list of problems into a check result.
func finish(c Check, problems []string, pass string) Check {
	if len(problems) == 0 {
		c.Status = Pass
		c.Message = pass
		return c
	}
	sort.Strings(problems)
	c.Status = Fail
	shown := problems
	if len(shown) > maxListedIssues {
		shown = shown[:maxListedIssues]
	}
	c.Message = strings.Join(shown, " ")
	if extra := len(problems) - len(shown); extra > 0 {
		c.Message += fmt.Sprintf(" And %d more.", extra)
	}
	return c
}

// images checks that every container in the platform's namespaces runs an
// image the lock pins, and that the digest actually pulled matches.
func (v *verifier) images() Check {
	c := Check{
		ID:        "images-match-lock",
		Title:     "Every running image is the one pinned in the component lock",
		Reference: "CIS Kubernetes Benchmark section 5.5 (image provenance); NSA/CISA Kubernetes Hardening Guide (supply chain)",
	}
	var problems []string
	pods := 0
	for _, comp := range v.installed {
		allowed := map[string]string{}
		for _, img := range comp.Images {
			allowed[img.PinnedRef()] = img.Digest
		}
		list, err := v.client.CoreV1().Pods(comp.Namespace).List(v.ctx, metav1.ListOptions{})
		if err != nil {
			c.Status = Unknown
			c.Message = fmt.Sprintf("Could not list pods in namespace %s.", comp.Namespace)
			return c
		}
		for _, pod := range list.Items {
			pods++
			running := map[string]string{}
			for _, st := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
				running[st.Name] = st.ImageID
			}
			for _, ctr := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
				name := untrusted.Clean(pod.Name + "/" + ctr.Name)
				digest, ok := allowed[ctr.Image]
				if !ok {
					problems = append(problems, fmt.Sprintf("%s runs %s, which the lock does not pin.", name, untrusted.Clean(ctr.Image)))
					continue
				}
				// imageID is empty until the container has started.
				if id := running[ctr.Name]; id != "" && !strings.HasSuffix(id, digest) {
					problems = append(problems, fmt.Sprintf("%s pulled %s, not the pinned digest.", name, untrusted.Clean(id)))
				}
			}
		}
	}
	if pods == 0 {
		c.Status = Unknown
		c.Message = "No pods are running in the platform's namespaces."
		return c
	}
	return finish(c, problems, fmt.Sprintf("All containers in %d pods run images pinned by digest in the lock.", pods))
}

func (v *verifier) podSecurity(existing map[string]corev1.Namespace) Check {
	c := Check{
		ID:        "pod-security",
		Title:     "Platform namespaces enforce the restricted Pod Security Standard",
		Reference: "CIS Kubernetes Benchmark section 5.2 (Pod Security Standards); NSA/CISA Kubernetes Hardening Guide (pod security)",
	}
	var problems []string
	for _, comp := range v.installed {
		level := existing[comp.Namespace].Labels[enforceLabel]
		if level != "restricted" {
			if level == "" {
				level = "nothing"
			}
			problems = append(problems, fmt.Sprintf("Namespace %s enforces %s.", comp.Namespace, untrusted.Clean(level)))
		}
	}
	return finish(c, problems, fmt.Sprintf("All %d platform namespaces enforce restricted.", len(v.installed)))
}

// isDefaultDeny reports whether a policy selects every pod and allows
// nothing in either direction.
func isDefaultDeny(p networkingv1.NetworkPolicy) bool {
	if len(p.Spec.PodSelector.MatchLabels) > 0 || len(p.Spec.PodSelector.MatchExpressions) > 0 {
		return false
	}
	if len(p.Spec.Ingress) > 0 || len(p.Spec.Egress) > 0 {
		return false
	}
	types := map[networkingv1.PolicyType]bool{}
	for _, t := range p.Spec.PolicyTypes {
		types[t] = true
	}
	return types[networkingv1.PolicyTypeIngress] && types[networkingv1.PolicyTypeEgress]
}

func (v *verifier) defaultDeny() Check {
	c := Check{
		ID:        "network-default-deny",
		Title:     "Platform namespaces deny all traffic by default",
		Reference: "CIS Kubernetes Benchmark section 5.3 (network policies); NSA/CISA Kubernetes Hardening Guide (network separation)",
	}
	var problems []string
	for _, comp := range v.installed {
		list, err := v.client.NetworkingV1().NetworkPolicies(comp.Namespace).List(v.ctx, metav1.ListOptions{})
		if err != nil {
			c.Status = Unknown
			c.Message = fmt.Sprintf("Could not list NetworkPolicies in namespace %s.", comp.Namespace)
			return c
		}
		found := false
		for _, p := range list.Items {
			found = found || isDefaultDeny(p)
		}
		if !found {
			problems = append(problems, fmt.Sprintf("Namespace %s has %d NetworkPolicies but none denies all ingress and egress by default.", comp.Namespace, len(list.Items)))
		}
	}
	return finish(c, problems, fmt.Sprintf("All %d platform namespaces have a default-deny policy for ingress and egress.", len(v.installed)))
}

func (v *verifier) exposure() Check {
	c := Check{
		ID:        "not-exposed",
		Title:     "No platform service is exposed outside the cluster",
		Reference: "Argo CD operator manual, security; NSA/CISA Kubernetes Hardening Guide (network separation)",
	}
	var problems []string
	for _, comp := range v.installed {
		services, err := v.client.CoreV1().Services(comp.Namespace).List(v.ctx, metav1.ListOptions{})
		if err != nil {
			c.Status = Unknown
			c.Message = fmt.Sprintf("Could not list Services in namespace %s.", comp.Namespace)
			return c
		}
		for _, s := range services.Items {
			if s.Spec.Type == corev1.ServiceTypeLoadBalancer || s.Spec.Type == corev1.ServiceTypeNodePort {
				problems = append(problems, fmt.Sprintf("Service %s/%s is of type %s.", comp.Namespace, untrusted.Clean(s.Name), s.Spec.Type))
			}
		}
		ingresses, err := v.client.NetworkingV1().Ingresses(comp.Namespace).List(v.ctx, metav1.ListOptions{})
		if err != nil {
			c.Status = Unknown
			c.Message = fmt.Sprintf("Could not list Ingresses in namespace %s.", comp.Namespace)
			return c
		}
		for _, i := range ingresses.Items {
			problems = append(problems, fmt.Sprintf("Ingress %s/%s exposes a platform service.", comp.Namespace, untrusted.Clean(i.Name)))
		}
	}
	return finish(c, problems, "No LoadBalancer or NodePort Service and no Ingress exists in the platform's namespaces.")
}

func (v *verifier) adminAccount() Check {
	c := Check{
		ID:        "argocd-admin-disabled",
		Title:     "Argo CD's built-in admin account is disabled",
		Reference: "Argo CD operator manual, user management (disable the admin user once SSO is set up)",
	}
	cm, err := v.client.CoreV1().ConfigMaps(v.namespaceOf(argoCD)).Get(v.ctx, "argocd-cm", metav1.GetOptions{})
	if err != nil {
		c.Status = Unknown
		c.Message = "Could not read the argocd-cm ConfigMap."
		return c
	}
	if cm.Data["admin.enabled"] == "false" {
		c.Status = Pass
		c.Message = "admin.enabled is false."
		return c
	}
	c.Status = Fail
	c.Message = "The admin account is enabled: admin.enabled is not set to false in argocd-cm."
	return c
}

func (v *verifier) defaultProject() Check {
	c := Check{
		ID:        "argocd-default-project-locked",
		Title:     "Argo CD's default project allows nothing",
		Reference: "Argo CD operator manual, projects (the default project should not be used)",
	}
	project, err := v.dyn.Resource(AppProjects).Namespace(v.namespaceOf(argoCD)).Get(v.ctx, "default", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		c.Status = Unknown
		c.Message = "The default AppProject does not exist yet. Argo CD creates it when it starts."
		return c
	}
	if err != nil {
		c.Status = Unknown
		c.Message = "Could not read the default AppProject."
		return c
	}

	var problems []string
	repos, _, _ := unstructured.NestedStringSlice(project.Object, "spec", "sourceRepos")
	if len(repos) > 0 {
		problems = append(problems, fmt.Sprintf("It allows %d source repository pattern(s).", len(repos)))
	}
	destinations, _, _ := unstructured.NestedSlice(project.Object, "spec", "destinations")
	if len(destinations) > 0 {
		problems = append(problems, fmt.Sprintf("It allows %d destination(s).", len(destinations)))
	}
	cluster, _, _ := unstructured.NestedSlice(project.Object, "spec", "clusterResourceWhitelist")
	if len(cluster) > 0 {
		problems = append(problems, "It allows cluster-scoped resources.")
	}
	return finish(c, problems, "The default project has no source repositories, destinations or cluster resources.")
}
