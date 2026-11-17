// Package wallclock reports time.Sleep and time.After in test files outside a testing/synctest.Test bubble.
package wallclock

import (
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

const message = "%s outside a synctest bubble: wrap the test in synctest.Test or wait on a channel"

// Analyzer reports a wall-clock wait in a _test.go file unless it sits inside the function literal
// passed to synctest.Test. A comment "//lint:allow wallclock REASON" on the line exempts the call.
var Analyzer = &analysis.Analyzer{
	Name:      "wallclock",
	Doc:       "report time.Sleep and time.After in tests outside a testing/synctest bubble",
	FactTypes: []analysis.Fact{new(waits)},
	Run:       run,
}

// waits marks a function that waits on the wall clock outside any bubble.
type waits struct{}

func (*waits) AFact()         {}
func (*waits) String() string { return "waits" }

func run(pass *analysis.Pass) (any, error) {
	var files []*ast.File
	for _, f := range pass.Files {
		if strings.HasSuffix(pass.Fset.File(f.Pos()).Name(), "_test.go") {
			files = append(files, f)
		}
	}
	allowed := allowedLines(pass, files)
	// A function that waits outside a bubble, directly or through another one, carries a fact.
	for changed := true; changed; {
		changed = false
		for _, f := range files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				obj, _ := pass.TypesInfo.Defs[fn.Name].(*types.Func)
				if obj == nil || isEntryPoint(fn.Name.Name) || pass.ImportObjectFact(obj, new(waits)) {
					continue
				}
				if len(offences(pass, fn.Body, allowed)) > 0 {
					pass.ExportObjectFact(obj, new(waits))
					changed = true
				}
			}
		}
	}
	for _, f := range files {
		for _, o := range offences(pass, f, allowed) {
			pass.Reportf(o.pos, message, o.what)
		}
	}
	return nil, nil
}

func isEntryPoint(name string) bool {
	for _, p := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

type offence struct {
	pos  token.Pos
	what string
}

// offences returns the waits and the calls to waiting functions under root that lie outside a bubble.
func offences(pass *analysis.Pass, root ast.Node, allowed map[string]bool) []offence {
	var out []offence
	var walk func(n ast.Node) bool
	walk = func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isFunc(pass, call.Fun, "testing/synctest", "Test") {
			ast.Inspect(call.Fun, walk)
			for _, a := range call.Args {
				if _, lit := a.(*ast.FuncLit); !lit {
					ast.Inspect(a, walk)
				}
			}
			return false
		}
		p := pass.Fset.Position(call.Pos())
		if allowed[key(p.Filename, p.Line)] {
			return true
		}
		for _, name := range []string{"Sleep", "After"} {
			if isFunc(pass, call.Fun, "time", name) {
				out = append(out, offence{call.Pos(), "time." + name})
			}
		}
		if fn := callee(pass, call.Fun); fn != nil && pass.ImportObjectFact(fn, new(waits)) {
			out = append(out, offence{call.Pos(), "call to " + fn.Name() + ", which waits on the wall clock,"})
		}
		return true
	}
	ast.Inspect(root, walk)
	return out
}

func callee(pass *analysis.Pass, fun ast.Expr) *types.Func {
	var id *ast.Ident
	switch e := ast.Unparen(fun).(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		id = e.Sel
	default:
		return nil
	}
	fn, _ := pass.TypesInfo.Uses[id].(*types.Func)
	return fn
}

func isFunc(pass *analysis.Pass, fun ast.Expr, pkg, name string) bool {
	fn := callee(pass, fun)
	return fn != nil && fn.Name() == name && fn.Pkg() != nil && fn.Pkg().Path() == pkg
}

func allowedLines(pass *analysis.Pass, files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, g := range f.Comments {
			for _, c := range g.List {
				if strings.HasPrefix(c.Text, "//lint:allow wallclock ") {
					p := pass.Fset.Position(c.Pos())
					out[key(p.Filename, p.Line)] = true
				}
			}
		}
	}
	return out
}

func key(file string, line int) string { return file + ":" + strconv.Itoa(line) }
