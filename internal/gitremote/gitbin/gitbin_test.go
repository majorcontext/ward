package gitbin_test

import (
	"os/exec"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/gitremote"
	"github.com/majorcontext/ward/internal/gitremote/gitbin"
	"github.com/majorcontext/ward/internal/gitremote/remotetest"
)

func TestConformance(t *testing.T) {
	t.Parallel()
	remotetest.Run(t, remotetest.Impl{New: func(t *testing.T) (gitremote.Remote, string) {
		dir := t.TempDir()
		if out, err := exec.Command("git", "init", "-q", "--bare", dir).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		return gitbin.Runner{Home: t.TempDir(), Timeout: time.Minute}, "file://" + dir
	}})
}
