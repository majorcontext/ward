package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A timed-out check must not leave the children of its shell running.
func TestLocalCheckTimeoutKillsChildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	dir := writeChecks(t, `{"checks":[{"name":"hang","timeout_seconds":1,"run":["sh","-c","sleep 300 & echo $! > `+pidFile+`; wait"]}]}`)
	var out, errb bytes.Buffer
	if code := runLocalCheck(t.Context(), []string{"--dir", dir}, time.Now, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out.String())
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(b))
	t.Cleanup(func() { exec.Command("kill", pid).Run() })
	for range 200 {
		st, _ := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
		if s := strings.TrimSpace(string(st)); s == "" || s[0] == 'Z' {
			return
		}
	}
	t.Fatalf("child %s of a timed-out check is still running", pid)
}
