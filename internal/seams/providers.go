package seams

import (
	"fmt"
	"go/ast"
	"go/token"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// SDKPrefixes are the import path prefixes of cloud and forge provider SDKs.
var SDKPrefixes = []string{
	"cloud.google.com/",
	"google.golang.org/api/",
	"golang.org/x/oauth2/google",
	"github.com/googleapis/",
	"github.com/aws/",
	"github.com/Azure/",
	"github.com/google/go-github",
	"github.com/xanzy/",
	"github.com/shurcooL/githubv4",
	"github.com/bradleyfalzon/ghinstallation",
	"github.com/cloudflare/cloudflare-go",
	"gitlab.com/gitlab-org/",
}

// Hosts are the provider host names that only allowlisted files may hold.
var Hosts = []string{"api.github.com", "ghcr.io", "github.com", "googleapis.com", "amazonaws.com", "r2.cloudflarestorage.com"}

var yamlRoots = []string{".github", "charts"}

func sdkOf(importPath string) bool {
	for _, p := range SDKPrefixes {
		if strings.HasPrefix(importPath, p) || importPath+"/" == p {
			return true
		}
	}
	return false
}

func underCmd(dir string) bool { return dir == "cmd" || strings.HasPrefix(dir, "cmd/") }

// CheckSDKImports reports a package that imports a provider SDK and is neither an implementation package of a seam,
// a package under cmd/, nor in allow. It also reports an allow entry that no package needs.
func CheckSDKImports(m *Module, impls map[string]bool, allow map[string]string) []string {
	importers := map[string]string{}
	for _, gf := range m.files {
		for _, imp := range gf.f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); sdkOf(p) {
				importers[gf.dir] = p
			}
		}
	}
	var out []string
	for dir, p := range importers {
		_, listed := allow[dir]
		if !impls[dir] && !underCmd(dir) && !listed {
			out = append(out, fmt.Sprintf("%s imports %s; only an implementation package of a seam or cmd/ may", dir, p))
		}
	}
	for dir := range allow {
		if _, ok := importers[dir]; !ok {
			out = append(out, "SDK allowlist entry is stale; delete it: "+dir)
		} else if impls[dir] || underCmd(dir) {
			out = append(out, "SDK allowlist entry is redundant; delete it: "+dir)
		}
	}
	sort.Strings(out)
	return out
}

func hostIn(s string) string {
	for _, h := range Hosts {
		if strings.Contains(s, h) {
			return h
		}
	}
	return ""
}

// HostHits returns each file that holds a provider host: a Go string literal (an import path and the module path
// excepted) or a line of a YAML file under .github or charts.
func HostHits(m *Module) (map[string]string, error) {
	hits := map[string]string{}
	for _, gf := range m.files {
		ast.Inspect(gf.f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ImportSpec:
				return false
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					v, _ := strconv.Unquote(n.Value)
					if h := hostIn(strings.ReplaceAll(v, m.Path, "")); h != "" {
						hits[gf.path] = h
					}
				}
			}
			return true
		})
	}
	for _, root := range yamlRoots {
		err := fs.WalkDir(m.fsys, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if ext := path.Ext(p); ext != ".yaml" && ext != ".yml" && ext != ".tpl" {
				return nil
			}
			src, err := fs.ReadFile(m.fsys, p)
			if err != nil {
				return err
			}
			if h := hostIn(string(src)); h != "" {
				hits[p] = h
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return hits, nil
}

func matches(entry, file string) bool {
	ok, _ := path.Match(entry, file)
	return ok
}

// CheckHosts reports a file that holds a provider host and matches no allow entry (a path or a glob), and an
// entry that matches no such file.
func CheckHosts(hits map[string]string, allow map[string]string) []string {
	var out []string
	for file, host := range hits {
		ok := false
		for entry := range allow {
			ok = ok || matches(entry, file)
		}
		if !ok {
			out = append(out, fmt.Sprintf("%s names %s; only a file of the host allowlist may", file, host))
		}
	}
	for entry := range allow {
		used := false
		for file := range hits {
			used = used || matches(entry, file)
		}
		if !used {
			out = append(out, "host allowlist entry is stale; delete it: "+entry)
		}
	}
	sort.Strings(out)
	return out
}
