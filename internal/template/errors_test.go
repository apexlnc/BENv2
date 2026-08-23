package template

import (
	"errors"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The refusals the package names, paired with the sentinel each detail type
// must unwrap to. The Load-site tests assert the same pairing at the production
// seam; these two assert that the classifications do not overlap, and that no
// error type can be added without one.
var classifications = []struct {
	name string // the sentinel's identifier, as the source walk below sees it
	err  error  // a detail value of the type that carries it
	want error
}{
	{"ErrEnginePanic", &EnginePanicError{Value: "boom"}, ErrEnginePanic},
	{"ErrUnknownVariable", &UnknownVariableError{Ref: "issue.titel"}, ErrUnknownVariable},
	{"ErrUnknownFilter", &UnknownFilterError{Name: "shout"}, ErrUnknownFilter},
	{"ErrReservedName", &ReservedNameError{Name: "forloop"}, ErrReservedName},
	{"ErrUntrustedUse", &UntrustedUseError{Ref: "issue.body"}, ErrUntrustedUse},
	{"ErrPromptTooLarge", &PromptTooLargeError{Bytes: 2, Max: 1}, ErrPromptTooLarge},
	{"ErrUnsupportedTag", &UnsupportedTagError{Tag: "include"}, ErrUnsupportedTag},
}

// A caller that classifies with errors.Is gets exactly one answer per refusal.
// Without this, a copy-pasted Unwrap returning a neighbour's sentinel passes
// every test that only checks the type it belongs to.
//
// The diagonal is the table's own row identity, not sentinel equality: two rows
// declared as the same value — `ErrUnknownFilter = ErrUnknownVariable` — would
// satisfy an equality test on both sides while collapsing two classifications
// into one.
func TestErrorClassificationsAreDisjoint(t *testing.T) {
	for i, c := range classifications {
		for j, other := range classifications {
			want := i == j
			if got := errors.Is(c.err, other.want); got != want {
				t.Errorf("errors.Is(%T, %s) = %t, want %t", c.err, other.name, got, want)
			}
		}
	}
}

// The table above is driven by the declarations it checks, so on its own it
// cannot see a refusal added without a sentinel — the fifth strictness type
// #74 was filed about. This reads the package source instead: every type that
// is an error unwraps to a package-level sentinel, and every sentinel the
// package declares is in the table.
func TestEveryErrorTypeCarriesAClassification(t *testing.T) {
	sentinels, errorTypes, unwrapReturns := parsePackageErrors(t)

	for _, name := range errorTypes {
		returns, ok := unwrapReturns[name]
		if !ok {
			t.Errorf("%s has no Unwrap: callers can only reach it by concrete type", name)
			continue
		}
		for _, ret := range returns {
			if !slices.Contains(sentinels, ret) {
				t.Errorf("%s.Unwrap returns %s, which is not a package-level sentinel", name, ret)
			}
		}
	}

	tabled := make([]string, 0, len(classifications))
	for _, c := range classifications {
		tabled = append(tabled, c.name)
	}
	slices.Sort(tabled)
	slices.Sort(sentinels)
	if !slices.Equal(tabled, sentinels) {
		t.Errorf("TestErrorClassificationsAreDisjoint covers %v, package declares %v", tabled, sentinels)
	}
}

// parsePackageErrors reads the package's non-test sources and returns the
// package-level `Err*` variables, the types with an Error method, and the
// identifiers each type's Unwrap returns.
func parsePackageErrors(t *testing.T) (sentinels, errorTypes []string, unwrapReturns map[string][]string) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	unwrapReturns = make(map[string][]string)
	fset := gotoken.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != gotoken.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						if strings.HasPrefix(name.Name, "Err") {
							sentinels = append(sentinels, name.Name)
						}
					}
				}
			case *ast.FuncDecl:
				recv, ok := receiverType(d)
				if !ok {
					continue
				}
				switch d.Name.Name {
				case "Error":
					errorTypes = append(errorTypes, recv)
				case "Unwrap":
					unwrapReturns[recv] = returnedIdents(t, recv, d)
				}
			}
		}
	}
	// A walk that finds nothing would pass every assertion above.
	if len(sentinels) == 0 || len(errorTypes) == 0 {
		t.Fatalf("source walk found %d sentinels and %d error types; the walk is broken, not the package",
			len(sentinels), len(errorTypes))
	}
	return sentinels, errorTypes, unwrapReturns
}

func receiverType(d *ast.FuncDecl) (string, bool) {
	if d.Recv == nil || len(d.Recv.List) != 1 {
		return "", false
	}
	expr := d.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// An Unwrap that computes its result — returning a wrapped cause, or one of two
// sentinels by condition — is outside the shape this contract admits, so it is
// reported rather than approximated.
func returnedIdents(t *testing.T, recv string, d *ast.FuncDecl) []string {
	t.Helper()
	var out []string
	ast.Inspect(d.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, res := range ret.Results {
			id, ok := res.(*ast.Ident)
			if !ok {
				t.Errorf("%s.Unwrap returns an expression, not a named sentinel", recv)
				continue
			}
			out = append(out, id.Name)
		}
		return true
	})
	return out
}

// Every error type carrying File and Line implements Located — found by walking
// the package source, not by a list here.
//
// A caller (config's loader) skips naming the workflow file when an error is
// Located, because `file:line` is strictly better than a file. So a new error
// type that carries a location and forgets the method gets double-named,
// silently, in another package. A hand-written table would catch that only if
// somebody remembered to add the row, which is the failure mode the table would
// exist to prevent.
//
// This file already parses its own AST for the sentinel/type pairing; the same
// walk answers this.
func TestEveryErrorCarryingAFileAndLineIsLocated(t *testing.T) {
	withLocation := parseErrorTypesWithLocation(t)
	if len(withLocation) == 0 {
		t.Fatal("the walk found no error type carrying File and Line; the walk is broken, not the package")
	}

	// A type the walk finds and this map does not fails below, at test time —
	// not at compile time, which an earlier version of this comment claimed.
	samples := map[string]error{
		"UnknownVariableError": &UnknownVariableError{File: "f", Line: 7},
		"UnknownFilterError":   &UnknownFilterError{File: "f", Line: 7},
		"ReservedNameError":    &ReservedNameError{File: "f", Line: 7},
		"UntrustedUseError":    &UntrustedUseError{File: "f", Line: 7},
		"UnsupportedTagError":  &UnsupportedTagError{File: "f", Line: 7},
	}
	for _, name := range withLocation {
		sample, ok := samples[name]
		if !ok {
			t.Errorf("%s carries File and Line and has no sample here; add one so its behaviour is checked", name)
			continue
		}
		l, ok := sample.(Located)
		if !ok {
			t.Errorf("%s carries File and Line but does not implement Located, so a caller that already "+
				"names the file will double-name it", name)
			continue
		}
		file, line := l.Location()
		if file != "f" || line != 7 {
			t.Errorf("%s.Location() = %q:%d, want f:7", name, file, line)
		}
		if !strings.Contains(sample.Error(), "f:7") {
			t.Errorf("%s renders a location its Location() does not report: %v", name, sample)
		}
	}

	// The other direction, and it is the half that matters for the caller: the
	// set of types *declaring* Location must equal the set carrying File and
	// Line — no more and no less.
	//
	// Naming the current non-located types instead would have left the real
	// bypass open. A new error implementing Location() without those fields is
	// skipped by config's withPath and ends up the only refusal naming no file
	// at all, which is worse than the double-naming the skip prevents. Nobody
	// would think to add it to a list of exclusions, because from over there it
	// looks like it is doing the right thing.
	declaring := parseTypesDeclaringLocation(t)
	if !slices.Equal(declaring, withLocation) {
		t.Errorf("types declaring Location() = %v, types carrying File and Line = %v; the two sets must "+
			"match, or a caller that trusts Location() is trusting a type with nothing to report",
			declaring, withLocation)
	}
}

// parseTypesDeclaringLocation names every type in the package with a Location
// method, by receiver.
func parseTypesDeclaringLocation(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	var out []string
	fset := gotoken.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "Location" {
				continue
			}
			if recv, ok := receiverType(fd); ok {
				out = append(out, recv)
			}
		}
	}
	slices.Sort(out)
	return out
}

// parseErrorTypesWithLocation names every struct in the package holding both a
// File string and a Line int.
func parseErrorTypesWithLocation(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	var out []string
	fset := gotoken.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			var file, line bool
			for _, fld := range st.Fields.List {
				id, ok := fld.Type.(*ast.Ident)
				if !ok {
					continue
				}
				for _, name := range fld.Names {
					file = file || (name.Name == "File" && id.Name == "string")
					line = line || (name.Name == "Line" && id.Name == "int")
				}
			}
			if file && line {
				out = append(out, ts.Name.Name)
			}
			return true
		})
	}
	slices.Sort(out)
	return out
}
