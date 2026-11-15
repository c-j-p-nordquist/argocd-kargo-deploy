// Package apply installs the bootstrap stage of a plan on a cluster.
//
// Everything is applied with server-side apply under one field manager and
// without forcing conflicts. Re-running an apply is safe, and a resource that
// something else already manages is left alone and reported as an error
// instead of being taken over.
package apply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/plan"
)

// FieldManager identifies this tool as the owner of the fields it applies.
const FieldManager = "gitops-platform-bootstrap"

const hookAnnotation = "helm.sh/hook"

// Phases, in the order they are applied.
const (
	phaseNamespaces = iota
	phaseCRDs
	phaseHooks
	phaseResources
	phaseSkipped
)

// Options control an apply.
type Options struct {
	// Timeout bounds each wait: for CRDs, for a hook job, and for workloads.
	Timeout time.Duration
	// Progress receives one line per step. It may be nil.
	Progress io.Writer
}

// Result describes what an apply did.
type Result struct {
	// Applied counts the resources applied, by kind.
	Applied map[string]int `json:"applied"`
	// Skipped lists resources in the plan that were deliberately not applied,
	// with the reason.
	Skipped []Skipped `json:"skipped"`
	// Workloads lists the Deployments and StatefulSets that were waited for.
	Workloads []string `json:"workloads"`
}

// Skipped is a resource that was left out.
type Skipped struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type object struct {
	*unstructured.Unstructured
	phase int
}

// Bootstrap applies the bootstrap stage of the plan and waits until its
// workloads are available.
func Bootstrap(ctx context.Context, client dynamic.Interface, mapper meta.ResettableRESTMapper, p plan.Plan, opts Options) (Result, error) {
	res := Result{Applied: map[string]int{}, Skipped: []Skipped{}, Workloads: []string{}}
	a := &applier{client: client, mapper: mapper, opts: opts}

	namespaces := map[string]string{}
	for _, c := range p.Components {
		namespaces[c.Stage+"/"+c.Name+".yaml"] = c.Namespace
	}

	var objects []object
	for _, f := range p.Files {
		if !strings.HasPrefix(f.Path, plan.StageBootstrap+"/") {
			continue
		}
		parsed, err := Parse(f.Data(), namespaces[f.Path])
		if err != nil {
			return res, fmt.Errorf("%s: %w", f.Path, err)
		}
		for _, o := range parsed {
			objects = append(objects, object{o, phaseOf(o)})
		}
	}
	if len(objects) == 0 {
		return res, errors.New("the plan has no bootstrap stage")
	}
	// Within a phase, apply in dependency order: identities and configuration
	// before the workloads that use them.
	sort.SliceStable(objects, func(i, j int) bool {
		if objects[i].phase != objects[j].phase {
			return objects[i].phase < objects[j].phase
		}
		return kindOrder(objects[i].GetKind()) < kindOrder(objects[j].GetKind())
	})

	for phase := phaseNamespaces; phase <= phaseResources; phase++ {
		var applied []object
		for _, o := range objects {
			if o.phase != phase {
				continue
			}
			if phase == phaseHooks && o.GetKind() == "Job" {
				// A hook job runs once per apply. Remove the previous run
				// first, as Helm does for before-hook-creation.
				if err := a.deleteAndWait(ctx, o); err != nil {
					return res, err
				}
			}
			if err := a.apply(ctx, o); err != nil {
				return res, err
			}
			res.Applied[o.GetKind()]++
			applied = append(applied, o)
		}
		if len(applied) == 0 {
			continue
		}

		switch phase {
		case phaseCRDs:
			a.progress("waiting for %d custom resource definitions", len(applied))
			if err := a.waitAll(ctx, applied, crdEstablished); err != nil {
				return res, err
			}
			// The cluster now serves kinds it did not serve a moment ago.
			mapper.Reset()
		case phaseHooks:
			var jobs []object
			for _, o := range applied {
				if o.GetKind() == "Job" {
					jobs = append(jobs, o)
				}
			}
			a.progress("waiting for %d setup job(s)", len(jobs))
			if err := a.waitAll(ctx, jobs, jobSucceeded); err != nil {
				return res, err
			}
		case phaseResources:
			var workloads []object
			for _, o := range applied {
				if o.GetKind() == "Deployment" || o.GetKind() == "StatefulSet" {
					workloads = append(workloads, o)
					res.Workloads = append(res.Workloads, o.GetKind()+"/"+o.GetName())
				}
			}
			a.progress("waiting for %d workloads to become available", len(workloads))
			if err := a.waitAll(ctx, workloads, workloadAvailable); err != nil {
				return res, err
			}
		}
	}

	for _, o := range objects {
		if o.phase == phaseSkipped {
			res.Skipped = append(res.Skipped, Skipped{
				Kind:   o.GetKind(),
				Name:   o.GetName(),
				Reason: fmt.Sprintf("Helm hook %q does not run at install time", o.GetAnnotations()[hookAnnotation]),
			})
		}
	}
	return res, nil
}

// Parse reads a stream of YAML manifests. Objects without a namespace get
// defaultNamespace; for cluster-scoped kinds the API server ignores it later.
func Parse(manifests []byte, defaultNamespace string) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifests), 4096)
	for {
		o := &unstructured.Unstructured{}
		err := dec.Decode(&o.Object)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(o.Object) == 0 {
			continue
		}
		if o.GetKind() == "" || o.GetAPIVersion() == "" || o.GetName() == "" {
			return nil, fmt.Errorf("a manifest is missing its kind, apiVersion or name")
		}
		if o.GetNamespace() == "" && defaultNamespace != "" {
			o.SetNamespace(defaultNamespace)
		}
		out = append(out, o)
	}
}

// phaseOf decides when an object is applied.
func phaseOf(o *unstructured.Unstructured) int {
	if hook, ok := o.GetAnnotations()[hookAnnotation]; ok {
		for _, h := range strings.Split(hook, ",") {
			if strings.TrimSpace(h) == "pre-install" {
				return phaseHooks
			}
		}
		// Upgrade, delete and test hooks have no place in a first install.
		// Applying a pre-delete hook as an ordinary resource would run it.
		return phaseSkipped
	}
	switch o.GetKind() {
	case "Namespace":
		return phaseNamespaces
	case "CustomResourceDefinition":
		return phaseCRDs
	}
	return phaseResources
}

var kinds = []string{
	"Namespace", "CustomResourceDefinition",
	"NetworkPolicy", "ServiceAccount", "Secret", "ConfigMap",
	"ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding",
	// Argo CD creates a wide-open default project if none exists when it
	// starts, so projects go in before the workloads.
	"Service", "AppProject", "Job", "Deployment", "StatefulSet",
}

func kindOrder(kind string) int {
	for i, k := range kinds {
		if k == kind {
			return i
		}
	}
	return len(kinds)
}

type applier struct {
	client dynamic.Interface
	mapper meta.ResettableRESTMapper
	opts   Options
}

func (a *applier) progress(format string, args ...any) {
	if a.opts.Progress != nil {
		fmt.Fprintf(a.opts.Progress, format+"\n", args...)
	}
}

// resource returns the client for the object's kind, scoped to its namespace
// when the kind is namespaced.
func (a *applier) resource(o object) (dynamic.ResourceInterface, error) {
	gvk := o.GroupVersionKind()
	mapping, err := a.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("%s %s: the cluster does not serve this kind: %w", o.GetKind(), o.GetName(), err)
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		return a.client.Resource(mapping.Resource).Namespace(o.GetNamespace()), nil
	}
	return a.client.Resource(mapping.Resource), nil
}

func (a *applier) apply(ctx context.Context, o object) error {
	r, err := a.resource(o)
	if err != nil {
		return err
	}
	// Cluster-scoped objects were given a default namespace while parsing.
	obj := o.DeepCopy()
	if gvk := o.GroupVersionKind(); isClusterScoped(a.mapper, gvk) {
		obj.SetNamespace("")
	}
	_, err = r.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: FieldManager})
	if apierrors.IsConflict(err) {
		return fmt.Errorf("%s %s is already managed by something else, so it was left alone: %w", o.GetKind(), o.GetName(), err)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", o.GetKind(), o.GetName(), err)
	}
	return nil
}

func isClusterScoped(mapper meta.RESTMapper, gvk schema.GroupVersionKind) bool {
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	return err == nil && mapping.Scope.Name() == meta.RESTScopeNameRoot
}

func (a *applier) deleteAndWait(ctx context.Context, o object) error {
	r, err := a.resource(o)
	if err != nil {
		return err
	}
	policy := metav1.DeletePropagationBackground
	err = r.Delete(ctx, o.GetName(), metav1.DeleteOptions{PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", o.GetKind(), o.GetName(), err)
	}
	return a.poll(ctx, func() (bool, string, error) {
		_, err := r.Get(ctx, o.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, "", nil
		}
		return false, o.GetKind() + " " + o.GetName() + " is still being deleted", err
	})
}

// condition reports whether an object has reached the wanted state. When it
// has not, why says what it is waiting for. A non-nil error means it never
// will.
type condition func(live *unstructured.Unstructured) (done bool, why string, err error)

func (a *applier) waitAll(ctx context.Context, objects []object, check condition) error {
	for _, o := range objects {
		r, err := a.resource(o)
		if err != nil {
			return err
		}
		err = a.poll(ctx, func() (bool, string, error) {
			live, err := r.Get(ctx, o.GetName(), metav1.GetOptions{})
			if err != nil {
				return false, "", err
			}
			done, why, err := check(live)
			return done, o.GetKind() + " " + o.GetName() + ": " + why, err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// poll calls check every two seconds until it is done, fails, or the timeout
// passes. On timeout the last reason is part of the error.
func (a *applier) poll(ctx context.Context, check func() (bool, string, error)) error {
	deadline := time.Now().Add(a.opts.Timeout)
	for {
		done, why, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: %s", a.opts.Timeout, why)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func conditions(live *unstructured.Unstructured) []map[string]any {
	raw, _, _ := unstructured.NestedSlice(live.Object, "status", "conditions")
	var out []map[string]any
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func crdEstablished(live *unstructured.Unstructured) (bool, string, error) {
	for _, c := range conditions(live) {
		if c["type"] == "Established" && c["status"] == "True" {
			return true, "", nil
		}
	}
	return false, "not established yet", nil
}

func jobSucceeded(live *unstructured.Unstructured) (bool, string, error) {
	for _, c := range conditions(live) {
		if c["type"] == "Failed" && c["status"] == "True" {
			return false, "", fmt.Errorf("Job %s failed: %v", live.GetName(), c["message"])
		}
	}
	if n, _, _ := unstructured.NestedInt64(live.Object, "status", "succeeded"); n > 0 {
		return true, "", nil
	}
	return false, "has not completed" + failureDetail(live), nil
}

func workloadAvailable(live *unstructured.Unstructured) (bool, string, error) {
	want, found, _ := unstructured.NestedInt64(live.Object, "spec", "replicas")
	if !found {
		want = 1
	}
	observed, _, _ := unstructured.NestedInt64(live.Object, "status", "observedGeneration")
	field := "availableReplicas"
	if live.GetKind() == "StatefulSet" {
		field = "readyReplicas"
	}
	have, _, _ := unstructured.NestedInt64(live.Object, "status", field)
	if observed >= live.GetGeneration() && have >= want {
		return true, "", nil
	}
	return false, fmt.Sprintf("%d of %d replicas available%s", have, want, failureDetail(live)), nil
}

// failureDetail returns the message of a condition that explains why pods
// are not being created, such as a Pod Security rejection.
func failureDetail(live *unstructured.Unstructured) string {
	for _, c := range conditions(live) {
		if (c["type"] == "ReplicaFailure" || c["type"] == "FailureTarget") && c["status"] == "True" {
			return fmt.Sprintf(" (%v)", c["message"])
		}
	}
	return ""
}
