package seamcheck

import (
	"os"
	"testing"

	"github.com/majorcontext/tools/seams"
	"github.com/majorcontext/tools/testcount"
)

func TestMain(m *testing.M) { os.Exit(testcount.Run(m)) }

func TestRepositorySeams(t *testing.T) {
	t.Parallel()
	seams.CheckRepository(t, "../..")
}
