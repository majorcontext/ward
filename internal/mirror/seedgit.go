package mirror

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// revRange is the stdin of rev-list: the tip, then each excluded commit.
func revRange(tip string, not []string) string {
	var b strings.Builder
	b.WriteString(tip + "\n")
	for _, n := range not {
		b.WriteString("^" + n + "\n")
	}
	return b.String()
}

// lineCap collects the output of a process and stops it once the output holds more than max lines.
type lineCap struct {
	buf    bytes.Buffer
	lines  int
	max    int
	over   bool
	cancel context.CancelFunc
}

func (w *lineCap) Write(p []byte) (int, error) {
	if w.over {
		return len(p), nil
	}
	if w.lines += bytes.Count(p, []byte("\n")); w.lines > w.max {
		w.over = true
		w.cancel()
		return len(p), nil
	}
	return w.buf.Write(p)
}

// listObjects lists the objects that in selects. The bool is true when more than max objects match.
func (g *gitRunner) listObjects(ctx context.Context, clone, in string, max int) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	w := &lineCap{max: max, cancel: cancel}
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--stdin", "--objects")
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout = clone, g.env(gitCreds{}), strings.NewReader(in), w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil && !w.over {
		return "", false, &gitError{stderr: stderr.String(), err: err, timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	}
	return w.buf.String(), w.over, nil
}

// fitsPush reports whether the objects that tip reaches and no commit of not reaches fit in one push: at most
// maxObjects objects, maxPacked packed bytes and maxInflated bytes of whole objects. It lists the objects once and
// reads both sizes of each object through the sizer process.
func (g *gitRunner) fitsPush(ctx context.Context, sizer *objReader, clone, tip string, not []string, maxObjects int, maxPacked, maxInflated int64) (bool, error) {
	list, over, err := g.listObjects(ctx, clone, revRange(tip, not), maxObjects)
	if err != nil || over {
		return false, err
	}
	var shas []string
	for line := range strings.Lines(list) {
		sha, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		shas = append(shas, sha)
	}
	sizes, err := sizer.ask(shas)
	if err != nil {
		return false, err
	}
	var inflated, packed int64
	for _, line := range sizes {
		var n, d int64
		if f := strings.Fields(line); len(f) == 4 {
			n, _ = strconv.ParseInt(f[2], 10, 64)
			d, _ = strconv.ParseInt(f[3], 10, 64)
		}
		inflated, packed = inflated+n, packed+d
	}
	return packed <= maxPacked && inflated <= maxInflated, nil
}

// firstParents lists the commits of the first-parent history of tip that no commit of not reaches, oldest first.
func (g *gitRunner) firstParents(ctx context.Context, clone, tip string, not []string) ([]string, error) {
	out, err := g.runIn(ctx, clone, gitCreds{}, revRange(tip, not), "rev-list", "--stdin", "--first-parent", "--reverse")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

type seedTip struct {
	Git  string
	Tip  string
	Time int64
}

// seedTips reads the local refs that a seed fetched, with the commit time of each tip. A tip that is no commit has time 0.
func (g *gitRunner) seedTips(ctx context.Context, clone string) ([]seedTip, error) {
	out, err := g.run(ctx, clone, gitCreds{}, "for-each-ref", "--format=%(refname)\t%(objectname)\t%(*committerdate:unix)\t%(committerdate:unix)", "refs/mirror/")
	if err != nil {
		return nil, err
	}
	var tips []seedTip
	for line := range strings.Lines(out) {
		f := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(f) != 4 {
			continue
		}
		t := seedTip{Git: strings.TrimPrefix(f[0], "refs/mirror/"), Tip: f[1]}
		for _, s := range f[2:] {
			if s != "" {
				t.Time, _ = strconv.ParseInt(s, 10, 64)
				break
			}
		}
		tips = append(tips, t)
	}
	return tips, nil
}
