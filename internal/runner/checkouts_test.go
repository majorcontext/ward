package runner_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/ward/internal/runner"
	"github.com/majorcontext/ward/internal/runner/warm"
	"github.com/majorcontext/ward/runnerproto"
)

func TestStartCheckoutsMakesWarmFetchTheTreeSource(t *testing.T) {
	root := t.TempDir()
	up := filepath.Join(root, "up")
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", up, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.Mkdir(up, 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(up, "a"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "c")
	commit, tree := run("rev-parse", "HEAD"), run("rev-parse", "HEAD^{tree}")
	fellBack := false
	cfg := runner.Config{Checkouts: &runner.CheckoutConfig{
		Warm:      warm.Config{Dir: filepath.Join(root, "clones"), PoolDir: filepath.Join(root, "ready"), Remote: func(string) (string, error) { return "file://" + up, nil }},
		SyncEvery: 1,
	}}
	cfg.Options.Fetch = func(context.Context, runnerproto.Request, string) error { fellBack = true; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	wait, err := cfg.StartCheckouts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	err = cfg.Options.Fetch(ctx, runnerproto.Request{Repo: "native:r", Commit: commit, Tree: tree}, dir)
	if got, _ := os.ReadFile(filepath.Join(dir, "a")); err != nil || string(got) != "1" || fellBack {
		t.Errorf("error %v, a = %q, fallback ran %v; want the file from the clone", err, got, fellBack)
	}
	cancel()
	wait()
}
