package runner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/ward/internal/runner"
)

func TestCheckoutsSettingsReachTheFetcher(t *testing.T) {
	grant := filepath.Join(t.TempDir(), "grant")
	if err := os.WriteFile(grant, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"WARD_RUNNER_CHECKOUT_DIR": t.TempDir(), "WARD_RUNNER_MOTTE_URL": "https://m.example.test/", "WARD_RUNNER_CHECKOUT_POOL": "3"}
	c, err := runner.LoadCheckouts(func(k string) string { return env[k] }, t.TempDir(), grant)
	if err != nil {
		t.Fatal(err)
	}
	url, _ := c.Warm.Remote("native:repo_X")
	header, _ := c.Warm.Header(t.Context())
	if url != "https://m.example.test/git/native/repo_X.git" || header != "Authorization: Bearer tok" || c.Warm.PoolSize != 3 {
		t.Errorf("url %q, header set %v, pool %d", url, header != "", c.Warm.PoolSize)
	}
	if _, err := c.Warm.Remote("no-colon"); err == nil {
		t.Error("a repository without a prefix got a URL")
	}
	env["WARD_RUNNER_CHECKOUT_POOL"] = "0"
	if _, err := runner.LoadCheckouts(func(k string) string { return env[k] }, t.TempDir(), grant); err == nil || !strings.Contains(err.Error(), "WARD_RUNNER_CHECKOUT_POOL") {
		t.Errorf("a pool of 0 gave %v", err)
	}
}
