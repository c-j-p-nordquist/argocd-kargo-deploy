package platform

import (
	"os"
	"strings"
	"testing"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/questions"
)

// valid is a complete file that uses only secure defaults.
const valid = `
schemaVersion: v1alpha1
access:
  exposure: port-forward
identity:
  oidc:
    issuer: https://accounts.example.com
git:
  host: github
  credential: github-app
secrets:
  backend: vault
tenancy:
  mode: multi-tenant
promotion:
  production: pull-request
components:
  argoRollouts: disabled
`

func validate(t *testing.T, doc string) Result {
	t.Helper()
	res, err := Validate([]byte(doc), questions.Default())
	if err != nil {
		t.Fatalf("Validate returned an error: %v", err)
	}
	return res
}

// wantProblem checks that exactly one problem was reported, with the given
// field and code.
func wantProblem(t *testing.T, res Result, field, code string) {
	t.Helper()
	if res.Valid {
		t.Fatalf("result is valid, want problem %s on %s", code, field)
	}
	if len(res.Problems) != 1 || res.Problems[0].Field != field || res.Problems[0].Code != code {
		t.Fatalf("problems = %+v, want exactly one: %s on %s", res.Problems, code, field)
	}
}

func TestValidWithSecureDefaults(t *testing.T) {
	res := validate(t, valid)
	if !res.Valid || len(res.Problems) != 0 || len(res.Deviations) != 0 {
		t.Errorf("got %+v, want valid with no problems or deviations", res)
	}
}

func TestExampleFileIsValid(t *testing.T) {
	data, err := os.ReadFile("../../examples/platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Validate(data, questions.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Valid {
		t.Errorf("example is not valid: %+v", res.Problems)
	}
}

func TestProblems(t *testing.T) {
	tests := []struct {
		name        string
		old, new    string
		field, code string
	}{
		{"missing schema version", "schemaVersion: v1alpha1\n", "", "schemaVersion", CodeMissingAnswer},
		{"wrong schema version", "v1alpha1", "v2", "schemaVersion", CodeUnsupportedVersion},
		{"missing answer", "  mode: multi-tenant\n", "  {}\n", "tenancy.mode", CodeMissingAnswer},
		{"value not an option", "exposure: port-forward", "exposure: wide-open", "access.exposure", CodeInvalidValue},
		{"boolean instead of text", "argoRollouts: disabled", "argoRollouts: false", "components.argoRollouts", CodeWrongType},
		{"unknown top-level field", "access:", "extra: 1\naccess:", "extra", CodeUnknownField},
		{"misspelled nested field", "  production: pull-request", "  production: pull-request\n  prodution: automatic", "promotion.prodution", CodeUnknownField},
		{"issuer is not https", "https://accounts.example.com", "http://accounts.example.com", "identity.oidc.issuer", CodeInvalidValue},
		{"issuer is empty", "https://accounts.example.com", `""`, "identity.oidc.issuer", CodeMissingAnswer},
		{"weakening option without acknowledgement", "backend: vault", "backend: in-cluster", "secrets.backend", CodeMissingAcknowledgement},
		{"github app on another host", "host: github", "host: gitlab", "git.credential", CodeIncompatibleAnswers},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(valid, tt.old) {
				t.Fatalf("test setup: %q not found in the valid document", tt.old)
			}
			wantProblem(t, validate(t, strings.Replace(valid, tt.old, tt.new, 1)), tt.field, tt.code)
		})
	}
}

func TestAcknowledgedDeviationIsReported(t *testing.T) {
	doc := strings.Replace(valid, "backend: vault", "backend: in-cluster", 1) + `
acknowledgements:
  - question: secrets-backend
    option: in-cluster
    reason: demo only
`
	res := validate(t, doc)
	if !res.Valid {
		t.Fatalf("problems = %+v, want valid", res.Problems)
	}
	if len(res.Deviations) != 1 {
		t.Fatalf("deviations = %+v, want one", res.Deviations)
	}
	d := res.Deviations[0]
	if d.Question != "secrets-backend" || d.Option != "in-cluster" || d.Reason != "demo only" || d.Security == "" {
		t.Errorf("deviation = %+v", d)
	}
}

func TestAcknowledgementProblems(t *testing.T) {
	inCluster := strings.Replace(valid, "backend: vault", "backend: in-cluster", 1)
	tests := []struct {
		name        string
		doc         string
		field, code string
	}{
		{
			"acknowledgement for an option that was not chosen",
			valid + "acknowledgements:\n  - {question: secrets-backend, option: in-cluster, reason: stale}\n",
			"acknowledgements[0]", CodeUnusedAcknowledgement,
		},
		{
			"acknowledgement for a different option does not count",
			inCluster + "acknowledgements:\n  - {question: secrets-backend, option: in-cluster, reason: ok}\n  - {question: access-exposure, option: public, reason: stale}\n",
			"acknowledgements[1]", CodeUnusedAcknowledgement,
		},
		{
			"acknowledgements is not a list",
			valid + "acknowledgements: yes please\n",
			"acknowledgements", CodeWrongType,
		},
		{
			"unknown key in an acknowledgement",
			inCluster + "acknowledgements:\n  - {question: secrets-backend, option: in-cluster, reason: ok, approvedBy: me}\n",
			"acknowledgements[0].approvedBy", CodeUnknownField,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantProblem(t, validate(t, tt.doc), tt.field, tt.code)
		})
	}
}

func TestAcknowledgementWithoutReasonDoesNotCount(t *testing.T) {
	doc := strings.Replace(valid, "backend: vault", "backend: in-cluster", 1) +
		"acknowledgements:\n  - {question: secrets-backend, option: in-cluster}\n"
	res := validate(t, doc)
	codes := map[string]bool{}
	for _, p := range res.Problems {
		codes[p.Field+" "+p.Code] = true
	}
	for _, want := range []string{
		"acknowledgements[0].reason " + CodeMissingAnswer,
		"secrets.backend " + CodeMissingAcknowledgement,
	} {
		if !codes[want] {
			t.Errorf("missing problem %q in %+v", want, res.Problems)
		}
	}
}

func TestEmptyFileReportsEveryMissingAnswer(t *testing.T) {
	res := validate(t, "")
	want := len(questions.Default().Questions) + 1 // plus schemaVersion
	if len(res.Problems) != want {
		t.Errorf("got %d problems, want %d: %+v", len(res.Problems), want, res.Problems)
	}
	if res.Deviations == nil {
		t.Error("Deviations is nil; it must encode as an empty JSON list")
	}
}

func TestUnparseableInput(t *testing.T) {
	for name, doc := range map[string]string{
		"syntax error":      "a: [unclosed",
		"two documents":     valid + "---\n" + valid,
		"top level is list": "- a\n- b\n",
		"duplicate key":     "git:\n  host: github\n  host: gitlab\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Validate([]byte(doc), questions.Default()); err == nil {
				t.Error("Validate returned no error")
			}
		})
	}
}

func TestSchemaCoversEveryQuestion(t *testing.T) {
	q := questions.Default()
	schema := Schema(q)
	for _, question := range q.Questions {
		node := schema
		for _, part := range strings.Split(question.Field, ".") {
			required := node["required"].([]string)
			found := false
			for _, r := range required {
				found = found || r == part
			}
			if !found {
				t.Errorf("%s: %q is not required in the schema", question.Field, part)
			}
			next, ok := node["properties"].(map[string]any)[part].(map[string]any)
			if !ok {
				t.Fatalf("%s: no schema for %q", question.Field, part)
			}
			node = next
		}
	}
}
