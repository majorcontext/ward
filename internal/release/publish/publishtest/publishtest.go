// Package publishtest is the conformance suite of publish.Publisher.
package publishtest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/majorcontext/ward/internal/release/publish"
)

const tag = "v1.2.3"

// Run runs the suite. newPublisher returns a publisher with nothing published and a function that
// reads what it published for the tag, by base name.
func Run(t *testing.T, newPublisher func(t *testing.T) (publish.Publisher, func() map[string]string)) {
	t.Run("files are published under their base names and replaced", func(t *testing.T) {
		t.Parallel()
		p, seen := newPublisher(t)
		a := write(t, "ward-linux-amd64", "one")
		b := write(t, "ward-linux-amd64.sha256", "sum")
		if err := p.Publish(tag, []string{a, b}); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"ward-linux-amd64": "one", "ward-linux-amd64.sha256": "sum"}
		if got := seen(); !reflect.DeepEqual(got, want) {
			t.Fatalf("published %v, want %v", got, want)
		}
		a = write(t, "ward-linux-amd64", "two")
		if err := p.Publish(tag, []string{a}); err != nil {
			t.Fatal(err)
		}
		if got := seen()["ward-linux-amd64"]; got != "two" {
			t.Errorf("republish left %q, want two", got)
		}
	})
	t.Run("an unreadable file fails the publish", func(t *testing.T) {
		t.Parallel()
		p, _ := newPublisher(t)
		if err := p.Publish(tag, []string{filepath.Join(t.TempDir(), "missing")}); err == nil {
			t.Error("publish of a missing file succeeded")
		}
	})
}

func write(t *testing.T, name, data string) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
