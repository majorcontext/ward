// Package warm serves the tree of a check from a local bare clone of the repository. It keeps one bare clone per
// repository, updates it when a Watcher reports a ref change, and keeps a few ready checkouts that it moves to the
// requested commit. A checkout holds plain files and no .git, so a check cannot write into the shared clone.
package warm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/runnerproto"
)

const (
	repoMarker   = "ward-repo"
	cloneTimeout = 30 * time.Minute
	fetchTimeout = 10 * time.Minute
	gitTimeout   = 5 * time.Minute
	watchRetry   = 5 * time.Second
	sweepEvery   = 10 * time.Minute
	noAttributes = "* -text -eol -filter -ident -working-tree-encoding\n"
)

var objectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Watcher reports each change of a ref until ctx ends. It calls changed with the repository, or with "" when any
// repository may have changed. Watch returns an error only when it cannot go on, and the caller starts it again.
type Watcher interface {
	Watch(ctx context.Context, changed func(repo string)) error
}

// Config says where the caches live and how large they grow. Dir, PoolDir and Remote are required.
type Config struct {
	// Dir holds the bare clones. It lives across restarts.
	Dir string
	// PoolDir holds the ready checkouts. It must be on the file system of the check directories, so that a handout is a
	// rename. The fetcher empties it at start.
	PoolDir string
	// Remote returns the git URL of repo.
	Remote func(repo string) (string, error)
	// Header returns a complete HTTP header line that authenticates a git request. Empty or nil means none.
	Header func(ctx context.Context) (string, error)
	// Fallback gets the tree when the warm path fails for a reason other than a refusal. Nil means the failure stands.
	Fallback func(ctx context.Context, req runnerproto.Request, dir string) error
	// PoolSize is the ready checkouts for each repository. Default 2.
	PoolSize int
	// MaxRepos and MaxBytes bound the cache. The fetcher removes the clone that was used least recently and is idle.
	// Defaults 16 and 20 GiB.
	MaxRepos int
	MaxBytes int64
	Clock    check.Clock
	Log      *slog.Logger
}

// refusal is an answer of the tree check, not a fault of the cache. The fallback does not run for it.
type refusal struct{ msg string }

func (e *refusal) Error() string { return e.msg }

// netError marks a git failure that talks to the remote, so a corrupt cache is not blamed for it.
type netError struct{ error }

func (e netError) Unwrap() error { return e.error }

type slot struct{ root, work, index, base string }

type repo struct {
	name, path string
	mu         sync.RWMutex
	// busy and used are guarded by Fetcher.mu.
	busy int
	used time.Time

	pmu      sync.Mutex
	slots    []slot
	building int
	syncing  bool
	again    bool
}

// Fetcher serves trees from the warm cache. Fetch has the signature of runner.Options.Fetch.
type Fetcher struct {
	cfg  Config
	git  gitCmd
	stop chan struct{}
	once sync.Once
	wg   sync.WaitGroup

	mu    sync.Mutex
	repos map[string]*repo
}

// New makes a fetcher. It starts no background work until a request or a sync needs it. Close stops that work.
func New(cfg Config) (*Fetcher, error) {
	if cfg.Dir == "" || cfg.PoolDir == "" || cfg.Remote == nil {
		return nil, errors.New("warm: Dir, PoolDir and Remote are required")
	}
	if cfg.PoolSize < 1 {
		cfg.PoolSize = 2
	}
	if cfg.MaxRepos < 1 {
		cfg.MaxRepos = 16
	}
	if cfg.MaxBytes < 1 {
		cfg.MaxBytes = 20 << 30
	}
	if cfg.Clock == nil {
		cfg.Clock = check.RealClock()
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("warm: git --version: %w", err)
	}
	if err := CheckGitVersion(string(out)); err != nil {
		return nil, fmt.Errorf("warm: %w", err)
	}
	f := &Fetcher{cfg: cfg, git: gitCmd{home: cfg.Dir}, stop: make(chan struct{}), repos: map[string]*repo{}}
	for _, d := range []string{cfg.Dir, cfg.PoolDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := f.reset(); err != nil {
		return nil, err
	}
	return f, nil
}

// reset forgets the ready checkouts of an earlier process and loads the clones that it left.
func (f *Fetcher) reset() error {
	stale, err := os.ReadDir(f.cfg.PoolDir)
	if err != nil {
		return err
	}
	for _, e := range stale {
		_ = os.RemoveAll(filepath.Join(f.cfg.PoolDir, e.Name()))
	}
	dirs, err := os.ReadDir(f.cfg.Dir)
	if err != nil {
		return err
	}
	for _, e := range dirs {
		path := filepath.Join(f.cfg.Dir, e.Name())
		name, err := os.ReadFile(filepath.Join(path, repoMarker))
		if err != nil {
			_ = os.RemoveAll(path)
			continue
		}
		f.repos[string(name)] = &repo{name: string(name), path: path, used: time.Unix(0, 0)}
	}
	return nil
}

// Wait returns when the background work that is under way has ended.
func (f *Fetcher) Wait() { f.wg.Wait() }

// Close cancels the background work, refuses new work and waits for the work that is under way. It is safe to call twice.
func (f *Fetcher) Close() {
	f.mu.Lock()
	f.once.Do(func() { close(f.stop) })
	f.mu.Unlock()
	f.wg.Wait()
}

// admit registers one operation with the lifetime of the fetcher. It returns a context that ends when ctx ends or Close
// runs, and a function that ends the operation. It reports false after Close.
func (f *Fetcher) admit(ctx context.Context) (context.Context, func(), bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed() {
		return ctx, nil, false
	}
	f.wg.Add(1)
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-f.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancel(); f.wg.Done() }, true
}

func (f *Fetcher) closed() bool {
	select {
	case <-f.stop:
		return true
	default:
		return false
	}
}

// Repos returns the repositories that have a clone.
func (f *Fetcher) Repos() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.repos {
		out = append(out, n)
	}
	return out
}

func (f *Fetcher) clonePath(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(f.cfg.Dir, hex.EncodeToString(sum[:12])+".git")
}

// acquire returns the entry of name and marks it busy, so that eviction leaves it. It makes room for a new entry first.
func (f *Fetcher) acquire(name string) *repo {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[name]
	if !ok {
		for len(f.repos) >= f.cfg.MaxRepos && f.evictLocked() {
		}
		r = &repo{name: name, path: f.clonePath(name)}
		f.repos[name] = r
	}
	r.busy++
	r.used = f.cfg.Clock.Now()
	return r
}

func (f *Fetcher) release(r *repo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.busy--
	for len(f.repos) > f.cfg.MaxRepos && f.evictLocked() {
	}
}

// evictLocked removes the idle repository that was used least recently. It reports whether it removed one.
func (f *Fetcher) evictLocked() bool {
	var lru *repo
	for _, r := range f.repos {
		if r.busy == 0 && (lru == nil || r.used.Before(lru.used)) {
			lru = r
		}
	}
	if lru == nil {
		return false
	}
	delete(f.repos, lru.name)
	f.drop(lru)
	return true
}

func (f *Fetcher) drop(r *repo) {
	_ = os.RemoveAll(r.path)
	r.pmu.Lock()
	defer r.pmu.Unlock()
	for _, s := range r.slots {
		_ = os.RemoveAll(s.root)
	}
	r.slots = nil
}

// Sweep removes idle clones, least recently used first, until the cache fits MaxBytes.
func (f *Fetcher) Sweep() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.sizeLocked() > f.cfg.MaxBytes && f.evictLocked() {
	}
}

func (f *Fetcher) sizeLocked() int64 {
	var n int64
	for _, r := range f.repos {
		n += dirSize(r.path)
		r.pmu.Lock()
		for _, s := range r.slots {
			n += dirSize(s.root)
		}
		r.pmu.Unlock()
	}
	return n
}

// Fetch writes the files of req.Tree to the empty directory dir. It refuses a commit whose tree is another tree and a
// tree that holds a submodule. A failure of the cache makes one new clone and a second try, and then runs the fallback.
func (f *Fetcher) Fetch(ctx context.Context, req runnerproto.Request, dir string) error {
	if !objectID.MatchString(req.Commit) {
		return &refusal{"commit is not an object id"}
	}
	ctx, done, ok := f.admit(ctx)
	if !ok {
		return errors.New("the fetcher is closed")
	}
	defer done()
	r := f.acquire(req.Repo)
	defer f.release(r)
	err := f.serve(ctx, r, req, dir)
	var ref *refusal
	var net netError
	if err != nil && !errors.As(err, &ref) && !errors.As(err, &net) && ctx.Err() == nil {
		f.cfg.Log.Warn("warm cache failed, cloning again", "repo", req.Repo, "error", err.Error())
		f.wipe(r)
		clearDir(dir)
		err = f.serve(ctx, r, req, dir)
	}
	if err != nil && !errors.As(err, &ref) && ctx.Err() == nil && f.cfg.Fallback != nil {
		f.cfg.Log.Warn("warm checkout failed, using the fallback", "repo", req.Repo, "error", err.Error())
		clearDir(dir)
		return f.cfg.Fallback(ctx, req, dir)
	}
	return err
}

func clearDir(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

func (f *Fetcher) serve(ctx context.Context, r *repo, req runnerproto.Request, dir string) error {
	if err := f.ensure(ctx, r); err != nil {
		return err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	tree, err := f.treeOf(ctx, r, req.Commit)
	if err == nil && tree == "" {
		if err = f.fetchCommit(ctx, r, req); err == nil {
			tree, err = f.treeOf(ctx, r, req.Commit)
		}
		if err == nil && tree == "" {
			err = fmt.Errorf("commit %s is not in %s", req.Commit, req.Repo)
		}
	}
	switch {
	case err != nil:
		return err
	case tree != req.Tree:
		return &refusal{fmt.Sprintf("commit %s has tree %s, not %s", req.Commit, tree, req.Tree)}
	}
	return f.checkout(ctx, r, req.Commit, dir)
}

func (f *Fetcher) wipe(r *repo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f.drop(r)
}

func (f *Fetcher) gitRun(ctx context.Context, timeout time.Duration, o gitOpts, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return f.git.run(ctx, o, args...)
}

// remote returns the URL and the options of a git run that talks to the remote.
func (f *Fetcher) remote(ctx context.Context, r *repo, name string) (url string, o gitOpts, err error) {
	if url, err = f.cfg.Remote(name); err != nil {
		return "", o, netError{err}
	}
	o.gitDir, o.url = r.path, url
	if f.cfg.Header != nil {
		if o.header, err = f.cfg.Header(ctx); err != nil {
			return "", o, netError{err}
		}
	}
	return url, o, nil
}

// cloned reports whether the clone exists. It takes only the read lock, so a request for a cached commit never waits for
// a sync that holds it too.
func (f *Fetcher) cloned(r *repo) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, err := os.Stat(filepath.Join(r.path, repoMarker))
	return err == nil
}

// ensure makes the clone when it is missing.
func (f *Fetcher) ensure(ctx context.Context, r *repo) error {
	if f.cloned(r) {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := os.Stat(filepath.Join(r.path, repoMarker)); err == nil {
		return nil
	}
	url, o, err := f.remote(ctx, r, r.name)
	if err != nil {
		return err
	}
	tmp := r.path + ".new"
	_ = os.RemoveAll(tmp)
	o.gitDir = ""
	if _, err := f.gitRun(ctx, cloneTimeout, o, "clone", "--bare", "--no-tags", "--quiet", url, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return netError{err}
	}
	for path, text := range map[string]string{filepath.Join(tmp, "info", "attributes"): noAttributes, filepath.Join(tmp, repoMarker): r.name} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			return err
		}
	}
	_ = os.RemoveAll(r.path)
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	f.later(func(context.Context) { f.Sweep() })
	return nil
}

// treeOf returns the tree of commit, or "" when the clone lacks the commit.
func (f *Fetcher) treeOf(ctx context.Context, r *repo, commit string) (string, error) {
	out, err := f.gitRun(ctx, gitTimeout, gitOpts{gitDir: r.path}, "rev-parse", "--verify", "--quiet", commit+"^{tree}")
	var ge *gitError
	if errors.As(err, &ge) && ge.code == 1 {
		return "", nil
	}
	return strings.TrimSpace(string(out)), err
}

// fetchCommit gets one commit that no synced ref reaches.
func (f *Fetcher) fetchCommit(ctx context.Context, r *repo, req runnerproto.Request) error {
	url, o, err := f.remote(ctx, r, req.Repo)
	if err != nil {
		return err
	}
	if _, err := f.gitRun(ctx, fetchTimeout, o, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", url, req.Commit); err != nil {
		return netError{err}
	}
	return nil
}

// later runs fn in the background with a context that Close cancels, tracked by Wait. It reports false after Close.
func (f *Fetcher) later(fn func(ctx context.Context)) bool {
	ctx, done, ok := f.admit(context.Background())
	if !ok {
		return false
	}
	go func() {
		defer done()
		fn(ctx)
	}()
	return true
}

// Run starts each watcher and syncs the clone of each reported repository. It returns when ctx ends. A sweep runs
// every ten minutes.
func (f *Fetcher) Run(ctx context.Context, ws ...Watcher) {
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := w.Watch(ctx, f.Changed); err != nil && ctx.Err() == nil {
					f.cfg.Log.Warn("watcher stopped", "error", err.Error())
				}
				select {
				case <-f.cfg.Clock.After(watchRetry):
				case <-ctx.Done():
				}
			}
		}()
	}
	for ctx.Err() == nil {
		select {
		case <-f.cfg.Clock.After(sweepEvery):
			f.Sweep()
		case <-ctx.Done():
		}
	}
	wg.Wait()
}
