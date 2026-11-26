// Package remotetest is the conformance suite of gitremote.Remote. It pushes commits of a local repository to a remote and reads the
// refs back.
package remotetest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/majorcontext/ward/internal/gitremote"
)

// Impl builds the implementation under test. New returns an empty remote and the URL that names it.
type Impl struct {
	New func(t *testing.T) (gitremote.Remote, string)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Run runs the suite.
func Run(t *testing.T, impl Impl) {
	ctx := context.Background()
	clone := t.TempDir()
	git(t, clone, "init", "-q", "-b", "main")
	git(t, clone, "commit", "-q", "--allow-empty", "-m", "one")
	c1 := git(t, clone, "rev-parse", "HEAD")
	git(t, clone, "commit", "-q", "--allow-empty", "-m", "two")
	c2 := git(t, clone, "rev-parse", "HEAD")
	const ref = "refs/heads/a"

	list := func(t *testing.T, r gitremote.Remote, url string) map[string]string {
		t.Helper()
		got, err := r.List(ctx, url, gitremote.Cred{})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// push fails the test when the result lacks an entry for a pushed ref: the zero Outcome is OK, so an absent entry would pass.
	push := func(t *testing.T, r gitremote.Remote, url string, specs ...gitremote.PushSpec) map[string]gitremote.Outcome {
		t.Helper()
		got, err := r.Push(ctx, clone, url, gitremote.Cred{}, specs)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range specs {
			if _, ok := got[s.Ref]; !ok {
				t.Fatalf("the result %v has no outcome for %s", got, s.Ref)
			}
		}
		return got
	}

	t.Run("an empty remote lists no ref", func(t *testing.T) {
		r, url := impl.New(t)
		if got := list(t, r, url); len(got) != 0 {
			t.Errorf("refs = %v", got)
		}
	})
	t.Run("a lease on absence creates a ref once", func(t *testing.T) {
		r, url := impl.New(t)
		if got := push(t, r, url, gitremote.PushSpec{Ref: ref, Commit: c1, LeaseSet: true})[ref]; got != gitremote.OK {
			t.Errorf("create = %v", got)
		}
		if got := list(t, r, url)[ref]; got != c1 {
			t.Errorf("listed %q, want %s", got, c1)
		}
		if got := push(t, r, url, gitremote.PushSpec{Ref: ref, Commit: c2, LeaseSet: true})[ref]; got != gitremote.Stale {
			t.Errorf("create of a ref that exists = %v, want Stale", got)
		}
		if got := list(t, r, url)[ref]; got != c1 {
			t.Errorf("a stale push moved the ref to %q", got)
		}
	})
	t.Run("a lease that holds moves the ref and a lease that does not holds it back", func(t *testing.T) {
		r, url := impl.New(t)
		push(t, r, url, gitremote.PushSpec{Ref: ref, Commit: c1})
		if got := push(t, r, url, gitremote.PushSpec{Ref: ref, Commit: c2, Lease: c2, LeaseSet: true})[ref]; got != gitremote.Stale {
			t.Errorf("wrong lease = %v, want Stale", got)
		}
		if got := push(t, r, url, gitremote.PushSpec{Ref: ref, Commit: c2, Lease: c1, LeaseSet: true})[ref]; got != gitremote.OK {
			t.Errorf("right lease = %v", got)
		}
		if got := list(t, r, url)[ref]; got != c2 {
			t.Errorf("listed %q, want %s", got, c2)
		}
	})
	t.Run("a push with no lease overwrites", func(t *testing.T) {
		r, url := impl.New(t)
		push(t, r, url, gitremote.PushSpec{Ref: ref, Commit: c2})
		if got := push(t, r, url, gitremote.PushSpec{Ref: ref, Commit: c1})[ref]; got != gitremote.OK {
			t.Errorf("force move = %v", got)
		}
		if got := list(t, r, url)[ref]; got != c1 {
			t.Errorf("listed %q, want %s", got, c1)
		}
	})
	t.Run("a delete removes a ref, and a delete of a missing ref succeeds", func(t *testing.T) {
		r, url := impl.New(t)
		push(t, r, url, gitremote.PushSpec{Ref: ref, Commit: c1})
		if got := push(t, r, url, gitremote.PushSpec{Ref: ref})[ref]; got != gitremote.OK {
			t.Errorf("delete = %v", got)
		}
		if got := list(t, r, url); len(got) != 0 {
			t.Errorf("refs after the delete = %v", got)
		}
		if got := push(t, r, url, gitremote.PushSpec{Ref: ref})[ref]; got != gitremote.OK {
			t.Errorf("delete of a missing ref = %v", got)
		}
	})
	t.Run("one push reports each ref", func(t *testing.T) {
		r, url := impl.New(t)
		push(t, r, url, gitremote.PushSpec{Ref: ref, Commit: c1})
		got := push(t, r, url,
			gitremote.PushSpec{Ref: ref, Commit: c2, Lease: c2, LeaseSet: true},
			gitremote.PushSpec{Ref: "refs/heads/b", Commit: c1, LeaseSet: true})
		if got[ref] != gitremote.Stale || got["refs/heads/b"] != gitremote.OK {
			t.Errorf("outcomes = %v", got)
		}
	})
}
