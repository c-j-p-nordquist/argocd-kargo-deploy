//go:build e2e

// Package e2e installs the bootstrap stage on a real cluster. It changes the
// cluster named by KUBECONFIG, so it only runs with the e2e build tag, and
// only against the throwaway cluster from the e2e compose profile.
package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/cli"
)

// k3s names its kubeconfig context "default".
const clusterContext = "default"

func run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cli.Run(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestInstallBootstrapStage(t *testing.T) {
	if os.Getenv("KUBECONFIG") == "" {
		t.Fatal("KUBECONFIG is not set; run this through the e2e compose profile")
	}
	dir := filepath.Join(t.TempDir(), "plan")

	code, stdout, stderr := run("plan", "--file", "../examples/platform.yaml", "--out", dir, "--output", "json")
	if code != cli.ExitOK {
		t.Fatalf("plan: exit %d: %s", code, stderr)
	}
	var planned struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal([]byte(stdout), &planned); err != nil {
		t.Fatal(err)
	}

	apply := func(extra ...string) (int, string, string) {
		args := []string{"apply", "--plan", planned.Hash, "--dir", dir, "--context", clusterContext, "--timeout", "8m", "--output", "json"}
		return run(append(args, extra...)...)
	}

	// The plan is incomplete, so a plain apply must refuse before it touches
	// the cluster.
	if code, _, stderr := apply(); code != cli.ExitInvalid || !strings.Contains(stderr, "plan_incomplete") {
		t.Fatalf("apply of an incomplete plan: exit %d: %s", code, stderr)
	}

	code, stdout, stderr = apply("--allow-incomplete")
	if code != cli.ExitOK {
		t.Fatalf("apply: exit %d: %s", code, stderr)
	}
	var result struct {
		Context   string         `json:"context"`
		Applied   map[string]int `json:"applied"`
		Workloads []string       `json:"workloads"`
		Skipped   []any          `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("apply output is not JSON: %v\n%s", err, stdout)
	}
	t.Logf("applied: %v", result.Applied)
	t.Logf("available: %v", result.Workloads)
	if result.Context != clusterContext || result.Applied["Namespace"] != 1 || result.Applied["CustomResourceDefinition"] == 0 || len(result.Workloads) == 0 {
		t.Errorf("unexpected result: %+v", result)
	}

	// Applying the same plan again must succeed and change nothing that
	// matters.
	if code, _, stderr := apply("--allow-incomplete"); code != cli.ExitOK {
		t.Errorf("second apply: exit %d: %s", code, stderr)
	}

	// Verify must report the live cluster truthfully: what this toolkit sets
	// up passes, and the parts of the baseline it does not set up yet fail.
	// When one of those gaps is closed, move its id to the passing list.
	code, stdout, stderr = run("verify", "--context", clusterContext, "--output", "json")
	if code != cli.ExitInvalid {
		t.Errorf("verify: exit %d, want %d while the baseline is not fully met: %s", code, cli.ExitInvalid, stderr)
	}
	var report struct {
		Compliant bool     `json:"compliant"`
		Installed []string `json:"installed"`
		Checks    []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("verify output is not JSON: %v\n%s", err, stdout)
	}
	want := map[string]string{
		"images-match-lock":             "pass",
		"pod-security":                  "pass",
		"not-exposed":                   "pass",
		"network-default-deny":          "fail",
		"argocd-admin-disabled":         "fail",
		"argocd-default-project-locked": "fail",
	}
	for _, c := range report.Checks {
		t.Logf("verify: %-7s %s: %s", c.Status, c.ID, c.Message)
		if want[c.ID] != c.Status {
			t.Errorf("verify: %s is %s, want %s", c.ID, c.Status, want[c.ID])
		}
		delete(want, c.ID)
	}
	for id := range want {
		t.Errorf("verify: check %s is missing", id)
	}
	if report.Compliant || strings.Join(report.Installed, ",") != "argo-cd" {
		t.Errorf("verify: compliant = %v, installed = %v", report.Compliant, report.Installed)
	}

	// A plan edited after approval must be refused.
	path := filepath.Join(dir, "bootstrap", "00-namespaces.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	relaxed := strings.Replace(string(data), "enforce: restricted", "enforce: privileged", 1)
	if err := os.WriteFile(path, []byte(relaxed), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := apply("--allow-incomplete"); code != cli.ExitInvalid || !strings.Contains(stderr, "plan_changed") {
		t.Errorf("apply of an edited plan: exit %d: %s", code, stderr)
	}
}
