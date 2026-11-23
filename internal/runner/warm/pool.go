package warm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// checkout fills dir with the files of commit. It adopts a ready checkout when the pool has one, and moves it from its
// base commit to commit. The pool then gets a new one.
func (f *Fetcher) checkout(ctx context.Context, r *repo, commit, dir string) error {
	from, index := emptyTree, ""
	if s, ok := r.take(); ok {
		if f.adopt(s, dir) {
			from, index = s.base, s.index
		}
		defer func() { _ = os.RemoveAll(s.root) }()
	}
	if index == "" {
		tmp, err := os.MkdirTemp(f.cfg.PoolDir, "index-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		index = filepath.Join(tmp, "index")
	}
	if err := f.update(ctx, r, dir, index, from, commit); err != nil {
		return err
	}
	f.refill(r, commit)
	return nil
}

func (r *repo) take() (slot, bool) {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	if len(r.slots) == 0 {
		return slot{}, false
	}
	s := r.slots[len(r.slots)-1]
	r.slots = r.slots[:len(r.slots)-1]
	return s, true
}

// adopt moves the files of a ready checkout onto the empty directory dir.
func (f *Fetcher) adopt(s slot, dir string) bool {
	if os.Remove(dir) == nil && os.Rename(s.work, dir) == nil {
		return true
	}
	if _, err := os.Stat(dir); err != nil {
		if os.Mkdir(dir, 0o777) == nil {
			_ = os.Chmod(dir, 0o777)
		}
	}
	return false
}

// update moves the files in dir from the tree of from to the tree of to. The index file must hold the tree of from, or
// not exist when from is the empty tree. It refuses a submodule entry.
func (f *Fetcher) update(ctx context.Context, r *repo, dir, index, from, to string) error {
	if from == to {
		return nil
	}
	var scan linkScan
	dctx, cancel := context.WithTimeout(ctx, gitTimeout)
	err := f.git.runTo(dctx, gitOpts{gitDir: r.path}, &scan, "diff-tree", "-r", "-t", "--raw", "--no-abbrev", "-z", "--no-renames", from, to)
	cancel()
	if err != nil {
		return err
	}
	if scan.found {
		return &refusal{"tree holds a submodule entry, which a runner does not check out"}
	}
	if err := removeDirs(dir, &scan); err != nil {
		return err
	}
	if _, err = f.gitRun(ctx, gitTimeout, gitOpts{gitDir: r.path, workTree: dir, index: index, open: true}, "read-tree", "-m", "-u", from, to); err != nil {
		return err
	}
	return makeDirs(dir, &scan)
}

// refill builds ready checkouts at base, in the background, until the pool is full.
func (f *Fetcher) refill(r *repo, base string) {
	r.pmu.Lock()
	need := f.cfg.PoolSize - len(r.slots) - r.building
	if need <= 0 || f.closed() {
		r.pmu.Unlock()
		return
	}
	r.building += need
	r.pmu.Unlock()
	f.mu.Lock()
	r.busy++
	f.mu.Unlock()
	if !f.later(func(ctx context.Context) { f.fill(ctx, r, base, need) }) {
		r.pmu.Lock()
		r.building -= need
		r.pmu.Unlock()
		f.release(r)
	}
}

func (f *Fetcher) fill(ctx context.Context, r *repo, base string, need int) {
	defer f.release(r)
	for range need {
		s, err := f.build(ctx, r, base)
		r.pmu.Lock()
		r.building--
		if err == nil {
			r.slots = append(r.slots, s)
		}
		r.pmu.Unlock()
		if err != nil && ctx.Err() == nil {
			f.cfg.Log.Warn("ready checkout not built", "repo", r.name, "error", err.Error())
		}
	}
}

func (f *Fetcher) build(ctx context.Context, r *repo, base string) (slot, error) {
	root, err := os.MkdirTemp(f.cfg.PoolDir, "slot-")
	if err != nil {
		return slot{}, err
	}
	s := slot{root: root, work: filepath.Join(root, "tree"), index: filepath.Join(root, "index"), base: base}
	if err = os.Mkdir(s.work, 0o777); err == nil {
		if err = os.Chmod(s.work, 0o777); err == nil {
			r.mu.RLock()
			err = f.update(ctx, r, s.work, s.index, emptyTree, base)
			r.mu.RUnlock()
		}
	}
	if err != nil {
		_ = os.RemoveAll(root)
		return slot{}, err
	}
	return s, nil
}

// rebase moves each ready checkout to the commit of HEAD, so that it stays near the newest work.
func (f *Fetcher) rebase(ctx context.Context, r *repo) {
	r.mu.RLock()
	out, err := f.gitRun(ctx, gitTimeout, gitOpts{gitDir: r.path}, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	r.mu.RUnlock()
	head := strings.TrimSpace(string(out))
	if err != nil || !objectID.MatchString(head) {
		return
	}
	r.pmu.Lock()
	old := r.slots
	r.slots = nil
	r.building += len(old)
	r.pmu.Unlock()
	for _, s := range old {
		r.mu.RLock()
		err := f.update(ctx, r, s.work, s.index, s.base, head)
		r.mu.RUnlock()
		r.pmu.Lock()
		r.building--
		if err == nil {
			s.base = head
			r.slots = append(r.slots, s)
		}
		r.pmu.Unlock()
		if err != nil {
			_ = os.RemoveAll(s.root)
		}
	}
	f.refill(r, head)
}

// Changed syncs the clone of repo in the background. An empty repo syncs every clone.
func (f *Fetcher) Changed(repo string) {
	names := []string{repo}
	if repo == "" {
		names = f.Repos()
	}
	for _, n := range names {
		f.later(func(ctx context.Context) { f.Sync(ctx, n) })
	}
}

// Sync fetches the branches of repo into its clone and then moves the ready checkouts. It does nothing for a repository
// without a clone. A sync that runs when another is under way makes the other run once more.
func (f *Fetcher) Sync(ctx context.Context, name string) {
	f.mu.Lock()
	r, ok := f.repos[name]
	if !ok {
		f.mu.Unlock()
		return
	}
	r.busy++
	f.mu.Unlock()
	defer f.release(r)
	r.pmu.Lock()
	if r.syncing {
		r.again = true
		r.pmu.Unlock()
		return
	}
	r.syncing = true
	r.pmu.Unlock()
	for {
		f.syncOnce(ctx, r)
		r.pmu.Lock()
		if !r.again || ctx.Err() != nil {
			r.syncing, r.again = false, false
			r.pmu.Unlock()
			return
		}
		r.again = false
		r.pmu.Unlock()
	}
}

func (f *Fetcher) syncOnce(ctx context.Context, r *repo) {
	r.mu.RLock()
	_, statErr := os.Stat(filepath.Join(r.path, repoMarker))
	var err error
	if statErr == nil {
		var url string
		var o gitOpts
		if url, o, err = f.remote(ctx, r, r.name); err == nil {
			_, err = f.gitRun(ctx, fetchTimeout, o, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--prune", url, "+refs/heads/*:refs/heads/*")
		}
	}
	r.mu.RUnlock()
	switch {
	case statErr != nil || ctx.Err() != nil:
	case err != nil:
		f.cfg.Log.Warn("sync failed", "repo", r.name, "error", err.Error())
	default:
		f.rebase(ctx, r)
	}
}

func dirSize(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// linkScan reads the NUL-separated output of diff-tree --raw -z -t, which alternates a header and a path. It finds a
// header whose new mode is 160000, and it lists the directories that the tree gains or loses with no file under them,
// which read-tree never writes or removes. It holds one record at a time.
type linkScan struct {
	part       []byte
	hdr        []string
	path       bool
	found      bool
	empty, del []string
}

func (l *linkScan) Write(p []byte) (int, error) {
	for _, c := range p {
		if c != 0 {
			if len(l.part) < 1<<16 {
				l.part = append(l.part, c)
			}
			continue
		}
		if !l.path {
			l.hdr = strings.Fields(string(l.part))
		} else {
			l.record(string(l.part))
		}
		l.path, l.part = !l.path, l.part[:0]
	}
	return len(p), nil
}

func (l *linkScan) record(path string) {
	if len(l.hdr) < 4 {
		return
	}
	switch {
	case l.hdr[1] == "160000":
		l.found = true
	case l.hdr[1] == "040000" && l.hdr[3] == emptyTree:
		l.empty = append(l.empty, path)
	case l.hdr[0] == ":040000" && l.hdr[1] == "000000":
		l.del = append(l.del, path)
	}
}

// removeDirs removes the directories that the tree loses and that hold no file, deepest first. It runs before read-tree,
// so it can never remove a file or a link that read-tree writes in place of a directory: Remove fails on a directory
// that still holds files, and read-tree removes those itself.
func removeDirs(dir string, scan *linkScan) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for i := len(scan.del) - 1; i >= 0; i-- {
		_ = root.Remove(scan.del[i])
	}
	return nil
}

// makeDirs makes the empty directories that the tree gains, with mode 0777 like the route path.
func makeDirs(dir string, scan *linkScan) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, p := range scan.empty {
		if !filepath.IsLocal(p) {
			return &refusal{"tree holds an unsafe path"}
		}
		at := ""
		for _, part := range strings.Split(p, "/") {
			at = filepath.Join(at, part)
			if err := root.Mkdir(at, 0o777); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if err := root.Chmod(at, 0o777); err != nil {
				return err
			}
		}
	}
	return nil
}
