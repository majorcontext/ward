// Package gitbin is the gitremote.Remote that runs the git binary with a clean environment.
package gitbin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/gitremote"
)

// Runner runs git. Home is the HOME of every run, Askpass is the program that git runs for a password, and Timeout bounds one
// run.
type Runner struct {
	Home    string
	Askpass string
	Timeout time.Duration
}

var _ gitremote.Remote = Runner{}

// Env returns the environment of a git run with credential c.
func (g Runner) Env(c gitremote.Cred) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + g.Home, "LC_ALL=C", "LANG=C",
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
		env = append(env, "GIT_ASKPASS="+g.Askpass, gitremote.AskpassFileEnv+"="+c.AskFile, gitremote.AskpassUserEnv+"="+c.AskUser)
	}
	return env
}

// Run runs git in dir with args and returns its output. A failure is a *gitremote.Error.
func (g Runner) Run(ctx context.Context, dir string, c gitremote.Cred, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = g.Env(c)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &gitremote.Error{Stderr: stderr.String(), Err: err, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	}
	return stdout.String(), nil
}

func (g Runner) List(ctx context.Context, url string, c gitremote.Cred) (map[string]string, error) {
	out, err := g.Run(ctx, g.Home, c, "", "ls-remote", "--refs", url)
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

func (g Runner) Push(ctx context.Context, clone, url string, c gitremote.Cred, specs []gitremote.PushSpec) (map[string]gitremote.Outcome, error) {
	results := map[string]gitremote.Outcome{}
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
		out, err := g.Run(ctx, clone, c, "", args...)
		if err == nil {
			parsePorcelain(out, results)
			return results, nil
		}
		var ge *gitremote.Error
		errors.As(err, &ge)
		if missing := missingDeletes(ge.Stderr); len(missing) > 0 {
			var rest []gitremote.PushSpec
			for _, s := range specs {
				if _, gone := missing[s.Ref]; gone && s.Commit == "" {
					results[s.Ref] = gitremote.OK
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
func parsePorcelain(out string, results map[string]gitremote.Outcome) int {
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
			results[to] = gitremote.OK
		case strings.Contains(parts[2], "stale info"):
			results[to] = gitremote.Stale
		default:
			results[to] = gitremote.Rejected
		}
	}
	return n
}
