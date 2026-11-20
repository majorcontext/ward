package mirror

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strings"
)

// objReader is one long-lived git cat-file --batch-check process over a clone. It answers many object questions
// with one process. Open it after the last fetch: it reads the object store of that moment.
type objReader struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	cancel context.CancelFunc
}

func (g *gitRunner) openReader(ctx context.Context, clone string) (*objReader, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize) %(objectsize:disk)")
	cmd.Dir, cmd.Env = clone, g.env(gitCreds{})
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	return &objReader{cmd: cmd, in: in, out: bufio.NewReader(out), cancel: cancel}, nil
}

// ask sends one object name or revision per element and returns one answer line per element, in order.
func (r *objReader) ask(specs []string) ([]string, error) {
	werr := make(chan error, 1)
	go func() {
		w := bufio.NewWriter(r.in)
		for _, s := range specs {
			w.WriteString(s + "\n")
		}
		werr <- w.Flush()
	}()
	lines := make([]string, 0, len(specs))
	for range specs {
		line, err := r.out.ReadString('\n')
		if err != nil {
			<-werr
			return nil, err
		}
		lines = append(lines, strings.TrimRight(line, "\n"))
	}
	return lines, <-werr
}

func (r *objReader) close() {
	r.in.Close()
	r.cancel()
	_ = r.cmd.Wait()
}

// commitsOf returns the commit that each revision names, or empty where it names no commit or the reader fails.
func (r *objReader) commitsOf(revs []string) []string {
	got := make([]string, len(revs))
	specs := make([]string, len(revs))
	for i, s := range revs {
		specs[i] = s + "^{commit}"
	}
	lines, err := r.ask(specs)
	if err != nil {
		return got
	}
	for i, l := range lines {
		if f := strings.Fields(l); len(f) > 1 && f[1] == "commit" {
			got[i] = f[0]
		}
	}
	return got
}
