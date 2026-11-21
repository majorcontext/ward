package seams

import (
	"fmt"
	"go/ast"
	"sort"
	"strings"
)

// Seam is one row of the Provider seams table.
type Seam struct {
	Name  string
	Iface string
	Suite string
	Impls []string
	Gap   string
}

func (s Seam) hasInterface() bool { return s.Iface != "" }

func isNone(cell string) bool { return strings.HasPrefix(strings.ToLower(cell), "none") }

func codeRef(cell string) string {
	if isNone(cell) {
		return ""
	}
	return strings.Trim(strings.TrimSpace(cell), "`")
}

// splitRef splits "dir.Ident" at the last dot.
func splitRef(ref string) (dir, ident string) {
	i := strings.LastIndex(ref, ".")
	if i < 0 {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// ParseTable reads the table under the heading "## Provider seams".
func ParseTable(doc []byte) ([]Seam, error) {
	var seams []Seam
	in, started := false, false
	for _, line := range strings.Split(string(doc), "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			if in {
				return seams, nil
			}
			in = strings.TrimSpace(line[3:]) == "Provider seams"
		case in && strings.HasPrefix(line, "|"):
			cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			if len(cells) != 5 {
				return nil, fmt.Errorf("provider seams row has %d cells, want 5: %q", len(cells), line)
			}
			if !started {
				started = true
				continue
			}
			if strings.HasPrefix(cells[0], "---") {
				continue
			}
			s := Seam{Name: strings.Trim(cells[0], "`"), Iface: codeRef(cells[1]), Suite: codeRef(cells[2]), Gap: cells[4]}
			if !isNone(cells[3]) {
				for _, p := range strings.Split(cells[3], ",") {
					s.Impls = append(s.Impls, strings.Trim(strings.TrimSpace(p), "`"))
				}
			}
			seams = append(seams, s)
		}
	}
	if len(seams) == 0 {
		return nil, fmt.Errorf("no Provider seams table found")
	}
	return seams, nil
}

// CheckTable reports each difference between the table and the code.
func CheckTable(m *Module, seams []Seam) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range seams {
		if s.Name == "" || strings.ContainsAny(s.Name, " \t") || seen[s.Name] {
			out = append(out, fmt.Sprintf("seam name %q must be one unique word", s.Name))
		}
		seen[s.Name] = true
		out = append(out, m.checkSeam(s)...)
	}
	out = append(out, m.unlisted(seams)...)
	sort.Strings(out)
	return out
}

func (m *Module) checkSeam(s Seam) []string {
	var out []string
	bad := func(format string, a ...any) { out = append(out, s.Name+": "+fmt.Sprintf(format, a...)) }
	if s.Gap == "" {
		switch {
		case !s.hasInterface():
			bad("has no interface and no gap reason")
		case s.Suite == "":
			bad("has no conformance suite and no gap reason")
		case len(s.Impls) < 2:
			bad("has %d implementations, want at least 2, or a gap reason", len(s.Impls))
		}
	}
	if s.hasInterface() {
		if dir, id := splitRef(s.Iface); !m.declares(dir, id, true) {
			bad("interface %s is not declared", s.Iface)
		}
	}
	if s.Suite != "" {
		if dir, id := splitRef(s.Suite); !m.declares(dir, id, false) {
			bad("suite %s is not declared", s.Suite)
		}
	}
	seen := map[string]bool{}
	for _, impl := range s.Impls {
		if seen[impl] {
			bad("implementation %s is listed twice", impl)
		}
		seen[impl] = true
		if !m.hasPackage(impl) {
			bad("implementation %s has no Go package", impl)
		} else if s.Suite != "" && !m.callsSuite(impl, s.Suite) {
			bad("implementation %s has no test that calls %s", impl, s.Suite)
		}
	}
	return out
}

// declares reports whether dir declares ident as an interface type or, when iface is false, as a function.
func (m *Module) declares(dir, ident string, iface bool) bool {
	for _, gf := range m.files {
		if gf.dir != dir || gf.test {
			continue
		}
		for _, d := range gf.f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !iface && d.Recv == nil && d.Name.Name == ident {
					return true
				}
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					if ts, ok := sp.(*ast.TypeSpec); ok && iface && ts.Name.Name == ident {
						_, isIface := ts.Type.(*ast.InterfaceType)
						return isIface
					}
				}
			}
		}
	}
	return false
}

func (m *Module) hasPackage(dir string) bool {
	for _, gf := range m.files {
		if gf.dir == dir && !gf.test {
			return true
		}
	}
	return false
}

// callsSuite reports whether a test file of dir calls the suite entry function from a top-level Test function.
func (m *Module) callsSuite(dir, suite string) bool {
	suiteDir, entry := splitRef(suite)
	found := false
	for _, gf := range m.files {
		if gf.dir != dir || !gf.test {
			continue
		}
		imports := importsOf(gf.f)
		for _, d := range gf.f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == entry {
					if id, ok := sel.X.(*ast.Ident); ok && m.pkgDir(imports[id.Name]) == suiteDir {
						found = true
					}
				}
				return true
			})
		}
	}
	return found
}

// unlisted reports a package that implements a listed interface and is not in the table. A package implements an
// interface when it sits under the directory of the interface (the suite excepted), or when it holds the
// assertion "var _ pkg.Iface = ...".
func (m *Module) unlisted(seams []Seam) []string {
	var out []string
	for _, s := range seams {
		if !s.hasInterface() {
			continue
		}
		ifaceDir, id := splitRef(s.Iface)
		suiteDir, _ := splitRef(s.Suite)
		listed := map[string]bool{}
		for _, impl := range s.Impls {
			listed[impl] = true
		}
		flag := map[string]string{}
		for _, gf := range m.files {
			if gf.test {
				continue
			}
			if strings.HasPrefix(gf.dir, ifaceDir+"/") && gf.dir != suiteDir && !strings.HasPrefix(gf.dir, suiteDir+"/") {
				flag[gf.dir] = "sits under " + ifaceDir
			}
			if gf.dir != ifaceDir && m.asserts(gf, ifaceDir, id) {
				flag[gf.dir] = "asserts " + s.Iface
			}
		}
		for _, dir := range sortedKeys(flag) {
			if !listed[dir] {
				out = append(out, fmt.Sprintf("%s: package %s %s and is not in the implementations of the table", s.Name, dir, flag[dir]))
			}
		}
	}
	return out
}

func (m *Module) asserts(gf goFile, ifaceDir, ident string) bool {
	imports := importsOf(gf.f)
	for _, d := range gf.f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "_" {
				continue
			}
			if sel, ok := vs.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == ident {
				if id, ok := sel.X.(*ast.Ident); ok && m.pkgDir(imports[id.Name]) == ifaceDir {
					return true
				}
			}
		}
	}
	return false
}

// ImplDirs returns the implementation packages of the table.
func ImplDirs(seams []Seam) map[string]bool {
	out := map[string]bool{}
	for _, s := range seams {
		for _, impl := range s.Impls {
			out[impl] = true
		}
	}
	return out
}

// CheckGaps compares the gap rows of the table with the gap file. When the file at the merge base is known, a gap
// that the base file does not hold also fails: the gap list only shrinks.
func CheckGaps(seams []Seam, file, atBase map[string]string, haveBase bool) []string {
	var out []string
	rows := map[string]bool{}
	for _, s := range seams {
		if s.Gap == "" {
			continue
		}
		rows[s.Name] = true
		if _, ok := file[s.Name]; !ok {
			out = append(out, "gap row is not in the gap file; the gap list only shrinks: "+s.Name)
		}
	}
	for name := range file {
		if !rows[name] {
			out = append(out, "gap file line has no gap row in the table; delete it: "+name)
		}
		if _, ok := atBase[name]; haveBase && !ok {
			out = append(out, "gap line is new against the merge base; the gap list only shrinks: "+name)
		}
	}
	sort.Strings(out)
	return out
}
