package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"
)

type gitObj struct {
	kind string
	data []byte
	id   string
	raw  [sha1.Size]byte
}

func newObj(kind string, data []byte) gitObj {
	sum := sha1.Sum(append(fmt.Appendf(nil, "%s %d\x00", kind, len(data)), data...))
	return gitObj{kind: kind, data: data, id: fmt.Sprintf("%x", sum), raw: sum}
}

// treeObjs returns the objects of files and the tree that holds them. A name with a slash is a file in a directory.
func treeObjs(files map[string]string) ([]gitObj, gitObj) {
	type entry struct{ sort, mode, name, raw string }
	dirs := map[string]map[string]string{}
	var entries []entry
	var objs []gitObj
	for n, content := range files {
		if dir, rest, ok := strings.Cut(n, "/"); ok {
			if dirs[dir] == nil {
				dirs[dir] = map[string]string{}
			}
			dirs[dir][rest] = content
			continue
		}
		b := newObj("blob", []byte(content))
		objs = append(objs, b)
		entries = append(entries, entry{n, "100644", n, string(b.raw[:])})
	}
	for dir, sub := range dirs {
		more, t := treeObjs(sub)
		objs = append(objs, more...)
		entries = append(entries, entry{dir + "/", "40000", dir, string(t.raw[:])})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].sort < entries[j].sort })
	var tree bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&tree, "%s %s\x00%s", e.mode, e.name, e.raw)
	}
	t := newObj("tree", tree.Bytes())
	return append(objs, t), t
}

// commitObjs returns the blobs, the trees and the commit of files on top of parent. An empty parent makes a root commit.
func commitObjs(files map[string]string, parent, msg string, when int64) []gitObj {
	objs, t := treeObjs(files)
	var c strings.Builder
	fmt.Fprintf(&c, "tree %s\n", t.id)
	if parent != "" {
		fmt.Fprintf(&c, "parent %s\n", parent)
	}
	fmt.Fprintf(&c, "author a <a@example.com> %[1]d +0000\ncommitter a <a@example.com> %[1]d +0000\n\n%s\n", when, msg)
	return append(objs, newObj("commit", []byte(c.String())))
}

func writeLoose(dir string, objs []gitObj) error {
	for _, o := range objs {
		p := filepath.Join(dir, "objects", o.id[:2], o.id[2:])
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		var z bytes.Buffer
		w, _ := zlib.NewWriterLevel(&z, zlib.NoCompression)
		_, _ = fmt.Fprintf(w, "%s %d\x00", o.kind, len(o.data))
		_, _ = w.Write(o.data)
		_ = w.Close()
		if err := os.WriteFile(p, z.Bytes(), 0o444); err != nil {
			return err
		}
	}
	return nil
}

func writeRef(dir, ref, id string) error {
	p := filepath.Join(dir, ref)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(id+"\n"), 0o644)
}

// history holds the commits that a test makes without a working clone: the objects go to motte over HTTP.
type history struct {
	node   *client.Client
	tip    string
	n      int64
	files  map[string]map[string]string
	parent map[string]string
}

func (h *harness) writer() *client.Client {
	if h.hist.node == nil {
		ws := "w" + strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(h.repoPath))
		ctx := h.t.Context()
		if _, err := h.operatorClient().Claim(ctx, ws, api.ClaimRequest{Epoch: 1, Owner: "o1", Repo: h.repo}); err != nil {
			h.t.Fatal(err)
		}
		h.hist.node = h.motte.Client(h.motte.NodeGrant(h.t, ws, 1, h.repo))
		h.hist.files, h.hist.parent = map[string]map[string]string{}, map[string]string{}
	}
	return h.hist.node
}

// commitOn uploads a commit of files on top of parent. The commit of a test with no parent is a root commit.
func (h *harness) commitOn(parent string, files map[string]string, msg string) string {
	h.t.Helper()
	h.writer()
	h.hist.n++
	objs := commitObjs(files, parent, msg, 1700000000+h.hist.n)
	put := make([]client.Object, len(objs))
	for i, o := range objs {
		put[i] = client.Object{Type: o.kind, Data: o.data}
	}
	if _, err := h.hist.node.PutObjects(h.t.Context(), put); err != nil {
		h.t.Fatal(err)
	}
	id := objs[len(objs)-1].id
	h.hist.files[id], h.hist.parent[id] = files, parent
	return id
}

// with returns the files of commit and one more.
func (h *harness) with(commit, file, content string) map[string]string {
	files := map[string]string{file: content}
	for k, v := range h.hist.files[commit] {
		if k != file {
			files[k] = v
		}
	}
	return files
}

// commit adds a commit on the last commit of the history and returns it.
func (h *harness) commit(file, content string) string {
	h.t.Helper()
	h.writer()
	h.hist.tip = h.commitOn(h.hist.tip, h.with(h.hist.tip, file, content), "edit "+file)
	return h.hist.tip
}

// amend replaces the last commit of the history with one of another message.
func (h *harness) amend(msg string) string {
	h.t.Helper()
	h.hist.tip = h.commitOn(h.hist.parent[h.hist.tip], h.hist.files[h.hist.tip], msg)
	return h.hist.tip
}

// swap points a ref of motte at a commit of the history. The ref moves by any step, as a force push does.
func (h *harness) swap(ref, commit string) {
	h.t.Helper()
	mc := h.motte.Client(h.motte.MergeGrant(h.t, h.repo))
	var gen int64
	if cur, err := mc.Ref(h.t.Context(), ref); err == nil {
		gen = cur.Generation
	}
	if _, err := mc.SwapRef(h.t.Context(), ref, gen, commit); err != nil {
		h.t.Fatal(err)
	}
}
