// Package mirrortest is the conformance suite every daemon-side fact source
// runs: the real store in internal/mirror, and the in-memory stand-in in
// internal/fake, unmodified.
//
// It exists for the reason credtest and agenttest do, sharpened by what this
// seam is for. The v2 publication check is only as trustworthy as the guarantee
// that its facts came from somewhere the run could not reach, and every test of
// the checker is written against the fake. A fake that answered where the real
// store refuses — served a remembered head, let one claim epoch read another's
// pin, returned facts bound to a question nobody asked — would make the
// checker's whole suite agree with a verifier that skips those checks. So the
// contract is stated once, here, and both implementations are held to it.
//
// It is deliberately about the *contract* and not about git. Which named
// refusal a store produces is its own business — no consumer branches on them,
// they all fail closed — so the suite asserts that a refusal happened and that
// no facts came with it. Everything that turns on git's own classification of a
// failure belongs to internal/mirror's own tests, against a real repository.
package mirrortest

import (
	"context"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Store is the surface under test: the two writes a claim cycle needs and the
// one read a verification makes.
type Store interface {
	RecordClaim(ctx context.Context, ref core.RemoteClaimRef) (core.RemoteClaim, error)
	Claim(ctx context.Context, ref core.RemoteClaimRef) (core.RemoteClaim, error)
	Discard(ctx context.Context, ref core.RemoteClaimRef) error
	RemoteFacts(ctx context.Context, run core.RemoteRunRef) (core.RemotePublishFacts, error)
	Repository() string
}

// Harness is one implementation plus the control over the canonical remote a
// conformance case needs. The control is the world the store reads, never the
// store itself: a suite that arranged the answer through the implementation
// under test would be asserting that it agrees with itself.
type Harness struct {
	Store Store
	// Commit appends a commit to a branch of the canonical remote, creating it
	// from the fixture's base when it does not exist, and returns its object
	// name.
	Commit func(t *testing.T, branch string) string
	// Rewrite replaces a branch with an unrelated history — the force push.
	Rewrite func(t *testing.T, branch string) string
	// Delete removes a branch from the canonical remote.
	Delete func(t *testing.T, branch string)
}

// branch derives the canonical issue branch, independently of both
// implementations. Three computations of one name, on purpose: they must agree,
// and a suite that took the name from the store could not see them stop
// agreeing (AGENTS.md, Conventions — anchor a closed contract somewhere it does
// not depend on the declaration it checks).
func branch(key string) string { return "ben/" + key }

func claimRef(key string, epoch int64) core.RemoteClaimRef {
	return core.RemoteClaimRef{Issue: key, Key: key, Epoch: epoch}
}

func runRef(ref core.RemoteClaimRef, run, verification string) core.RemoteRunRef {
	return core.RemoteRunRef{Claim: ref, Run: run, Verification: verification}
}

// Contract runs the suite. newHarness is called once per case, so no case can
// inherit another's store.
func Contract(t *testing.T, newHarness func(*testing.T) Harness) {
	t.Helper()
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, h Harness)
	}{
		{"an unrecorded claim has no facts", unrecordedClaimRefuses},
		{"a recorded claim with no branch observes absence", absentBranchObserved},
		{"a published descendant is observed and ordered", descendantObserved},
		{"a claim that added nothing observes its own base", noOpObserved},
		{"a rewritten branch does not descend", rewriteObserved},
		{"a deleted branch is not remembered", deletedBranchNotRemembered},
		{"facts echo the request they answer", factsEchoTheRequest},
		{"another epoch cannot read this claim's pin", epochsDoNotMix},
		{"re-recording one epoch does not move its base", recordIsIdempotent},
		{"a new epoch re-pins to the branch as it now stands", newEpochRepins},
		{"a stale discard does not forget the current epoch", staleDiscardKeepsCurrentEpoch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, newHarness(t))
		})
	}
}

// staleDiscardKeepsCurrentEpoch covers cleanup arriving after a new claim has
// already replaced the key's pin. Cleanup owns the epoch it was given, not the
// key forever; deleting the current record would strand the running cycle.
func staleDiscardKeepsCurrentEpoch(t *testing.T, h Harness) {
	ctx := context.Background()
	first := claimRef("discard", 1)
	if _, err := h.Store.RecordClaim(ctx, first); err != nil {
		t.Fatalf("RecordClaim (first): %v", err)
	}
	h.Commit(t, branch(first.Key))
	second := claimRef("discard", 2)
	want, err := h.Store.RecordClaim(ctx, second)
	if err != nil {
		t.Fatalf("RecordClaim (second): %v", err)
	}
	if err := h.Store.Discard(ctx, first); err != nil {
		t.Fatalf("Discard (stale): %v", err)
	}
	got, err := h.Store.Claim(ctx, second)
	if err != nil {
		t.Fatalf("Claim (current) after stale discard: %v", err)
	}
	if got != want {
		t.Fatalf("Claim (current) = %+v after stale discard, want %+v", got, want)
	}
}

// unrecordedClaimRefuses is the fail-closed floor. Without a pin there is no
// "advanced" to assert — every commit descends from nothing — so a store that
// answered here would let a run publishing somebody else's branch verify.
func unrecordedClaimRefuses(t *testing.T, h Harness) {
	ref := claimRef("unrecorded", 1)
	h.Commit(t, branch(ref.Key))

	facts, err := h.Store.RemoteFacts(context.Background(), runRef(ref, "run-1", "v1"))
	if err == nil {
		t.Fatalf("RemoteFacts for an unrecorded claim returned %+v, want a refusal", facts)
	}
	if facts != (core.RemotePublishFacts{}) {
		t.Errorf("a refused observation returned facts %+v, want the zero value: a caller that "+
			"reads facts beside an error reads a verdict nobody reached", facts)
	}
}

func absentBranchObserved(t *testing.T, h Harness) {
	ctx := context.Background()
	ref := claimRef("absent", 1)
	claim, err := h.Store.RecordClaim(ctx, ref)
	if err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	if claim.BaseSHA == "" {
		t.Fatal("a recorded claim carries no base: there is nothing for leg 1 to measure against")
	}
	if claim.TargetBranch == "" {
		t.Fatal("a recorded claim carries no target: there is nothing to bind the pull-request base to")
	}
	if claim.Branch != branch(ref.Key) {
		t.Errorf("claim branch = %q, want %q", claim.Branch, branch(ref.Key))
	}

	facts, err := h.Store.RemoteFacts(ctx, runRef(ref, "run-1", "v1"))
	if err != nil {
		t.Fatalf("RemoteFacts: %v", err)
	}
	if !facts.Fetched {
		t.Error("Fetched is false on an answered observation: a consumer cannot tell it from a remembered one")
	}
	if facts.RemoteHead != "" {
		t.Errorf("RemoteHead = %q, want empty: the canonical remote has no such branch", facts.RemoteHead)
	}
	if facts.DescendsBase {
		t.Error("DescendsBase is true for a branch that does not exist")
	}
	if facts.BaseSHA != claim.BaseSHA {
		t.Errorf("facts pin %s, the claim pins %s", facts.BaseSHA, claim.BaseSHA)
	}
	if facts.Repository != h.Store.Repository() {
		t.Errorf("facts report repository %q, the store reads %q", facts.Repository, h.Store.Repository())
	}
}

func descendantObserved(t *testing.T, h Harness) {
	ctx := context.Background()
	ref := claimRef("descendant", 1)
	claim, err := h.Store.RecordClaim(ctx, ref)
	if err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	head := h.Commit(t, branch(ref.Key))

	facts, err := h.Store.RemoteFacts(ctx, runRef(ref, "run-1", "v1"))
	if err != nil {
		t.Fatalf("RemoteFacts: %v", err)
	}
	if facts.RemoteHead != head {
		t.Errorf("RemoteHead = %s, the canonical remote is at %s", facts.RemoteHead, head)
	}
	if !facts.DescendsBase {
		t.Errorf("a commit added on top of the base %s does not descend from it", claim.BaseSHA)
	}
	if !facts.Fetched {
		t.Error("Fetched is false on an answered observation")
	}
}

// noOpObserved is the claim whose run added nothing: the branch exists and is
// exactly the pin. The store reports both facts and calls neither a verdict —
// descent is reflexive, and "advanced" is the consumer's question.
func noOpObserved(t *testing.T, h Harness) {
	ctx := context.Background()
	ref := claimRef("noop", 1)
	head := h.Commit(t, branch(ref.Key))
	claim, err := h.Store.RecordClaim(ctx, ref)
	if err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	if claim.BaseSHA != head {
		t.Fatalf("a claim over an existing branch pinned %s, the branch is at %s: the pin must be the "+
			"branch's head at claim time, or handed-off work reads as this run's", claim.BaseSHA, head)
	}

	facts, err := h.Store.RemoteFacts(ctx, runRef(ref, "run-1", "v1"))
	if err != nil {
		t.Fatalf("RemoteFacts: %v", err)
	}
	if facts.RemoteHead != claim.BaseSHA {
		t.Errorf("RemoteHead = %s, want the untouched base %s", facts.RemoteHead, claim.BaseSHA)
	}
	if !facts.DescendsBase {
		t.Error("DescendsBase is false for a branch still at its base: descent is reflexive, and a " +
			"consumer that tells a no-op from a rewrite by this field would misread one as the other")
	}
}

func rewriteObserved(t *testing.T, h Harness) {
	ctx := context.Background()
	ref := claimRef("rewritten", 1)
	if _, err := h.Store.RecordClaim(ctx, ref); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	h.Commit(t, branch(ref.Key))
	head := h.Rewrite(t, branch(ref.Key))

	facts, err := h.Store.RemoteFacts(ctx, runRef(ref, "run-1", "v1"))
	if err != nil {
		t.Fatalf("RemoteFacts: %v", err)
	}
	if facts.RemoteHead != head {
		t.Errorf("RemoteHead = %s, the canonical remote is at %s", facts.RemoteHead, head)
	}
	if facts.DescendsBase {
		t.Error("a force-pushed branch of unrelated history reports as descending from the claim-time base")
	}
}

// deletedBranchNotRemembered is the staleness case at the source. A store that
// cached the last head it fetched would keep reporting a publication that has
// since been deleted, and every leg above it would agree.
func deletedBranchNotRemembered(t *testing.T, h Harness) {
	ctx := context.Background()
	ref := claimRef("deleted", 1)
	if _, err := h.Store.RecordClaim(ctx, ref); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	h.Commit(t, branch(ref.Key))
	if _, err := h.Store.RemoteFacts(ctx, runRef(ref, "run-1", "v1")); err != nil {
		t.Fatalf("RemoteFacts: %v", err)
	}
	h.Delete(t, branch(ref.Key))

	facts, err := h.Store.RemoteFacts(ctx, runRef(ref, "run-1", "v2"))
	if err != nil {
		t.Fatalf("RemoteFacts after the branch was deleted: %v", err)
	}
	if facts.RemoteHead != "" {
		t.Errorf("RemoteHead = %s after the branch was deleted: this is a remembered head, not an observed one",
			facts.RemoteHead)
	}
}

// factsEchoTheRequest is what makes the binding checkable. A source that
// answered without echoing leaves its consumer unable to tell this
// verification's facts from a replay of an earlier one.
func factsEchoTheRequest(t *testing.T, h Harness) {
	ctx := context.Background()
	ref := claimRef("bound", 1)
	if _, err := h.Store.RecordClaim(ctx, ref); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	h.Commit(t, branch(ref.Key))

	for _, run := range []core.RemoteRunRef{
		runRef(ref, "run-1", "v1"),
		runRef(ref, "run-1", "v2"),
		runRef(ref, "run-2", "v3"),
	} {
		facts, err := h.Store.RemoteFacts(ctx, run)
		if err != nil {
			t.Fatalf("RemoteFacts(%+v): %v", run, err)
		}
		if facts.Run != run {
			t.Errorf("facts for %+v are bound to %+v", run, facts.Run)
		}
	}
}

// epochsDoNotMix is the case a remote branch's lifetime makes necessary: the
// publication of one claim cycle sits untouched on the canonical remote, and a
// later cycle over the same issue must not be able to read it as its own.
func epochsDoNotMix(t *testing.T, h Harness) {
	ctx := context.Background()
	first := claimRef("epochs", 1)
	if _, err := h.Store.RecordClaim(ctx, first); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	h.Commit(t, branch(first.Key))

	second := claimRef("epochs", 2)
	facts, err := h.Store.RemoteFacts(ctx, runRef(second, "run-2", "v1"))
	if err == nil {
		t.Fatalf("RemoteFacts for epoch %d against a pin recorded for epoch %d returned %+v, want a refusal",
			second.Epoch, first.Epoch, facts)
	}
	if facts != (core.RemotePublishFacts{}) {
		t.Errorf("a refused observation returned facts %+v, want the zero value", facts)
	}
}

// recordIsIdempotent covers the restart between recording a claim and starting
// its run. Re-recording must return the pin already taken: a store that re-read
// the branch would adopt whatever the run has pushed since as the baseline, and
// leg 1 would then be measuring the run against itself.
func recordIsIdempotent(t *testing.T, h Harness) {
	ctx := context.Background()
	ref := claimRef("idempotent", 1)
	first, err := h.Store.RecordClaim(ctx, ref)
	if err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	h.Commit(t, branch(ref.Key))

	again, err := h.Store.RecordClaim(ctx, ref)
	if err != nil {
		t.Fatalf("RecordClaim (again): %v", err)
	}
	if again.BaseSHA != first.BaseSHA {
		t.Errorf("re-recording epoch %d moved the base from %s to %s", ref.Epoch, first.BaseSHA, again.BaseSHA)
	}

	read, err := h.Store.Claim(ctx, ref)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if read.BaseSHA != first.BaseSHA {
		t.Errorf("Claim reads base %s, RecordClaim pinned %s", read.BaseSHA, first.BaseSHA)
	}
}

// newEpochRepins is the other half: a genuinely new claim cycle starts from the
// branch as it now stands, so the work the previous cycle published is not
// this one's to be credited with.
func newEpochRepins(t *testing.T, h Harness) {
	ctx := context.Background()
	first := claimRef("repin", 1)
	if _, err := h.Store.RecordClaim(ctx, first); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	head := h.Commit(t, branch(first.Key))

	second := claimRef("repin", 2)
	claim, err := h.Store.RecordClaim(ctx, second)
	if err != nil {
		t.Fatalf("RecordClaim (new epoch): %v", err)
	}
	if claim.BaseSHA != head {
		t.Errorf("epoch %d pinned %s, the branch stands at %s: the previous cycle's commits are inside "+
			"this cycle's range and would read as its work", second.Epoch, claim.BaseSHA, head)
	}

	facts, err := h.Store.RemoteFacts(ctx, runRef(second, "run-2", "v1"))
	if err != nil {
		t.Fatalf("RemoteFacts: %v", err)
	}
	if facts.RemoteHead != claim.BaseSHA {
		t.Errorf("RemoteHead = %s, want the freshly pinned %s", facts.RemoteHead, claim.BaseSHA)
	}
}
