package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/majorcontext/ward/internal/mirror"
)

// askpassBin is the ward binary that git runs for the remote credential. A test binary starts too slowly for that.
var askpassBin string

func TestMain(m *testing.M) {
	if mirror.MaybeAskpass(os.Args[1:], os.Stdout) {
		return
	}
	dir, err := os.MkdirTemp("", "ward-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	askpassBin = filepath.Join(dir, "ward")
	if out, err := exec.Command("go", "build", "-o", askpassBin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build ward: %v\n%s", err, out)
		os.Exit(1)
	}
	// Each test spawns many git processes. A crowd of them makes every test slow, so few run at once.
	_ = flag.Set("test.parallel", "4")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
