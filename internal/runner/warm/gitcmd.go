package warm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

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

// run returns the standard output. With o.open, git runs under umask 0, so that the check user can write the files.
func (g gitCmd) run(ctx context.Context, o gitOpts, args ...string) ([]byte, error) {
	name, shown := "git", strings.Join(args, " ")
	if o.open {
		name, args = "sh", append([]string{"-c", `umask 0; exec git "$@"`, "sh"}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = g.env(o)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	ge := &gitError{args: shown, stderr: errb.String(), code: -1, err: err}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		ge.code = exit.ExitCode()
	}
	if len(ge.stderr) > 512 {
		ge.stderr = ge.stderr[len(ge.stderr)-512:]
	}
	return nil, ge
}
