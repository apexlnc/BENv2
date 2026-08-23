package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The claim-time base is the one fact in this fake with a contract worth
// asserting rather than just scripting. §9.7 leg 1 measures both "advanced past
// its claim-time base" and "descends from it" against it, so a base that moved
// between attempts would redefine the question on every continuation — and a
// verification test could then pass while modeling a check nothing performs.
//
// These are in-package on purpose: whether a *pin* was left behind is the
// property under test, and from outside only its value is visible.

func issueFor(identifier string) core.Issue {
	return core.Issue{Identifier: identifier, State: "open"}
}

// SPEC §6.2: pinned at the first claim-aware prepare and read back by every
// later prepare in that assignment epoch.
func TestPreparePinsTheBasePerWorkspaceNotPerAttempt(t *testing.T) {
	ctx := context.Background()
	w := NewWorkspaces()
	const epoch7 = 11
	if err := w.BeginClaimBase(ctx, issueFor("7"), epoch7); err != nil {
		t.Fatal(err)
	}

	first, _, err := w.PrepareClaim(ctx, issueFor("7"), 1, epoch7)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if first.BaseSHA != DefaultBaseSHA {
		t.Fatalf("base = %q, want the default head %q", first.BaseSHA, DefaultBaseSHA)
	}

	// A continuation, and then a retry: neither may move the base.
	for _, attempt := range []int{2, 3} {
		got, _, err := w.PrepareClaim(ctx, issueFor("7"), attempt, epoch7)
		if err != nil {
			t.Fatalf("Prepare attempt %d: %v", attempt, err)
		}
		if got.BaseSHA != first.BaseSHA {
			t.Errorf("attempt %d base = %q, want the pin %q", attempt, got.BaseSHA, first.BaseSHA)
		}
	}

	// A second issue off the same unchanged default branch shares that commit.
	// Distinctness is not a property of base SHAs — two fresh issues genuinely
	// pin to the same fetched default head — so the fake must not
	// promise one, or code reading BaseSHA as issue identity would pass here.
	if err := w.BeginClaimBase(ctx, issueFor("8"), 12); err != nil {
		t.Fatal(err)
	}
	other, _, err := w.PrepareClaim(ctx, issueFor("8"), 1, 12)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if other.BaseSHA != first.BaseSHA {
		t.Errorf("issue 8 base = %q, want the same default head as issue 7 (%q)", other.BaseSHA, first.BaseSHA)
	}

	// The default branch advances. A fresh issue pins to the new head; the two
	// already pinned keep theirs, which is what makes the base *claim-time*
	// rather than "wherever the default branch is now" (SPEC §6.2).
	const moved = "2222222222222222222222222222222222222222"
	w.SetDefaultBase(moved)
	if err := w.BeginClaimBase(ctx, issueFor("9"), 13); err != nil {
		t.Fatal(err)
	}
	fresh, _, err := w.PrepareClaim(ctx, issueFor("9"), 1, 13)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if fresh.BaseSHA != moved {
		t.Errorf("issue 9 base = %q, want the moved head %q", fresh.BaseSHA, moved)
	}
	again, _, err := w.PrepareClaim(ctx, issueFor("7"), 4, epoch7)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if again.BaseSHA != first.BaseSHA {
		t.Errorf("issue 7's pin followed the default branch to %q, want %q", again.BaseSHA, first.BaseSHA)
	}

	// What Prepares reports is what Prepare returned, since a test spanning
	// attempts asserts on the record rather than on every return value.
	for _, c := range w.Prepares("7") {
		if c.BaseSHA != first.BaseSHA {
			t.Errorf("recorded attempt %d base = %q, want %q", c.Attempt, c.BaseSHA, first.BaseSHA)
		}
	}
}

// A failure before pending→pinned leaves the durable pending intent in place;
// the retry completes that same epoch rather than manufacturing another one.
func TestAPrePinPrepareFailureLeavesPendingIntent(t *testing.T) {
	ctx := context.Background()
	w := NewWorkspaces()
	const epoch = 11
	if err := w.BeginClaimBase(ctx, issueFor("7"), epoch); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("fetching the base repository")
	w.FailPrepare = func(identifier string, attempt int) error {
		if identifier == "7" && attempt == 1 {
			return boom
		}
		return nil
	}

	ws, _, err := w.PrepareClaim(ctx, issueFor("7"), 1, epoch)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the scripted failure", err)
	}
	if ws.Path == "" || ws.Key == "" || ws.Branch == "" {
		t.Errorf("workspace = %+v, want the concrete provider's pre-pin identity", ws)
	}
	if ws.ClaimEpoch != 0 || ws.BaseSHA != "" {
		t.Errorf("workspace = %+v, want no authorizing pair beside a pre-pin failure", ws)
	}
	if state, err := w.ClaimBase(ctx, issueFor("7")); err != nil || state != (core.ClaimBase{State: core.ClaimBasePending, Epoch: epoch}) {
		t.Errorf("claim base = %+v, %v; want the original pending intent", state, err)
	}
	if got := w.Prepares("7"); len(got) != 1 || got[0].BaseSHA != "" {
		t.Errorf("recorded = %+v, want the failed attempt recorded with no base", got)
	}

	// Nothing was pinned, so the retry is still the *first* prepare as far as
	// the base is concerned: it pins to the default head in force then, which
	// is the head that moved while the failing attempt was getting nowhere.
	const moved = "2222222222222222222222222222222222222222"
	w.SetDefaultBase(moved)
	retry, _, err := w.PrepareClaim(ctx, issueFor("7"), 2, epoch)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if retry.BaseSHA != moved {
		t.Errorf("retry base = %q, want the head in force at the first prepare that got one (%q)", retry.BaseSHA, moved)
	}
}

// The other side of the line: the worktree exists and the provider keeps it for
// forensics (SPEC §6.6) — an after_create abort and the worktree refusals are
// all past the pin, so the workspace carries its base and the pin stands.
func TestAPostPinPrepareFailureCarriesTheBase(t *testing.T) {
	ctx := context.Background()
	w := NewWorkspaces()
	const epoch = 11
	if err := w.BeginClaimBase(ctx, issueFor("7"), epoch); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("after_create aborted workspace creation")
	w.SetPrepareErrorWithWorkspace(boom)

	ws, _, err := w.PrepareClaim(ctx, issueFor("7"), 1, epoch)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the scripted failure", err)
	}
	if ws.BaseSHA == "" {
		t.Fatal("the kept workspace carries no base; the real provider resolved one before this point")
	}
	if got := w.Prepares("7"); len(got) != 1 || got[0].BaseSHA != ws.BaseSHA {
		t.Errorf("recorded = %+v, want the base the call returned (%q)", got, ws.BaseSHA)
	}

	// The pin stands, so the retry verifies against the base the failed attempt
	// already established.
	w.SetPrepareErrorWithWorkspace(nil)
	retry, _, err := w.PrepareClaim(ctx, issueFor("7"), 2, epoch)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if retry.BaseSHA != ws.BaseSHA {
		t.Errorf("retry base = %q, want the standing pin %q", retry.BaseSHA, ws.BaseSHA)
	}
}

// PublishFacts refuses rather than answering the zero value: core.PublishFacts{}
// is "the branch does not exist", a verdict of its own, so a test that forgot to
// script evidence would get a contradiction it never asked for.
func TestPublishFactsFailsClosedWithoutScriptedEvidence(t *testing.T) {
	ctx := context.Background()
	w := NewWorkspaces()
	if err := w.BeginClaimBase(ctx, issueFor("7"), 11); err != nil {
		t.Fatal(err)
	}
	ws, _, err := w.PrepareClaim(ctx, issueFor("7"), 1, 11)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if facts, err := w.PublishFacts(ctx, ws); err == nil {
		t.Fatalf("PublishFacts() = %+v, nil; want a refusal when nothing was scripted", facts)
	}

	w.SetFacts(func(ws core.Workspace) (core.PublishFacts, error) {
		return core.PublishFacts{Head: ws.BaseSHA, DescendsBase: true}, nil
	})
	facts, err := w.PublishFacts(ctx, ws)
	if err != nil {
		t.Fatalf("PublishFacts: %v", err)
	}
	// The evidence is measured against the pin, which is what SetFacts hands the
	// workspace for.
	if facts.Head != ws.BaseSHA {
		t.Errorf("facts = %+v, want the scripted head %q", facts, ws.BaseSHA)
	}
}

// Pre-hook local evidence and post-run publication evidence are different
// observations. Scripting the latter must not silently answer the former: a
// fake that conflated them would let the hook-ordering defect in #94 pass.
func TestPrepareWithLocalFactsFailsClosedWithoutItsOwnScript(t *testing.T) {
	ctx := context.Background()
	w := NewWorkspaces()
	w.SetFacts(func(core.Workspace) (core.PublishFacts, error) {
		return core.PublishFacts{Head: "post-run"}, nil
	})

	if err := w.BeginClaimBase(ctx, issueFor("7"), 11); err != nil {
		t.Fatal(err)
	}
	first, _, err := w.PrepareClaim(ctx, issueFor("7"), 1, 11)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.BeginClaimBase(ctx, issueFor("7"), 12); err != nil {
		t.Fatal(err)
	}
	ws, facts, err := w.PrepareClaim(ctx, issueFor("7"), 1, 12)
	if err == nil {
		t.Fatalf("PrepareClaim() = (%+v, %+v, nil), want an unscripted-evidence refusal", ws, facts)
	}

	const advanced = "2222222222222222222222222222222222222222"
	w.SetPrepareFacts(func(ws core.Workspace) (core.LocalBranchFacts, error) {
		return core.LocalBranchFacts{Head: advanced, DescendsBase: true}, nil
	})
	ws, facts, err = w.PrepareClaim(ctx, issueFor("7"), 1, 12)
	if err != nil {
		t.Fatalf("PrepareClaim: %v", err)
	}
	if ws.BaseSHA != advanced || facts.Head != advanced || facts.BaseSHA != first.BaseSHA || !facts.DescendsBase {
		t.Errorf("workspace/facts = %+v / %+v, want head %q against outgoing %q", ws, facts, advanced, first.BaseSHA)
	}
}

// AttemptFacts is the one evidence read here whose unscripted answer is the zero
// value rather than a refusal, and the difference is what the zero value *means*
// (SPEC §9.6, #61).
//
// core.PublishFacts{} is "the branch does not exist", a routing verdict, so a test
// that forgot to script it would get a contradiction it never asked for.
// core.AttemptFacts{} is "the attempt committed nothing" — the honest account of
// almost every run in this package's own tests, and one nothing routes on. What is
// still load-bearing is that an error stays an error: the loop must report an
// unread branch as unread, never as empty.
func TestAttemptFactsAnswersAnEmptyAccountUnscripted(t *testing.T) {
	ctx := context.Background()
	w := NewWorkspaces()
	if err := w.BeginClaimBase(ctx, issueFor("7"), 11); err != nil {
		t.Fatal(err)
	}
	ws, _, err := w.PrepareClaim(ctx, issueFor("7"), 1, 11)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	facts, err := w.AttemptFacts(ctx, ws)
	if err != nil {
		t.Fatalf("AttemptFacts unscripted = %v, want the empty account", err)
	}
	if len(facts.Commits) != 0 || len(facts.Files) != 0 {
		t.Errorf("facts = %+v, want the empty account", facts)
	}

	// And a scripted failure is a failure, not an empty account.
	fail := errors.New("fatal: not a git repository")
	w.SetAttemptFacts(func(core.Workspace) (core.AttemptFacts, error) { return core.AttemptFacts{}, fail })
	if _, err := w.AttemptFacts(ctx, ws); !errors.Is(err, fail) {
		t.Errorf("AttemptFacts = %v, want the scripted failure", err)
	}
}
