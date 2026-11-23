package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/runner/warm"
	"github.com/majorcontext/ward/internal/runner/warm/motteevents"
	"github.com/majorcontext/ward/internal/runner/warm/periodic"
)

const (
	defaultSyncEvery = time.Minute
	eventsPoll       = 2 * time.Second
)

// CheckoutConfig is the setting of the warm checkouts (see package warm).
type CheckoutConfig struct {
	Warm       warm.Config
	MotteURL   string
	EventsFile string
	SyncEvery  time.Duration
}

func loadCheckouts(getenv func(string) string, work, grantFile string) (*CheckoutConfig, error) {
	dir := getenv("WARD_RUNNER_CHECKOUT_DIR")
	if dir == "" {
		return nil, nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("WARD_RUNNER_CHECKOUT_DIR needs git on the path")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("WARD_RUNNER_CHECKOUT_DIR: %w", err)
	}
	base := getenv("WARD_RUNNER_MOTTE_URL")
	c := &CheckoutConfig{MotteURL: base, EventsFile: getenv("WARD_RUNNER_MOTTE_EVENTS_GRANT_FILE"), SyncEvery: defaultSyncEvery}
	c.Warm = warm.Config{
		Dir:     abs,
		PoolDir: filepath.Join(work, ".ready"),
		Remote:  func(repo string) (string, error) { return motteGitURL(base, repo) },
		Header: func(context.Context) (string, error) {
			g, err := readText(grantFile)
			return "Authorization: Bearer " + g, err
		},
	}
	for name, p := range map[string]*int{"WARD_RUNNER_CHECKOUT_POOL": &c.Warm.PoolSize, "WARD_RUNNER_CHECKOUT_MAX_REPOS": &c.Warm.MaxRepos} {
		if v := getenv(name); v != "" {
			if *p, err = strconv.Atoi(v); err != nil || *p < 1 {
				return nil, fmt.Errorf("%s must be a positive whole number", name)
			}
		}
	}
	if v := getenv("WARD_RUNNER_CHECKOUT_MAX_BYTES"); v != "" {
		if c.Warm.MaxBytes, err = strconv.ParseInt(v, 10, 64); err != nil || c.Warm.MaxBytes < 1 {
			return nil, errors.New("WARD_RUNNER_CHECKOUT_MAX_BYTES must be a positive whole number")
		}
	}
	if v := getenv("WARD_RUNNER_CHECKOUT_SYNC_EVERY"); v != "" {
		if c.SyncEvery, err = time.ParseDuration(v); err != nil || c.SyncEvery < time.Second {
			return nil, errors.New("WARD_RUNNER_CHECKOUT_SYNC_EVERY must be a duration of at least 1s")
		}
	}
	if c.EventsFile != "" {
		if _, err := readText(c.EventsFile); err != nil {
			return nil, fmt.Errorf("WARD_RUNNER_MOTTE_EVENTS_GRANT_FILE: %w", err)
		}
	}
	return c, nil
}

// motteGitURL returns the git URL of a motte repository: native:repo_X is BASE/git/native/repo_X.git.
func motteGitURL(base, repo string) (string, error) {
	prefix, path, ok := strings.Cut(repo, ":")
	if !ok || prefix == "" || path == "" {
		return "", fmt.Errorf("repository %q is not PREFIX:PATH", repo)
	}
	return strings.TrimRight(base, "/") + "/git/" + prefix + "/" + path + ".git", nil
}

// StartCheckouts makes the warm fetcher the tree source of c.Options, with the old Fetch as its fallback, and runs its
// watchers until ctx ends. The returned function waits for them. It does nothing when the checkouts are off.
func (c *Config) StartCheckouts(ctx context.Context, log *slog.Logger) (wait func(), err error) {
	if c.Checkouts == nil {
		return func() {}, nil
	}
	k := c.Checkouts
	k.Warm.Fallback, k.Warm.Log = c.Options.Fetch, log
	f, err := warm.New(k.Warm)
	if err != nil {
		return nil, err
	}
	c.Options.Fetch = f.Fetch
	ws := []warm.Watcher{periodic.Watcher{Repos: f.Repos, Every: k.SyncEvery, Clock: check.RealClock()}}
	if k.EventsFile != "" {
		grant := func(context.Context) (string, error) { return readText(k.EventsFile) }
		ws = append(ws, &motteevents.Watcher{Client: client.New(k.MotteURL, grant), Poll: eventsPoll, Clock: check.RealClock()})
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.Run(ctx, ws...)
		f.Close()
	}()
	return wg.Wait, nil
}
