package apply

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const manifests = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: server
---
# An empty document follows.
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: server
  namespace: elsewhere
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: things.example.io
---
apiVersion: batch/v1
kind: Job
metadata:
  name: setup
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
---
apiVersion: batch/v1
kind: Job
metadata:
  name: teardown
  annotations:
    helm.sh/hook: pre-delete
---
apiVersion: batch/v1
kind: Job
metadata:
  name: migrate
  annotations:
    helm.sh/hook: post-upgrade
`

func parse(t *testing.T) map[string]*unstructured.Unstructured {
	t.Helper()
	objects, err := Parse([]byte(manifests), "argocd")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*unstructured.Unstructured{}
	for _, o := range objects {
		byName[o.GetKind()+"/"+o.GetName()] = o
	}
	if len(byName) != 6 {
		t.Fatalf("parsed %d objects, want 6", len(byName))
	}
	return byName
}

func TestParseDefaultsTheNamespace(t *testing.T) {
	objects := parse(t)
	if got := objects["Deployment/server"].GetNamespace(); got != "argocd" {
		t.Errorf("namespace = %q, want the default", got)
	}
	if got := objects["ServiceAccount/server"].GetNamespace(); got != "elsewhere" {
		t.Errorf("namespace = %q, want the one in the manifest", got)
	}
}

func TestParseRejectsIncompleteManifests(t *testing.T) {
	for name, doc := range map[string]string{
		"no name": "apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n",
		"no kind": "apiVersion: v1\nmetadata:\n  name: x\n",
	} {
		if _, err := Parse([]byte(doc), "ns"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPhases(t *testing.T) {
	objects := parse(t)
	want := map[string]int{
		"CustomResourceDefinition/things.example.io": phaseCRDs,
		"Job/setup":             phaseHooks,
		"Deployment/server":     phaseResources,
		"ServiceAccount/server": phaseResources,
		// Running a pre-delete or upgrade hook during an install would be
		// harmful, so they must never be applied.
		"Job/teardown": phaseSkipped,
		"Job/migrate":  phaseSkipped,
	}
	for name, phase := range want {
		if got := phaseOf(objects[name]); got != phase {
			t.Errorf("%s: phase = %d, want %d", name, got, phase)
		}
	}
}

func TestKindOrderPutsIdentitiesBeforeWorkloads(t *testing.T) {
	order := []string{"NetworkPolicy", "ServiceAccount", "ConfigMap", "ClusterRole", "RoleBinding", "Service", "Deployment", "SomethingElse"}
	for i := 1; i < len(order); i++ {
		if kindOrder(order[i-1]) >= kindOrder(order[i]) {
			t.Errorf("%s should be applied before %s", order[i-1], order[i])
		}
	}
}

func status(kind string, generation int64, st map[string]any, spec map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"kind": kind, "metadata": map[string]any{"name": "x"}, "status": st}}
	o.SetGeneration(generation)
	if spec != nil {
		o.Object["spec"] = spec
	}
	return o
}

func TestWorkloadAvailable(t *testing.T) {
	ready := status("Deployment", 2, map[string]any{"observedGeneration": int64(2), "availableReplicas": int64(1)}, nil)
	if done, _, _ := workloadAvailable(ready); !done {
		t.Error("an available Deployment was not recognised")
	}

	stale := status("Deployment", 3, map[string]any{"observedGeneration": int64(2), "availableReplicas": int64(1)}, nil)
	if done, _, _ := workloadAvailable(stale); done {
		t.Error("a Deployment whose latest change is not rolled out yet was reported available")
	}

	blocked := status("Deployment", 1, map[string]any{
		"observedGeneration": int64(1),
		"conditions":         []any{map[string]any{"type": "ReplicaFailure", "status": "True", "message": "violates PodSecurity"}},
	}, map[string]any{"replicas": int64(2)})
	done, why, _ := workloadAvailable(blocked)
	if done || !strings.Contains(why, "0 of 2") || !strings.Contains(why, "violates PodSecurity") {
		t.Errorf("done = %v, why = %q", done, why)
	}

	sts := status("StatefulSet", 1, map[string]any{"observedGeneration": int64(1), "readyReplicas": int64(1)}, nil)
	if done, _, _ := workloadAvailable(sts); !done {
		t.Error("a ready StatefulSet was not recognised")
	}
}

func TestJobSucceeded(t *testing.T) {
	if done, _, err := jobSucceeded(status("Job", 1, map[string]any{"succeeded": int64(1)}, nil)); !done || err != nil {
		t.Errorf("done = %v, err = %v", done, err)
	}
	if done, _, err := jobSucceeded(status("Job", 1, map[string]any{}, nil)); done || err != nil {
		t.Errorf("a running job: done = %v, err = %v", done, err)
	}
	failed := status("Job", 1, map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "True", "message": "BackoffLimitExceeded"}}}, nil)
	if _, _, err := jobSucceeded(failed); err == nil {
		t.Error("a failed job did not give an error")
	}
}
