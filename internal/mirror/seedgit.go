package mirror

import (
	"context"
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

// newObjects counts the objects and the packed bytes that tip reaches and no commit of not reaches.
func (g *gitRunner) newObjects(ctx context.Context, clone, tip string, not []string) (int, int64, error) {
	in := revRange(tip, not)
	out, err := g.runIn(ctx, clone, gitCreds{}, in, "rev-list", "--stdin", "--objects", "--count")
	if err != nil {
		return 0, 0, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	out, err = g.runIn(ctx, clone, gitCreds{}, in, "rev-list", "--stdin", "--objects", "--disk-usage")
	if err != nil {
		return 0, 0, err
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return n, size, nil
}

// firstParents lists the commits of the first-parent history of tip that no commit of not reaches, oldest first.
func (g *gitRunner) firstParents(ctx context.Context, clone, tip string, not []string) ([]string, error) {
	out, err := g.runIn(ctx, clone, gitCreds{}, revRange(tip, not), "rev-list", "--stdin", "--first-parent", "--reverse")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// peel returns the commit that sha names, or empty when sha is no commit and no tag of a commit.
func (g *gitRunner) peel(ctx context.Context, clone, sha string) string {
	out, err := g.run(ctx, clone, gitCreds{}, "rev-parse", "--verify", "-q", sha+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
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
