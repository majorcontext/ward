package install_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/ward/internal/runner/install"
)

func host(missing ...string) install.Host {
	gone := map[string]bool{}
	for _, m := range missing {
		gone[m] = true
	}
	err := func(name string) error {
		if gone[name] {
			return errors.New(name + " missing")
		}
		return nil
	}
	return install.Host{
		LookPath: func(n string) (string, error) { return "/usr/bin/" + n, err(n) },
		KVM:      func(string) error { return err("kvm") },
		FSType: func(string) (string, error) {
			if gone["disk"] {
				return "nfs", nil
			}
			return "ext4", nil
		},
		LookupUser: func(string) (int, int, error) { return os.Getuid(), os.Getgid(), err("user") },
		HasSystemd: func() bool { return !gone["systemd"] },
	}
}

func TestInstallWritesTheUnitAndNamesWhatTheHostLacks(t *testing.T) {
	root, grant := t.TempDir(), filepath.Join(t.TempDir(), "grant")
	if err := os.WriteFile(grant, []byte("g"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := install.Options{WardURL: "https://ward.example.org", Kid: "metal-1", MotteURL: "https://motte.example.org", GrantFile: grant, Image: "registry.example.org/env@sha256:1", Root: root}
	var out bytes.Buffer
	missing, err := install.Run(opts, host(), &out)
	if err != nil || len(missing) != 0 {
		t.Fatalf("complete host: missing %v, error %v", missing, err)
	}
	env, _ := os.ReadFile(filepath.Join(root, "etc/ward-runner/runner.env"))
	unit, _ := os.ReadFile(filepath.Join(root, "etc/systemd/system/ward-runner.service"))
	for _, want := range []string{"WARD_RUNNER_MODE=pull", "WARD_RUNNER_KID=metal-1", "WARD_RUNNER_SANDBOX=gvisor", "WARD_RUNNER_CACHE_DIR=/var/lib/ward-runner/cache", "WARD_RUNNER_CHECKOUT_DIR=/var/lib/ward-runner/checkouts"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env file lacks %s:\n%s", want, env)
		}
	}
	if !strings.Contains(string(unit), "ExecStart=/usr/local/bin/ward runner") || !strings.Contains(out.String(), `"scopes":["runner","check"]`) {
		t.Errorf("unit:\n%s\noutput:\n%s", unit, out.String())
	}
	if st, err := os.Stat(filepath.Join(root, "etc/ward-runner/check-key")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("check key: %v %v", st, err)
	}

	opts.GrantFile = filepath.Join(root, "no-grant")
	out.Reset()
	missing, err = install.Run(opts, host("docker", "runsc", "git", "kvm", "disk", "user", "systemd"), &out)
	if err != nil || len(missing) != 10 {
		t.Errorf("empty host: %d missing, error %v:\n%s", len(missing), err, out.String())
	}
}
