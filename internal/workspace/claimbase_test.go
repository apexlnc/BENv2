package workspace

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

func TestClaimBaseLifecycleScopesPinsToAssignmentEpoch(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})
	iss := issue("7")

	state, err := p.ClaimBase(ctx, iss)
	if err != nil || state != (core.ClaimBase{State: core.ClaimBaseAbsent}) {
		t.Fatalf("initial ClaimBase = %+v, %v; want absent", state, err)
	}
	const firstEpoch = 41
	if err := p.BeginClaimBase(ctx, iss, firstEpoch); err != nil {
		t.Fatalf("BeginClaimBase(first): %v", err)
	}
	wantPending := core.ClaimBase{State: core.ClaimBasePending, Epoch: firstEpoch}
	if state, err = p.ClaimBase(ctx, iss); err != nil || state != wantPending {
		t.Fatalf("pending ClaimBase = %+v, %v; want %+v", state, err, wantPending)
	}

	// A fresh provider reads the same pending intent, and repeating that epoch is
	// a no-op. A different epoch cannot overwrite an unfinished transition.
	p2, err := providerFromOptions(t, Options{Root: p.root, WorkflowKey: "wf", Repository: repo(f.origin), Locks: p.LockDomain(), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.claimBasePath("7"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p2.BeginClaimBase(ctx, iss, firstEpoch); err != nil {
		t.Fatalf("BeginClaimBase(same pending): %v", err)
	}
	after, err := os.ReadFile(p.claimBasePath("7"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("idempotent BeginClaimBase rewrote the record: before=%s after=%s", before, after)
	}
	if err := p2.BeginClaimBase(ctx, iss, firstEpoch+1); !errors.Is(err, ErrClaimBaseState) {
		t.Fatalf("conflicting pending BeginClaimBase = %v, want ErrClaimBaseState", err)
	}

	first, prior, err := p2.PrepareClaim(ctx, iss, 1, firstEpoch)
	if err != nil {
		t.Fatalf("PrepareClaim(first): %v", err)
	}
	if first.ClaimEpoch != firstEpoch || first.BaseSHA != f.head(t) {
		t.Errorf("first workspace epoch/base = %d/%s, want %d/%s", first.ClaimEpoch, first.BaseSHA, firstEpoch, f.head(t))
	}
	if prior != (core.LocalBranchFacts{}) {
		t.Errorf("fresh claim prior facts = %+v, want none without an outgoing pin", prior)
	}
	firstPin := first.BaseSHA
	firstState := core.ClaimBase{
		State: core.ClaimBasePinned, Epoch: firstEpoch, BaseSHA: firstPin, TargetBranch: "main",
	}
	if state, err = p2.ClaimBase(ctx, iss); err != nil || state != firstState {
		t.Fatalf("first pinned ClaimBase = %+v, %v", state, err)
	}

	priorHead := agentCommit(t, first.Path, "prior-claim.txt")
	if err := p2.Dispose(ctx, first, false); err != nil {
		t.Fatalf("Dispose(first claim): %v", err)
	}
	if state, err = p2.ClaimBase(ctx, iss); err != nil || state != firstState {
		t.Fatalf("ClaimBase after disposal = %+v, %v; disposal must retain the outgoing pair", state, err)
	}
	if got := runGit(t, p.baseDir, "rev-parse", claimBaseRef("7", firstEpoch)); got != firstPin {
		t.Fatalf("claim pin after disposal = %s, want retained %s", got, firstPin)
	}
	const secondEpoch = 92
	if err := p2.BeginClaimBase(ctx, iss, secondEpoch); err != nil {
		t.Fatalf("BeginClaimBase(second): %v", err)
	}
	wantPending = core.ClaimBase{
		State: core.ClaimBasePending, Epoch: secondEpoch,
		OutgoingEpoch: firstEpoch, OutgoingBaseSHA: firstPin, OutgoingTargetBranch: "main",
	}
	if state, err = p2.ClaimBase(ctx, iss); err != nil || state != wantPending {
		t.Fatalf("second pending ClaimBase = %+v, %v; want %+v", state, err, wantPending)
	}
	second, prior, err := p2.PrepareClaim(ctx, iss, 1, secondEpoch)
	if err != nil {
		t.Fatalf("PrepareClaim(second): %v", err)
	}
	if second.ClaimEpoch != secondEpoch || second.BaseSHA != priorHead {
		t.Errorf("second workspace epoch/base = %d/%s, want %d/%s", second.ClaimEpoch, second.BaseSHA, secondEpoch, priorHead)
	}
	if prior.Head != priorHead || !prior.AdvancedPastBase(firstPin) {
		t.Errorf("second-claim prior facts = %+v, want head %s past outgoing %s", prior, priorHead, firstPin)
	}
	if got := runGit(t, p.baseDir, "rev-parse", claimBaseRef("7", firstEpoch)); got != firstPin {
		t.Errorf("outgoing pin was not retained: got %s, want %s", got, firstPin)
	}

	// Retry in the same epoch does not move the base, even after the branch does.
	agentCommit(t, second.Path, "current-claim.txt")
	retry, retryPrior, err := p2.PrepareClaim(ctx, iss, 2, secondEpoch)
	if err != nil {
		t.Fatalf("PrepareClaim(retry): %v", err)
	}
	if retry.BaseSHA != priorHead || retry.ClaimEpoch != secondEpoch {
		t.Errorf("same-epoch retry reminted the base: %+v", retry)
	}
	if retryPrior != (core.LocalBranchFacts{}) {
		t.Errorf("same-epoch retry returned a repin observation: %+v", retryPrior)
	}
}

func TestAbandonPendingClaimBaseAllowsALaterAssignment(t *testing.T) {
	parallel(t)
	ctx := context.Background()

	t.Run("fresh pending becomes absent", func(t *testing.T) {
		p := newProvider(t, newFixture(t), Hooks{})
		iss := issue("7")
		if err := p.BeginClaimBase(ctx, iss, 41); err != nil {
			t.Fatal(err)
		}
		if err := p.AbandonPendingClaimBase(ctx, iss); err != nil {
			t.Fatalf("AbandonPendingClaimBase: %v", err)
		}
		if got, err := p.ClaimBase(ctx, iss); err != nil || got.State != core.ClaimBaseAbsent {
			t.Fatalf("ClaimBase after abandonment = %+v, %v; want absent", got, err)
		}
		if err := p.BeginClaimBase(ctx, iss, 92); err != nil {
			t.Fatalf("later assignment remained blocked: %v", err)
		}
	})

	t.Run("outgoing pin is restored", func(t *testing.T) {
		f := newFixture(t)
		p := newProvider(t, f, Hooks{})
		iss := issue("8")
		const (
			outgoing  = int64(31)
			abandoned = int64(41)
			later     = int64(92)
		)
		if err := p.BeginClaimBase(ctx, iss, outgoing); err != nil {
			t.Fatal(err)
		}
		first, _, err := p.PrepareClaim(ctx, iss, 1, outgoing)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.BeginClaimBase(ctx, iss, abandoned); err != nil {
			t.Fatal(err)
		}
		// Model a pin write that published its reachability root but failed
		// before the record rename. Pending still authorizes only outgoing.
		if _, err := p.baseGit(ctx, "update-ref", claimBaseRef("8", abandoned), first.BaseSHA); err != nil {
			t.Fatal(err)
		}

		if err := p.AbandonPendingClaimBase(ctx, iss); err != nil {
			t.Fatalf("AbandonPendingClaimBase: %v", err)
		}
		want := core.ClaimBase{
			State: core.ClaimBasePinned, Epoch: outgoing, BaseSHA: first.BaseSHA, TargetBranch: "main",
		}
		if got, err := p.ClaimBase(ctx, iss); err != nil || got != want {
			t.Fatalf("ClaimBase after abandonment = %+v, %v; want %+v", got, err, want)
		}
		if _, ok, err := p.revParse(ctx, claimBaseRef("8", abandoned)); err != nil || ok {
			t.Errorf("abandoned reachability ref = present %v, err %v; want absent", ok, err)
		}
		if err := p.BeginClaimBase(ctx, iss, later); err != nil {
			t.Fatalf("later assignment remained blocked: %v", err)
		}
		wantPending := core.ClaimBase{
			State: core.ClaimBasePending, Epoch: later,
			OutgoingEpoch: outgoing, OutgoingBaseSHA: first.BaseSHA, OutgoingTargetBranch: "main",
		}
		if got, err := p.ClaimBase(ctx, iss); err != nil || got != wantPending {
			t.Fatalf("later pending state = %+v, %v; want %+v", got, err, wantPending)
		}
	})
}

func TestClaimBaseRefusesNonPositiveEpoch(t *testing.T) {
	parallel(t)
	p := newProvider(t, newFixture(t), Hooks{})
	for _, epoch := range []int64{0, -1} {
		if err := p.BeginClaimBase(context.Background(), issue("7"), epoch); !errors.Is(err, ErrClaimEpoch) {
			t.Errorf("BeginClaimBase(epoch=%d) = %v, want ErrClaimEpoch", epoch, err)
		}
		if _, _, err := p.PrepareClaim(context.Background(), issue("7"), 1, epoch); !errors.Is(err, ErrClaimEpoch) {
			t.Errorf("PrepareClaim(epoch=%d) = %v, want ErrClaimEpoch", epoch, err)
		}
	}
}

func TestClaimBaseMalformedRecordFailsClosed(t *testing.T) {
	parallel(t)
	tests := []struct {
		name string
		body string
	}{
		{"unknown state", `{"version":1,"state":"maybe","epoch":1}`},
		{"zero epoch", `{"version":1,"state":"pending","epoch":0}`},
		{"pending with pinned base", `{"version":1,"state":"pending","epoch":1,"base_sha":"abc"}`},
		{"pinned without base", `{"version":1,"state":"pinned","epoch":1}`},
		{"unknown field", `{"version":1,"state":"pending","epoch":1,"surprise":true}`},
		{"multiple values", `{"version":1,"state":"pending","epoch":1}{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProvider(t, newFixture(t), Hooks{})
			if err := os.MkdirAll(p.claimBaseDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, p.claimBasePath("7"), tt.body)
			if _, err := p.ClaimBase(context.Background(), issue("7")); !errors.Is(err, ErrClaimBaseState) {
				t.Fatalf("ClaimBase = %v, want ErrClaimBaseState", err)
			}
		})
	}
}

func TestClaimBaseMissingReachabilityRefFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, p.baseDir, "update-ref", "-d", claimBaseRef("7", ws.ClaimEpoch))
	if _, err := p.ClaimBase(ctx, issue("7")); !errors.Is(err, ErrClaimBaseState) {
		t.Fatalf("ClaimBase after ref loss = %v, want ErrClaimBaseState", err)
	}
}
