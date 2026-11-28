package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// A help flag must print usage to stdout and exit 0; a bad flag must still exit 2 with the usage on stderr.
func TestHelpExitsZeroAndBadFlagExitsTwo(t *testing.T) {
	t.Parallel()
	env := func(string) string { return "" }
	cmds := map[string]func(args []string, out, errb io.Writer) int{
		"land": func(a []string, o, e io.Writer) int { return runLand(context.Background(), a, env, time.Now, o, e) },
		"token": func(a []string, o, e io.Writer) int {
			return runToken(context.Background(), append([]string{"create"}, a...), env, time.Now, o, e)
		},
		"login":  func(a []string, o, e io.Writer) int { return runLogin(context.Background(), a, env, time.Now, o, e) },
		"check":  func(a []string, o, e io.Writer) int { return runLocalCheck(context.Background(), a, time.Now, o, e) },
		"runner": func(a []string, o, e io.Writer) int { return runRunnerInstall(a, o, e) },
	}
	for name, run := range cmds {
		for _, h := range []string{"-h", "--help"} {
			var out, errb bytes.Buffer
			if code := run([]string{h}, &out, &errb); code != 0 || !strings.Contains(out.String(), "usage") || errb.Len() != 0 {
				t.Errorf("%s %s: code %d, stdout %q, stderr %q", name, h, code, out.String(), errb.String())
			}
		}
		var out, errb bytes.Buffer
		if code := run([]string{"--no-such-flag"}, &out, &errb); code != 2 || out.Len() != 0 || !strings.Contains(errb.String(), "usage") {
			t.Errorf("%s bad flag: code %d, stdout %q, stderr %q", name, code, out.String(), errb.String())
		}
	}
}

func TestRootHelpCoversVersionAndRunner(t *testing.T) {
	t.Parallel()
	for _, a := range [][]string{{"-h"}, {"--help"}, {"help"}, {"version", "--help"}, {"version", "-h"}, {"runner", "--help"}} {
		if !wantsHelp(a) {
			t.Errorf("wantsHelp(%v) = false", a)
		}
	}
	for _, a := range [][]string{nil, {"version"}, {"runner"}, {"land", "x"}} {
		if wantsHelp(a) {
			t.Errorf("wantsHelp(%v) = true", a)
		}
	}
}
