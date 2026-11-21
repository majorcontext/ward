package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/majorcontext/ward/internal/runner/install"
)

// runRunnerInstall is `ward runner install`. It returns 0 when the host is ready, 1 when something is missing and 2 on a usage or write error.
func runRunnerInstall(args []string, stdout, stderr io.Writer) int {
	var o install.Options
	fs := flag.NewFlagSet("ward runner install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.WardURL, "ward-url", "", "base URL of ward")
	fs.StringVar(&o.Kid, "kid", "", "identity of this runner")
	fs.StringVar(&o.KeyFile, "key-file", "", "Ed25519 key file; created when absent")
	fs.StringVar(&o.MotteURL, "motte-url", "", "base URL of motte")
	fs.StringVar(&o.GrantFile, "motte-grant-file", "", "file with a motte read grant")
	fs.StringVar(&o.Image, "image", "", "OCI image in which checks run")
	fs.StringVar(&o.Engine, "engine", "", "container engine command (default docker)")
	fs.StringVar(&o.Sandbox, "sandbox", "", "gvisor, container or auto (default gvisor)")
	fs.StringVar(&o.CacheDir, "cache-dir", "", "durable cache directory on local disk")
	fs.StringVar(&o.WorkDir, "work-dir", "", "directory of the tree checkouts")
	fs.StringVar(&o.User, "user", "", "service user (default ward-runner)")
	fs.StringVar(&o.Binary, "binary", "", "path of the ward binary (default /usr/local/bin/ward)")
	fs.StringVar(&o.Root, "root", "", "prefix for every written path (default /)")
	fs.IntVar(&o.Concurrency, "concurrency", 1, "most checks at one time")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	missing, err := install.Run(o, install.SystemHost(), stdout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ward runner install:", err)
		return 2
	}
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(stderr, "ward runner install: %d prerequisite(s) missing\n", len(missing))
		return 1
	}
	return 0
}
