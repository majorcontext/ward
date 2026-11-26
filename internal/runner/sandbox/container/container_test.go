package container_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/majorcontext/ward/internal/runner/sandbox"
	"github.com/majorcontext/ward/internal/runner/sandbox/container"
	"github.com/majorcontext/ward/internal/runner/sandbox/sandboxtest"
	"github.com/majorcontext/ward/runnerproto"
)

func engine(t *testing.T) string {
	t.Helper()
	for _, e := range []string{os.Getenv("WARD_TEST_ENGINE"), "docker", "podman"} {
		if e == "" {
			continue
		}
		if _, err := exec.LookPath(e); err == nil && exec.Command(e, "info").Run() == nil {
			return e
		}
	}
	if os.Getenv("WARD_REQUIRE_ENGINE") != "" {
		t.Fatal("WARD_REQUIRE_ENGINE is set and no container engine answers")
	}
	t.Skip("no container engine")
	return ""
}

func TestConformance(t *testing.T) {
	t.Parallel()
	e := engine(t)
	image := os.Getenv("WARD_TEST_IMAGE")
	if image == "" {
		image = "busybox:1.37"
	}
	sandboxtest.Run(t, sandboxtest.Impl{Isolated: true, Isolation: runnerproto.IsolationContainer, New: func(t *testing.T) sandbox.Sandbox {
		s, err := container.New(container.Config{Engine: e, Image: image})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}})
}
