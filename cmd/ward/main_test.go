package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// askpassBin answers git like the ward binary does, but starts at once: compiling ward for a test costs more than the tests.
var askpassBin string

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
