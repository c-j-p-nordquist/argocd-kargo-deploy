package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/questions"
)

func run(args ...string) (code int, stdout, stderr string) {
	return runWithInput("", args...)
}

func runWithInput(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Run(args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestNoArgsIsUsageError(t *testing.T) {
	code, _, stderr := run()
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "Commands:") {
		t.Errorf("stderr does not list commands: %q", stderr)
	}
}

func TestHelp(t *testing.T) {
	code, stdout, _ := run("help")
	if code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "apply") || !strings.Contains(stdout, "(changes the cluster)") {
		t.Errorf("help does not mark apply as changing the cluster: %q", stdout)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, stderr := run("bogus")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, `unknown command "bogus"`) {
		t.Errorf("unexpected stderr: %q", stderr)
	}
}

func TestInvalidOutputFormat(t *testing.T) {
	code, _, stderr := run("version", "--output", "xml")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "must be text or json") {
		t.Errorf("unexpected stderr: %q", stderr)
	}
}

func TestUnexpectedArgument(t *testing.T) {
	if code, _, _ := run("version", "extra"); code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestNotImplementedCommandsReportJSONError(t *testing.T) {
	for _, name := range []string{"verify"} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := run(name, "--output", "json")
			if code != ExitNotImplemented {
				t.Errorf("exit = %d, want %d", code, ExitNotImplemented)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			var got struct {
				Error Error `json:"error"`
			}
			if err := json.Unmarshal([]byte(stderr), &got); err != nil {
				t.Fatalf("stderr is not JSON: %v\n%s", err, stderr)
			}
			if got.Error.Code != "not_implemented" {
				t.Errorf("code = %q, want not_implemented", got.Error.Code)
			}
		})
	}
}

func TestVersionJSON(t *testing.T) {
	code, stdout, _ := run("version", "--output", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if got["name"] != Name || got["version"] != Version {
		t.Errorf("got %v", got)
	}
}

func TestQuestionsJSONRoundTrips(t *testing.T) {
	code, stdout, stderr := run("questions", "--output", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var got questions.Questionnaire
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	want := questions.Default()
	if got.SchemaVersion != want.SchemaVersion || len(got.Questions) != len(want.Questions) {
		t.Errorf("got version %q with %d questions, want %q with %d",
			got.SchemaVersion, len(got.Questions), want.SchemaVersion, len(want.Questions))
	}
}

func TestQuestionsText(t *testing.T) {
	code, stdout, _ := run("questions")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	for _, want := range []string{"port-forward (default)", "in-cluster (requires acknowledgement)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output does not contain %q", want)
		}
	}
}

const examplePath = "../../examples/platform.yaml"

func TestValidateExampleFile(t *testing.T) {
	code, stdout, stderr := run("validate", "--file", examplePath, "--output", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	var got struct {
		File       string `json:"file"`
		Valid      bool   `json:"valid"`
		Deviations []struct {
			Question string `json:"question"`
		} `json:"deviations"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if !got.Valid || got.File != examplePath {
		t.Errorf("got %+v", got)
	}
	if len(got.Deviations) != 1 || got.Deviations[0].Question != "secrets-backend" {
		t.Errorf("deviations = %+v, want the in-cluster secrets backend", got.Deviations)
	}
}

func TestValidateInvalidFromStdin(t *testing.T) {
	code, stdout, stderr := runWithInput("schemaVersion: v1alpha1\n", "validate", "--file", "-", "--output", "json")
	if code != ExitInvalid {
		t.Fatalf("exit = %d, want %d (stderr = %s)", code, ExitInvalid, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty: problems belong in the result", stderr)
	}
	var got struct {
		Valid    bool `json:"valid"`
		Problems []struct {
			Code string `json:"code"`
		} `json:"problems"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if got.Valid || len(got.Problems) == 0 {
		t.Errorf("got %+v, want invalid with problems", got)
	}
}

func TestValidateMissingFile(t *testing.T) {
	code, _, stderr := run("validate", "--file", "does-not-exist.yaml", "--output", "json")
	if code != ExitFailure {
		t.Errorf("exit = %d, want %d", code, ExitFailure)
	}
	if !strings.Contains(stderr, `"file_not_found"`) {
		t.Errorf("unexpected stderr: %s", stderr)
	}
}

func TestValidateUnparseableYAML(t *testing.T) {
	code, _, stderr := runWithInput("a: [unclosed\n", "validate", "--file", "-", "--output", "json")
	if code != ExitFailure {
		t.Errorf("exit = %d, want %d", code, ExitFailure)
	}
	if !strings.Contains(stderr, `"invalid_yaml"`) {
		t.Errorf("unexpected stderr: %s", stderr)
	}
}

func TestSchemaIsJSON(t *testing.T) {
	code, stdout, _ := run("schema")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if got["additionalProperties"] != false {
		t.Errorf("schema must reject unknown top-level fields")
	}
}

func TestPreflightWithMissingKubeconfig(t *testing.T) {
	code, stdout, stderr := run("preflight", "--kubeconfig", "does-not-exist.yaml", "--output", "json")
	if code != ExitFailure {
		t.Errorf("exit = %d, want %d", code, ExitFailure)
	}
	if stdout != "" || !strings.Contains(stderr, `"kubeconfig_invalid"`) {
		t.Errorf("stdout = %q, stderr = %s", stdout, stderr)
	}
}

func TestComponentsListsTheLock(t *testing.T) {
	code, stdout, stderr := run("components", "--output", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var got struct {
		Components []struct {
			Name   string `json:"name"`
			Images []struct {
				Digest string `json:"digest"`
			} `json:"images"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if len(got.Components) == 0 || len(got.Components[0].Images) == 0 || got.Components[0].Images[0].Digest == "" {
		t.Errorf("unexpected components: %+v", got)
	}

	code, stdout, _ = run("components")
	if code != ExitOK || !strings.Contains(stdout, "pinned by digest") {
		t.Errorf("text output: exit = %d, stdout = %q", code, stdout)
	}
}

func TestPlanWritesAPlanAndPrintsItsHash(t *testing.T) {
	out := t.TempDir()
	code, stdout, stderr := run("plan", "--file", examplePath, "--out", out, "--output", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var got struct {
		Directory     string   `json:"directory"`
		Hash          string   `json:"hash"`
		NotYetApplied []string `json:"notYetApplied"`
		Files         []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if got.Directory != out || !strings.HasPrefix(got.Hash, "sha256:") || len(got.Files) == 0 {
		t.Errorf("got %+v", got)
	}
	if len(got.NotYetApplied) == 0 {
		t.Error("the plan does not say which answers it leaves out")
	}

	// The text form must say the plan is incomplete and print the same hash.
	code, text, _ := run("plan", "--file", examplePath, "--out", out)
	if code != ExitOK || !strings.Contains(text, "This plan is incomplete") || !strings.Contains(text, got.Hash) {
		t.Errorf("text output: exit = %d\n%s", code, text)
	}
}

func TestPlanRefusesInvalidAnswers(t *testing.T) {
	out := t.TempDir()
	code, stdout, stderr := runWithInput("schemaVersion: v1alpha1\n", "plan", "--file", "-", "--out", out, "--output", "json")
	if code != ExitInvalid {
		t.Errorf("exit = %d, want %d", code, ExitInvalid)
	}
	if stdout != "" || !strings.Contains(stderr, `"answers_invalid"`) {
		t.Errorf("stdout = %q, stderr = %s", stdout, stderr)
	}
}

func TestApplyRefusals(t *testing.T) {
	dir := t.TempDir()
	if code, _, stderr := run("plan", "--file", examplePath, "--out", dir); code != ExitOK {
		t.Fatalf("plan failed: %s", stderr)
	}
	var planned struct {
		Hash string `json:"hash"`
	}
	_, stdout, _ := run("plan", "--file", examplePath, "--out", dir, "--output", "json")
	if err := json.Unmarshal([]byte(stdout), &planned); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
		exit int
		code string
	}{
		{"no hash", []string{"--dir", dir, "--context", "c"}, ExitUsage, "usage"},
		{"no context", []string{"--dir", dir, "--plan", planned.Hash}, ExitUsage, "usage"},
		{"wrong hash", []string{"--dir", dir, "--context", "c", "--plan", "sha256:0000"}, ExitInvalid, "plan_mismatch"},
		{"no plan in the directory", []string{"--dir", t.TempDir(), "--context", "c", "--plan", planned.Hash}, ExitInvalid, "plan_not_found"},
		{"incomplete plan", []string{"--dir", dir, "--context", "c", "--plan", planned.Hash}, ExitInvalid, "plan_incomplete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(append([]string{"apply", "--output", "json"}, tt.args...)...)
			if code != tt.exit || !strings.Contains(stderr, `"`+tt.code+`"`) || stdout != "" {
				t.Errorf("exit = %d, want %d with code %s\nstdout: %s\nstderr: %s", code, tt.exit, tt.code, stdout, stderr)
			}
		})
	}
}
