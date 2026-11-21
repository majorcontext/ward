package runner_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/ward/internal/runner"
	"github.com/majorcontext/ward/runnerproto"
)

func entry(name, mode, sha string) api.TreeEntry {
	return api.TreeEntry{Name: base64.StdEncoding.EncodeToString([]byte(name)), Mode: mode, SHA: sha}
}

func TestMotteFetcherWritesTheExactTree(t *testing.T) {
	blobs := map[string]string{"b1": "echo hi\n", "b2": "package x\n", "b3": "run.sh", "b4": "deep\n"}
	for _, tc := range []struct {
		name, commitTree, want string
		root                   []api.TreeEntry
	}{
		{"tree", "t0", "", []api.TreeEntry{entry("run.sh", "100755", "b1"), entry("pkg", "40000", "t1"), entry("link", "120000", "b3")}},
		{"other tree", "tX", "has tree tX", nil},
		{"escaping name", "t0", "unsafe entry name", []api.TreeEntry{entry("..", "40000", "t1")}},
		{"submodule", "t0", "mode 160000", []api.TreeEntry{entry("sub", "160000", "b1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/v1/vcs/commits/", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(api.Commit{Tree: tc.commitTree})
			})
			mux.HandleFunc("/v1/vcs/trees/", func(w http.ResponseWriter, r *http.Request) {
				all := []api.Tree{{SHA: "t0", Entries: tc.root}, {SHA: "t1", Entries: []api.TreeEntry{entry("x.go", "100644", "b2"), entry("deep", "40000", "t2")}}, {SHA: "t2", Entries: []api.TreeEntry{entry("d.txt", "100644", "b4")}}}
				out := api.Trees{Trees: all[:2]}
				switch {
				case strings.HasSuffix(r.URL.Path, "t1"):
					out.Trees = all[1:2]
				case strings.HasSuffix(r.URL.Path, "t2"):
					out.Trees = all[2:]
				}
				_ = json.NewEncoder(w).Encode(out)
			})
			mux.HandleFunc("/v1/vcs/blobs/batch", func(w http.ResponseWriter, r *http.Request) {
				var req api.BlobsRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				var out api.Blobs
				for _, s := range req.SHAs {
					out.Blobs = append(out.Blobs, base64.StdEncoding.EncodeToString([]byte(blobs[s])))
				}
				_ = json.NewEncoder(w).Encode(out)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			dir := t.TempDir()
			f := runner.MotteFetcher{URL: srv.URL, Grant: func(context.Context) (string, error) { return "g", nil }}
			err := f.Fetch(t.Context(), runnerproto.Request{Commit: "c", Tree: "t0"}, dir)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "pkg", "x.go")); string(got) != "package x\n" {
				t.Errorf("pkg/x.go = %q", got)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "pkg", "deep", "d.txt")); string(got) != "deep\n" {
				t.Errorf("pkg/deep/d.txt = %q", got)
			}
			if st, _ := os.Stat(filepath.Join(dir, "pkg", "deep")); st == nil || st.Mode().Perm() != 0o777 {
				t.Errorf("pkg/deep mode %v, want 0777", st)
			}
			if st, _ := os.Stat(filepath.Join(dir, "run.sh")); st == nil || st.Mode().Perm()&0o111 == 0 {
				t.Error("run.sh is not executable")
			}
			if l, _ := os.Readlink(filepath.Join(dir, "link")); l != "run.sh" {
				t.Errorf("link -> %q", l)
			}
		})
	}
}
