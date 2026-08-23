package orchestrator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// §9.8's parked label rule is gated on owedEffect.projection, and every test of
// that rule is driven by the mark — so all of them would still pass if a future
// label write forgot to set it, and the stale-label bug this ticket closed would
// be back on that path alone.
//
// This is the independent anchor the convention asks for (AGENTS.md): it holds at
// the boundary where a `ben:*` label actually reaches the tracker, and it fails if
// an unmarked write appears, if a write appears somewhere not named below, or if one
// of the places named below stops writing at all. Source-level rather than
// behavioural because that is the only place "these are all of them" can be observed.
func TestEveryStateLabelWriteIsOwedAsAProjection(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	// The functions allowed to write a `ben:*` label, and why each is one. Named
	// rather than counted: a count says "two exist" and passes if one is deleted and
	// a different one appears, while this fails on the write that is actually new.
	//
	// Each is the single point of its own kind. transitionCaused is the only §9.2 edge
	// that moves a label; projectRecovery is the only §9.10 verdict that does, and it
	// cannot go through transitionCaused because recovery is not *taking* an edge — it
	// restores the state the tracker already says the issue is in (adoptRecovered), and
	// routing a reconstructed `done` through queued → claimed → … would project four
	// labels to reach the one that already stands. applyParked is the deliberately
	// sticky epoch-fault exception: it restores the projection without taking the
	// needs-review → backoff edge, because taking that edge would authorize a timer
	// and prepare under a claim whose safety fact is missing.
	allowed := map[string]string{
		"transitionCaused": "SPEC §9.2's state edges",
		"projectRecovery":  "SPEC §9.10's recovery verdicts, which set state without taking an edge",
		"applyParked":      "SPEC §9.2/§9.8's sticky epoch-fault re-park, which must not take a dispatching edge",
	}

	var sites []string
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		// The node stack, so a call can be asked what encloses it.
		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)

			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetStateLabels" {
				return true
			}
			where := fset.Position(call.Pos()).String()
			sites = append(sites, where)
			if !enclosedByCallTo(stack, "oweProjection") {
				t.Errorf("%s: a ben:* label write not owed through oweProjection; §9.8's parked label rule cannot see it, so a poll response's labels would be trusted while this write is still pending",
					where)
			}
			fn := enclosingFunc(stack)
			if _, ok := allowed[fn]; !ok {
				t.Errorf("%s: a ben:* label write in %s, which is not one of the places allowed to make one. "+
					"Adding a third is a decision about where the projection comes from, not a detail — "+
					"make it here, with the reason, or route the write through an existing one",
					where, fn)
			}
			seen[fn] = true
			return true
		})
	}

	// The other half, which a per-site check cannot make: each allowed site still
	// exists. Without it, deleting the recovery projection — and with it §9.10 step
	// 4's tracker repair — leaves this test green.
	for fn, why := range allowed {
		if !seen[fn] {
			t.Errorf("no ben:* label write in %s; %s no longer projects one", fn, why)
		}
	}
	if len(sites) != len(allowed) {
		t.Errorf("ben:* label writes = %v, want one per allowed site (%d)", sites, len(allowed))
	}
}

// enclosingFunc names the innermost function declaration on the stack.
func enclosingFunc(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if fn, ok := stack[i].(*ast.FuncDecl); ok {
			return fn.Name.Name
		}
	}
	return "(no enclosing function)"
}

// enclosedByCallTo reports whether any node on the stack is a call to the named
// method — i.e. whether the innermost node sits inside its arguments, which for
// oweProjection means inside the effect closure it will run.
func enclosedByCallTo(stack []ast.Node, method string) bool {
	for _, n := range stack {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
			return true
		}
	}
	return false
}
