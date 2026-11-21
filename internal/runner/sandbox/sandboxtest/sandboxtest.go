// Package sandboxtest is the conformance suite of sandbox.Sandbox.
package sandboxtest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/runner/sandbox"
)

// Impl describes the implementation under test. Commands use only sh, cat, sleep and test.
type Impl struct {
	New func(t *testing.T) sandbox.Sandbox
	// Isolated is true when the implementation hides the host file system outside Dir from the check.
	Isolated bool
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	hit chan struct{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if b.hit != nil && strings.Contains(b.buf.String(), "started") {
		close(b.hit)
		b.hit = nil
	}
	return n, err
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func tree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("tree-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func run(t *testing.T, s sandbox.Sandbox, dir string, timeout time.Duration, argv ...string) (sandbox.Outcome, string) {
	t.Helper()
	var out syncBuffer
	o, err := s.Run(context.Background(), sandbox.Spec{Dir: dir, Argv: argv, Timeout: timeout, Output: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return o, out.String()
}

// Run runs the suite against the implementation that impl.New returns.
func Run(t *testing.T, impl Impl) {
	t.Helper()
	const long = time.Minute
	t.Run("exit code", func(t *testing.T) {
		s := impl.New(t)
		for _, code := range []string{"0", "3"} {
			o, _ := run(t, s, tree(t), long, "sh", "-c", "exit "+code)
			if want := map[string]int{"0": 0, "3": 3}[code]; o.ExitCode != want || o.TimedOut {
				t.Errorf("exit %s: got %+v", code, o)
			}
		}
	})
	t.Run("missing command", func(t *testing.T) {
		if o, _ := run(t, impl.New(t), tree(t), long, "no-such-command-ward"); o.ExitCode != 127 {
			t.Errorf("got %+v, want exit code 127", o)
		}
	})
	t.Run("working directory and output", func(t *testing.T) {
		o, out := run(t, impl.New(t), tree(t), long, "sh", "-c", "cat marker; echo err >&2")
		if o.ExitCode != 0 || !strings.Contains(out, "tree-content") || !strings.Contains(out, "err") {
			t.Errorf("got %+v and output %q", o, out)
		}
	})
	t.Run("writes reach the tree", func(t *testing.T) {
		dir := tree(t)
		if o, _ := run(t, impl.New(t), dir, long, "sh", "-c", "echo built > artifact"); o.ExitCode != 0 {
			t.Fatalf("got %+v", o)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "artifact")); err != nil || strings.TrimSpace(string(got)) != "built" {
			t.Errorf("artifact: %q, %v", got, err)
		}
	})
	t.Run("timeout kills the group", func(t *testing.T) {
		done := make(chan sandbox.Outcome, 1)
		go func() {
			o, _ := run(t, impl.New(t), tree(t), time.Second, "sh", "-c", "sleep 300 & sleep 300")
			done <- o
		}()
		select {
		case o := <-done:
			if !o.TimedOut {
				t.Errorf("got %+v, want TimedOut", o)
			}
		case <-time.After(60 * time.Second): //lint:allow wallclock a stuck sandbox is the failure under test
			t.Fatal("Run did not return after the timeout")
		}
	})
	t.Run("cancel ends the run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := &syncBuffer{hit: make(chan struct{})}
		hit := out.hit
		errc := make(chan error, 1)
		go func() {
			_, err := impl.New(t).Run(ctx, sandbox.Spec{Dir: tree(t), Argv: []string{"sh", "-c", "echo started; sleep 300"}, Timeout: long, Output: out})
			errc <- err
		}()
		select {
		case <-hit:
		case <-time.After(60 * time.Second): //lint:allow wallclock a sandbox that never starts is the failure under test
			t.Fatal("the command did not start")
		}
		cancel()
		select {
		case err := <-errc:
			if err == nil {
				t.Error("Run returned no error after cancel")
			}
		case <-time.After(60 * time.Second): //lint:allow wallclock a stuck sandbox is the failure under test
			t.Fatal("Run did not return after cancel")
		}
	})
	if impl.Isolated {
		t.Run("host is hidden", func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "host-secret")
			if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if o, out := run(t, impl.New(t), tree(t), long, "test", "!", "-e", outside); o.ExitCode != 0 {
				t.Errorf("the check saw a host file: %+v %q", o, out)
			}
		})
	}
}
