package memory_test

import (
	"testing"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/check/checktest"
	"github.com/majorcontext/ward/internal/land"
	"github.com/majorcontext/ward/internal/land/landtest"
	"github.com/majorcontext/ward/internal/mirror"
	"github.com/majorcontext/ward/internal/mirror/mirrortest"
	"github.com/majorcontext/ward/internal/state/memory"
)

func TestCheckStoreConformance(t *testing.T) {
	t.Parallel()
	checktest.Run(t, func(*testing.T) check.Store { return memory.New() })
}

func TestLandStoreConformance(t *testing.T) {
	t.Parallel()
	landtest.Run(t, func(*testing.T) land.Store { return memory.New() })
}

func TestMirrorStoreConformance(t *testing.T) {
	t.Parallel()
	mirrortest.Run(t, func(*testing.T) mirror.Store { return memory.New() })
}
