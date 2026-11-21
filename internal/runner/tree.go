package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/ward/runnerproto"
)

const (
	treeBulk      = 100000
	blobBatchSize = 8 << 20
	blobBatchMax  = 64
)

// MotteFetcher reads trees from motte with a read grant. Grant returns the grant text for each call, so a rotated grant applies at once.
type MotteFetcher struct {
	URL   string
	Grant func(ctx context.Context) (string, error)
}

// Fetch writes the files of req.Tree to dir. It reads the commit first and refuses a commit whose tree is another tree.
func (m MotteFetcher) Fetch(ctx context.Context, req runnerproto.Request, dir string) error {
	c := client.New(m.URL, m.Grant)
	commit, err := c.Commit(ctx, req.Commit)
	if err != nil {
		return err
	}
	if commit.Tree != req.Tree {
		return fmt.Errorf("commit %s has tree %s, not %s", req.Commit, commit.Tree, req.Tree)
	}
	trees := map[string][]api.TreeEntry{}
	walked := map[string]bool{}
	var load func(sha string) error
	load = func(sha string) error {
		if walked[sha] {
			return nil
		}
		walked[sha] = true
		if _, ok := trees[sha]; !ok {
			out, err := c.Trees(ctx, sha, treeBulk)
			if err != nil {
				return err
			}
			for _, t := range out.Trees {
				trees[t.SHA] = t.Entries
			}
			if _, ok := trees[sha]; !ok {
				return fmt.Errorf("motte did not return tree %s", sha)
			}
		}
		for _, e := range trees[sha] {
			if e.Mode == "40000" {
				if err := load(e.SHA); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := load(req.Tree); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var files []file
	if err := plan(trees, req.Tree, "", &files, root); err != nil {
		return err
	}
	return writeBlobs(ctx, c, root, files)
}

type file struct {
	path string
	mode string
	sha  string
	size int64
}

// plan creates the directories and lists the other entries. It refuses a name that could leave the tree.
func plan(trees map[string][]api.TreeEntry, sha, prefix string, files *[]file, root *os.Root) error {
	for _, e := range trees[sha] {
		raw, err := base64.StdEncoding.DecodeString(e.Name)
		name := string(raw)
		if err != nil || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
			return fmt.Errorf("tree %s: unsafe entry name %q", sha, name)
		}
		p := prefix + name
		switch e.Mode {
		case "40000":
			if err := root.Mkdir(p, 0o777); err != nil {
				return err
			}
			if err := root.Chmod(p, 0o777); err != nil {
				return err
			}
			if err := plan(trees, e.SHA, p+"/", files, root); err != nil {
				return err
			}
		case "100644", "100755", "120000":
			*files = append(*files, file{p, e.Mode, e.SHA, e.Size})
		default:
			return fmt.Errorf("tree %s: entry %q has mode %s, which a runner does not check out", sha, name, e.Mode)
		}
	}
	return nil
}

func writeBlobs(ctx context.Context, c *client.Client, root *os.Root, files []file) error {
	for len(files) > 0 {
		n, size := 0, int64(0)
		for n < len(files) && n < blobBatchMax && (n == 0 || size+files[n].size <= blobBatchSize) {
			size += files[n].size
			n++
		}
		batch := files[:n]
		files = files[n:]
		shas := make([]string, len(batch))
		for i, f := range batch {
			shas[i] = f.sha
		}
		blobs, err := c.Blobs(ctx, shas)
		if err != nil {
			return err
		}
		if len(blobs) != len(batch) {
			return errors.New("motte returned a different number of blobs")
		}
		for i, f := range batch {
			if err := writeOne(root, f, blobs[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeOne(root *os.Root, f file, data []byte) error {
	if f.mode == "120000" {
		return root.Symlink(string(data), f.path)
	}
	perm := os.FileMode(0o666)
	if f.mode == "100755" {
		perm = 0o777
	}
	w, err := root.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return root.Chmod(f.path, perm)
}
