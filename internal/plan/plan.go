// Package plan assembles the manifests an installation would apply and gives
// them a content hash.
//
// A plan is built only from what is embedded in the binary: the component
// lock and the manifests rendered from it. Building a plan needs no network
// and no cluster, and the same answers always give the same hash.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/components"
	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/platform"
	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/questions"
)

// SchemaVersion identifies the shape of plan.json.
const SchemaVersion = "v1alpha1"

// IndexFile is the file in a plan directory that lists the plan's contents.
const IndexFile = "plan.json"

// Stages. The bootstrap stage is applied by the CLI. The platform stage is
// what Argo CD syncs once it is running.
const (
	StageBootstrap = "bootstrap"
	StagePlatform  = "platform"
)

// bootstrapComponent is the one component the CLI installs itself.
const bootstrapComponent = "argo-cd"

// optionalComponents maps an optional component to the answer that turns it
// on.
var optionalComponents = map[string]struct{ field, value string }{
	"argo-rollouts": {"components.argoRollouts", "enabled"},
}

// appliedFields are the answers that already change the manifests. Every
// other answer is reported as not yet applied.
var appliedFields = map[string]bool{
	"components.argoRollouts": true,
	"identity.oidc.issuer":    true,
	"identity.oidc.clientId":  true,
	"identity.adminGroup":     true,
}

// Plan is everything an installation would apply.
type Plan struct {
	SchemaVersion string `json:"schemaVersion"`
	// Hash identifies the exact content of Files.
	Hash       string               `json:"hash"`
	Components []Component          `json:"components"`
	Files      []File               `json:"files"`
	Deviations []platform.Deviation `json:"deviations"`
	// NotYetApplied lists the answers that do not change the manifests yet.
	// The plan is incomplete while this list is not empty.
	NotYetApplied []string `json:"notYetApplied"`
}

// Component is one component in the plan.
type Component struct {
	Name         string `json:"name"`
	Stage        string `json:"stage"`
	Namespace    string `json:"namespace"`
	ChartVersion string `json:"chartVersion"`
	Images       int    `json:"images"`
}

// File is one manifest file in the plan.
type File struct {
	// Path is relative to the plan directory and uses forward slashes.
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`

	data []byte
}

// Data returns the file's content.
func (f File) Data() []byte { return f.data }

// Incomplete reports whether some answers are not reflected in the plan.
func (p Plan) Incomplete() bool { return len(p.NotYetApplied) > 0 }

// Build assembles the plan for a set of validated answers.
func Build(answers platform.Result, lock components.Lock) (Plan, error) {
	if !answers.Valid {
		return Plan{}, errors.New("the answers are not valid")
	}

	p := Plan{SchemaVersion: SchemaVersion, Deviations: answers.Deviations}
	namespaces := map[string][]string{}

	for _, c := range lock.Components {
		if c.Optional {
			on, known := optionalComponents[c.Name]
			if !known {
				return Plan{}, fmt.Errorf("optional component %q has no answer that turns it on", c.Name)
			}
			if answers.Answers[on.field] != on.value {
				continue
			}
		}
		stage := StagePlatform
		if c.Name == bootstrapComponent {
			stage = StageBootstrap
		}

		data, err := components.Rendered(c)
		if err != nil {
			return Plan{}, err
		}
		if c.Name == bootstrapComponent {
			if data, err = configureArgoCD(data, answers.Answers); err != nil {
				return Plan{}, err
			}
			policies, err := argoCDPolicies(c)
			if err != nil {
				return Plan{}, err
			}
			p.Files = append(p.Files, newFile(stage+"/10-argo-cd-policies.yaml", policies))
		}
		if err := checkImages(c, data); err != nil {
			return Plan{}, err
		}

		p.Components = append(p.Components, Component{
			Name:         c.Name,
			Stage:        stage,
			Namespace:    c.Namespace,
			ChartVersion: c.Chart.Version,
			Images:       len(c.Images),
		})
		p.Files = append(p.Files, newFile(stage+"/"+c.Name+".yaml", data))
		namespaces[stage] = append(namespaces[stage], c.Namespace)
	}

	for stage, names := range namespaces {
		p.Files = append(p.Files, newFile(stage+"/00-namespaces.yaml", namespaceManifests(names)))
	}
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	p.Hash = Hash(p.Files)

	for _, q := range questions.Default().Questions {
		if !appliedFields[q.Field] {
			p.NotYetApplied = append(p.NotYetApplied, q.Field)
		}
	}
	return p, nil
}

func newFile(path string, data []byte) File {
	sum := sha256.Sum256(data)
	return File{Path: path, SHA256: hex.EncodeToString(sum[:]), data: data}
}

// checkImages fails if the manifests use any image that is not in the lock,
// pinned by the digest the lock records.
func checkImages(c components.Component, manifests []byte) error {
	allowed := map[string]bool{}
	for _, img := range c.Images {
		allowed[img.PinnedRef()] = true
	}
	refs, err := components.ImageRefs(manifests)
	if err != nil {
		return fmt.Errorf("%s: %w", c.Name, err)
	}
	if len(refs) == 0 {
		return fmt.Errorf("%s: the manifests contain no images", c.Name)
	}
	for _, ref := range refs {
		if !allowed[ref] {
			return fmt.Errorf("%s: image %s is not pinned in the component lock", c.Name, ref)
		}
	}
	return nil
}

// namespaceManifests returns Namespace objects that enforce the restricted
// Pod Security Standard.
func namespaceManifests(names []string) []byte {
	sort.Strings(names)
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteString("---\n")
		}
		fmt.Fprintf(&b, `apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    app.kubernetes.io/part-of: gitops-platform
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
`, name)
	}
	return []byte(b.String())
}

// Hash returns the content hash of a set of files. It covers every path and
// every byte, so any added, removed, renamed or changed file gives a
// different hash.
func Hash(files []File) string {
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s  %s\n", f.SHA256, f.Path)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Write stores the plan in dir. It refuses to write into a directory that
// holds anything other than a previous plan.
func (p Plan) Write(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		switch e.Name() {
		case IndexFile, StageBootstrap, StagePlatform:
		default:
			return fmt.Errorf("%s contains %q, which is not part of a plan; choose an empty directory", dir, e.Name())
		}
	}
	for _, name := range []string{IndexFile, StageBootstrap, StagePlatform} {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}

	for _, f := range p.Files {
		path := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, f.data, 0o644); err != nil {
			return err
		}
	}
	index, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, IndexFile), append(index, '\n'), 0o644)
}

// Read loads a plan from dir and checks it: every file listed in plan.json
// must be present with the recorded hash, no other file may be present, and
// the files must add up to the plan's hash. It is how a later step confirms
// that the plan on disk is the one that was approved.
func Read(dir string) (Plan, error) {
	raw, err := os.ReadFile(filepath.Join(dir, IndexFile))
	if err != nil {
		return Plan{}, err
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return Plan{}, fmt.Errorf("%s: %w", IndexFile, err)
	}
	if p.SchemaVersion != SchemaVersion {
		return Plan{}, fmt.Errorf("%s: schemaVersion is %q, want %q", IndexFile, p.SchemaVersion, SchemaVersion)
	}

	listed := map[string]bool{}
	for i, f := range p.Files {
		if f.Path != filepath.ToSlash(filepath.Clean(f.Path)) || strings.HasPrefix(f.Path, "../") || filepath.IsAbs(f.Path) {
			return Plan{}, fmt.Errorf("%s lists the unsafe path %q", IndexFile, f.Path)
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return Plan{}, err
		}
		if got := newFile(f.Path, data); got.SHA256 != f.SHA256 {
			return Plan{}, fmt.Errorf("%s has changed since the plan was written", f.Path)
		}
		p.Files[i].data = data
		listed[f.Path] = true
	}

	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); rel != IndexFile && !listed[rel] {
			return fmt.Errorf("%s is in the plan directory but not in the plan", rel)
		}
		return nil
	})
	if err != nil {
		return Plan{}, err
	}

	if got := Hash(p.Files); got != p.Hash {
		return Plan{}, fmt.Errorf("the files add up to %s, but %s records %s", got, IndexFile, p.Hash)
	}
	return p, nil
}
