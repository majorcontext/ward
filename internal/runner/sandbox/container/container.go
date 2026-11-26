// Package container runs a check in an OCI container through the command line of an engine such as docker or podman.
package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/majorcontext/ward/internal/procgroup"
	"github.com/majorcontext/ward/internal/runner/sandbox"
	"github.com/majorcontext/ward/runnerproto"
)

const (
	cleanupTime = 15 * time.Second
	// engineFailure is the exit code of docker and podman when the engine fails before the command starts.
	engineFailure = 125
)

// lastBytes keeps the last 512 bytes of the output, for the message of an engine failure.
type lastBytes struct{ b []byte }

func (l *lastBytes) Write(p []byte) (int, error) {
	l.b = append(l.b, p...)
	if len(l.b) > 512 {
		l.b = l.b[len(l.b)-512:]
	}
	return len(p), nil
}

func (l *lastBytes) String() string { return string(l.b) }

// Config says how the container runs. Engine and Image are required.
type Config struct {
	Engine string
	Image  string
	// Runtime is the OCI runtime name that the engine passes with --runtime. Empty means the default of the engine.
	Runtime string
	// Network is the engine network mode. The default none gives the check no network.
	Network string
	// User is the uid:gid of the check. The default is the uid and gid of the runner, so that the runner can delete what the
	// check wrote, and 65532:65532 when the runner is root.
	User      string
	Memory    string
	PidsLimit int
}

// Sandbox is the engine-backed implementation.
type Sandbox struct {
	cfg Config
	// keepID maps the uid of the runner to itself in the container. Rootless podman otherwise maps it to root and puts
	// --user in a subordinate range, so the check cannot write the bind mounts that the runner owns.
	keepID bool
}

var _ sandbox.Sandbox = (*Sandbox)(nil)

// Isolation is the level of the default runtime of the engine, or of the runtime that cfg names.
func (s *Sandbox) Isolation() runnerproto.Isolation {
	if s.cfg.Runtime == "runsc" {
		return runnerproto.IsolationGVisor
	}
	return runnerproto.IsolationContainer
}

// New returns a sandbox. It fills the defaults of cfg.
func New(cfg Config) (*Sandbox, error) {
	if cfg.Engine == "" || cfg.Image == "" {
		return nil, errors.New("container sandbox: engine and image are required")
	}
	if _, err := exec.LookPath(cfg.Engine); err != nil {
		return nil, err
	}
	if cfg.Network == "" {
		cfg.Network = "none"
	}
	keepID := filepath.Base(cfg.Engine) == "podman" && cfg.User == "" && os.Getuid() != 0
	if cfg.User == "" {
		cfg.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		if os.Getuid() == 0 {
			cfg.User = "65532:65532"
		}
	}
	if cfg.Memory == "" {
		cfg.Memory = "4g"
	}
	if cfg.PidsLimit == 0 {
		cfg.PidsLimit = 1024
	}
	return &Sandbox{cfg: cfg, keepID: keepID}, nil
}

func (s *Sandbox) args(name string, spec sandbox.Spec) []string {
	a := []string{"run", "--rm", "--init", "--name", name,
		"--network", s.cfg.Network,
		"--user", s.cfg.User,
		"--read-only", "--tmpfs", "/tmp:rw,exec,nosuid,size=1g",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", s.cfg.Memory, "--pids-limit", strconv.Itoa(s.cfg.PidsLimit),
		"--entrypoint", spec.Argv[0],
		"-e", "HOME=/tmp",
		"-v", spec.Dir + ":/work", "-w", "/work"}
	if spec.Cache != "" {
		a = append(a, "-v", spec.Cache+":"+sandbox.CacheMount)
		for _, e := range sandbox.CacheEnv(sandbox.CacheMount) {
			a = append(a, "-e", e)
		}
	}
	if s.keepID {
		a = append(a, "--userns", "keep-id")
	}
	if s.cfg.Runtime != "" {
		a = append(a, "--runtime", s.cfg.Runtime)
	}
	a = append(a, s.cfg.Image)
	return append(a, spec.Argv[1:]...)
}

func (s *Sandbox) Run(ctx context.Context, spec sandbox.Spec) (sandbox.Outcome, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return sandbox.Outcome{}, err
	}
	name := "ward-check-" + hex.EncodeToString(id[:])
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	cmd := exec.Command(s.cfg.Engine, s.args(name, spec)...)
	var last lastBytes
	out := io.MultiWriter(spec.Output, &last)
	cmd.Stdout, cmd.Stderr = out, out
	remove := func() {
		c, stop := context.WithTimeout(context.WithoutCancel(ctx), cleanupTime)
		defer stop()
		_ = exec.CommandContext(c, s.cfg.Engine, "rm", "--force", name).Run()
	}
	err := procgroup.Run(runCtx, cmd, remove)
	defer remove()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return sandbox.Outcome{}, ctx.Err()
	case runCtx.Err() != nil:
		return sandbox.Outcome{TimedOut: true}, nil
	case errors.As(err, &exit) && exit.ExitCode() == engineFailure:
		return sandbox.Outcome{}, fmt.Errorf("%s could not run the container: %s", s.cfg.Engine, last.String())
	case errors.As(err, &exit):
		return sandbox.Outcome{ExitCode: exit.ExitCode()}, nil
	}
	return sandbox.Outcome{}, err
}
