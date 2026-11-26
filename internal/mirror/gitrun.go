package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/majorcontext/ward/internal/gitremote"
	"github.com/majorcontext/ward/internal/gitremote/gitbin"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const gitTimeout = 10 * time.Minute

// gitRunner runs the git binary for the work that stays inside the mirror: fetch, clone, local refs and maintenance. The
// gitremote.Remote seam does the work that talks to a remote.
type gitRunner struct{ gitbin.Runner }

// gitCreds is the credential of one run.
type gitCreds = gitremote.Cred

type gitError = gitremote.Error

func (g *gitRunner) env(c gitCreds) []string { return g.Env(c) }

func (g *gitRunner) run(ctx context.Context, dir string, c gitCreds, args ...string) (string, error) {
	return g.Run(ctx, dir, c, "", args...)
}

func (g *gitRunner) runIn(ctx context.Context, dir string, c gitCreds, stdin string, args ...string) (string, error) {
	return g.Run(ctx, dir, c, stdin, args...)
}

// initBare creates an empty bare repository without starting git.
func (g *gitRunner) initBare(path string) error {
	for _, d := range []string{"objects/pack", "objects/info", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(path, d), 0o755); err != nil {
			return err
		}
	}
	files := map[string]string{
		"HEAD":   "ref: refs/heads/main\n",
		"config": "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(path, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (g *gitRunner) fetch(ctx context.Context, clone, url string, c gitCreds, shas []string) error {
	args := append([]string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", url}, shas...)
	_, err := g.run(ctx, clone, c, args...)
	return err
}

func localRef(git string) string { return "refs/mirror/" + git }

// setLocalRefs points or deletes the local refs in one transaction. An empty commit deletes.
func (g *gitRunner) setLocalRefs(ctx context.Context, clone string, refs map[string]string) error {
	var in strings.Builder
	for git, sha := range refs {
		if sha == "" {
			fmt.Fprintf(&in, "delete %s\n", localRef(git))
		} else {
			fmt.Fprintf(&in, "update %s %s\n", localRef(git), sha)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "update-ref", "--stdin")
	cmd.Dir, cmd.Env, cmd.Stdin = clone, g.env(gitCreds{}), strings.NewReader(in.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &gitError{Stderr: stderr.String(), Err: err}
	}
	return nil
}

// autoGCDue applies the test of gc --auto without starting git: more than 27 loose objects in objects/17 (the
// default gc.auto of 6700 over 256 directories) or more than 50 packs without a keep file (the default gc.autoPackLimit).
func autoGCDue(clone string) bool {
	if loose, _ := os.ReadDir(filepath.Join(clone, "objects", "17")); len(loose) > 27 {
		return true
	}
	packs, _ := filepath.Glob(filepath.Join(clone, "objects", "pack", "*.pack"))
	n := 0
	for _, p := range packs {
		if _, err := os.Stat(strings.TrimSuffix(p, ".pack") + ".keep"); err != nil {
			n++
		}
	}
	return n > 50
}

func (g *gitRunner) gc(ctx context.Context, clone string, prune bool) error {
	args := []string{"gc", "--auto", "--quiet"}
	if prune {
		args = []string{"gc", "--prune=now", "--quiet"}
	} else if !autoGCDue(clone) {
		return nil
	}
	_, err := g.run(ctx, clone, gitCreds{KeepGC: true}, args...)
	return err
}

func classifyRemote(err error) Class {
	var ge *gitError
	if !errors.As(err, &ge) {
		return ClassLocal
	}
	s := strings.ToLower(ge.Stderr)
	has := func(subs ...string) bool {
		for _, sub := range subs {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}
	switch {
	case ge.TimedOut:
		return ClassRemoteUnreachable
	case has("authentication failed", "could not read username", "could not read password", "returned error: 401", "returned error: 403"):
		return ClassRemoteAuth
	case has("could not resolve host", "failed to connect", "connection refused", "timed out", "unable to access", "couldn't connect", "connection reset", "empty reply", "ssl"):
		return ClassRemoteUnreachable
	case has("[remote rejected]", "remote rejected"):
		return ClassRemoteRejected
	}
	return ClassLocal
}

// transportFailure reports whether a fetch from motte failed in the network or in motte, not in the clone.
func transportFailure(err error) bool {
	var ge *gitError
	if !errors.As(err, &ge) {
		return false
	}
	if ge.TimedOut {
		return true
	}
	s := strings.ToLower(ge.Stderr)
	for _, sub := range []string{"unable to access", "could not resolve host", "failed to connect", "connection", "returned error", "remote error",
		"not our ref", "rpc failed", "early eof", "authentication failed", "could not read", "timed out", "unexpected disconnect", "the remote end hung up"} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

var failureText = map[Class]string{
	ClassMotte:             "the read or fetch from the source store failed",
	ClassRemoteUnreachable: "the remote did not answer",
	ClassRemoteAuth:        "the remote refused the credential",
	ClassRemoteRejected:    "the remote refused the ref update",
	ClassLocal:             "git or the local clone failed",
}
