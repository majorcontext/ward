package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A binary that carries no tag in `ward version` cannot be traced to its release.
func TestBuildGivesEachArchTheTagAsItsVersion(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	if err := dispatch([]string{"build", "-out", "x", "-version", "v1.2.3", "-dry-run"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, arch := range arches {
		if !strings.Contains(out.String(), "GOARCH="+arch+" go build") || !strings.Contains(out.String(), "-X main.version=v1.2.3") {
			t.Errorf("no build of %s with the version:\n%s", arch, out.String())
		}
	}
	if dispatch([]string{"build", "-out", "x", "-version", "main", "-dry-run"}, &out) == nil {
		t.Error("a version that is no tag was accepted")
	}
}

// The sums file must verify with sha256sum -c.
func TestSumsListEachBinaryWithItsDigest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSums(dir, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if want := "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881  a\n"; string(got) != want {
		t.Errorf("sums = %q, want %q", got, want)
	}
}
