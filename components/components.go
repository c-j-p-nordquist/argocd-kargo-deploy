// Package components describes the third-party components the toolkit
// installs and the lock file that pins them.
//
// components.yaml is the hand-edited input. components.lock is generated from
// it by tools/lockgen and embedded in the binary, so a given build of the CLI
// can only ever install the exact artifacts it was released with.
package components

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SchemaVersion identifies the shape of the lock file.
const SchemaVersion = "v1alpha1"

// Verification methods.
const (
	// MethodKeyless is a cosign signature made with a short-lived
	// certificate. The certificate names the CI workflow that signed, and the
	// signature is recorded in a public transparency log.
	MethodKeyless = "keyless"
	// MethodKeylessProvenance is a signed SLSA provenance attestation instead
	// of an image signature. The signer is the SLSA generator workflow;
	// SourceRepository ties the build to the project's repository.
	MethodKeylessProvenance = "keyless-provenance"
	// MethodKey is a cosign signature made with the project's long-lived key.
	// It is not recorded in a transparency log.
	MethodKey = "key"
	// MethodNone means upstream publishes nothing verifiable. The image is
	// pinned by digest only, and Reason says why.
	MethodNone = "none"
)

//go:embed components.lock
var lockData []byte

//go:embed components.yaml
var configData []byte

//go:embed keys/*.pem
var keys embed.FS

//go:embed rendered/*.yaml
var rendered embed.FS

// KubeVersion is the Kubernetes version charts are rendered for. It is the
// oldest version the toolkit supports, so the manifests use only APIs that
// every supported cluster serves.
const KubeVersion = "1.30.0"

// Config is components.yaml.
type Config struct {
	CooldownDays int               `yaml:"cooldownDays"`
	Components   []ComponentConfig `yaml:"components"`
}

// ComponentConfig is one component in components.yaml.
type ComponentConfig struct {
	Name     string `yaml:"name"`
	Purpose  string `yaml:"purpose"`
	Optional bool   `yaml:"optional"`
	// Namespace and Release default to Name.
	Namespace string `yaml:"namespace"`
	Release   string `yaml:"release"`
	// Chart is the OCI repository of the Helm chart, without a scheme.
	Chart   string `yaml:"chart"`
	Version string `yaml:"version"`
	// Set holds the Helm --set values the chart is rendered with.
	Set     []string `yaml:"set"`
	Signers []Signer `yaml:"signers"`
}

// Signer says how images whose repository starts with Images are verified.
type Signer struct {
	Images       string `yaml:"images"`
	Verification `yaml:",inline"`
}

// Verification says how one image is verified. In components.yaml, Identity
// may contain {tag}, which stands for the image's tag.
type Verification struct {
	Method           string `yaml:"method" json:"method"`
	Identity         string `yaml:"identity,omitempty" json:"identity,omitempty"`
	Issuer           string `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	SourceRepository string `yaml:"sourceRepository,omitempty" json:"sourceRepository,omitempty"`
	// Key is the path of the public key, relative to the components directory.
	Key             string `yaml:"key,omitempty" json:"key,omitempty"`
	KeySHA256       string `yaml:"keySHA256,omitempty" json:"keySHA256,omitempty"`
	DigestAlgorithm string `yaml:"digestAlgorithm,omitempty" json:"digestAlgorithm,omitempty"`
	Reason          string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// Lock is components.lock.
type Lock struct {
	SchemaVersion string      `yaml:"schemaVersion" json:"schemaVersion"`
	Components    []Component `yaml:"components" json:"components"`
}

// Component is one pinned component.
type Component struct {
	Name     string `yaml:"name" json:"name"`
	Purpose  string `yaml:"purpose" json:"purpose"`
	Optional bool   `yaml:"optional" json:"optional"`
	// Namespace is where the component is installed.
	Namespace string   `yaml:"namespace" json:"namespace"`
	Chart     Chart    `yaml:"chart" json:"chart"`
	Manifest  Manifest `yaml:"manifest" json:"manifest"`
	Images    []Image  `yaml:"images" json:"images"`
}

// Manifest is the chart rendered to plain Kubernetes manifests, with every
// image pinned by digest. It is what gets installed.
type Manifest struct {
	// Path is relative to the components directory.
	Path   string `yaml:"path" json:"path"`
	SHA256 string `yaml:"sha256" json:"sha256"`
}

// Chart is a Helm chart pinned by digest. Charts are not signature-checked.
type Chart struct {
	Repository string `yaml:"repository" json:"repository"`
	Version    string `yaml:"version" json:"version"`
	Digest     string `yaml:"digest" json:"digest"`
	// Created is when the chart was published, as recorded by its registry.
	Created string `yaml:"created" json:"created"`
}

// Image is a container image pinned by digest.
type Image struct {
	Repository   string       `yaml:"repository" json:"repository"`
	Tag          string       `yaml:"tag" json:"tag"`
	Digest       string       `yaml:"digest" json:"digest"`
	Verification Verification `yaml:"verification" json:"verification"`
}

// Ref returns the image reference pinned by digest.
func (i Image) Ref() string { return i.Repository + "@" + i.Digest }

// PinnedRef returns the reference as it appears in rendered manifests: the
// tag for readability and the digest for integrity.
func (i Image) PinnedRef() string { return i.Repository + ":" + i.Tag + "@" + i.Digest }

// LoadConfig returns the embedded components.yaml.
func LoadConfig() (Config, error) {
	return ParseConfig(configData)
}

// ParseConfig reads and checks a components.yaml.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	if err := decodeStrict(data, &c); err != nil {
		return Config{}, fmt.Errorf("components.yaml: %w", err)
	}
	if c.CooldownDays < 0 {
		return Config{}, fmt.Errorf("components.yaml: cooldownDays must not be negative")
	}
	seen := map[string]bool{}
	for i, comp := range c.Components {
		if comp.Name == "" || comp.Purpose == "" || comp.Chart == "" || comp.Version == "" {
			return Config{}, fmt.Errorf("components.yaml: component %q needs a name, purpose, chart and version", comp.Name)
		}
		if seen[comp.Name] {
			return Config{}, fmt.Errorf("components.yaml: duplicate component %q", comp.Name)
		}
		seen[comp.Name] = true
		if comp.Namespace == "" {
			c.Components[i].Namespace = comp.Name
		}
		if comp.Release == "" {
			c.Components[i].Release = comp.Name
		}
		if len(comp.Signers) == 0 {
			return Config{}, fmt.Errorf("components.yaml: component %q has no signers", comp.Name)
		}
		for _, s := range comp.Signers {
			if s.Images == "" {
				return Config{}, fmt.Errorf("components.yaml: component %q has a signer without images", comp.Name)
			}
			if err := s.Verification.check(); err != nil {
				return Config{}, fmt.Errorf("components.yaml: component %q, images %q: %w", comp.Name, s.Images, err)
			}
		}
	}
	return c, nil
}

// LoadLock returns the embedded components.lock.
func LoadLock() (Lock, error) {
	return ParseLock(lockData)
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ParseLock reads and checks a components.lock. Nothing in a valid lock
// floats: every chart and image has a digest, and every image says how it is
// verified.
func ParseLock(data []byte) (Lock, error) {
	var l Lock
	if err := decodeStrict(data, &l); err != nil {
		return Lock{}, fmt.Errorf("components.lock: %w", err)
	}
	if l.SchemaVersion != SchemaVersion {
		return Lock{}, fmt.Errorf("components.lock: schemaVersion is %q, want %q", l.SchemaVersion, SchemaVersion)
	}
	for _, comp := range l.Components {
		if comp.Name == "" || comp.Chart.Repository == "" || comp.Chart.Version == "" {
			return Lock{}, fmt.Errorf("components.lock: component %q is missing its name or chart", comp.Name)
		}
		if !digestPattern.MatchString(comp.Chart.Digest) {
			return Lock{}, fmt.Errorf("components.lock: component %q: chart digest %q is not a sha256 digest", comp.Name, comp.Chart.Digest)
		}
		if comp.Namespace == "" || comp.Manifest.Path == "" || len(comp.Manifest.SHA256) != 64 {
			return Lock{}, fmt.Errorf("components.lock: component %q is missing its namespace or manifest", comp.Name)
		}
		if len(comp.Images) == 0 {
			return Lock{}, fmt.Errorf("components.lock: component %q has no images", comp.Name)
		}
		for _, img := range comp.Images {
			if img.Repository == "" || img.Tag == "" {
				return Lock{}, fmt.Errorf("components.lock: component %q has an image without a repository or tag", comp.Name)
			}
			if !digestPattern.MatchString(img.Digest) {
				return Lock{}, fmt.Errorf("components.lock: image %s: digest %q is not a sha256 digest", img.Repository, img.Digest)
			}
			if err := img.Verification.check(); err != nil {
				return Lock{}, fmt.Errorf("components.lock: image %s: %w", img.Repository, err)
			}
			if strings.Contains(img.Verification.Identity, "{") {
				return Lock{}, fmt.Errorf("components.lock: image %s: identity still contains a placeholder", img.Repository)
			}
		}
	}
	return l, nil
}

// check reports whether the verification has the fields its method needs.
func (v Verification) check() error {
	switch v.Method {
	case MethodKeyless:
		if v.Identity == "" || v.Issuer == "" {
			return fmt.Errorf("method %s needs an identity and issuer", v.Method)
		}
	case MethodKeylessProvenance:
		if v.Identity == "" || v.Issuer == "" || v.SourceRepository == "" {
			return fmt.Errorf("method %s needs an identity, issuer and sourceRepository", v.Method)
		}
	case MethodKey:
		if v.Key == "" || v.KeySHA256 == "" || v.DigestAlgorithm == "" {
			return fmt.Errorf("method %s needs a key, keySHA256 and digestAlgorithm", v.Method)
		}
		if _, err := Key(v); err != nil {
			return err
		}
	case MethodNone:
		if v.Reason == "" {
			return fmt.Errorf("method %s needs a reason", v.Method)
		}
	default:
		return fmt.Errorf("unknown verification method %q", v.Method)
	}
	return nil
}

// Key returns the embedded public key for a key verification, after checking
// it against the pinned hash.
func Key(v Verification) ([]byte, error) {
	data, err := keys.ReadFile(v.Key)
	if err != nil {
		return nil, fmt.Errorf("public key %q is not embedded: %w", v.Key, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != v.KeySHA256 {
		return nil, fmt.Errorf("public key %q has sha256 %s, but %s is pinned", v.Key, got, v.KeySHA256)
	}
	return data, nil
}

func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(out)
}

// Rendered returns the component's embedded manifests, after checking them
// against the hash in the lock.
func Rendered(c Component) ([]byte, error) {
	data, err := rendered.ReadFile(c.Manifest.Path)
	if err != nil {
		return nil, fmt.Errorf("manifests for %q are not embedded: %w", c.Name, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != c.Manifest.SHA256 {
		return nil, fmt.Errorf("manifests for %q have sha256 %s, but the lock records %s", c.Name, got, c.Manifest.SHA256)
	}
	return data, nil
}

// imageArg matches a command-line argument that passes an image to a
// controller, such as --acme-http01-solver-image=quay.io/....
var imageArg = regexp.MustCompile(`^--[A-Za-z0-9-]*image[A-Za-z0-9-]*=(\S+)$`)

// ImageRefs returns every image reference in a stream of YAML manifests,
// sorted: the value of every "image" key, and every image passed as a
// command-line argument.
func ImageRefs(manifests []byte) ([]string, error) {
	found := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, value := range v {
				if s, ok := value.(string); ok && key == "image" && s != "" {
					found[s] = true
				}
				walk(value)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		case string:
			if m := imageArg.FindStringSubmatch(v); m != nil {
				found[m[1]] = true
			}
		}
	}

	dec := yaml.NewDecoder(bytes.NewReader(manifests))
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("manifests are not valid YAML: %w", err)
		}
		walk(doc)
	}

	refs := make([]string, 0, len(found))
	for ref := range found {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, nil
}
