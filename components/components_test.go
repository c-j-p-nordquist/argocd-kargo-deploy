package components

import (
	"strings"
	"testing"
)

func TestEmbeddedLockIsValid(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Components) == 0 {
		t.Fatal("the lock has no components")
	}
}

// The lock must describe exactly the components and versions in
// components.yaml, so the two cannot drift apart unnoticed.
func TestLockMatchesConfig(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Components) != len(cfg.Components) {
		t.Fatalf("lock has %d components, components.yaml has %d", len(lock.Components), len(cfg.Components))
	}
	for i, want := range cfg.Components {
		got := lock.Components[i]
		if got.Name != want.Name || got.Chart.Repository != want.Chart || got.Chart.Version != want.Version || got.Optional != want.Optional {
			t.Errorf("component %d: lock has %s %s@%s (optional=%v), components.yaml has %s %s@%s (optional=%v); regenerate the lock",
				i, got.Name, got.Chart.Repository, got.Chart.Version, got.Optional,
				want.Name, want.Chart, want.Version, want.Optional)
		}
	}
}

// An image may only go unverified when components.yaml says so explicitly.
func TestUnverifiedImagesAreDeclared(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, c := range cfg.Components {
		for _, s := range c.Signers {
			if s.Method == MethodNone {
				declared[s.Images] = true
			}
		}
	}
	for _, c := range lock.Components {
		for _, img := range c.Images {
			if img.Verification.Method == MethodNone && !declared[img.Repository] {
				t.Errorf("%s is unverified in the lock but not declared as such in components.yaml", img.Repository)
			}
		}
	}
}

func TestEmbeddedKeysMatchTheirPinnedHashes(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	keys := 0
	for _, c := range lock.Components {
		for _, img := range c.Images {
			if img.Verification.Method != MethodKey {
				continue
			}
			keys++
			if _, err := Key(img.Verification); err != nil {
				t.Errorf("%s: %v", img.Repository, err)
			}
		}
	}
	if keys == 0 {
		t.Skip("no component uses a key")
	}
}

func TestKeyWithWrongHashIsRejected(t *testing.T) {
	v := Verification{Method: MethodKey, Key: "keys/cert-manager-pubkey-2021-09-20.pem", KeySHA256: strings.Repeat("0", 64), DigestAlgorithm: "sha512"}
	if _, err := Key(v); err == nil {
		t.Error("Key accepted a key that does not match its pinned hash")
	}
}

const validLock = `
schemaVersion: v1alpha1
components:
  - name: example
    purpose: An example.
    optional: false
    chart:
      repository: registry.example/charts/example
      version: 1.0.0
      digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      created: "2026-01-01T00:00:00Z"
    images:
      - repository: registry.example/example
        tag: v1.0.0
        digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
        verification:
          method: keyless
          identity: https://github.com/example/example/.github/workflows/release.yaml@refs/tags/v1.0.0
          issuer: https://token.actions.githubusercontent.com
`

func TestParseLockAcceptsAValidLock(t *testing.T) {
	if _, err := ParseLock([]byte(validLock)); err != nil {
		t.Fatal(err)
	}
}

func TestParseLockRejectsAnythingThatFloats(t *testing.T) {
	tests := []struct {
		name, old, new string
	}{
		{"wrong schema version", "v1alpha1", "v9"},
		{"chart without digest", "      digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", ""},
		{"image without digest", "        digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", ""},
		{"truncated digest", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "sha256:bbbb"},
		{"image without tag", "        tag: v1.0.0\n", ""},
		{"no verification method", "          method: keyless\n", ""},
		{"keyless without identity", "          identity: https://github.com/example/example/.github/workflows/release.yaml@refs/tags/v1.0.0\n", ""},
		{"unverified without a reason", "          method: keyless\n", "          method: none\n"},
		{"placeholder left in identity", "refs/tags/v1.0.0", "refs/tags/{tag}"},
		{"unknown field", "    optional: false\n", "    optional: false\n    surprise: true\n"},
		{"component without images", "    images:\n", "    pictures:\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(validLock, tt.old) {
				t.Fatalf("test setup: %q not found in the valid lock", tt.old)
			}
			if _, err := ParseLock([]byte(strings.Replace(validLock, tt.old, tt.new, 1))); err == nil {
				t.Error("ParseLock accepted the lock")
			}
		})
	}
}

func TestParseConfigRejectsIncompleteSigners(t *testing.T) {
	const cfg = `
cooldownDays: 14
components:
  - name: example
    purpose: An example.
    chart: registry.example/charts/example
    version: 1.0.0
    signers:
      - images: registry.example/example
        method: keyless-provenance
        identity: https://example.com/workflow
        issuer: https://token.actions.githubusercontent.com
`
	if _, err := ParseConfig([]byte(cfg)); err == nil {
		t.Error("ParseConfig accepted keyless-provenance without a sourceRepository")
	}
	if _, err := ParseConfig([]byte(cfg + "        sourceRepository: example/example\n")); err != nil {
		t.Errorf("ParseConfig rejected a complete signer: %v", err)
	}
}
