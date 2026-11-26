package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/check/checktest"
	"github.com/majorcontext/ward/internal/identity/usertoken"
	"github.com/majorcontext/ward/internal/land"
	"github.com/majorcontext/ward/internal/land/landtest"
	"github.com/majorcontext/ward/internal/mirror"
	"github.com/majorcontext/ward/internal/mirror/mirrortest"
	"github.com/majorcontext/ward/internal/state/sqlite"
	"github.com/majorcontext/ward/internal/usertokentest"
)

func open(t *testing.T) *sqlite.Store {
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCheckStoreConformance(t *testing.T) {
	t.Parallel()
	checktest.Run(t, func(t *testing.T) check.Store { return open(t) })
}

func TestLandStoreConformance(t *testing.T) {
	t.Parallel()
	landtest.Run(t, func(t *testing.T) land.Store { return open(t) })
}

func TestMirrorStoreConformance(t *testing.T) {
	t.Parallel()
	mirrortest.Run(t, func(t *testing.T) mirror.Store { return open(t) })
}

func TestUserTokenStoreConformance(t *testing.T) {
	t.Parallel()
	usertokentest.Run(t, func(t *testing.T) usertoken.Store { return open(t) })
}
