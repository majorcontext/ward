package warm

import (
	"bytes"
	"context"
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
	raw, err := f.gitRun(ctx, gitTimeout, gitOpts{gitDir: r.path}, "diff-tree", "-r", "--raw", "-z", "--no-renames", from, to)
	if err != nil {
		return err
	}
	for _, rec := range bytes.Split(raw, []byte{0}) {
		if fl := strings.Fields(string(rec)); len(fl) > 1 && fl[0][0] == ':' && fl[1] == "160000" {
			return &refusal{"tree holds a submodule entry, which a runner does not check out"}
		}
	}
	_, err = f.gitRun(ctx, gitTimeout, gitOpts{gitDir: r.path, workTree: dir, index: index, open: true}, "read-tree", "-m", "-u", from, to)
	return err
}

// refill builds ready checkouts at base, in the background, until the pool is full.
func (f *Fetcher) refill(r *repo, base string) {
	r.pmu.Lock()
	need := f.cfg.PoolSize - len(r.slots) - r.building
	if need <= 0 || f.ctx.Err() != nil {
		r.pmu.Unlock()
		return
	}
	r.building += need
	r.pmu.Unlock()
	f.mu.Lock()
	r.busy++
	f.mu.Unlock()
	f.later(func() {
		defer f.release(r)
		for range need {
			s, err := f.build(f.ctx, r, base)
			r.pmu.Lock()
			r.building--
			if err == nil {
				r.slots = append(r.slots, s)
			}
			r.pmu.Unlock()
			if err != nil && f.ctx.Err() == nil {
				f.cfg.Log.Warn("ready checkout not built", "repo", r.name, "error", err.Error())
			}
		}
	})
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
		f.later(func() { f.Sync(f.ctx, n) })
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
