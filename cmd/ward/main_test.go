package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	metasqlite "github.com/majorcontext/motte/meta/sqlite"
	"github.com/majorcontext/motte/testserver"
)

// askpassBin answers git like the ward binary does, but starts at once: compiling ward for a test costs more than the tests.
var askpassBin string

// bareTemplate is a bare repository that each remote copies, and metaTemplate is a migrated database of motte that each
// motte copies. Both are made once, because each test would otherwise start git or run the migrations for them.
var bareTemplate, workTemplate, metaTemplate string

// seededTB gives the first directory that testserver asks for the migrated database of motte.
type seededTB struct {
	testing.TB
	seeded bool
}

func (s *seededTB) TempDir() string {
	d := s.TB.TempDir()
	if !s.seeded {
		s.seeded = true
		b, err := os.ReadFile(metaTemplate)
		if err != nil || os.WriteFile(filepath.Join(d, "meta.db"), b, 0o644) != nil {
			s.Fatal("copy the database template", err)
		}
	}
	return d
}

// sharedMotte is the motte of the tests that need no mirror.
var sharedMotte *testserver.Server

// mainTB lets testserver start a server for the whole package: it has no test to hold the cleanup.
type mainTB struct {
	testing.TB
	dir string
}

func (mainTB) Helper() {}

func (b mainTB) TempDir() string {
	d, err := os.MkdirTemp(b.dir, "motte")
	if err != nil {
		panic(err)
	}
	return d
}

func (mainTB) Cleanup(func()) {}

func (mainTB) Fatal(args ...any) { panic(fmt.Sprint(args...)) }

const askpassScript = `#!/bin/sh
case "$1" in
Username*) echo "$WARD_ASKPASS_USER" ;;
*) IFS= read -r secret <"$WARD_ASKPASS_FILE"; echo "$secret" ;;
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
	for _, args := range [][]string{{"init", "--bare", "-q", "--template=", bareTemplate}, {"-C", bareTemplate, "config", "http.receivepack", "true"}, {"init", "-q", "--template=", workTemplate}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			fmt.Fprintln(os.Stderr, "git", args, err, string(out))
			os.Exit(1)
		}
	}
	if err := os.Mkdir(filepath.Join(bareTemplate, "hooks"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	metaTemplate = filepath.Join(dir, "meta.tmpl")
	db, err := metasqlite.Open(context.Background(), metaTemplate)
	if err != nil || db.Close() != nil {
		fmt.Fprintln(os.Stderr, "migrate the database template", err)
		os.Exit(1)
	}
	sharedMotte = testserver.StartLocal(mainTB{dir: dir})
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
