package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/verify"
)

// The §9.7 checker and the loop that routes its answer are strangers by design
// (SPEC §11): `verify` says what the evidence is and refuses to name an action,
// the loop declares a narrow Verifier seam and imports no adapter, and neither
// may import the other. Binding them is an assembly's job — and this package is
// a second assembly, because `cmd/ben` is package main and unimportable.
//
// So this is a second binding of the same two enums, and the risk that creates
// is named rather than waved at: if either enum grows a member and only one
// binding is updated, the production shim and this one part company, and a suite
// that assembles its own would keep passing. cmd/ben/verifier_test.go pins the
// production one. TestTheVerdictBindingIsExhaustive below pins this one at an
// independent boundary — the enums themselves — so a new member fails here
// whatever this table says, which is the anchor AGENTS.md asks for whenever a
// test would otherwise be driven by the declaration it checks.

// newVerifier builds the real §9.7 checker over the scenario's fakes and adapts
// it to the loop's seam.
func newVerifier(ws verify.Workspaces, tr verify.Tracker) (orchestrator.Verifier, error) {
	c, err := verify.New(ws, tr)
	if err != nil {
		return nil, err
	}
	return checkedVerifier{c}, nil
}

type checkedVerifier struct {
	checker *verify.Checker
}

func (v checkedVerifier) Verify(ctx context.Context, issue core.Issue, ws core.Workspace) (orchestrator.VerifyResult, error) {
	res, err := v.checker.Verify(ctx, issue, ws)
	if err != nil {
		// The zero VerifyResult is VerdictUnknown, so a caller that ignored this
		// error still could not read the result as success (SPEC §9.7).
		return orchestrator.VerifyResult{}, err
	}
	verdict, err := routableVerdict(res.Verdict)
	if err != nil {
		return orchestrator.VerifyResult{}, err
	}
	return orchestrator.VerifyResult{Verdict: verdict, PRURL: res.PRURL, Detail: res.Detail}, nil
}

// verdictBinding is the translation, as data so that a test can check it covers
// both enums. Never a numeric conversion: the members line up today, so
// `orchestrator.Verdict(res.Verdict)` would compile and pass everything here,
// and would mistranslate in silence the first time either enum gains one.
var verdictBinding = map[verify.Verdict]orchestrator.Verdict{
	verify.VerdictPublished:    orchestrator.VerdictPublished,
	verify.VerdictIncomplete:   orchestrator.VerdictIncomplete,
	verify.VerdictContradicted: orchestrator.VerdictContradicted,
}

// routableVerdict translates one closed enum into the other, failing closed.
//
// verify.VerdictUnknown is deliberately absent from the table. It is "never an
// answer" and is only ever returned alongside an error, so reaching this line
// with a nil error means the checker broke its own contract — and a broken
// contract has no correct interpretation to pick. Erroring parks the issue,
// where guessing would spend an attempt, or at published be unrecoverable.
func routableVerdict(v verify.Verdict) (orchestrator.Verdict, error) {
	if routed, ok := verdictBinding[v]; ok {
		return routed, nil
	}
	return orchestrator.VerdictUnknown, fmt.Errorf("verification returned %s, which is not a routable verdict", v)
}

// TestTheVerdictBindingIsExhaustive anchors the table above at the enums rather
// than at itself.
//
// Both enums are unexported-width iotas whose String() falls through to a
// `Verdict(%d)` format for anything unrecognized, and that fallback is the
// boundary this reads: scan upward from zero, and every value that names itself
// is a member. A member added to either enum therefore appears here without this
// file being edited, and fails — which is the property a test driven by
// verdictBinding could not have, since a table missing an entry and a table that
// is complete look identical from inside it.
func TestTheVerdictBindingIsExhaustive(t *testing.T) {
	evidence := namedMembers(func(i int) string { return verify.Verdict(i).String() })
	routed := namedMembers(func(i int) string { return orchestrator.Verdict(i).String() })

	// Unknown is the zero value of both and is bound in neither direction; every
	// other member must be.
	for i := 1; i < evidence; i++ {
		v := verify.Verdict(i)
		got, err := routableVerdict(v)
		if err != nil {
			t.Errorf("routableVerdict(%s) = %v; every evidence verdict but unknown must route", v, err)
			continue
		}
		if got.String() != v.String() {
			t.Errorf("routableVerdict(%s) = %s; the two enums' members are the same words and must stay bound to their namesake", v, got)
		}
	}
	if evidence != routed {
		t.Errorf("verify has %d named verdicts and orchestrator has %d; one enum has grown a member the other has not, and the binding cannot be total",
			evidence, routed)
	}
	if len(verdictBinding) != evidence-1 {
		t.Errorf("the binding has %d entries for %d routable evidence verdicts", len(verdictBinding), evidence-1)
	}
}

// namedMembers counts the leading values of an iota enum that name themselves,
// stopping at the first that falls through to its numeric format.
func namedMembers(name func(int) string) int {
	// A ceiling, so a String() that somehow names every integer cannot hang the
	// suite. Nothing here is near it; if a verdict enum ever reaches 64 members
	// the failure this reports is the right one anyway.
	const ceiling = 64
	for i := range ceiling {
		if name(i) == fmt.Sprintf("Verdict(%d)", i) {
			return i
		}
	}
	return ceiling
}
