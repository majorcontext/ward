package fake_test

import (
	"testing"

	"github.com/majorcontext/ward/internal/runner/sandbox"
	"github.com/majorcontext/ward/internal/runner/sandbox/fake"
	"github.com/majorcontext/ward/internal/runner/sandbox/sandboxtest"
)

func TestConformance(t *testing.T) {
	sandboxtest.Run(t, sandboxtest.Impl{Isolation: "", New: func(*testing.T) sandbox.Sandbox { return fake.Sandbox{} }})
}
