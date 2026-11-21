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
	Listen      string
	Options     Options
	SandboxKind string
	// Pull is true when the runner polls ward for leases and serves no hook.
	Pull bool
}

// Load reads the configuration from getenv and builds the runner options. SandboxKind is gvisor, container or auto.
// With auto the runner uses gVisor when runsc is on the path and a plain container otherwise.
func Load(getenv func(string) string) (Config, error) {
	c := Config{Listen: ":8081"}
	if v := getenv("WARD_RUNNER_LISTEN"); v != "" {
		c.Listen = v
	}
	switch getenv("WARD_RUNNER_MODE") {
	case "", "push":
	case "pull":
		c.Pull = true
	default:
		return c, errors.New("WARD_RUNNER_MODE must be push or pull")
	}
	required := []string{"WARD_RUNNER_WARD_URL", "WARD_RUNNER_KID", "WARD_RUNNER_KEY_FILE", "WARD_RUNNER_MOTTE_URL", "WARD_RUNNER_MOTTE_GRANT_FILE", "WARD_RUNNER_IMAGE"}
	if !c.Pull {
		required = append(required, "WARD_RUNNER_SECRET_FILE")
	}
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
	secretFile, grantFile, kid := getenv("WARD_RUNNER_SECRET_FILE"), getenv("WARD_RUNNER_MOTTE_GRANT_FILE"), getenv("WARD_RUNNER_KID")
	if !c.Pull {
		if _, err := readText(secretFile); err != nil {
			return c, fmt.Errorf("WARD_RUNNER_SECRET_FILE: %w", err)
		}
	}
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
	if c.SandboxKind = getenv("WARD_RUNNER_SANDBOX"); c.SandboxKind == "" {
		c.SandboxKind = "auto"
	}
	sb, err := newSandbox(&c.SandboxKind, cc)
	if err != nil {
		return c, err
	}
	fetch := MotteFetcher{URL: getenv("WARD_RUNNER_MOTTE_URL"), Grant: func(context.Context) (string, error) { return readText(grantFile) }}
	c.Options = Options{
		WardURL: getenv("WARD_RUNNER_WARD_URL"),
		Secret: func() ([]byte, error) {
			s, err := readText(secretFile)
			return []byte(s), err
		},
		Token: func(repo string, exp time.Time) (string, error) {
			return runnerproto.MintCheckToken(kid, ed25519.PrivateKey(key), repo, "ward-runner", exp)
		},
		RunnerToken: func(exp time.Time) (string, error) {
			return runnerproto.MintRunnerToken(kid, ed25519.PrivateKey(key), "ward-runner", exp)
		},
		Isolation:   runnerproto.Isolation(c.SandboxKind),
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

func newSandbox(kind *string, cc container.Config) (sandbox.Sandbox, error) {
	switch *kind {
	case "auto":
		if gvisor.Available() == nil {
			*kind = "gvisor"
		} else {
			*kind = "container"
		}
		return newSandbox(kind, cc)
	case "gvisor":
		return gvisor.New(cc)
	case "container":
		return container.New(cc)
	}
	return nil, fmt.Errorf("WARD_RUNNER_SANDBOX must be auto, gvisor or container, not %q", *kind)
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
