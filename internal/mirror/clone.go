package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (e *Engine) clonesDir() string { return filepath.Join(e.cfg.DataDir, "clones") }

func (e *Engine) clonePath(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(e.clonesDir(), hex.EncodeToString(sum[:16])+".git")
}

// ensureClone returns the path of the bare clone of repo and creates it when it is missing. It marks the clone as used.
func (e *Engine) ensureClone(ctx context.Context, repo string) (string, error) {
	path := e.clonePath(repo)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		if err := os.MkdirAll(e.clonesDir(), 0o755); err != nil {
			return "", err
		}
		if err := e.git.initBare(ctx, path); err != nil {
			return "", err
		}
	}
	now := e.clock.Now()
	_ = os.Chtimes(path, now, now)
	return path, nil
}

func (e *Engine) wipeClone(repo string) { _ = os.RemoveAll(e.clonePath(repo)) }

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

type cloneInfo struct {
	repo string
	path string
	size int64
	used time.Time
}

// afterBatch checks disk use. It deletes idle clones past the idle time, then idle clones, least recently used first,
// until the cache fits its budget. A clone with a dirty ref or in use stays.
func (e *Engine) afterBatch(ctx context.Context) {
	stats, err := e.store.Stats(ctx)
	if err != nil {
		return
	}
	dirty := map[string]bool{}
	for _, s := range stats {
		if en, ok := e.entries[s.Mirror]; ok && s.Dirty > 0 {
			dirty[en.Repo] = true
		}
	}
	byDir := map[string]string{}
	for _, en := range e.cfg.Mirrors {
		byDir[filepath.Base(e.clonePath(en.Repo))] = en.Repo
	}
	var clones []cloneInfo
	var total int64
	entries, _ := os.ReadDir(e.clonesDir())
	for _, d := range entries {
		path := filepath.Join(e.clonesDir(), d.Name())
		repo, configured := byDir[d.Name()]
		if !configured {
			_ = os.RemoveAll(path)
			continue
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		c := cloneInfo{repo: repo, path: path, size: dirSize(path), used: info.ModTime()}
		clones = append(clones, c)
		total += c.size
	}
	now := e.clock.Now()
	sort.Slice(clones, func(i, j int) bool { return clones[i].used.Before(clones[j].used) })
	remove := func(c cloneInfo) bool {
		lock := e.repos.get(c.repo)
		if !lock.TryLock() {
			return false
		}
		defer lock.Unlock()
		_ = os.RemoveAll(c.path)
		total -= c.size
		return true
	}
	var kept []cloneInfo
	for _, c := range clones {
		if !dirty[c.repo] && now.Sub(c.used) > e.cfg.IdleTTL && remove(c) {
			continue
		}
		kept = append(kept, c)
	}
	for _, c := range kept {
		if total <= e.cfg.DiskBudget {
			break
		}
		if !dirty[c.repo] {
			remove(c)
		}
	}
	e.m.cloneBytes.Set(float64(total))
	if total > e.cfg.DiskBudget {
		e.m.overBudget.Set(1)
		e.log.Warn("clone cache is over its disk budget", "bytes", total, "budget", e.cfg.DiskBudget)
	} else {
		e.m.overBudget.Set(0)
	}
}
