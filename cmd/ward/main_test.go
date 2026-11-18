package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// askpassBin answers git like the ward binary does, but starts at once: compiling ward for a test costs more than the tests.
var askpassBin string

// bareTemplate is a bare repository that each remote copies, and gitExecPath is the exec path of git. Both are made once,
// because each test would otherwise start git for them.
var bareTemplate, workTemplate, gitExecPath string

const askpassScript = `#!/bin/sh
case "$1" in
Username*) echo "$WARD_ASKPASS_USER" ;;
*) cat "$WARD_ASKPASS_FILE" ;;
esac
`

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ward-askpass")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bareTemplate, workTemplate = filepath.Join(dir, "bare.git"), filepath.Join(dir, "work")
	if err := os.Mkdir(workTemplate, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, args := range [][]string{{"init", "--bare", "-q", bareTemplate}, {"-C", bareTemplate, "config", "http.receivepack", "true"}, {"init", "-q", workTemplate}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			fmt.Fprintln(os.Stderr, "git", args, err, string(out))
			os.Exit(1)
		}
	}
	path, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gitExecPath = strings.TrimSpace(string(path))
	askpassBin = filepath.Join(dir, "askpass")
	if err := os.WriteFile(askpassBin, []byte(askpassScript), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Each test spawns many git processes. A crowd of them makes every test slow, so few run at once.
	_ = flag.Set("test.parallel", strconv.Itoa(min(runtime.NumCPU(), 8)))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
