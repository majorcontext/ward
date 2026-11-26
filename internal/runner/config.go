package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/config"
	"github.com/majorcontext/ward/internal/runner/sandbox"
	"github.com/majorcontext/ward/internal/runner/sandbox/container"
	"github.com/majorcontext/ward/internal/runner/sandbox/gvisor"
	"github.com/majorcontext/ward/runnerproto"
)

// Config is the configuration of `ward runner`, read from the environment.
type Config struct {
	Options     Options
	SandboxKind string
}

// Load reads the configuration from getenv and builds the runner options. SandboxKind is gvisor, container or auto.
// With auto the runner uses gVisor when runsc is on the path and a plain container otherwise.
func Load(getenv func(string) string) (Config, error) {
	c := Config{}
	if m := getenv("WARD_RUNNER_MODE"); m != "" && m != "pull" {
		return c, errors.New("WARD_RUNNER_MODE must be pull or absent")
	}
	required := []string{"WARD_RUNNER_WARD_URL", "WARD_RUNNER_KID", "WARD_RUNNER_KEY_FILE", "WARD_RUNNER_MOTTE_URL", "WARD_RUNNER_MOTTE_GRANT_FILE", "WARD_RUNNER_IMAGE"}
	for _, name := range required {
		if getenv(name) == "" {
			return c, fmt.Errorf("%s is required", name)
		}
	}
	for _, name := range []string{"WARD_RUNNER_WARD_URL", "WARD_RUNNER_MOTTE_URL"} {
		if u, err := url.Parse(getenv(name)); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return c, fmt.Errorf("%s must be an http or https URL", name)
		}
	}
	key, err := config.ReadPrivateKey(getenv("WARD_RUNNER_KEY_FILE"))
	if err != nil {
		return c, fmt.Errorf("WARD_RUNNER_KEY_FILE: %w", err)
	}
	grantFile, kid := getenv("WARD_RUNNER_MOTTE_GRANT_FILE"), getenv("WARD_RUNNER_KID")
	if _, err := readText(grantFile); err != nil {
		return c, fmt.Errorf("WARD_RUNNER_MOTTE_GRANT_FILE: %w", err)
	}
	conc := 1
	if v := getenv("WARD_RUNNER_CONCURRENCY"); v != "" {
		if conc, err = strconv.Atoi(v); err != nil || conc < 1 {
			return c, errors.New("WARD_RUNNER_CONCURRENCY must be a positive whole number")
		}
	}
	work := getenv("WARD_RUNNER_WORK_DIR")
	if work == "" {
		work = os.TempDir()
	}
	if work, err = filepath.Abs(work); err != nil {
		return c, fmt.Errorf("WARD_RUNNER_WORK_DIR: %w", err)
	}
	engine := getenv("WARD_RUNNER_ENGINE")
	if engine == "" {
		engine = "docker"
	}
	cc := container.Config{Engine: engine, Image: getenv("WARD_RUNNER_IMAGE"), Network: getenv("WARD_RUNNER_NETWORK"), Memory: getenv("WARD_RUNNER_MEMORY")}
	kind, err := resolveSandbox(getenv("WARD_RUNNER_SANDBOX"), gvisor.Available())
	if err != nil {
		return c, err
	}
	sb, err := newSandbox(kind, cc)
	if err != nil {
		return c, err
	}
	c.SandboxKind = string(sb.Isolation())
	fetch := MotteFetcher{URL: getenv("WARD_RUNNER_MOTTE_URL"), Grant: func(context.Context) (string, error) { return readText(grantFile) }}
	c.Options = Options{
		WardURL: getenv("WARD_RUNNER_WARD_URL"),
		Token: func(repo string, exp time.Time) (string, error) {
			return runnerproto.MintCheckToken(kid, ed25519.PrivateKey(key), repo, "ward-runner", exp)
		},
		RunnerToken: func(exp time.Time) (string, error) {
			return runnerproto.MintRunnerToken(kid, ed25519.PrivateKey(key), "ward-runner", exp)
		},
		Fetch:       fetch.Fetch,
		Sandbox:     sb,
		Concurrency: conc,
		WorkDir:     work,
	}
	if v := getenv("WARD_RUNNER_CACHE_DIR"); v != "" {
		if c.Options.CacheDir, err = filepath.Abs(v); err != nil {
			return c, fmt.Errorf("WARD_RUNNER_CACHE_DIR: %w", err)
		}
	}
	return c, nil
}

// resolveSandbox returns the sandbox kind for the setting of WARD_RUNNER_SANDBOX. The default and auto mean gvisor, the
// default minimum isolation of a repository, so a runner without runsc fails at start. Auto never falls back to a container:
// a weaker sandbox needs the explicit setting container. runsc is the result of gvisor.Available.
func resolveSandbox(setting string, runsc error) (runnerproto.Isolation, error) {
	switch setting {
	case "", "auto", "gvisor":
		if runsc != nil {
			return "", fmt.Errorf("WARD_RUNNER_SANDBOX=%s needs runsc: %w; install it, or set WARD_RUNNER_SANDBOX=container for a repository that allows it", cmpOr(setting, "auto"), runsc)
		}
		return runnerproto.IsolationGVisor, nil
	case "container":
		return runnerproto.IsolationContainer, nil
	}
	return "", fmt.Errorf("WARD_RUNNER_SANDBOX must be auto, gvisor or container, not %q", setting)
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func newSandbox(kind runnerproto.Isolation, cc container.Config) (sandbox.Sandbox, error) {
	if kind == runnerproto.IsolationGVisor {
		return gvisor.New(cc)
	}
	return container.New(cc)
}

func readText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("file is unreadable")
	}
	raw = bytes.TrimRight(raw, "\r\n")
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "", errors.New("file is empty")
	}
	return string(raw), nil
}
