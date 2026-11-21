package gvisor_test

import (
	"os"
	"testing"

	"github.com/majorcontext/ward/internal/runner/sandbox"
	"github.com/majorcontext/ward/internal/runner/sandbox/container"
	"github.com/majorcontext/ward/internal/runner/sandbox/gvisor"
	"github.com/majorcontext/ward/internal/runner/sandbox/sandboxtest"
)

func TestConformance(t *testing.T) {
	if err := gvisor.Available(); err != nil {
		if os.Getenv("WARD_REQUIRE_GVISOR") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	image := os.Getenv("WARD_TEST_IMAGE")
	if image == "" {
		image = "busybox:1.37"
	}
	engine := os.Getenv("WARD_TEST_ENGINE")
	if engine == "" {
		engine = "docker"
	}
	sandboxtest.Run(t, sandboxtest.Impl{Isolated: true, New: func(t *testing.T) sandbox.Sandbox {
		s, err := gvisor.New(container.Config{Engine: engine, Image: image})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}})
}
