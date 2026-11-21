package check_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/ward/internal/check"
)

func TestParseRunnersPullEntry(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(secret, []byte("a-secret-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, json, err string }{
		{"default minimum is gvisor", `[{"repo":"r","mode":"pull","kids":["a"],"env":"e"}]`, ""},
		{"container allowed by name", `[{"repo":"r","mode":"pull","kids":["a","b"],"min_isolation":"container"}]`, ""},
		{"pull needs kids", `[{"repo":"r","mode":"pull"}]`, "kids is required"},
		{"pull takes no url", `[{"repo":"r","mode":"pull","kids":["a"],"url":"http://x"}]`, "belong to mode push"},
		{"kid repeats", `[{"repo":"r","mode":"pull","kids":["a","a"]}]`, "unique"},
		{"unknown level", `[{"repo":"r","mode":"pull","kids":["a"],"min_isolation":"none"}]`, "min_isolation"},
		{"push takes no kids", `[{"repo":"r","url":"http://x","secret_file":"` + secret + `","kids":["a"]}]`, "belong to mode pull"},
		{"unknown mode", `[{"repo":"r","mode":"fax"}]`, "mode must be"},
	} {
		got, err := check.ParseRunners([]byte(c.json))
		switch {
		case c.err == "" && err != nil, c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: %v, want %q", c.name, err, c.err)
		case c.err == "" && c.name == "default minimum is gvisor" && got[0].MinIsolation != "gvisor":
			t.Errorf("%s: minimum %q", c.name, got[0].MinIsolation)
		}
	}
}
