// Command release builds and publishes the ward command-line binary of a release tag.
// Use: go run ./internal/release/cmd/release build|publish -h
package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/majorcontext/ward/internal/release/publish"
	dirpub "github.com/majorcontext/ward/internal/release/publish/dir"
	"github.com/majorcontext/ward/internal/release/publish/github"
)

var arches = []string{"amd64", "arm64"}

func main() {
	if err := dispatch(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		if !errors.Is(err, flag.ErrHelp) {
			os.Exit(1)
		}
	}
}

func dispatch(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: release build|publish [flags]")
	}
	switch args[0] {
	case "build":
		return buildCmd(args[1:], out)
	case "publish":
		return publishCmd(args[1:], out)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

type runner struct {
	dryRun bool
	out    io.Writer
}

func (r runner) run(env []string, name string, args ...string) error {
	fmt.Fprintln(r.out, "+", strings.Join(append(slices.Clone(env), name), " "), strings.Join(args, " "))
	if r.dryRun {
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// buildCmd cross-compiles cmd/ward for each arch with the tag as its version, and writes the binaries and SHA256SUMS to -out.
func buildCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	outDir := fs.String("out", "", "directory for ward-linux-ARCH and SHA256SUMS")
	version := fs.String("version", "", "the release tag, which `ward version` prints")
	dry := fs.Bool("dry-run", false, "print the commands and run none")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outDir == "" || !strings.HasPrefix(*version, "v") {
		return errors.New("build needs -out and a -version that starts with v")
	}
	r := runner{dryRun: *dry, out: out}
	var names []string
	for _, arch := range arches {
		name := "ward-linux-" + arch
		names = append(names, name)
		if err := r.run([]string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + arch}, "go", "build", "-trimpath",
			"-ldflags", "-s -w -X main.version="+*version, "-o", filepath.Join(*outDir, name), "./cmd/ward"); err != nil {
			return err
		}
	}
	if *dry {
		return nil
	}
	return writeSums(*outDir, names)
}

func writeSums(dir string, names []string) error {
	var sums strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
	}
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o644)
}

// publishCmd sends every file of -dir to -dest. A dest is "github", which attaches the files to the release of -tag through
// the gh CLI (GH_TOKEN and GH_REPO select the account and repository), or "dir=PATH", which copies the files to PATH.
func publishCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory of artifacts")
	dest := fs.String("dest", "github", "github or dir=PATH")
	tag := fs.String("tag", "", "release tag; required for github")
	dry := fs.Bool("dry-run", false, "print the commands and run none")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("publish needs -dir")
	}
	files, err := filepath.Glob(filepath.Join(*dir, "*"))
	if err != nil || len(files) == 0 {
		return fmt.Errorf("no files in %s", *dir)
	}
	r := runner{dryRun: *dry, out: out}
	var p publish.Publisher
	switch {
	case *dest == "github":
		p = github.Publisher{Run: func(args ...string) error { return r.run(nil, "gh", args...) }}
	case strings.HasPrefix(*dest, "dir="):
		if *dry {
			fmt.Fprintln(out, "+ copy", len(files), "files to", strings.TrimPrefix(*dest, "dir="))
			return nil
		}
		p = dirpub.Publisher{To: strings.TrimPrefix(*dest, "dir=")}
	default:
		return fmt.Errorf("unknown dest %q", *dest)
	}
	return p.Publish(*tag, files)
}
