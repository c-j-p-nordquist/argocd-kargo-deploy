package main

import (
	"strings"
	"testing"
)

func TestParseVersionAcceptsOnlyReleases(t *testing.T) {
	for _, s := range []string{"1.2.3", "v1.21.3", "10.11.6"} {
		if _, _, ok := parseVersion(s); !ok {
			t.Errorf("%q was not accepted", s)
		}
	}
	for _, s := range []string{"v3.0.0-alpha.1", "1.2", "latest", "sha256-abc.sig", "1.2.3.4", "v1.2.3-rc.1"} {
		if _, _, ok := parseVersion(s); ok {
			t.Errorf("%q was accepted as a release", s)
		}
	}
}

func TestLessComparesNumerically(t *testing.T) {
	if !less([3]int{2, 9, 0}, [3]int{2, 10, 0}) {
		t.Error("2.9.0 should be older than 2.10.0")
	}
	if less([3]int{1, 0, 0}, [3]int{1, 0, 0}) || less([3]int{2, 0, 0}, [3]int{1, 9, 9}) {
		t.Error("less is wrong for equal or newer versions")
	}
}

const config = `# A comment that must survive.
components:
  - name: first
    chart: registry.example/first
    version: 1.0.0
    set:
      - version=1.0.0

  - name: second
    # Another comment.
    chart: registry.example/second
    version: 1.0.0
`

func TestSetVersionChangesOnlyTheNamedComponent(t *testing.T) {
	got, err := setVersion(config, "second", "1.0.0", "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(config, "second\n    version: 1.0.0", "second\n    version: 1.1.0", 1)
	if got != want {
		t.Errorf("got:\n%s", got)
	}

	got, err = setVersion(config, "first", "1.0.0", "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "2.0.0") != 1 || !strings.Contains(got, "- version=1.0.0") || !strings.Contains(got, "# A comment that must survive.") {
		t.Errorf("got:\n%s", got)
	}
}

func TestSetVersionFailsWhenItCannotFindTheLine(t *testing.T) {
	if _, err := setVersion(config, "missing", "1.0.0", "1.1.0"); err == nil {
		t.Error("no error for an unknown component")
	}
	if _, err := setVersion(config, "first", "9.9.9", "1.1.0"); err == nil {
		t.Error("no error when the current version does not match")
	}
}

func TestPinDoesNotTouchLongerReferences(t *testing.T) {
	rendered := "image: registry.example/redis:8.6.4\nimage: registry.example/redis:8.6.4-alpine\nargs: [--image=registry.example/redis:8.6.4]\n"
	got := pin(rendered, "registry.example/redis:8.6.4", "registry.example/redis:8.6.4@sha256:abc")
	want := "image: registry.example/redis:8.6.4@sha256:abc\nimage: registry.example/redis:8.6.4-alpine\nargs: [--image=registry.example/redis:8.6.4@sha256:abc]\n"
	if got != want {
		t.Errorf("got:\n%s", got)
	}
}
