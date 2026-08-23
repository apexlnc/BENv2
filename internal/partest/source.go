package partest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Which tests may join a cohort is a judgement, and a judgement recorded as a
// list is the one kind of claim that cannot check itself: a test driven by the
// list proves the entries it names behave, never that the list names the right
// entries (AGENTS.md, Conventions). What follows reads the *source* of each
// candidate instead, so a test that acquires a `t.Setenv`, a pinned window or a
// process-liveness sample after joining a cohort fails at a boundary that does
// not move when somebody edits the list.
//
// It is a coarse instrument on purpose. It cannot see a case that is timing
// sensitive for a reason nobody wrote down, and it over-reaches on shared
// names — two functions called `Wait` are one function to it. Both errors point
// the same way: towards reporting a marker that is not there, which fails
// loudly, rather than missing one that is, which would not.

// Marker is a fact readable from a function's source that constrains which
// cohort it may join.
type Marker struct {
	// Name appears in a caller's failure message.
	Name string
	// Calls are selector names — "Setenv" matches both `t.Setenv` and
	// `os.Setenv`. The receiver is deliberately not part of the match: the two
	// are the same hazard, and the second is worse.
	Calls []string
	// Keys are composite-literal field names — "StallTimeout" matches
	// `core.RunLimits{StallTimeout: …}`.
	Keys []string
	// Types narrows Keys to literals of these types, matched on the simple name
	// so "RunLimits" matches `core.RunLimits{…}`. Empty means any literal, which
	// is the coarser reading and the right default for a key whose name is the
	// whole hazard.
	//
	// It exists because a field name is not owned by one struct: `AttemptTimeout`
	// is both the §7.4 window a run is bounded by and, since #156, the operand
	// the publisher's TTL gate is arithmetic over. The first says a case waits out
	// a window on a machine whose load it does not control; the second is
	// evaluated at Ready and never elapses. A marker that cannot tell them apart
	// mis-classifies every correct use of the second, permanently — so a marker
	// named for a type says the type.
	Types []string
	// Funcs carry the marker by definition rather than by anything visible in
	// their body, and so do their callers.
	Funcs []string
	// Go matches a `go` statement: the function fans out concurrently on its
	// own, so a cohort would multiply rather than bound its fan-out.
	Go bool
	// Why explains the constraint in a caller's failure message.
	Why string
}

// Source is one package's function declarations and the calls between them, so
// a marker inside a helper counts against everything that reaches it.
type Source struct {
	dir   string
	files []*ast.File
	names []string
	// bodies maps a simple function name to every declaration of it. Methods
	// are keyed by their name alone, and a name declared twice maps to both.
	bodies map[string][]*ast.FuncDecl
}

// TestFiles selects a package's `_test.go` files.
func TestFiles(name string) bool { return strings.HasSuffix(name, "_test.go") }

// ImplementationFiles selects everything else — the files that ship.
func ImplementationFiles(name string) bool { return !strings.HasSuffix(name, "_test.go") }

// ParseSource reads every `.go` file in dir that keep accepts.
//
// It refuses an empty result rather than returning one. A scan that quietly
// looked at nothing reports every candidate clean, which is indistinguishable
// from a clean package and is how this whole mechanism would go inert.
func ParseSource(dir string, keep func(filename string) bool) (*Source, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	src := &Source{dir: dir, bodies: map[string][]*ast.FuncDecl{}}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || !keep(name) {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		src.files = append(src.files, f)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if len(src.bodies[fd.Name.Name]) == 0 {
				src.names = append(src.names, fd.Name.Name)
			}
			src.bodies[fd.Name.Name] = append(src.bodies[fd.Name.Name], fd)
		}
	}
	if len(src.files) == 0 {
		abs, _ := filepath.Abs(dir)
		return nil, fmt.Errorf("no matching .go files under %s: the scan would report every candidate clean", abs)
	}
	return src, nil
}

// Declares reports whether the package declares a function of this name.
func (s *Source) Declares(fn string) bool { return len(s.bodies[fn]) > 0 }

// TestFunctions returns every `func TestXxx(…)` the package declares, in the
// order it declares them. TestMain is excluded: it is the harness, not a test.
func (s *Source) TestFunctions() []string {
	var out []string
	for _, name := range s.names {
		if strings.HasPrefix(name, "Test") && name != "TestMain" {
			out = append(out, name)
		}
	}
	return out
}

// Inspect walks every parsed file, for the package-specific reading a caller
// needs beyond markers.
func (s *Source) Inspect(visit func(ast.Node) bool) {
	for _, f := range s.files {
		ast.Inspect(f, visit)
	}
}

// Carries reports whether fn — or anything it calls within this package —
// carries the marker.
func (s *Source) Carries(fn string, m Marker) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(name string) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		if slices.Contains(m.Funcs, name) {
			return true
		}
		for _, decl := range s.bodies[name] {
			hit, callees := scanBody(decl.Body, m)
			if hit {
				return true
			}
			for _, callee := range callees {
				if walk(callee) {
					return true
				}
			}
		}
		return false
	}
	return walk(fn)
}

// scanBody reports whether a body carries the marker directly, and every
// package-local function it calls. Function literals are part of the body, so a
// marker inside a `t.Run` closure counts against the test that wrote it.
func scanBody(body *ast.BlockStmt, m Marker) (bool, []string) {
	hit := false
	var callees []string
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case nil:
			return false
		case *ast.GoStmt:
			if m.Go {
				hit = true
			}
		case *ast.CompositeLit:
			// Keys are matched from the literal rather than from a bare
			// KeyValueExpr, so Types has the literal's type to qualify against.
			// Nested literals are their own node, so this still reaches a
			// `core.RunLimits{…}` written inside another composite.
			if len(m.Types) > 0 && !slices.Contains(m.Types, calleeName(node.Type)) {
				return true
			}
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && slices.Contains(m.Keys, key.Name) {
					hit = true
				}
			}
		case *ast.CallExpr:
			if callee := calleeName(node.Fun); callee != "" {
				callees = append(callees, callee)
				if slices.Contains(m.Calls, callee) {
					hit = true
				}
			}
		}
		return true
	})
	return hit, callees
}

// calleeName is the simple name of whatever a call invokes, or "".
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
