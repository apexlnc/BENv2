package remotews_test

import (
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
)

// The quiet gate, the §9.10 run marker, and the end-of-claim disposal — the
// three places a possibly-live foreign process could otherwise come to share a
// workspace with a replacement (SPEC §9.8).

// A retry while the previous run's execution domain is unconfirmed is refused,
// and the refusal is the transient one the daemon retries rather than a failure
// of the claim.
func TestARetryWaitsForDomainQuiet(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)
	r.dispatch("run-1")

	// The stream ends. That is not termination, and it must not be readable as
	// one: the whole point of the three independent facts.
	r.backend.Complete("run-1")
	_, err := r.prepare(2, 11)
	wantErr(t, err, remote.ErrNotQuiet)

	// Even the direct process being reaped is not enough — descendants are a
	// separate fact.
	r.backend.Reap("run-1")
	_, err = r.prepare(2, 11)
	wantErr(t, err, remote.ErrNotQuiet)

	// Only an explicit domain-quiet observation authorizes reuse.
	r.backend.SetDomainQuiet("run-1")
	second, err := r.prepare(2, 11)
	if err != nil {
		t.Fatalf("PrepareClaim after domain quiet: %v", err)
	}
	if second.Path != first.Path {
		t.Fatalf("the retry moved to workspace %s, want the cycle's %s", second.Path, first.Path)
	}
	if got := r.sandbox().SandboxID; got == "" {
		t.Fatal("the retry left no sandbox")
	}
	// And the finished run's journal is retired, so the next dispatch can reserve
	// its own identity rather than colliding with a record for a run that is over.
	if _, err := remote.OpenJournal(r.journals, r.claim()); !isNoRecord(err) {
		t.Fatalf("the finished run's journal survived the retry: %v", err)
	}
	r.localIsUntouched()
}

// The §9.10 marker is read off the run journal rather than a second file, and
// its three answers are the journal's three shapes.
func TestTheRunMarkerIsTheRunJournal(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)

	marker, err := r.provider.ReadRunMarkerFor(r.issue)
	if err != nil {
		t.Fatal(err)
	}
	if marker.State != core.RunMarkerAbsent {
		t.Fatalf("a prepared workspace with no dispatch reads %v, want absent", marker.State)
	}

	// The launch precondition: a pinned verification base, or the attempt could
	// not be verified whatever it did.
	if err := r.provider.BeginRunMarkerFor(r.issue); err != nil {
		t.Fatalf("BeginRunMarkerFor: %v", err)
	}

	r.dispatch("run-1")
	marker, err = r.provider.ReadRunMarkerFor(r.issue)
	if err != nil {
		t.Fatal(err)
	}
	if marker.State != core.RunMarkerIdentified {
		t.Fatalf("a dispatched run reads %v, want identified", marker.State)
	}
	if marker.Evidence.Scheme != remotews.EvidenceScheme || marker.Evidence.ID != ws.Key {
		t.Fatalf("evidence is %+v, want this substrate's scheme naming %s", marker.Evidence, ws.Key)
	}
	// The evidence carries no boot identity: a workspace cycle is globally unique
	// by construction, unlike a pid.
	if marker.Evidence.Boot != "" {
		t.Fatalf("evidence carries a boot identity %q", marker.Evidence.Boot)
	}
}

// RunGone answers only on proof of absence, and every other case is
// possibly-live.
func TestRunGoneOnlyConfirmsOnPositiveEvidence(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	evidence := core.RunEvidence{Scheme: remotews.EvidenceScheme, ID: ws.Key}

	// No journal: nothing this daemon can name is running.
	gone, err := r.provider.RunGone(evidence)
	if err != nil || !gone {
		t.Fatalf("RunGone with no journal = %v, %v; want true", gone, err)
	}

	r.dispatch("run-1")
	if gone, err := r.provider.RunGone(evidence); err != nil || gone {
		t.Fatalf("RunGone over a live run = %v, %v; want false", gone, err)
	}

	// An unreachable control plane is the absence of an answer, never an answer.
	r.backend.SetUnreachable(true)
	if gone, err := r.provider.RunGone(evidence); gone || err == nil {
		t.Fatalf("RunGone over an unreachable backend = %v, %v; want false with an error", gone, err)
	}
	r.backend.SetUnreachable(false)

	r.backend.Quiet("run-1")
	if gone, err := r.provider.RunGone(evidence); err != nil || !gone {
		t.Fatalf("RunGone over a quiet run = %v, %v; want true", gone, err)
	}

	// Another substrate's evidence is refused rather than interpreted.
	_, err = r.provider.RunGone(core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "4242"})
	wantErr(t, err, remotews.ErrEvidenceScheme)
}

// Clearing the marker re-asks the backend rather than trusting its caller: the
// one route to removal §9.10 admits is positive evidence of absence, and on this
// substrate that is a fresh domain-quiet observation.
func TestClearingTheMarkerRequiresAFreshQuietObservation(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	r.dispatch("run-1")

	wantErr(t, r.provider.ClearRunMarkerFor(r.issue), remote.ErrNotQuiet)

	r.backend.Quiet("run-1")
	if err := r.provider.ClearRunMarkerFor(r.issue); err != nil {
		t.Fatalf("ClearRunMarkerFor after quiet: %v", err)
	}
	marker, err := r.provider.ReadRunMarkerFor(r.issue)
	if err != nil || marker.State != core.RunMarkerAbsent {
		t.Fatalf("after clearing: %+v, %v", marker, err)
	}
}

// A backend that definitively never accepted the dispatch is quiet. Without
// that reading a launch that never landed would leave a journal nothing could
// ever retire, and the claim would park forever over a run that does not exist.
func TestALaunchTheBackendNeverAcceptedIsQuiet(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	identity := func() remote.Identity {
		r.mustPrepare(1, 11)
		return r.sandbox()
	}()
	// A journal for a run the backend has never heard of: the shape a crash
	// between Reserve/Dispatch and the backend accepting produces.
	spec := remote.ProcessSpec{Identity: identity, Argv: []string{"agent"}}
	digest, err := remote.ProcessRequestDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	ref := remote.ProcessRef{Identity: identity, RunID: "ghost", RequestDigest: digest}
	journal, err := remote.Reserve(context.Background(), r.journals, ref, remote.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Dispatch(context.Background(),
		func(context.Context, remote.ProcessRef) (remote.Status, error) {
			return remote.Status{}, remote.ErrNoProcess
		}); err == nil {
		t.Fatal("the scripted dispatch failure did not surface")
	}

	if err := r.provider.ClearRunMarkerFor(r.issue); err != nil {
		t.Fatalf("a dispatch the backend never accepted did not clear: %v", err)
	}
}

// The ordinary §9.10 probe is observational because it is also used for
// revoked or completed claims. Exact replay is reachable only through
// ResolveRun, whose caller has already selected orphan backoff and supplies the
// current approval-cycle anchor.
func TestOnlyTheAuthorityGatedProbeReplaysAnUnansweredStart(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	identity := r.sandbox()
	spec := remote.ProcessSpec{Identity: identity, Argv: []string{"agent"}}
	digest, err := remote.ProcessRequestDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	ref := remote.ProcessRef{Identity: identity, RunID: "unanswered", RequestDigest: digest}
	journal, err := remote.Reserve(context.Background(), r.journals, ref, remote.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	r.backend.SetStartFault(context.DeadlineExceeded, false)
	if _, err := journal.Dispatch(context.Background(), func(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
		return r.backend.Start(ctx, ref, spec)
	}); err == nil {
		t.Fatal("the scripted unanswered Start did not fail")
	}

	resolves := 0
	resolver := func(ctx context.Context, got remote.ProcessRef) (remote.Status, error) {
		resolves++
		if got != ref {
			t.Fatalf("resolver got %s, want %s", got, ref)
		}
		return r.backend.Start(ctx, got, spec)
	}
	evidence := core.RunEvidence{Scheme: remotews.EvidenceScheme, ID: ws.Key}
	if gone, err := r.provider.RunGone(evidence); gone || !errors.Is(err, remote.ErrProcessUnresolved) {
		t.Fatalf("read-only RunGone = (%v, %v), want unresolved", gone, err)
	}
	if resolves != 0 || r.backend.RunCreations() != 0 {
		t.Fatalf("read-only RunGone made %d replays and %d runs", resolves, r.backend.RunCreations())
	}

	if gone, err := r.provider.ResolveRun(r.issue, evidence, r.claim().Epoch, resolver); err != nil || gone {
		t.Fatalf("authority-gated ResolveRun = (%v, %v), want one live run", gone, err)
	}
	if resolves != 1 || r.backend.RunCreations() != 1 {
		t.Fatalf("ResolveRun made %d replays and %d runs, want one of each", resolves, r.backend.RunCreations())
	}
	recorded, err := r.journals.Load(r.claim())
	if err != nil || recorded.BackendRunID == "" {
		t.Fatalf("resolved journal = %+v, %v; want the permanent backend id", recorded, err)
	}
}

// Removing and re-applying the queue label selects a new workspace cycle even
// when BEN's assignment never moves. The old cycle record and journal must not
// gain authority from that new approval: ResolveRun checks the persisted anchor
// and a fresh standing-anchor read before either Status or exact Start replay.
func TestResolveRunNeverReplaysASupersededApprovalWithTheSameAssignment(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	oldApproval := r.claim().Epoch
	identity := r.sandbox()
	spec := remote.ProcessSpec{Identity: identity, Argv: []string{"agent"}}
	digest, err := remote.ProcessRequestDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	ref := remote.ProcessRef{Identity: identity, RunID: "superseded", RequestDigest: digest}
	journal, err := remote.Reserve(context.Background(), r.journals, ref, remote.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	r.backend.SetStartFault(context.DeadlineExceeded, false)
	if _, err := journal.Dispatch(context.Background(), func(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
		return r.backend.Start(ctx, ref, spec)
	}); err == nil {
		t.Fatal("the scripted unanswered Start did not fail")
	}

	// Same issue and assignment epoch, but a new standing approval-label event.
	// BeginClaimBase has not replaced the old @100 cycle record yet.
	const reapproval = int64(200)
	r.cycles.set(reapproval)
	evidence := core.RunEvidence{Scheme: remotews.EvidenceScheme, ID: ws.Key}
	replays := 0
	resolver := func(ctx context.Context, got remote.ProcessRef) (remote.Status, error) {
		replays++
		return r.backend.Start(ctx, got, spec)
	}
	for _, tc := range []struct {
		name     string
		expected int64
	}{
		{name: "reapproval preceded the orchestrator history read", expected: reapproval},
		{name: "reapproval followed the orchestrator history read", expected: oldApproval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if gone, err := r.provider.ResolveRun(r.issue, evidence, tc.expected, resolver); gone || !errors.Is(err, remotews.ErrCycleState) {
				t.Fatalf("ResolveRun after same-assignment reapproval = (%v, %v), want ErrCycleState", gone, err)
			}
		})
	}
	if replays != 0 || r.backend.RunCreations() != 0 {
		t.Fatalf("superseded cycle made %d replays and %d runs", replays, r.backend.RunCreations())
	}
}

// A status read recovers an unanswered Start by adopting the exact active run.
// It remains non-quiet while live, then the ordinary quiet gate can retire its
// journal and let the claim proceed.
func TestAnUnansweredStartIsRecoveredWithoutAuthorizingEarlyReuse(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	identity := func() remote.Identity {
		r.mustPrepare(1, 11)
		return r.sandbox()
	}()
	spec := remote.ProcessSpec{Identity: identity, Argv: []string{"agent"}}
	digest, err := remote.ProcessRequestDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	ref := remote.ProcessRef{Identity: identity, RunID: "unanswered", RequestDigest: digest}
	journal, err := remote.Reserve(context.Background(), r.journals, ref, remote.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	r.backend.SetStartFault(context.DeadlineExceeded, true)
	if _, err := journal.Dispatch(context.Background(), func(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
		return r.backend.Start(ctx, ref, spec)
	}); err == nil {
		t.Fatal("the scripted lost response did not surface")
	}

	err = r.provider.ClearRunMarkerFor(r.issue)
	wantErr(t, err, remote.ErrNotQuiet)
	if _, err := remote.OpenJournal(r.journals, r.claim()); err != nil {
		t.Fatalf("the recovered live start's journal was retired: %v", err)
	}

	r.backend.Quiet("unanswered")
	if err := r.provider.ClearRunMarkerFor(r.issue); err != nil {
		t.Fatalf("clearing the recovered run after domain quiet: %v", err)
	}
	if _, err := remote.OpenJournal(r.journals, r.claim()); !isNoRecord(err) {
		t.Fatalf("the recovered quiet run's journal survived: %v", err)
	}
}

// A resource id proves Start was accepted. Losing access to that resource is
// therefore missing evidence, not proof that its execution domain is quiet.
func TestAnAcceptedButUnavailableRunIsNotQuiet(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	r.dispatch("run-1")
	r.backend.SetProcessUnavailable("run-1", true)

	err := r.provider.ClearRunMarkerFor(r.issue)
	wantErr(t, err, remote.ErrNotQuiet)
	wantErr(t, err, remote.ErrProcessUnavailable)
	if _, err := remote.OpenJournal(r.journals, r.claim()); err != nil {
		t.Fatalf("the unavailable accepted run's journal was retired: %v", err)
	}
}

// Disposal: the before_remove hook, the configured policy, then the durable
// records — and never while a run's termination is unconfirmed.
func TestDisposalOrderAndOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		keep        bool
		want        remotews.Outcome
		disposition remotews.Disposition
		// retained says the workspace-cycle record survives, which is §6.4's
		// forensics case and every suspend: the record is the only thing that can
		// name the sandbox for a human or a later claim under this approval.
		retained bool
	}{
		{name: "a published claim deleted by policy", keep: false, want: remotews.OutcomePublished,
			disposition: remotews.DispositionDeleted},
		{name: "a published claim suspended by policy", keep: false, want: remotews.OutcomePublished,
			disposition: remotews.DispositionRetained, retained: true},
		{name: "a failed claim retained for forensics", keep: true, want: remotews.OutcomeFailed,
			disposition: remotews.DispositionRetained, retained: true},
		{name: "a failed claim explicitly deleted", keep: true, want: remotews.OutcomeFailed,
			disposition: remotews.DispositionDeleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, withHooks(remote.Hooks{BeforeRemove: "echo remove"}))
			r.disposer.setDisposition(tc.disposition)
			ctx := context.Background()
			if err := r.begin(11); err != nil {
				t.Fatal(err)
			}
			ws := r.mustPrepare(1, 11)

			if err := r.provider.Dispose(ctx, ws, tc.keep); err != nil {
				t.Fatalf("Dispose: %v", err)
			}
			calls := r.disposer.Calls()
			if len(calls) != 1 || calls[0].Outcome != tc.want || calls[0].Claim != r.claim() {
				t.Fatalf("disposals %+v, want one %s for %s", calls, tc.want, r.claim())
			}
			if !calls[0].Quiet {
				t.Fatal("the disposal was not gated on a quiet run")
			}
			hooks := r.hookScripts()
			if hooks[len(hooks)-1] != string(remote.HookBeforeRemove) {
				t.Fatalf("hooks ran %v; before_remove must be last", hooks)
			}

			_, found, err := r.provider.ResolveWorkspace(ctx, r.issue)
			if err != nil {
				t.Fatal(err)
			}
			if found != tc.retained {
				t.Fatalf("the cycle record retained = %v, want %v", found, tc.retained)
			}
			// Repeated disposal after a crash must not fail.
			if err := r.provider.Dispose(ctx, ws, tc.keep); err != nil {
				t.Fatalf("a repeated Dispose: %v", err)
			}
		})
	}
}

func TestOutcomeSpecificPoliciesReachTheDisposer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		outcome     remotews.Outcome
		disposition remotews.Disposition
		retained    bool
		complete    func(context.Context, *remotews.Provider, core.Workspace) error
	}{
		{
			name: "failure", outcome: remotews.OutcomeFailed,
			disposition: remotews.DispositionRetained, retained: true,
			complete: func(ctx context.Context, p *remotews.Provider, ws core.Workspace) error {
				return p.CompleteFailure(ctx, ws)
			},
		},
		{
			name: "revocation", outcome: remotews.OutcomeRevoked,
			disposition: remotews.DispositionDeleted,
			complete: func(ctx context.Context, p *remotews.Provider, ws core.Workspace) error {
				return p.CompleteEndedCycle(ctx, ws)
			},
		},
		{
			name: "shutdown", outcome: remotews.OutcomeShutdown,
			disposition: remotews.DispositionDeleted, retained: true,
			complete: func(ctx context.Context, p *remotews.Provider, ws core.Workspace) error {
				return p.CompleteShutdown(ctx, ws)
			},
		},
		{
			// The suspend half of the same policy, which is the one an ended workspace
			// cycle reaches under the default `on_revoked`: the record survives,
			// because it is the only thing that can still name the sandbox for a human
			// (#252).
			name: "revocation suspended by policy", outcome: remotews.OutcomeRevoked,
			disposition: remotews.DispositionRetained, retained: true,
			complete: func(ctx context.Context, p *remotews.Provider, ws core.Workspace) error {
				return p.CompleteEndedCycle(ctx, ws)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.disposer.setDisposition(tc.disposition)
			ctx := context.Background()
			if err := r.begin(11); err != nil {
				t.Fatal(err)
			}
			ws := r.mustPrepare(1, 11)
			if err := tc.complete(ctx, r.provider, ws); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			calls := r.disposer.Calls()
			if len(calls) != 1 || calls[0].Outcome != tc.outcome {
				t.Fatalf("disposals = %+v, want one %s", calls, tc.outcome)
			}
			_, found, err := r.provider.ResolveWorkspace(ctx, r.issue)
			if err != nil {
				t.Fatal(err)
			}
			if found != tc.retained {
				t.Fatalf("cycle retained = %v, want %v", found, tc.retained)
			}
		})
	}
}

// The whole sequence a workspace cycle that outlives its claim takes: the claim
// publishes and disposes under `on_success: suspend` — which remote review
// forces, so the reviewer can resume the same tree — and only later does the
// tracker end the cycle and the `on_revoked` policy delete it (#252).
//
// Two disposals over one cycle is the shape, so the things worth asserting are
// the ones that change when it is: which outcomes reach the policy, that the
// durable identity retires on the delete and not before, that a replay after it
// is a no-op rather than a failure, and that the §6.5 script does not run a
// second time. The last is not enforced here — one firing is addressed by
// (claim, phase, attempt) and its terminal result is durable, so the second call
// resolves the recorded firing (remote.RunScript) — which is exactly why it is
// asserted at this boundary, where a change to the id or the call site would
// break it silently.
func TestEndedCycleDisposalOverAPublishedClaimsSandbox(t *testing.T) {
	t.Parallel()
	r := newRig(t, withHooks(remote.Hooks{BeforeRemove: "echo remove"}))
	r.disposer.setDisposition(remotews.DispositionRetained)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)

	if err := r.provider.Dispose(ctx, ws, false); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	fired := beforeRemoveFirings(r)
	if fired != 1 {
		t.Fatalf("before_remove fired %d times at the published disposal, want 1", fired)
	}

	r.disposer.setDisposition(remotews.DispositionDeleted)
	if err := r.provider.CompleteEndedCycle(ctx, ws); err != nil {
		t.Fatalf("CompleteRevocation at the end of the cycle: %v", err)
	}
	if after := beforeRemoveFirings(r); after != fired {
		t.Fatalf("before_remove ran %d times in total, want the %d the published disposal earned", after, fired)
	}
	if calls := r.disposer.Calls(); len(calls) != 2 || calls[1].Outcome != remotews.OutcomeRevoked {
		t.Fatalf("disposals = %+v, want the published one then a revoked one", calls)
	}

	// A confirmed delete retires the cycle identity and its pin, and a repeated
	// sweep over the retired record is a no-op rather than a failure — which is
	// what makes a lost response, a restart and a further tick all safe.
	if _, found, err := r.provider.ResolveWorkspace(ctx, r.issue); err != nil || found {
		t.Fatalf("cycle retained = %v (err %v) after a confirmed delete", found, err)
	}
	if err := r.provider.CompleteEndedCycle(ctx, ws); err != nil {
		t.Fatalf("a repeated ended-cycle disposal: %v", err)
	}
	if calls := r.disposer.Calls(); len(calls) != 2 {
		t.Fatalf("a replay re-applied the policy: %+v", calls)
	}
	r.localIsUntouched()
}

// beforeRemoveFirings is how many before_remove scripts the backend actually
// executed — not how many times the hook was asked for.
func beforeRemoveFirings(r *rig) int {
	n := 0
	for _, phase := range r.hookScripts() {
		if phase == string(remote.HookBeforeRemove) {
			n++
		}
	}
	return n
}

// A cycle record is keyed by workspace key, and the key is per *issue* — it
// outlives every cycle stored under it. So a completion owed for one approval can
// arrive after a revocation and a reapproval have installed another, and the
// record it resolves is then the replacement's.
//
// Applying it there is the one disposal nobody asked for: it would suspend or
// delete a sandbox that has just been acquired for live work, and on a delete
// retire the pin §9.7 verifies that work against. The address carries the
// approval anchor, so the caller's own workspace is enough to refuse.
func TestCompletionRefusesACycleTheRecordNoLongerNames(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.cycles.set(11)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	owed := r.mustPrepare(1, 11)

	// The label is revoked and a human approves again: a different approval, and
	// therefore a different cycle address under the same key.
	r.cycles.set(22)
	if err := r.begin(12); err != nil {
		t.Fatal(err)
	}
	replacement := r.mustPrepare(1, 12)
	if owed.Path == replacement.Path {
		t.Fatalf("both cycles address %q; this test needs a reapproval to move the address", owed.Path)
	}

	// A cycle this record has never named — an address from another approval
	// entirely — is refused rather than applied to whatever occupies the key.
	stale := owed
	stale.WorkspacePaths.Path = owed.Path + "-from-another-era"
	wantErr(t, r.provider.CompleteEndedCycle(ctx, stale), remotews.ErrCycleMoved)
	if calls := r.disposer.Calls(); len(calls) != 0 {
		t.Fatalf("the retention policy was applied to a replacement cycle: %+v", calls)
	}
	// The replacement is untouched: still recorded, still verifiable.
	if _, found, err := r.provider.ResolveWorkspace(ctx, r.issue); err != nil || !found {
		t.Fatalf("cycle retained = %v (err %v); a stale completion retired the replacement", found, err)
	}

	// A caller that names no cycle at all is refused on the same terms, and the
	// asymmetry is the point: the permissive reading of "no address" is "dispose
	// whichever cycle currently occupies this key", which is the bug above with the
	// evidence removed. Every real caller has one — a provider-produced
	// core.Workspace carries Cycle.Address whether it came from a prepare, a
	// resolve, or step 5's Cycle.Ref.
	wantErr(t, r.provider.CompleteEndedCycle(ctx, core.Workspace{Key: replacement.Key}), remotews.ErrCycleMoved)
	if calls := r.disposer.Calls(); len(calls) != 0 {
		t.Fatalf("a key-only workspace disposed a cycle it could not name: %+v", calls)
	}

	// And the completion the replacement *is* owed still lands.
	if err := r.provider.CompleteEndedCycle(ctx, replacement); err != nil {
		t.Fatalf("CompleteRevocation for the current cycle: %v", err)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 {
		t.Fatalf("disposals = %+v, want the replacement's own", calls)
	}
}

// CycleApproval is what lets the loop see a remove-and-reapply, and this is the
// fact it reports: which approval the *record* is anchored to, not which one is
// standing now.
//
// The two differ exactly when a required label was withdrawn and applied again,
// and the loop compares them. Reading the record rather than remembering an
// approval is the point — a withdrawal before the claim was even retained would
// have made a remembered value name the new approval beside the old sandbox.
func TestCycleApprovalReportsTheRecordsOwnAnchor(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.cycles.set(11)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)

	got, err := r.provider.CycleApproval(ctx, r.issue)
	if err != nil || got != 11 {
		t.Fatalf("CycleApproval = %d (err %v), want the recorded anchor 11", got, err)
	}

	// A revocation and a reapproval: the record moves to the new approval, and the
	// loop's comparison against the standing one is what sees it.
	r.cycles.set(22)
	if err := r.begin(12); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 12)
	if got, err := r.provider.CycleApproval(ctx, r.issue); err != nil || got != 22 {
		t.Fatalf("CycleApproval = %d (err %v) after a reapproval, want 22", got, err)
	}

	// An issue this provider holds no cycle for supersedes nothing.
	other := core.Issue{Identifier: "99"}
	if got, err := r.provider.CycleApproval(ctx, other); err != nil || got != 0 {
		t.Fatalf("CycleApproval = %d (err %v) for an issue with no record, want 0", got, err)
	}
}

// Nothing is disposed while a run's termination is unconfirmed, and the refusal
// reaches the caller rather than being logged and swallowed.
func TestDisposalRefusesWhileARunIsUnconfirmed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	r.dispatch("run-1")
	r.backend.Complete("run-1")

	wantErr(t, r.provider.Dispose(ctx, ws, false), remote.ErrNotQuiet)
	if calls := r.disposer.Calls(); len(calls) != 0 {
		t.Fatalf("the retention policy was applied over a live run: %+v", calls)
	}
}

// A strategy with no retention policy refuses rather than silently leaking a
// backend lease.
func TestDisposalWithoutAPolicyRefuses(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *remotews.Options) { o.Disposer = nil })
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	wantErr(t, r.provider.Dispose(ctx, ws, false), remotews.ErrNoDisposer)
}

// The account of a finished attempt is read daemon-side or not at all. The
// tempting second source — asking the sandbox for `git log` — is the agent's own
// report of its own work, and rendering it into the next prompt as an observation
// is what SPEC §3.5 rules out.
func TestAttemptFactsAreEmptyWithoutADaemonSideSource(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	facts, err := r.provider.AttemptFacts(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Commits) != 0 || len(facts.Files) != 0 {
		t.Fatalf("AttemptFacts invented %+v", facts)
	}
}

// The verification identity §9.7 binds to: this claim, this attempt, and the one
// observation being made.
func TestRunRefNamesTheClaimTheAttemptAndTheObservation(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)
	one, err := r.provider.RunRef(first, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !one.Complete() {
		t.Fatalf("run ref %+v is incomplete", one)
	}
	if one.Claim.Issue != issueID || one.Claim.Key != first.Key || one.Claim.Epoch != 11 {
		t.Fatalf("run ref names %+v", one.Claim)
	}

	second := r.mustPrepare(2, 11)
	two, err := r.provider.RunRef(second, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if two.Run == one.Run {
		t.Fatalf("attempts 1 and 2 share the run identity %q, so a verdict earned by one would settle the other", one.Run)
	}
	if two.Verification == one.Verification {
		t.Fatal("two observations share one verification identity")
	}

	// A workspace from another epoch cannot be verified against this record.
	stale := first
	stale.ClaimEpoch = 99
	_, err = r.provider.RunRef(stale, "v3")
	wantErr(t, err, remotews.ErrCycleState)
}

func isNoRecord(err error) bool {
	return err != nil && wrapsNoRecord(err)
}

func wrapsNoRecord(err error) bool {
	type unwrapper interface{ Unwrap() error }
	for err != nil {
		if err == remote.ErrNoRecord {
			return true
		}
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
