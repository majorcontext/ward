package dir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/ward/internal/release/publish"
	"github.com/majorcontext/ward/internal/release/publish/publishtest"
)

func TestConformance(t *testing.T) {
	t.Parallel()
	publishtest.Run(t, func(t *testing.T) (publish.Publisher, func() map[string]string) {
		to := filepath.Join(t.TempDir(), "out")
		return Publisher{To: to}, func() map[string]string {
			got := map[string]string{}
			entries, _ := os.ReadDir(to)
			for _, e := range entries {
				data, _ := os.ReadFile(filepath.Join(to, e.Name()))
				got[e.Name()] = string(data)
			}
			return got
		}
	})
}
