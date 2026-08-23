package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/verify"
)

// Consumer-defined seams cost nothing only if the real implementations satisfy
// them (the pattern verify/contract_test.go establishes). Pinned at compile
// time here so B11's wiring is not the first thing to find out.
var (
	_ checker               = (*verify.Checker)(nil)
	_ orchestrator.Verifier = checkedVerifier{}
)

type checkerFunc func(context.Context, core.Issue, core.Workspace) (verify.Result, error)

func (f checkerFunc) Verify(ctx context.Context, i core.Issue, w core.Workspace) (verify.Result, error) {
	return f(ctx, i, w)
}

type stubWorkspaces struct{}

func (stubWorkspaces) PublishFacts(context.Context, core.Workspace) (core.PublishFacts, error) {
	return core.PublishFacts{}, nil
}

type stubTracker struct{}

func (stubTracker) FindPR(context.Context, core.Issue, string) (*core.PR, error) { return nil, nil }

// newVerifier's refusal contract is asserted directly here rather than left
// for `ben run` to discover during production assembly. verify.New
// refuses a missing seam because a verifier that silently skipped a leg would
// report published on less evidence than §9.7 names — and that refusal is only
// worth anything if this constructor propagates it instead of handing back a
// half-built checker.
func TestNewVerifierPropagatesTheSeamRefusal(t *testing.T) {
	if _, err := newVerifier(stubWorkspaces{}, stubTracker{}); err != nil {
		t.Fatalf("newVerifier with both seams: %v", err)
	}
	for _, tc := range []struct {
		name string
		ws   verify.Workspaces
		tr   verify.Tracker
	}{
		{name: "no workspace seam", tr: stubTracker{}},
		{name: "no tracker seam", ws: stubWorkspaces{}},
		{name: "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newVerifier(tc.ws, tc.tr)
			if err == nil {
				t.Fatalf("newVerifier = %v, want the missing seam refused", got)
			}
			if got != nil {
				t.Errorf("verifier = %v, want nil beside the error", got)
			}
		})
	}
}

func TestRoutableVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      verify.Verdict
		want    orchestrator.Verdict
		wantErr bool
	}{
		{name: "published", in: verify.VerdictPublished, want: orchestrator.VerdictPublished},
		{name: "incomplete", in: verify.VerdictIncomplete, want: orchestrator.VerdictIncomplete},
		{name: "contradicted", in: verify.VerdictContradicted, want: orchestrator.VerdictContradicted},
		// Never an answer: verify returns it only alongside an error, so a nil
		// error with this verdict is a checker that broke its own contract.
		{name: "unknown", in: verify.VerdictUnknown, want: orchestrator.VerdictUnknown, wantErr: true},
		// What a future verify member looks like to today's shim.
		{name: "out of range", in: verify.Verdict(99), want: orchestrator.VerdictUnknown, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := routableVerdict(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("verdict = %v, want %v", got, tc.want)
			}
		})
	}
}

// The guard that survives someone else's change: walk verify's enum instead of
// listing it. Verdict.String() falls back to "Verdict(%d)" for anything it does
// not name, so the first fallback marks the end of the real members — and a
// verdict added to verify without being taught to this shim fails here rather
// than reaching production as a silent mistranslation.
//
// A table alone could not do this. It would keep passing, having never heard of
// the new member.
func TestEveryVerdictVerifyNamesIsRoutable(t *testing.T) {
	const sanityBound = 64
	named := 0
	for i := range sanityBound {
		v := verify.Verdict(i)
		if strings.HasPrefix(v.String(), "Verdict(") {
			break
		}
		named++
		got, err := routableVerdict(v)
		if v == verify.VerdictUnknown {
			// The one named member that is deliberately not routable.
			if err == nil {
				t.Errorf("routableVerdict(%s) = %v, want the unstated verdict refused", v, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("verify names %s but the shim cannot route it: %v", v, err)
			continue
		}
		// Not a numeric identity: the names must agree, which is the property
		// a cast would satisfy today and break later.
		if got.String() != v.String() {
			t.Errorf("verify.%s maps to orchestrator.%s", v, got)
		}
	}
	if named < 4 {
		t.Fatalf("walked only %d named verdicts; the enum or its String() moved and this guard stopped guarding", named)
	}
}

func TestCheckedVerifierCarriesTheAnswerAndFailsClosed(t *testing.T) {
	t.Run("a published verdict carries its PR link", func(t *testing.T) {
		v := checkedVerifier{checkerFunc(func(context.Context, core.Issue, core.Workspace) (verify.Result, error) {
			return verify.Result{Verdict: verify.VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
		})}
		got, err := v.Verify(context.Background(), core.Issue{}, core.Workspace{})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if got.Verdict != orchestrator.VerdictPublished || got.PRURL != "https://example.test/pull/1" {
			t.Errorf("result = %+v, want published with the PR link", got)
		}
	})

	t.Run("a contradiction carries its operator line", func(t *testing.T) {
		v := checkedVerifier{checkerFunc(func(context.Context, core.Issue, core.Workspace) (verify.Result, error) {
			return verify.Result{Verdict: verify.VerdictContradicted, Detail: "no commits on the branch"}, nil
		})}
		got, err := v.Verify(context.Background(), core.Issue{}, core.Workspace{})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if got.Verdict != orchestrator.VerdictContradicted || got.Detail != "no commits on the branch" {
			t.Errorf("result = %+v, want the contradiction and its detail", got)
		}
	})

	t.Run("an error yields a result that cannot read as success", func(t *testing.T) {
		// The published verdict alongside an error is the shape §9.7 fails
		// closed on: the error decides, and the result must not survive it.
		want := errors.New("origin unreachable")
		v := checkedVerifier{checkerFunc(func(context.Context, core.Issue, core.Workspace) (verify.Result, error) {
			return verify.Result{Verdict: verify.VerdictPublished, PRURL: "https://example.test/pull/1"}, want
		})}
		got, err := v.Verify(context.Background(), core.Issue{}, core.Workspace{})
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want it returned unwrapped", err)
		}
		if got.Verdict != orchestrator.VerdictUnknown || got.PRURL != "" {
			t.Errorf("result = %+v, want the zero value beside an error", got)
		}
	})

	t.Run("an unstated verdict is refused rather than routed", func(t *testing.T) {
		v := checkedVerifier{checkerFunc(func(context.Context, core.Issue, core.Workspace) (verify.Result, error) {
			return verify.Result{}, nil
		})}
		got, err := v.Verify(context.Background(), core.Issue{}, core.Workspace{})
		if err == nil {
			t.Fatalf("result = %+v, want a checker that stated nothing refused", got)
		}
		if got.Verdict != orchestrator.VerdictUnknown {
			t.Errorf("verdict = %v, want the zero value", got.Verdict)
		}
	})
}
