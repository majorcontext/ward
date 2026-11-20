package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/majorcontext/ward/internal/check"
)

const localCheckHelp = `usage: ward check [--dir PATH] [--only NAME]

Runs the checks of .ward/checks.json from the working tree, one after another, and prints a summary.
The run is local and not authoritative. It never contacts a ward server and posts no result.
A result that counts for a land is keyed by (repo, tree, check hash, env) and only a runner with a check token posts it.
A pass here does not count for a land.
Exit 0: every check passed. Exit 1: a check failed. Exit 2: usage error or an unreadable check file.`

type localResult struct {
	name string
	took time.Duration
	err  error
}

// runLocalCheck is `ward check [--dir PATH] [--only NAME]`. It returns the exit code.
func runLocalCheck(ctx context.Context, args []string, now func() time.Time, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", ".", "directory that holds .ward/checks.json")
	only := fs.String("only", "", "run only the check with this name")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, localCheckHelp)
		return 2
	}
	data, err := os.ReadFile(filepath.Join(*dir, check.FilePath))
	if err != nil {
		fmt.Fprintln(stderr, "ward check:", err)
		return 2
	}
	checks, err := check.ParseFile(data)
	if err != nil {
		fmt.Fprintln(stderr, "ward check:", err)
		return 2
	}
	if *only != "" {
		i := slices.IndexFunc(checks, func(c check.Check) bool { return c.Name == *only })
		if i < 0 {
			fmt.Fprintf(stderr, "ward check: no check named %q\n", *only)
			return 2
		}
		checks = checks[i : i+1]
	}
	var results []localResult
	for _, c := range checks {
		fmt.Fprintf(stdout, "== %s\n", c.Name)
		start := now()
		err := runLocalOne(ctx, *dir, c, stdout, stderr)
		results = append(results, localResult{c.Name, now().Sub(start), err})
	}
	failed := 0
	fmt.Fprintln(stdout, "== summary")
	for _, r := range results {
		if r.err != nil {
			failed++
			fmt.Fprintf(stdout, "fail  %s  %s  %v\n", r.name, r.took.Round(time.Millisecond), r.err)
			continue
		}
		fmt.Fprintf(stdout, "pass  %s  %s\n", r.name, r.took.Round(time.Millisecond))
	}
	fmt.Fprintln(stdout, "local run: nothing was posted to a ward server, and this does not count for a land")
	if failed > 0 {
		return 1
	}
	return 0
}

func runLocalOne(ctx context.Context, dir string, c check.Check, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Run[0], c.Run[1:]...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %ds", c.TimeoutSeconds)
	}
	return err
}
