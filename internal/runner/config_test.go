package runner

import (
	"errors"
	"testing"

	"github.com/majorcontext/ward/runnerproto"
)

func TestResolveSandboxNeverDropsToAWeakerSandbox(t *testing.T) {
	t.Parallel()
	missing := errors.New("runsc: not found")
	for _, c := range []struct {
		setting string
		runsc   error
		want    runnerproto.Isolation
		fails   bool
	}{
		{"", nil, runnerproto.IsolationGVisor, false},
		{"auto", nil, runnerproto.IsolationGVisor, false},
		{"auto", missing, "", true},
		{"", missing, "", true},
		{"gvisor", missing, "", true},
		{"container", missing, runnerproto.IsolationContainer, false},
		{"fake", nil, "", true},
	} {
		got, err := resolveSandbox(c.setting, c.runsc)
		if (err != nil) != c.fails || got != c.want {
			t.Errorf("%q with runsc %v: %q, %v; want %q, fails %v", c.setting, c.runsc, got, err, c.want, c.fails)
		}
	}
}
