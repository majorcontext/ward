package check_test

import (
	"strings"
	"testing"

	"github.com/majorcontext/ward/internal/check"
)

func TestParseRunnersPullEntry(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, json, err string }{
		{"default minimum is gvisor", `[{"repo":"r","mode":"pull","kids":["a"],"env":"e"}]`, ""},
		{"container allowed by name", `[{"repo":"r","mode":"pull","kids":["a","b"],"min_isolation":"container"}]`, ""},
		{"pull needs kids", `[{"repo":"r","mode":"pull"}]`, "kids is required"},
		{"no mode is pull", `[{"repo":"r","kids":["a"]}]`, ""},
		{"push is gone", `[{"repo":"r","mode":"push","url":"http://x","secret_file":"s"}]`, "unknown field"},
		{"push mode is refused", `[{"repo":"r","mode":"push","kids":["a"]}]`, "mode must be"},
		{"kid repeats", `[{"repo":"r","mode":"pull","kids":["a","a"]}]`, "unique"},
		{"unknown level", `[{"repo":"r","mode":"pull","kids":["a"],"min_isolation":"none"}]`, "min_isolation"},
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
