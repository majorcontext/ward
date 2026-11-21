// Package seams enforces the provider seam rules of AGENTS.md.
package seams

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

type goFile struct {
	path string
	dir  string
	test bool
	f    *ast.File
}

// Module is the parsed Go source of one module.
type Module struct {
	fsys  fs.FS
	Path  string
	files []goFile
}

// Load parses every Go file of the module in fsys, skipping testdata and hidden directories.
func Load(fsys fs.FS) (*Module, error) {
	gomod, err := fs.ReadFile(fsys, "go.mod")
	if err != nil {
		return nil, err
	}
	m := &Module{fsys: fsys}
	for _, line := range strings.Split(string(gomod), "\n") {
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			m.Path = strings.TrimSpace(rest)
		}
	}
	if m.Path == "" {
		return nil, fmt.Errorf("go.mod names no module")
	}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, src, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		m.files = append(m.files, goFile{p, path.Dir(p), strings.HasSuffix(p, "_test.go"), f})
		return nil
	})
	return m, err
}

// pkgDir returns the module-relative directory of an import path, or "" when the path is outside the module.
func (m *Module) pkgDir(importPath string) string {
	if rest, ok := strings.CutPrefix(importPath, m.Path+"/"); ok {
		return rest
	}
	return ""
}

// importsOf maps the local name of each import of f to its path.
func importsOf(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = p
	}
	return out
}

// ParseAllowlist reads "key # reason" lines. A line with no reason is an error.
func ParseAllowlist(name string, data []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line, reason, _ := strings.Cut(sc.Text(), "#")
		line, reason = strings.TrimSpace(line), strings.TrimSpace(reason)
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, " \t") || reason == "" {
			return nil, fmt.Errorf("%s: want %q, got %q", name, "key # reason", sc.Text())
		}
		out[line] = reason
	}
	return out, sc.Err()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
