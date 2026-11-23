package periodic_test

import (
	"testing"

	"github.com/majorcontext/ward/internal/runner/warm/periodic"
	"github.com/majorcontext/ward/internal/runner/warm/warmtest"
)

func TestConformance(t *testing.T) {
	warmtest.Run(t, warmtest.Impl{New: func(_ *testing.T, c *warmtest.Clock) warmtest.Harness {
		return warmtest.Harness{
			Watcher: periodic.Watcher{Repos: func() []string { return []string{"native:r"} }, Every: 1, Clock: c},
			Repo:    "native:r", Change: func() {},
		}
	}})
}
