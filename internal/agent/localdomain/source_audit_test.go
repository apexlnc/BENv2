package localdomain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// The API names do not prove kernel ordering. Pin the exact Go 1.26 mapping
// independently of the gated Linux launch test: clone places the child first;
// only the trusted pre-exec path creates the cgroup namespace (#274).
func TestSupervisorCloneMappingKeepsNewCgroupOutOfCloneFlags(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), packageSource(t, "launch_linux.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	fields := sysProcAttrFields(t, file, "supervisorSysProcAttr")
	clone := selectorNames(fields["Cloneflags"])
	wantClone := []string{"CLONE_NEWNS", "CLONE_NEWPID", "CLONE_NEWUSER"}
	if strings.Join(clone, ",") != strings.Join(wantClone, ",") {
		t.Fatalf("Cloneflags selectors = %v, want %v", clone, wantClone)
	}
	unshare := selectorNames(fields["Unshareflags"])
	if strings.Join(unshare, ",") != "CLONE_NEWCGROUP" {
		t.Fatalf("Unshareflags selectors = %v, want CLONE_NEWCGROUP", unshare)
	}
	for _, required := range []string{"UseCgroupFD", "CgroupFD", "PidFD", "UidMappings", "GidMappings"} {
		if fields[required] == nil {
			t.Errorf("SysProcAttr has no %s field", required)
		}
	}
	if literal, ok := fields["UseCgroupFD"].(*ast.Ident); !ok || literal.Name != "true" {
		t.Errorf("UseCgroupFD is not literal true")
	}
}

func TestProviderTrampolinePinsOSThreadThroughExec(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, packageSource(t, "supervisor_linux.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	function := functionDeclaration(t, file, "runProviderTrampoline")

	var topLevelLock token.Pos
	for _, statement := range function.Body.List {
		expression, ok := statement.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expression.X.(*ast.CallExpr)
		if ok && qualifiedCallName(call) == "runtime.LockOSThread" {
			topLevelLock = call.Pos()
		}
	}

	calls := map[string][]token.Pos{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok {
			calls[qualifiedCallName(call)] = append(calls[qualifiedCallName(call)], call.Pos())
		}
		return true
	})
	if len(calls["runtime.LockOSThread"]) != 1 || topLevelLock == token.NoPos {
		t.Fatalf("LockOSThread calls = %d, top-level position = %v; want one unconditional top-level pin",
			len(calls["runtime.LockOSThread"]), fset.Position(topLevelLock))
	}
	if len(calls["runtime.UnlockOSThread"]) != 0 {
		t.Fatalf("UnlockOSThread calls = %d, want none before process-replacing exec", len(calls["runtime.UnlockOSThread"]))
	}
	for _, required := range []string{"unix.Prctl", "unix.Capset", "unix.Exec"} {
		positions := calls[required]
		if len(positions) == 0 {
			t.Fatalf("%s is absent from provider trampoline", required)
		}
		for _, position := range positions {
			if position < topLevelLock {
				t.Fatalf("%s at %s precedes LockOSThread at %s", required, fset.Position(position), fset.Position(topLevelLock))
			}
		}
	}
}

func TestHandleTeardownHasNoNumericOrProcessGroupFallback(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), packageSource(t, "cgroup_linux.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	methods := map[string]*ast.FuncDecl{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv == nil || len(function.Recv.List) != 1 {
			continue
		}
		star, ok := function.Recv.List[0].Type.(*ast.StarExpr)
		identifier, receiverOK := star.X.(*ast.Ident)
		if ok && receiverOK && identifier.Name == "linuxTarget" {
			methods[function.Name.Name] = function
		}
	}
	term := methods["term"]
	kill := methods["kill"]
	if term == nil || kill == nil {
		t.Fatal("linuxTarget term/kill methods not found")
	}
	termSelectors := selectorNames(term.Body)
	if !containsName(termSelectors, "PidfdSendSignal") || containsName(termSelectors, "Kill") {
		t.Fatalf("term selectors = %v, want pidfd SIGTERM only", termSelectors)
	}
	var killFile string
	ast.Inspect(kill.Body, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if ok && literal.Value == `"cgroup.kill"` {
			killFile = literal.Value
		}
		return true
	})
	killSelectors := selectorNames(kill.Body)
	if killFile == "" || containsName(killSelectors, "Kill") || containsName(killSelectors, "PidfdSendSignal") {
		t.Fatalf("kill must only write cgroup.kill; literal=%q selectors=%v", killFile, killSelectors)
	}
}

func TestMountSetupOnlyUnmountsItsOwnMounts(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), packageSource(t, "mount_linux.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Unmount" {
			return true
		}
		argument, ok := call.Args[0].(*ast.Ident)
		if !ok || argument.Name != "scratch" && argument.Name != "staging" {
			t.Errorf("mount setup unmount target = %T, want an owned scratch or staging identifier", call.Args[0])
			return true
		}
		targets = append(targets, argument.Name)
		return true
	})
	sort.Strings(targets)
	if strings.Join(targets, ",") != "scratch,staging" {
		t.Fatalf("production mount setup unmounts %v, want its two owned mounts", targets)
	}
}

func containsName(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func packageSource(t *testing.T, name string) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve package source directory")
	}
	return filepath.Join(filepath.Dir(current), name)
}

func functionDeclaration(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name {
			return function
		}
	}
	t.Fatalf("function %s not found", name)
	return nil
}

func qualifiedCallName(call *ast.CallExpr) string {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	identifier, ok := selector.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return identifier.Name + "." + selector.Sel.Name
}

func sysProcAttrFields(t *testing.T, file *ast.File, function string) map[string]ast.Expr {
	t.Helper()
	fields := map[string]ast.Expr{}
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.FuncDecl)
		if !ok || decl.Name.Name != function {
			return true
		}
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			selector, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "SysProcAttr" {
				return true
			}
			for _, element := range literal.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := pair.Key.(*ast.Ident)
				if ok {
					fields[key.Name] = pair.Value
				}
			}
			return false
		})
		return false
	})
	if len(fields) == 0 {
		t.Fatalf("%s SysProcAttr literal not found", function)
	}
	return fields
}

func selectorNames(expression ast.Node) []string {
	var names []string
	ast.Inspect(expression, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			names = append(names, selector.Sel.Name)
		}
		return true
	})
	sort.Strings(names)
	return names
}
