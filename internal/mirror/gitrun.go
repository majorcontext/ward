package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const gitTimeout = 10 * time.Minute

// gitRunner runs the git binary with a clean environment.
type gitRunner struct {
	home    string
	askpass string
	timeout time.Duration
}

// gitCreds is the credential of one run. Header is a complete HTTP header line. AskFile and AskUser feed the askpass helper.
type gitCreds struct {
	Header  string
	AskUser string
	AskFile string
	// KeepGC leaves gc.auto at its default, for the explicit maintenance command.
	KeepGC bool
}

type gitError struct {
	stderr   string
	err      error
	timedOut bool
}

func (e *gitError) Error() string { return "git: " + e.err.Error() }
func (e *gitError) Unwrap() error { return e.err }

func (g *gitRunner) env(c gitCreds) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + g.home, "LC_ALL=C", "LANG=C",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
	}
	if ca := os.Getenv("GIT_SSL_CAINFO"); ca != "" {
		env = append(env, "GIT_SSL_CAINFO="+ca)
	}
	cfg := [][2]string{{"protocol.version", "2"}, {"credential.helper", ""}, {"core.askPass", ""}}
	if !c.KeepGC {
		cfg = append(cfg, [2]string{"gc.auto", "0"})
	}
	if c.Header != "" {
		cfg = append(cfg, [2]string{"http.extraHeader", c.Header})
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	if c.AskFile != "" {
		env = append(env, "GIT_ASKPASS="+g.askpass, askpassFileEnv+"="+c.AskFile, askpassUserEnv+"="+c.AskUser)
	}
	return env
}

func (g *gitRunner) run(ctx context.Context, dir string, c gitCreds, args ...string) (string, error) {
	return g.runIn(ctx, dir, c, "", args...)
}

func (g *gitRunner) runIn(ctx context.Context, dir string, c gitCreds, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = g.env(c)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &gitError{stderr: stderr.String(), err: err, timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	}
	return stdout.String(), nil
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
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "update-ref", "--stdin")
	cmd.Dir, cmd.Env, cmd.Stdin = clone, g.env(gitCreds{}), strings.NewReader(in.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &gitError{stderr: stderr.String(), err: err}
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

// lsRemote returns the refs of the remote that name a commit, as git name to commit. It skips HEAD and peeled tags.
func (g *gitRunner) lsRemote(ctx context.Context, url string, c gitCreds) (map[string]string, error) {
	out, err := g.run(ctx, g.home, c, "ls-remote", "--refs", url)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for line := range strings.Lines(out) {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && strings.HasPrefix(name, "refs/") {
			refs[name] = sha
		}
	}
	return refs, nil
}

type pushSpec struct {
	Ref      string
	Commit   string
	Lease    string
	LeaseSet bool
}

type outcome int

const (
	outcomeOK outcome = iota
	outcomeStale
	outcomeRejected
)

// push sends the specs in one git push and reports each ref. A delete of a ref that the remote lacks is a success.
// An error means that the push as a whole failed.
func (g *gitRunner) push(ctx context.Context, clone, url string, c gitCreds, specs []pushSpec) (map[string]outcome, error) {
	results := map[string]outcome{}
	for len(specs) > 0 {
		args := []string{"push", "--porcelain", "--no-verify"}
		for _, s := range specs {
			if s.LeaseSet {
				args = append(args, "--force-with-lease="+s.Ref+":"+s.Lease)
			}
		}
		args = append(args, url)
		for _, s := range specs {
			switch {
			case s.Commit == "":
				args = append(args, ":"+s.Ref)
			case s.LeaseSet:
				// A leading plus makes git ignore the lease, and a lease that holds already allows a non-fast-forward move.
				args = append(args, s.Commit+":"+s.Ref)
			default:
				args = append(args, "+"+s.Commit+":"+s.Ref)
			}
		}
		out, err := g.run(ctx, clone, c, args...)
		if err == nil {
			parsePorcelain(out, results)
			return results, nil
		}
		var ge *gitError
		errors.As(err, &ge)
		if missing := missingDeletes(ge.stderr); len(missing) > 0 {
			var rest []pushSpec
			for _, s := range specs {
				if _, gone := missing[s.Ref]; gone && s.Commit == "" {
					results[s.Ref] = outcomeOK
				} else {
					rest = append(rest, s)
				}
			}
			if len(rest) < len(specs) {
				specs = rest
				continue
			}
		}
		if n := parsePorcelain(out, results); n > 0 {
			return results, nil
		}
		return nil, err
	}
	return results, nil
}

func missingDeletes(stderr string) map[string]struct{} {
	out := map[string]struct{}{}
	for line := range strings.Lines(stderr) {
		if !strings.Contains(line, "remote ref does not exist") {
			continue
		}
		if _, rest, ok := strings.Cut(line, "unable to delete '"); ok {
			if name, _, ok := strings.Cut(rest, "'"); ok {
				out[name] = struct{}{}
			}
		}
	}
	return out
}

// parsePorcelain records the ref lines of a git push --porcelain and returns how many it found.
func parsePorcelain(out string, results map[string]outcome) int {
	n := 0
	for line := range strings.Lines(out) {
		parts := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
		if len(parts) < 3 || len(parts[0]) != 1 || !strings.Contains(parts[1], ":") {
			continue
		}
		_, to, _ := strings.Cut(parts[1], ":")
		n++
		switch {
		case parts[0] != "!":
			results[to] = outcomeOK
		case strings.Contains(parts[2], "stale info"):
			results[to] = outcomeStale
		default:
			results[to] = outcomeRejected
		}
	}
	return n
}

func classifyRemote(err error) Class {
	var ge *gitError
	if !errors.As(err, &ge) {
		return ClassLocal
	}
	s := strings.ToLower(ge.stderr)
	has := func(subs ...string) bool {
		for _, sub := range subs {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}
	switch {
	case ge.timedOut:
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
	if ge.timedOut {
		return true
	}
	s := strings.ToLower(ge.stderr)
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
