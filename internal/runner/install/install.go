// Package install prepares a Linux host to run `ward runner` in pull mode. It checks what the host must provide, writes a
// systemd unit and an environment file, and reports what is missing. It knows no cloud provider: it reads only the host.
package install

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/majorcontext/ward/internal/config"
	"github.com/majorcontext/ward/internal/runner/warm"
)

// Options say how the runner is set up. WardURL, Kid, MotteURL, GrantFile and Image are required.
type Options struct {
	WardURL     string
	Kid         string
	KeyFile     string
	MotteURL    string
	GrantFile   string
	Image       string
	Engine      string
	Sandbox     string
	CacheDir    string
	WorkDir     string
	CheckoutDir string
	User        string
	Concurrency int
	// Binary is the path of the ward binary that the unit runs.
	Binary string
	// Root is a prefix for every file that Run writes. The default is "/".
	Root string
}

// Host is what Run asks the machine. A test replaces it.
type Host struct {
	LookPath   func(name string) (string, error)
	KVM        func(user string) error
	FSType     func(path string) (string, error)
	LookupUser func(name string) (uid, gid int, err error)
	HasSystemd func() bool
	// GitVersion returns the output of git --version.
	GitVersion func() (string, error)
}

// notLocalDisk are the file systems that a durable cache must not live on: a network hit is slower than a build, and tmpfs
// loses the cache at a reboot.
var notLocalDisk = map[string]bool{"tmpfs": true, "nfs": true, "cifs": true, "smb": true, "fuse": true, "9p": true, "ceph": true, "lustre": true}

func (o *Options) fill() error {
	for name, v := range map[string]string{"--ward-url": o.WardURL, "--kid": o.Kid, "--motte-url": o.MotteURL, "--motte-grant-file": o.GrantFile, "--image": o.Image} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&o.Root, "/")
	def(&o.Engine, "docker")
	def(&o.Sandbox, "gvisor")
	def(&o.User, "ward-runner")
	def(&o.KeyFile, "/etc/ward-runner/check-key")
	def(&o.CacheDir, "/var/lib/ward-runner/cache")
	def(&o.WorkDir, "/var/lib/ward-runner/work")
	def(&o.CheckoutDir, "/var/lib/ward-runner/checkouts")
	def(&o.Binary, "/usr/local/bin/ward")
	if o.Concurrency < 1 {
		o.Concurrency = 1
	}
	for _, v := range []string{o.WardURL, o.Kid, o.KeyFile, o.MotteURL, o.GrantFile, o.Image, o.Engine, o.Sandbox, o.CacheDir, o.WorkDir, o.CheckoutDir, o.User, o.Binary} {
		if strings.ContainsAny(v, " \t\r\n\"'\\") {
			return fmt.Errorf("value %q holds a space, a quote or a backslash", v)
		}
	}
	return nil
}

// Run checks the host, writes the files and returns what is missing. It prints a line for each step to out.
func Run(o Options, h Host, out io.Writer) (missing []string, err error) {
	if err := o.fill(); err != nil {
		return nil, err
	}
	miss := func(format string, a ...any) {
		m := fmt.Sprintf(format, a...)
		missing = append(missing, m)
		_, _ = fmt.Fprintln(out, "missing:", m)
	}
	ok := func(format string, a ...any) { _, _ = fmt.Fprintln(out, "ok:", fmt.Sprintf(format, a...)) }

	if p, err := h.LookPath(o.Engine); err != nil {
		miss("container engine %q is not on the path; install docker or podman, or pass --engine", o.Engine)
	} else {
		ok("container engine %s", p)
	}
	if p, err := h.LookPath("runsc"); err != nil {
		miss("runsc (gVisor) is not on the path; register it as a runtime of the engine")
	} else {
		ok("runsc %s", p)
	}
	if p, err := h.LookPath("git"); err != nil {
		miss("git is not on the path; the runner needs it for the warm checkouts")
	} else if out, err := h.GitVersion(); err != nil {
		miss("git --version failed: %v", err)
	} else if err := warm.CheckGitVersion(out); err != nil {
		miss("%v", err)
	} else {
		ok("git %s", p)
	}
	if err := h.KVM(o.User); err != nil {
		miss("the KVM platform of runsc: %v", err)
	} else {
		ok("/dev/kvm is usable")
	}
	if !h.HasSystemd() {
		miss("systemd is not running; the unit is written but nothing starts it")
	}
	uid, gid, uerr := h.LookupUser(o.User)
	if uerr != nil {
		miss("user %q does not exist; create it and add it to the group of the engine", o.User)
	}
	if _, err := os.Stat(o.GrantFile); err != nil {
		miss("grant file %s is missing; it holds a motte read grant", o.GrantFile)
	}
	if err := prepareDirs(o, h, uid, gid, uerr == nil, miss, ok); err != nil {
		return missing, err
	}
	pub, err := ensureKey(o, uid, gid, uerr == nil)
	if err != nil {
		return missing, err
	}
	if err := write(o, "/etc/ward-runner/runner.env", envFile, 0o640, uid, gid, uerr == nil); err != nil {
		return missing, err
	}
	if err := write(o, "/etc/systemd/system/ward-runner.service", unitFile, 0o644, 0, 0, false); err != nil {
		return missing, err
	}
	_, _ = fmt.Fprintf(out, "wrote %s and %s\n", o.path("/etc/ward-runner/runner.env"), o.path("/etc/systemd/system/ward-runner.service"))
	_, _ = fmt.Fprintf(out, "register this runner in ward:\n  WARD_TRUSTED_KEYS entry: {\"kid\":%q,\"key\":%q,\"scopes\":[\"runner\",\"check\"]}\n  WARD_RUNNERS entry of each repository: add %q to \"kids\"\n", o.Kid, pub, o.Kid)
	_, _ = fmt.Fprintln(out, "then run: systemctl daemon-reload && systemctl enable --now ward-runner")
	return missing, nil
}

func (o Options) path(p string) string { return filepath.Join(o.Root, p) }

func prepareDirs(o Options, h Host, uid, gid int, chown bool, miss func(string, ...any), ok func(string, ...any)) error {
	for _, d := range []string{o.CacheDir, o.WorkDir, o.CheckoutDir} {
		dir := filepath.Join(o.Root, d)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o750); err != nil {
			return err
		}
		if chown {
			_ = os.Chown(dir, uid, gid)
		}
	}
	for _, d := range []string{o.CacheDir, o.WorkDir, o.CheckoutDir} {
		fs, err := h.FSType(filepath.Join(o.Root, d))
		switch {
		case err != nil:
			miss("the file system of %s is unknown: %v", d, err)
		case notLocalDisk[fs]:
			miss("%s is on %s; use a directory on local disk", d, fs)
		default:
			ok("%s is on local disk (%s)", d, fs)
		}
	}
	return nil
}

// ensureKey creates the check key when its file is missing and returns the public key in standard base64.
func ensureKey(o Options, uid, gid int, chown bool) (string, error) {
	path := o.path(o.KeyFile)
	_, statErr := os.Stat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
			return "", err
		}
	}
	if chown {
		_ = os.Chown(path, uid, gid)
	}
	key, err := config.ReadPrivateKey(path)
	if err != nil {
		return "", fmt.Errorf("key file %s: %w", o.KeyFile, err)
	}
	return base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), nil
}

var (
	envFile  = template.Must(template.New("env").Parse(envText))
	unitFile = template.Must(template.New("unit").Parse(unitText))
)

const envText = `WARD_RUNNER_MODE=pull
WARD_RUNNER_WARD_URL={{.WardURL}}
WARD_RUNNER_KID={{.Kid}}
WARD_RUNNER_KEY_FILE={{.KeyFile}}
WARD_RUNNER_MOTTE_URL={{.MotteURL}}
WARD_RUNNER_MOTTE_GRANT_FILE={{.GrantFile}}
WARD_RUNNER_IMAGE={{.Image}}
WARD_RUNNER_ENGINE={{.Engine}}
WARD_RUNNER_SANDBOX={{.Sandbox}}
WARD_RUNNER_CONCURRENCY={{.Concurrency}}
WARD_RUNNER_WORK_DIR={{.WorkDir}}
WARD_RUNNER_CACHE_DIR={{.CacheDir}}
WARD_RUNNER_CHECKOUT_DIR={{.CheckoutDir}}
`

const unitText = `[Unit]
Description=ward check runner
After=network-online.target
Wants=network-online.target

[Service]
User={{.User}}
EnvironmentFile=/etc/ward-runner/runner.env
ExecStart={{.Binary}} runner
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

func write(o Options, path string, t *template.Template, mode os.FileMode, uid, gid int, chown bool) error {
	p := o.path(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := t.Execute(f, o); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if chown {
		_ = os.Chown(p, uid, gid)
	}
	return nil
}
