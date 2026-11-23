package warm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/majorcontext/ward/internal/procgroup"
)

// MinGit is the oldest git that the warm path runs: the environment variables GIT_CONFIG_COUNT, GIT_CONFIG_KEY_n and
// GIT_CONFIG_VALUE_n, which carry the credential header, need git 2.31.
const MinGit = "2.31"

var versionText = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// CheckGitVersion fails when the output of git --version names a git older than MinGit.
func CheckGitVersion(out string) error {
	m := versionText.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("the output of git --version is not a version: %q", strings.TrimSpace(out))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 2 || major == 2 && minor < 31 {
		return fmt.Errorf("git %s.%s is too old: the warm checkouts need git %s or later", m[1], m[2], MinGit)
	}
	return nil
}

const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

type gitCmd struct{ home string }

// gitOpts selects the repository and the environment of one git run. Header is a complete HTTP header line; it reaches
// git through the environment, never through the argument list.
type gitOpts struct {
	gitDir, workTree, index, header string
	open                            bool
}

type gitError struct {
	args   string
	stderr string
	code   int
	err    error
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", e.args, e.err, strings.TrimSpace(e.stderr))
}

func (e *gitError) Unwrap() error { return e.err }

func (g gitCmd) env(o gitOpts) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + g.home, "LC_ALL=C", "LANG=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
	if ca := os.Getenv("GIT_SSL_CAINFO"); ca != "" {
		env = append(env, "GIT_SSL_CAINFO="+ca)
	}
	for _, kv := range [][2]string{{"GIT_DIR", o.gitDir}, {"GIT_WORK_TREE", o.workTree}, {"GIT_INDEX_FILE", o.index}} {
		if kv[1] != "" {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	cfg := [][2]string{{"protocol.version", "2"}, {"credential.helper", ""}, {"core.askPass", ""}, {"gc.autoDetach", "false"}}
	if o.header != "" {
		cfg = append(cfg, [2]string{"http.extraHeader", o.header})
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

const maxOutput = 1 << 20

// tail keeps the last n bytes written to it.
type tail struct {
	b []byte
	n int
}

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.n {
		t.b = t.b[len(t.b)-t.n:]
	}
	return len(p), nil
}

// capped keeps at most maxOutput bytes and fails the run past that.
type capped struct{ b []byte }

func (c *capped) Write(p []byte) (int, error) {
	if len(c.b)+len(p) > maxOutput {
		return 0, errors.New("output is too large")
	}
	c.b = append(c.b, p...)
	return len(p), nil
}

// run returns the standard output, at most 1 MiB of it.
func (g gitCmd) run(ctx context.Context, o gitOpts, args ...string) ([]byte, error) {
	var out capped
	err := g.runTo(ctx, o, &out, args...)
	return out.b, err
}

// runTo writes the standard output to stdout. With o.open, git runs under umask 0, so that the check user can write
// the files. The run is a process group, so that a helper of git ends with it.
func (g gitCmd) runTo(ctx context.Context, o gitOpts, stdout io.Writer, args ...string) error {
	name, shown := "git", strings.Join(args, " ")
	if o.open {
		name, args = "sh", append([]string{"-c", `umask 0; exec git "$@"`, "sh"}, args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Env = g.env(o)
	errb := &tail{n: 512}
	cmd.Stdout, cmd.Stderr = stdout, errb
	err := procgroup.Run(ctx, cmd, nil)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ge := &gitError{args: shown, stderr: string(errb.b), code: -1, err: err}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		ge.code = exit.ExitCode()
	}
	return ge
}
