package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/components"
	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/platform"
	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/questions"
)

func answers(t *testing.T, edit func(string) string) platform.Result {
	t.Helper()
	data, err := os.ReadFile("../../examples/platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	if edit != nil {
		doc = edit(doc)
	}
	res, err := platform.Validate([]byte(doc), questions.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Valid {
		t.Fatalf("answers are not valid: %+v", res.Problems)
	}
	return res
}

func build(t *testing.T, edit func(string) string) Plan {
	t.Helper()
	lock, err := components.LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(answers(t, edit), lock)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func withRollouts(doc string) string {
	return strings.Replace(doc, "argoRollouts: disabled", "argoRollouts: enabled", 1)
}

func paths(p Plan) []string {
	var out []string
	for _, f := range p.Files {
		out = append(out, f.Path)
	}
	return out
}

func TestBuildFromExampleAnswers(t *testing.T) {
	p := build(t, nil)

	want := []string{
		"bootstrap/00-namespaces.yaml",
		"bootstrap/argo-cd.yaml",
		"platform/00-namespaces.yaml",
		"platform/cert-manager.yaml",
		"platform/external-secrets.yaml",
		"platform/kargo.yaml",
		"platform/kyverno.yaml",
	}
	if got := paths(p); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("files = %v\nwant    %v", got, want)
	}
	if !strings.HasPrefix(p.Hash, "sha256:") || len(p.Hash) != len("sha256:")+64 {
		t.Errorf("hash = %q", p.Hash)
	}
	if len(p.Deviations) != 1 {
		t.Errorf("deviations = %+v, want the in-cluster secrets backend carried through", p.Deviations)
	}
	for _, c := range p.Components {
		wantStage := StagePlatform
		if c.Name == "argo-cd" {
			wantStage = StageBootstrap
		}
		if c.Stage != wantStage {
			t.Errorf("%s: stage = %s, want %s", c.Name, c.Stage, wantStage)
		}
	}
}

func TestSameAnswersGiveTheSameHash(t *testing.T) {
	if a, b := build(t, nil), build(t, nil); a.Hash != b.Hash {
		t.Errorf("hashes differ: %s and %s", a.Hash, b.Hash)
	}
}

func TestOptionalComponentFollowsTheAnswer(t *testing.T) {
	without, with := build(t, nil), build(t, withRollouts)
	if strings.Contains(strings.Join(paths(without), " "), "argo-rollouts") {
		t.Error("argo-rollouts is in the plan although it was not asked for")
	}
	if !strings.Contains(strings.Join(paths(with), " "), "platform/argo-rollouts.yaml") {
		t.Error("argo-rollouts is missing although it was asked for")
	}
	if without.Hash == with.Hash {
		t.Error("the hash did not change when a component was added")
	}
}

func TestNamespacesEnforceRestrictedPodSecurity(t *testing.T) {
	p := build(t, nil)
	for _, f := range p.Files {
		if !strings.HasSuffix(f.Path, "00-namespaces.yaml") {
			continue
		}
		namespaces := strings.Count(string(f.data), "kind: Namespace")
		enforced := strings.Count(string(f.data), "pod-security.kubernetes.io/enforce: restricted")
		if namespaces == 0 || namespaces != enforced {
			t.Errorf("%s: %d namespaces, %d enforce restricted", f.Path, namespaces, enforced)
		}
	}
	platformNS := ""
	for _, f := range p.Files {
		if f.Path == "platform/00-namespaces.yaml" {
			platformNS = string(f.data)
		}
	}
	for _, name := range []string{"cert-manager", "external-secrets", "kargo", "kyverno"} {
		if !strings.Contains(platformNS, "name: "+name+"\n") {
			t.Errorf("platform namespaces are missing %s", name)
		}
	}
}

func TestEveryAnswerIsAppliedOrReported(t *testing.T) {
	p := build(t, nil)
	reported := map[string]bool{}
	for _, f := range p.NotYetApplied {
		reported[f] = true
	}
	for _, q := range questions.Default().Questions {
		if appliedFields[q.Field] == reported[q.Field] {
			t.Errorf("%s: applied = %v, reported as not applied = %v; it must be exactly one", q.Field, appliedFields[q.Field], reported[q.Field])
		}
	}
}

func TestInvalidAnswersAreRefused(t *testing.T) {
	lock, err := components.LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(platform.Result{Valid: false}, lock); err == nil {
		t.Error("Build accepted invalid answers")
	}
}

func TestImageNotInTheLockIsRefused(t *testing.T) {
	c := components.Component{
		Name:   "example",
		Images: []components.Image{{Repository: "registry.example/app", Tag: "v1", Digest: "sha256:" + strings.Repeat("a", 64)}},
	}
	pinned := "registry.example/app:v1@sha256:" + strings.Repeat("a", 64)
	manifest := func(image string) []byte {
		return []byte("kind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - image: " + image + "\n")
	}

	if err := checkImages(c, manifest(pinned)); err != nil {
		t.Errorf("the pinned image was refused: %v", err)
	}
	for name, image := range map[string]string{
		"tag without digest": "registry.example/app:v1",
		"different digest":   "registry.example/app:v1@sha256:" + strings.Repeat("b", 64),
		"different image":    "registry.example/other:v1@sha256:" + strings.Repeat("a", 64),
	} {
		if err := checkImages(c, manifest(image)); err == nil {
			t.Errorf("%s: %s was accepted", name, image)
		}
	}
}

func TestWriteThenReadRoundTrips(t *testing.T) {
	p := build(t, nil)
	dir := filepath.Join(t.TempDir(), "plan")
	if err := p.Write(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash != p.Hash {
		t.Errorf("hash read back = %s, want %s", got.Hash, p.Hash)
	}
	// Writing again over a previous plan is allowed.
	if err := build(t, withRollouts).Write(dir); err != nil {
		t.Errorf("writing over a previous plan failed: %v", err)
	}
}

func TestReadDetectsTampering(t *testing.T) {
	tests := map[string]func(t *testing.T, dir string){
		"changed file": func(t *testing.T, dir string) {
			appendTo(t, filepath.Join(dir, "bootstrap", "argo-cd.yaml"), "\n# changed\n")
		},
		"added file": func(t *testing.T, dir string) {
			appendTo(t, filepath.Join(dir, "platform", "extra.yaml"), "kind: ClusterRoleBinding\n")
		},
		"removed file": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "platform", "kyverno.yaml")); err != nil {
				t.Fatal(err)
			}
		},
		"file and its recorded hash both changed": func(t *testing.T, dir string) {
			path := filepath.Join(dir, "bootstrap", "00-namespaces.yaml")
			before, _ := os.ReadFile(path)
			appendTo(t, path, "# changed\n")
			after, _ := os.ReadFile(path)
			index := filepath.Join(dir, IndexFile)
			raw, _ := os.ReadFile(index)
			edited := strings.Replace(string(raw), newFile("", before).SHA256, newFile("", after).SHA256, 1)
			if edited == string(raw) {
				t.Fatal("test setup: recorded hash not found")
			}
			if err := os.WriteFile(index, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, tamper := range tests {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "plan")
			if err := build(t, nil).Write(dir); err != nil {
				t.Fatal(err)
			}
			tamper(t, dir)
			if _, err := Read(dir); err == nil {
				t.Error("Read accepted the tampered plan")
			}
		})
	}
}

func TestWriteRefusesADirectoryWithOtherContent(t *testing.T) {
	dir := t.TempDir()
	appendTo(t, filepath.Join(dir, "notes.txt"), "mine")
	if err := build(t, nil).Write(dir); err == nil {
		t.Error("Write wrote into a directory that holds other files")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Errorf("the other file was disturbed: %v", err)
	}
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}
