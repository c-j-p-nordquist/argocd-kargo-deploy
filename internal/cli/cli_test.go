package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/questions"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Run(args, &out, &errOut)
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
	for _, name := range []string{"preflight", "validate", "plan", "apply", "verify"} {
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
