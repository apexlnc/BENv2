package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// A run record and a held claim each block an identity change, and an empty loop
// permits one.
//
// The barrier exists because the question is about the record set, and the record
// set is loop-owned. Answered from any other goroutine it is answered and then
// invalidated — which is the next test.
func TestAdoptIdentityRefusesWhileWorkIsOutstanding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*Orchestrator)
		want    bool
	}{
		{"nothing outstanding", func(*Orchestrator) {}, true},
		{"a run record", func(o *Orchestrator) {
			o.records["1"] = &Record{Issue: fake.Issue("1", epoch), State: StateRunning}
		}, false},
		{"a held claim", func(o *Orchestrator) {
			o.held["1"] = &heldClaim{issue: fake.Issue("1", epoch), cycleAnchor: 1}
		}, false},
		{
			// An ended workspace cycle owing its disposal, and the only one of these
			// three that can be outstanding with neither a record nor a held claim
			// behind it (#252, cycle.go). It names a cycle address in *this*
			// provider's store, so carrying it across an identity change would ask a
			// different backend under a different principal to dispose an address it
			// never issued.
			"an ended workspace cycle's disposal", func(o *Orchestrator) {
				o.endedCycles = map[string]*endedCycle{
					"1": {workspace: core.Workspace{Key: "issue-1"}, why: "issue went terminal"},
				}
			}, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := idleWithSource(t, fake.NewTracker())
			tc.arrange(o)

			ran := false
			// An identity change: a different principal is what outstanding work
			// is bound to.
			cur, _ := o.cfg.Runtime.Load()
			moved := *cur.Runtime
			moved.ClaimPrincipal = "someone-else"
			a := &adoption{ack: make(chan error, 1), prev: cur.Runtime, next: &moved, commit: func() { ran = true }}
			// Driven through the loop's handler directly: this is the authority
			// goroutine's step, and running it here is what running it there means.
			o.onAdopt(a)

			err := <-a.ack
			if tc.want {
				if err != nil || !ran {
					t.Errorf("adoption refused with nothing outstanding: err = %v, commit ran = %v", err, ran)
				}
				return
			}
			if ran {
				t.Error("the commit ran while work bound to the old identity was outstanding")
			}
			if !errors.Is(err, config.ErrWorkOutstanding) {
				t.Errorf("err = %v, want ErrWorkOutstanding — the watcher defers on that and retries on anything else", err)
			}
		})
	}
}

// The test and the commit are one loop step, so a claim cannot land between them.
//
// This is the TOCTOU the advisory read cannot close: IdentityQuiescent runs on the
// caller's goroutine, and dispatch claims on the authority goroutine. The two
// answers are driven into the same window here — the advisory read says quiescent,
// then a record appears, then the publication is attempted — and the disjunction is
// what must hold: either the claim exists and the adoption was refused, or the
// adoption committed and no claim was made. Never both.
func TestAnAdoptionCannotInterleaveWithAClaim(t *testing.T) {
	o, _ := idleWithSource(t, fake.NewTracker())

	if !o.IdentityQuiescent() {
		t.Fatal("a fresh loop is not quiescent; the race below could not be staged")
	}

	// The claim lands after the advisory read and before the publication — the
	// window the barrier has to close.
	o.records["1"] = &Record{Issue: fake.Issue("1", epoch), State: StateQueued}
	o.publish(o.records["1"])

	ran := false
	cur, _ := o.cfg.Runtime.Load()
	moved := *cur.Runtime
	moved.ClaimPrincipal = "someone-else"
	a := &adoption{ack: make(chan error, 1), prev: cur.Runtime, next: &moved, commit: func() { ran = true }}
	o.onAdopt(a)
	err := <-a.ack

	switch {
	case ran && err != nil:
		t.Error("the commit ran and the caller was told it failed: the watcher would report a failed reload over a published configuration")
	case ran:
		t.Error("the adoption committed with a claim outstanding; the advisory read it trusted was already stale")
	case err == nil:
		t.Error("the adoption reported success without committing")
	case !errors.Is(err, config.ErrWorkOutstanding):
		t.Errorf("err = %v, want ErrWorkOutstanding", err)
	}
}

// The advisory and the barrier have to answer the same question.
//
// IdentityQuiescent exists for exactly one purpose: keeping the config watcher from
// rebuilding a deferred candidate on every tick, which costs a credential check and a
// tracker round trip each time (config.WatchOptions.Quiescent). An advisory that
// reports quiescent where the barrier refuses does not merely fail to help — it
// produces precisely the loop it was added to prevent: rebuild, refuse, rebuild,
// refuse, once per tick, for as long as the work stands.
//
// So the property is an implication and it is checked at both ends: for every kind of
// work AdoptIdentity refuses over, the advisory must already say so. Driven through a
// running loop, because the mirror the advisory reads is published there.
func TestTheQuiescenceAdvisoryRefusesWhateverTheBarrierRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*testing.T) *harness
	}{
		{"a live run record", func(t *testing.T) *harness {
			h := start(t, harnessOpts{
				issues: []core.Issue{fake.Issue("1", epoch)},
				script: startedOnly,
				hang:   true,
			})
			h.WaitState("1", StateRunning)
			return h
		}},
		{"a candidate scan that has not resolved", func(t *testing.T) *harness {
			h := start(t, harnessOpts{runGone: domainQuiet})
			h.Tracker.SetFailClaimedByPrincipal(errors.New("tracker unavailable"))
			if err := h.restart(harnessOpts{runGone: domainQuiet, recoverErr: true}); err == nil {
				t.Fatal("the startup scan was supposed to fail, leaving it owed")
			}
			return h
		}},
		{"a workspace §9.10 step 5 deferred", func(t *testing.T) *harness {
			h := start(t, harnessOpts{runGone: domainQuiet})
			deferredResidue(t, h, "9")
			if err := h.restart(harnessOpts{runGone: domainLive}); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			waitFor(t, "the residue to be deferred", func() bool {
				return len(h.Logs.find("probing again on later ticks")) > 0
			})
			return h
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.arrange(t)

			moved := *h.Bundle
			moved.ClaimPrincipal = "someone-else"
			err := h.o.AdoptIdentity(context.Background(), h.Bundle, &moved, func() {
				t.Error("committed an identity change while work bound to the old one was outstanding")
			})
			if !errors.Is(err, config.ErrWorkOutstanding) {
				t.Fatalf("AdoptIdentity = %v, want ErrWorkOutstanding", err)
			}
			if h.o.IdentityQuiescent() {
				t.Error("the advisory reports quiescent where the barrier refuses; the watcher then rebuilds " +
					"a candidate and is refused once per tick, which is the recurring credential check and " +
					"tracker round trip the advisory exists to remove")
			}
		})
	}
}

// The advisory has to be right *before the loop's first turn*, which is the one
// window the per-turn refresh cannot cover.
//
// Recover runs on the caller's goroutine, before Run, and it is where the candidate
// scan and the step 5 sweep record what they could not finish. The config watcher is
// already up by then — B11 starts it first, because Recover reads through the adapters
// it published — so `Quiescent` can be asked in the gap between Recover returning and
// the first signal being handled. Answered from an unrefreshed mirror, that gap hands
// the watcher a "quiescent" for a daemon that has just written down two kinds of debt,
// and the candidate it builds on that answer is refused by the barrier.
//
// Driven with no loop at all, which is exactly what the gap is — and over both of
// Recover's exits, because they record different debts and each has to say so: the
// early one when the candidate read fails, the ordinary one when the sweep it runs
// leaves a workspace deferred.
func TestTheQuiescenceAdvisoryIsCurrentBeforeTheLoopStarts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*testing.T) (*Orchestrator, bool)
	}{
		{"a candidate read that failed", func(t *testing.T) (*Orchestrator, bool) {
			tracker := fake.NewTracker()
			tracker.SetFailClaimedByPrincipal(errors.New("tracker unavailable"))
			o, _ := idleWithSource(t, tracker)
			return o, false
		}},
		{"a workspace the sweep deferred", func(t *testing.T) (*Orchestrator, bool) {
			tracker := fake.NewTracker()
			residue := fake.Issue("9", epoch)
			residue.Labels, residue.Dispatchable, residue.State = nil, false, "closed"
			tracker.Set(residue)
			ws := fake.NewWorkspaces()
			if _, err := prepareWorkspaceForTest(ws, context.Background(), core.Issue{Identifier: "9"}, 1); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			// Its run is not confirmed gone, so step 5 leaves it in place and promises
			// another look — the debt Recover's ordinary exit has to publish.
			ws.SetRunMarker("9", core.RunMarker{
				State:    core.RunMarkerIdentified,
				Evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "999", Boot: "boot-1"},
			})
			o, _ := idleWithAdapters(t, tracker, ws, domainLive)
			return o, true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, wantOK := tc.arrange(t)

			if !o.IdentityQuiescent() {
				t.Fatal("a freshly built orchestrator is not quiescent; there is nothing for Recover to change")
			}
			switch err := o.Recover(context.Background()); {
			case wantOK && err != nil:
				t.Fatalf("Recover: %v", err)
			case !wantOK && err == nil:
				t.Fatal("the candidate read was supposed to fail, leaving the scan owed")
			}
			if o.IdentityQuiescent() {
				t.Error("the advisory still reports quiescent after a recovery that wrote down work it could " +
					"not finish; the watcher would rebuild a candidate the barrier is about to refuse, and " +
					"Run has not even been called yet")
			}
		})
	}
}

// §9.10 step 5's unfinished cleanup is work bound to the identity, and an identity
// change may not be adopted over it.
//
// A deferred workspace is a *promise*: step 5 says such a directory is "left in place
// and swept once that run is confirmed gone", and the only thing that can keep it is a
// daemon still addressing the root it was found under. Let a root change past while
// that debt stands and the promise is not deferred but dropped — a later pass can only
// scan the new root, and once this process exits nothing ever looks at the old one
// again.
func TestAnIdentityReloadIsRefusedWhileWorkspaceCleanupIsOutstanding(t *testing.T) {
	h := start(t, harnessOpts{runGone: domainQuiet})
	deferredResidue(t, h, "9")

	probe := newProber(domainLive)
	if err := h.restart(harnessOpts{runGone: probe.probe}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	waitFor(t, "the residue to be deferred", func() bool {
		return len(h.Logs.find("probing again on later ticks")) > 0
	})
	if len(h.o.Status()) != 0 {
		t.Fatal("a record is outstanding; this test needs the debt to be the only reason for the refusal")
	}

	moved := *h.Bundle
	moved.ClaimPrincipal = "someone-else"
	err := h.o.AdoptIdentity(context.Background(), h.Bundle, &moved, func() {
		t.Error("committed a root change while a workspace was promised a later sweep")
	})
	if !errors.Is(err, config.ErrWorkOutstanding) {
		t.Fatalf("AdoptIdentity = %v, want ErrWorkOutstanding", err)
	}
	if !strings.Contains(err.Error(), "workspace cleanup outstanding: true") {
		t.Errorf("err = %v; the refusal has to name what it is waiting on, or an operator sees a reload "+
			"deferred with nothing to point at", err)
	}

	// And it is a deferral, not a refusal: the run ends, the promise is kept, and the
	// same publication is then permitted.
	probe.set(domainQuiet)
	h.tickUntil("the deferred workspace to be swept", func() bool {
		return len(h.Workspaces.Disposals("9")) > 0
	})
	h.tickUntil("the identity change to be adopted once the debt is settled", func() bool {
		return h.o.AdoptIdentity(context.Background(), h.Bundle, &moved, func() {}) == nil
	})
}

// An owed run-marker removal blocks an identity change, and it is the one kind of
// outstanding work that is not about *finishing* anything.
//
// `markerStore` compares workspace.root as a string, and `config.Load` cleans and
// absolutizes the root but does not resolve symlinks — so `/var/x` and
// `/private/var/x` are one directory under two names that compare unequal. Across
// such a reload a pending clear is neither abandoned nor awaited by the new run's
// launch, and its retry deletes that run's live marker. Canonicalizing the comparison
// is not available: EvalSymlinks is filesystem I/O, it runs on the authority
// goroutine, and the directory need not exist yet. Refusing the move removes the case.
//
// The refusal is checked by *attribution*, not just by kind: the loop is driven until
// its own answer names the removal as the only thing outstanding, so this cannot pass
// on the strength of a record that had not drained yet.
func TestAnIdentityReloadIsRefusedWhileARunMarkerRemovalIsOwed(t *testing.T) {
	ready := make(chan struct{})
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		prepareGate: func() { <-ready },
		runGone:     domainQuiet,
	})
	// A launch that never happens leaves a marker describing nothing, and clearing it
	// is what fails — so the removal stays owed while the record itself terminalizes
	// and releases.
	h.Runner.SetFailStart(errors.New("exec: no such file or directory"))
	h.Workspaces.SetFailMarkerClear(errors.New("state directory is read-only"))
	close(ready)
	waitFor(t, "the removal to be owed", func() bool {
		return len(h.Logs.find("could not clear the run marker")) > 0
	})

	moved := *h.Bundle
	moved.ClaimPrincipal = "someone-else"
	var refusal error
	h.tickUntil("the owed removal to be the only thing outstanding", func() bool {
		refusal = h.o.AdoptIdentity(context.Background(), h.Bundle, &moved, func() {
			t.Error("committed a root change while a run-marker removal was owed; across a root alias its " +
				"retry deletes the new run's live marker (SPEC §9.10)")
		})
		return refusal != nil &&
			strings.Contains(refusal.Error(), "0 run records, 0 held claims, 1 pending run-marker clears") &&
			strings.Contains(refusal.Error(), "workspace cleanup outstanding: false")
	})
	if !errors.Is(refusal, config.ErrWorkOutstanding) {
		t.Fatalf("AdoptIdentity = %v, want ErrWorkOutstanding — the watcher defers on that and retries on "+
			"anything else", refusal)
	}
	if h.o.IdentityQuiescent() {
		t.Error("the advisory reports quiescent while the barrier refuses over an owed removal")
	}

	// And it is a deferral: the removal lands, and the same publication is permitted.
	h.Workspaces.SetFailMarkerClear(nil)
	h.tickUntil("the identity change to be adopted once the removal lands", func() bool {
		return h.o.AdoptIdentity(context.Background(), h.Bundle, &moved, func() {}) == nil
	})
}

// Cancellation and commit are decided by one compare-and-swap, so exactly one
// happens — and whichever wins, the caller reports it.
//
// The defect this pins: with the state moved to committed *before* the commit ran,
// or with an unbuffered acknowledgement, a caller that timed out could report a
// failed reload while the loop published anyway. Two answers to what is in force,
// produced by the machinery built to prevent them.
func TestACancelledAdoptionNeverPublishesBehindTheCallersBack(t *testing.T) {
	o, _ := idleWithSource(t, fake.NewTracker())

	// Cancellation wins: the loop must find the state moved and not commit.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &adoption{ack: make(chan error, 1)}
	ran := false
	a.commit = func() { ran = true }
	if !a.state.CompareAndSwap(adoptPending, adoptCancelled) {
		t.Fatal("could not stage the cancellation")
	}
	o.onAdopt(a)
	if ran {
		t.Error("the commit ran after the caller had cancelled; the reload was reported as failed")
	}
	select {
	case err := <-a.ack:
		t.Errorf("the loop acknowledged a cancelled adoption (%v); the caller has stopped listening and the buffer is not a mailbox", err)
	default:
	}
	_ = ctx

	// Commit wins: a caller whose ctx fires afterwards must still report the
	// commit's result, not the timeout.
	b := &adoption{ack: make(chan error, 1)}
	committed := false
	b.commit = func() { committed = true }
	o.onAdopt(b)
	if !committed {
		t.Fatal("the commit did not run with nothing outstanding")
	}
	if got := b.state.Load(); got != adoptCommitted {
		t.Errorf("state = %d after a successful commit, want committed(%d)", got, adoptCommitted)
	}
	if !b.state.CompareAndSwap(adoptPending, adoptCancelled) {
		// This is the caller's post-cancellation path: the CAS fails, so it must
		// wait for the result rather than reporting the timeout.
		if err := <-b.ack; err != nil {
			t.Errorf("the caller would have reported %v for an adoption that committed", err)
		}
		return
	}
	t.Error("a committed adoption could still be cancelled")
}

// A panic inside the commit is a failed reload, not a dead daemon.
//
// It has to be recovered here: this runs on the authority goroutine, the config
// watcher's own recovery is on the goroutine that called AdoptIdentity and cannot
// see this one, and §5.4 says never crash. The state is left un-committed, because
// nothing can panic between the single store and the word that records it.
func TestAPanickingCommitIsAFailedReloadNotADeadDaemon(t *testing.T) {
	o, src := idleWithSource(t, fake.NewTracker())
	before, _ := src.Load()

	// Observed from inside the commit, because that is the only place the
	// distinction is visible: `committed` set before the store would mean a panic
	// here is reported as a failure with the state already claiming success.
	var during int32
	a := &adoption{ack: make(chan error, 1)}
	a.commit = func() {
		during = a.state.Load()
		panic("adapter constructor blew up")
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the panic escaped the authority goroutine and would have taken the daemon down: %v", r)
			}
		}()
		o.onAdopt(a)
	}()

	err := <-a.ack
	if !errors.Is(err, config.ErrReloadPanic) {
		t.Errorf("err = %v, want ErrReloadPanic", err)
	}
	if during != adoptExecuting {
		t.Errorf("state was %d while the commit was running, want executing(%d): committed may only be set once the store has happened",
			during, adoptExecuting)
	}
	if got := a.state.Load(); got == adoptCommitted {
		t.Error("a panicking commit reported itself committed")
	}
	if after, _ := src.Load(); after.Revision != before.Revision {
		t.Errorf("revision moved from %d to %d on a commit that panicked", before.Revision, after.Revision)
	}
}

// AdoptIdentity is the public seam, and it must not outlive a cancelled context —
// including when no loop is running to answer it.
func TestAdoptIdentityHonoursCancellation(t *testing.T) {
	o, _ := idleWithSource(t, fake.NewTracker())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	// Nothing is draining o.signals, so the only way out is the context.
	if err := o.AdoptIdentity(ctx, nil, nil, func() { ran = true }); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if ran {
		t.Error("the commit ran without the loop ever deciding")
	}
}

// A record and a held claim keep no bundle. That is the shape the whole design
// rests on — every adapter call resolves from the configuration in force, or from
// a snapshot captured at the entry of the operation making it — so it is asserted
// directly rather than left to follow from the behaviour tests.
//
// Retirement needs no bookkeeping *because* of this: the source keeps only the
// current snapshot, no adapter has a Close, and an invoked value stays valid
// through its return. If explicit adapter teardown is ever added, that changes and
// leases become necessary.
func TestRecordsAndHeldClaimsRetainNoBundle(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  any
	}{
		{"Record", Record{}},
		{"heldClaim", heldClaim{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if found := fieldsOfType(tc.typ, "*orchestrator.Bundle"); len(found) != 0 {
				t.Errorf("%s retains %v; a record that owns adapters is a record that can outlive the configuration they were built from",
					tc.name, found)
			}
		})
	}
}

// A reload during Prepare launches through the runner in force at the launch, and
// the workspace it prepared under stays the one that prepared it.
//
// §5.4 gives launches to the reload, and beginStart is that linearization point:
// pinning the runner at dispatch would launch an attempt through an adapter built
// for a configuration a human has already replaced, while the limits and prompt
// beside it came from the new one — the mismatch this ticket exists to remove.
//
// The barrier is the prepare gate, not a sleep: the reload is published while
// Prepare is provably inside the call, and the assertion is on which generation of
// adapters each half went through.
func TestAReloadDuringPrepareLaunchesThroughTheNewRunner(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		prepareGate: func() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		},
	})

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Prepare was never entered")
	}

	// A second generation of adapters, published while Prepare is in flight.
	next := fake.NewRunner()
	next.SetScript(func(core.RunSpec, int) []core.Event { return fake.Succeed("session-2") })
	first, _ := h.Source.Load()
	swapped := *first.Runtime
	swapped.Runner = next
	h.Source.publish(first.Definition, &swapped, nil)

	close(release)

	waitFor(t, "the launch", func() bool { return next.StartCount() > 0 })
	if got := h.Runner.StartCount(); got != 0 {
		t.Errorf("the dispatch-time runner started %d attempts; the launch must go through the adapters in force when it happens (SPEC §5.4)", got)
	}

	// And the workspace is the one that prepared it: Prepare captured its provider
	// at entry and completed through it, so the worktree exists under that root.
	if got := h.Workspaces.PrepareCount("1"); got != 1 {
		t.Errorf("Prepare ran %d times, want 1 through the provider it captured", got)
	}
}

// fieldsOfType names the fields of v whose type prints as want, at any depth of
// pointer. Reflection rather than a hand-written list, so a field added later is
// covered without anyone remembering this test.
func fieldsOfType(v any, want string) []string {
	var found []string
	rt := reflect.TypeOf(v)
	for i := range rt.NumField() {
		if f := rt.Field(i); f.Type.String() == want {
			found = append(found, f.Name)
		}
	}
	return found
}

// A publication that changes no identity is not gated on the record set.
//
// The defect this pins: the barrier was asked for every commit and the loop refused
// whenever any work existed, so a limits edit, a hook change, or a credential
// rotation that keeps the principal landed only on an idle daemon. §5.4 gives all
// of them to the reload however busy it is — "applies to future dispatch, retry
// scheduling, reconciliation, hooks, and launches" is not conditional on the queue
// being empty.
func TestAPolicyOnlyPublicationIsNotGatedOnOutstandingWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		next func(*Bundle, *config.WorkflowDefinition) *Bundle
		want bool // may it commit with work outstanding?
	}{
		{"a limits edit", func(b *Bundle, def *config.WorkflowDefinition) *Bundle {
			next := *b
			next.Definition = def
			return &next
		}, true},
		{"a rebuilt runner, same identity", func(b *Bundle, _ *config.WorkflowDefinition) *Bundle {
			next := *b
			next.Runner = fake.NewRunner()
			return &next
		}, true},
		{"a rebuilt credential source, same principal", func(b *Bundle, _ *config.WorkflowDefinition) *Bundle {
			next := *b
			next.Repository.AuthSource = fake.NewRemoteAuth("x-access-token", "rotated")
			return &next
		}, true},
		{"a different principal", func(b *Bundle, _ *config.WorkflowDefinition) *Bundle {
			next := *b
			next.ClaimPrincipal = "another-bot"
			return &next
		}, false},
		{"a different repository", func(b *Bundle, _ *config.WorkflowDefinition) *Bundle {
			next := *b
			next.Repository.RemoteURL = "https://example.test/other/repo.git"
			return &next
		}, false},
		{"a different workspace root", func(b *Bundle, _ *config.WorkflowDefinition) *Bundle {
			next := *b
			next.Definition = definition(t, "3", "")
			moved := *next.Definition
			moved.Config.Workspace.Root = "/tmp/somewhere-else"
			next.Definition = &moved
			return &next
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := idleWithSource(t, fake.NewTracker())
			// Work outstanding, which is the whole question.
			o.records["1"] = &Record{Issue: fake.Issue("1", epoch), State: StateRunning}

			cur, _ := o.cfg.Runtime.Load()
			ran := false
			a := &adoption{
				ack: make(chan error, 1), prev: cur.Runtime,
				next:   tc.next(cur.Runtime, definition(t, "3", "  max_retry_backoff_ms: 4000\n")),
				commit: func() { ran = true },
			}
			o.onAdopt(a)

			if tc.want {
				if !ran {
					t.Errorf("refused a publication that changes no identity: %v", <-a.ack)
				}
				return
			}
			if ran {
				t.Error("committed an identity change with a claim outstanding")
			}
			if err := <-a.ack; !errors.Is(err, config.ErrWorkOutstanding) {
				t.Errorf("err = %v, want ErrWorkOutstanding", err)
			}
		})
	}
}

// A panicking commit reports the panic even to a caller whose context has since
// fired.
//
// The defect: the recovery reset the state to pending, so the caller's
// post-cancellation compare-and-swap could win and report ctx.Err() — a timeout, for
// a publication that had actually panicked. `failed` is terminal for that reason.
func TestAPanickedAdoptionIsNotReportedAsATimeout(t *testing.T) {
	o, _ := idleWithSource(t, fake.NewTracker())

	a := &adoption{ack: make(chan error, 1), commit: func() { panic("boom") }}
	o.onAdopt(a)

	if got := a.state.Load(); got != adoptFailed {
		t.Errorf("state = %d after a panicking commit, want failed(%d): pending would reopen the cancellation race",
			got, adoptFailed)
	}
	// The caller's context fires only now, after the loop has finished.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.awaitAdoption(ctx, a); !errors.Is(err, config.ErrReloadPanic) {
		t.Errorf("err = %v, want ErrReloadPanic — a timeout would send an operator looking for a slow loop instead of a broken adapter", err)
	}
	if a.stack == nil {
		t.Error("no stack was carried out of the loop turn; a panic's only diagnosis was dropped")
	}
}

// A hook resolves when its call fires, not when it was queued.
//
// The defect: attemptEnded captured the provider before queueing the effect, so a
// reload landing while the effect sat behind a slow tracker write ran the *old*
// hook — and §5.4's linearization point for a hook is the entry of the provider
// call that runs it, which for after_run is its own call, entered after the reload.
//
// Driven against a constructed-but-not-running orchestrator: New starts no
// goroutines, so the effect can be queued, a reload published, and the queue then
// drained by hand. The barrier is the channel receive, not a deadline.
func TestAQueuedAfterRunHookUsesTheProviderInForceWhenItFires(t *testing.T) {
	def := definition(t, "3", "")
	first := fake.NewHookedWorkspaces(fake.NewWorkspaces())
	base := &Bundle{
		Definition: def, Tracker: fake.NewTracker(), Workspaces: first,
		Runner: fake.NewRunner(), Verifier: alwaysPublished,
		ClaimPrincipal: fake.DefaultPrincipal,
	}
	src := newTestSource(def, base)
	o, err := New(Config{Runtime: src, Log: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	r := &Record{
		Issue: fake.Issue("1", epoch), State: StateVerifying,
		Workspace:      core.Workspace{WorkspacePaths: core.WorkspacePaths{Path: "/tmp/ws"}, Key: "issue-1", Branch: "ben/issue-1"},
		ranThisAttempt: true,
	}
	o.records["1"] = r

	ctx := context.Background()
	o.attemptEnded(ctx, r)

	// The reload lands while the effect is still queued — a hook edit rebuilds the
	// provider (config.AdapterChange.Workspace), so the new generation is a new
	// instance.
	second := fake.NewHookedWorkspaces(fake.NewWorkspaces())
	next := *base
	next.Workspaces = second
	src.publish(def, &next, nil)

	// Drain by hand: one effect was owed, and running it is what "the call fires"
	// means.
	select {
	case fn := <-o.effects:
		fn(ctx)
	default:
		t.Fatal("no effect was queued; the after-run hook was never owed")
	}

	if got := second.AfterRunCount("1"); got != 1 {
		t.Errorf("the provider in force when the call fired ran the hook %d times, want 1", got)
	}
	if got := first.AfterRunCount("1"); got != 0 {
		t.Errorf("the provider captured at queue time ran the hook %d times; an edit that landed while the effect waited never reached the run it was written for", got)
	}
}
