package usertokentest_test

import (
	"testing"

	"github.com/majorcontext/ward/internal/identity/usertoken"
	"github.com/majorcontext/ward/internal/usertokentest"
)

func TestMem(t *testing.T) {
	t.Parallel()
	usertokentest.Run(t, func(*testing.T) usertoken.Store { return usertokentest.NewMem() })
}
