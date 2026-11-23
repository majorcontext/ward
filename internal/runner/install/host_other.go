//go:build !linux

package install

import (
	"errors"
	"os/exec"
)

var errNotLinux = errors.New("this host is not Linux")

// SystemHost reports every Linux-only fact as missing: the runner supports Linux hosts only.
func SystemHost() Host {
	return Host{
		LookPath:   exec.LookPath,
		KVM:        func(string) error { return errNotLinux },
		FSType:     func(string) (string, error) { return "", errNotLinux },
		LookupUser: func(string) (int, int, error) { return 0, 0, errNotLinux },
		HasSystemd: func() bool { return false },
		GitVersion: func() (string, error) {
			out, err := exec.Command("git", "--version").Output()
			return string(out), err
		},
	}
}
