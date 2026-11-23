package warm_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/ward/internal/runner/warm"
	"github.com/majorcontext/ward/runnerproto"
)

type upstream struct {
	t          *testing.T
	work, bare string
}

func (u upstream) git(args ...string) string {
	u.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = u.work
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		u.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newUpstream(t *testing.T) upstream {
	root := t.TempDir()
	u := upstream{t: t, work: filepath.Join(root, "work"), bare: filepath.Join(root, "up.git")}
	if err := os.Mkdir(u.work, 0o755); err != nil {
		t.Fatal(err)
	}
	u.git("init", "-q", "-b", "main")
	u.git("init", "-q", "--bare", u.bare)
	u.git("-C", u.bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	u.git("remote", "add", "origin", u.bare)
	return u
}

// commit writes files, commits them and pushes to refspec. It returns the commit and its tree.
func (u upstream) commit(refspec string, files map[string]string) (commit, tree string) {
	for p, text := range files {
		full := filepath.Join(u.work, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			u.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o755); err != nil {
			u.t.Fatal(err)
		}
	}
	u.git("add", "-A")
	u.git("commit", "-q", "-m", "c", "--allow-empty")
	u.git("push", "-q", "origin", "HEAD:"+refspec)
	return u.git("rev-parse", "HEAD"), u.git("rev-parse", "HEAD^{tree}")
}

func newFetcher(t *testing.T, ups map[string]upstream, change func(*warm.Config)) (*warm.Fetcher, string) {
	root := t.TempDir()
	cfg := warm.Config{
		Dir: filepath.Join(root, "clones"), PoolDir: filepath.Join(root, "pool"),
		Remote: func(repo string) (string, error) { return "file://" + ups[repo].bare, nil },
	}
	if change != nil {
		change(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f, err := warm.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); f.Wait() })
	return f, root
}

func fetch(t *testing.T, f *warm.Fetcher, repo, commit, tree string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	return dir, f.Fetch(t.Context(), runnerproto.Request{Repo: repo, Commit: commit, Tree: tree}, dir)
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFetchWritesTheExactTree(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	u.commit("main", map[string]string{".gitattributes": "*.txt text eol=crlf\n", "a.txt": "x\ny\n", "pkg/run.sh": "#!/bin/sh\n"})
	if err := os.Symlink("a.txt", filepath.Join(u.work, "link")); err != nil {
		t.Fatal(err)
	}
	c, tree := u.commit("main", nil)
	u.git("update-index", "--add", "--cacheinfo", "160000,"+c+",sub")
	u.git("commit", "-q", "-m", "sub")
	u.git("push", "-q", "origin", "HEAD:refs/heads/sub")
	sub, subTree := u.git("rev-parse", "HEAD"), u.git("rev-parse", "HEAD^{tree}")
	called := false
	f, _ := newFetcher(t, map[string]upstream{"r": u}, func(c *warm.Config) {
		c.Fallback = func(context.Context, runnerproto.Request, string) error { called = true; return nil }
	})
	for _, tc := range []struct{ name, commit, tree, want string }{
		{"tree", c, tree, ""},
		{"other tree", c, subTree, "has tree"},
		{"submodule", sub, subTree, "submodule"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := fetch(t, f, "r", tc.commit, tc.tree)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := read(t, filepath.Join(dir, "a.txt")); got != "x\ny\n" {
				t.Errorf("a.txt = %q, want the blob bytes", got)
			}
			if l, _ := os.Readlink(filepath.Join(dir, "link")); l != "a.txt" {
				t.Errorf("link -> %q", l)
			}
			if st, err := os.Stat(filepath.Join(dir, "pkg")); err != nil || st.Mode().Perm() != 0o777 {
				t.Errorf("pkg mode %v, want 0777", st)
			}
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				t.Error("the checkout holds .git")
			}
		})
	}
	if called {
		t.Error("a refusal ran the fallback")
	}
}

func TestSyncedCommitNeedsNoRemote(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	c1, t1 := u.commit("main", map[string]string{"a": "1"})
	f, _ := newFetcher(t, map[string]upstream{"r": u}, nil)
	if _, err := fetch(t, f, "r", c1, t1); err != nil {
		t.Fatal(err)
	}
	c2, t2 := u.commit("main", map[string]string{"a": "2", "b": "new"})
	f.Wait()
	f.Sync(t.Context(), "r")
	f.Wait()
	if err := os.RemoveAll(u.bare); err != nil {
		t.Fatal(err)
	}
	dir, err := fetch(t, f, "r", c2, t2)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "a")) != "2" || read(t, filepath.Join(dir, "b")) != "new" {
		t.Error("the checkout is not the synced commit")
	}
}

func TestMissingCommitIsFetchedAlone(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	c1, t1 := u.commit("main", map[string]string{"a": "1"})
	f, _ := newFetcher(t, map[string]upstream{"r": u}, nil)
	if _, err := fetch(t, f, "r", c1, t1); err != nil {
		t.Fatal(err)
	}
	c2, t2 := u.commit("refs/changes/1", map[string]string{"a": "2"})
	dir, err := fetch(t, f, "r", c2, t2)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "a")) != "2" {
		t.Error("the checkout is not the requested commit")
	}
}

func TestCheckCannotChangeTheCache(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	c, tree := u.commit("main", map[string]string{"a": "1", "d/b": "2"})
	f, root := newFetcher(t, map[string]upstream{"r": u}, nil)
	first, err := fetch(t, f, "r", c, tree)
	if err != nil {
		t.Fatal(err)
	}
	f.Wait()
	_ = os.WriteFile(filepath.Join(first, "a"), []byte("changed"), 0o666)
	_ = os.Remove(filepath.Join(first, "d", "b"))
	_ = os.WriteFile(filepath.Join(first, "extra"), nil, 0o666)
	second, err := fetch(t, f, "r", c, tree)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(second, "a")) != "1" || read(t, filepath.Join(second, "d", "b")) != "2" {
		t.Error("a write of the first check reached the second")
	}
	if _, err := os.Stat(filepath.Join(second, "extra")); err == nil {
		t.Error("a file of the first check reached the second")
	}
	if _, err := os.Stat(filepath.Join(root, "clones")); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("git", "-C", filepath.Join(root, "clones"), "worktree", "list").CombinedOutput(); strings.Contains(string(out), root+"/pool") {
		t.Error("a checkout is a worktree of the clone")
	}
}

func TestCorruptCloneIsClonedAgain(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	c, tree := u.commit("main", map[string]string{"a": "1"})
	f, root := newFetcher(t, map[string]upstream{"r": u}, nil)
	if _, err := fetch(t, f, "r", c, tree); err != nil {
		t.Fatal(err)
	}
	clones, _ := filepath.Glob(filepath.Join(root, "clones", "*.git"))
	if len(clones) != 1 {
		t.Fatalf("clones %v", clones)
	}
	if err := os.RemoveAll(filepath.Join(clones[0], "objects")); err != nil {
		t.Fatal(err)
	}
	dir, err := fetch(t, f, "r", c, tree)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "a")) != "1" {
		t.Error("the checkout is wrong after the new clone")
	}
}

func TestCacheStaysBounded(t *testing.T) {
	t.Parallel()
	ups := map[string]upstream{}
	ids := map[string][2]string{}
	for _, n := range []string{"a", "b", "c"} {
		ups[n] = newUpstream(t)
		c, tree := ups[n].commit("main", map[string]string{"f": n})
		ids[n] = [2]string{c, tree}
	}
	f, root := newFetcher(t, ups, func(c *warm.Config) { c.MaxRepos, c.PoolSize = 2, 1 })
	for _, n := range []string{"a", "b", "c"} {
		if _, err := fetch(t, f, n, ids[n][0], ids[n][1]); err != nil {
			t.Fatal(err)
		}
		f.Wait()
	}
	clones, _ := filepath.Glob(filepath.Join(root, "clones", "*.git"))
	slots, _ := filepath.Glob(filepath.Join(root, "pool", "slot-*"))
	if len(clones) != 2 || len(f.Repos()) != 2 || len(slots) > 2 {
		t.Errorf("clones %d, repos %v, ready checkouts %d; want 2, 2 repos and at most 2", len(clones), f.Repos(), len(slots))
	}
}

func TestFailureFallsBack(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	c, tree := u.commit("main", map[string]string{"a": "1"})
	var got string
	f, _ := newFetcher(t, map[string]upstream{"r": {bare: filepath.Join(t.TempDir(), "none.git")}}, func(c *warm.Config) {
		c.Fallback = func(_ context.Context, req runnerproto.Request, _ string) error { got = req.Commit; return nil }
	})
	if _, err := fetch(t, f, "r", c, tree); err != nil || got != c {
		t.Errorf("error %v, fallback commit %q, want %q", err, got, c)
	}
}
