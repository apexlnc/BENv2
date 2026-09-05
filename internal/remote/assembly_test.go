package remote_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// The wiring surface: what assembly hands this package, what it hands back, and
// what it refuses (#192).
//
// The lifecycle cases next door drive a correctly-assembled runner, which is the
// one arrangement that cannot say anything about a wrong one. Everything here is
// a claim the doc comments make about the seams themselves — a substrate that
// adds nothing to a harness's capabilities and an adapter's refusal that reaches
// no backend — and each is a claim a plausible implementation gets wrong in the
// direction of being helpful.

// A runner with a missing seam is refused at construction, not at the first
// dispatch.
//
// The five are separate rows because the failure they prevent differs: a nil
// backend cannot run anything, while a nil translator would run the agent and
// then silently deliver no events — a run that burns a real attempt and looks
// like a harness that said nothing. New performs no I/O, so this is the last
// place either can be caught cheaply (SPEC §5.7, §7.1).
func TestRunnerConstructionRefusesAnIncompleteAssembly(t *testing.T) {
	complete := func() remote.Options {
		return remote.Options{
			Backend:   remotetest.New(testProfile),
			Store:     remotetest.NewMemStore(),
			Consumer:  remotetest.NewConsumer(),
			Bind:      func(core.RunSpec) (remote.Binding, error) { return remote.Binding{}, nil },
			Invoke:    func(core.RunSpec) (remote.Invocation, error) { return remote.Invocation{}, nil },
			Translate: remotetest.Translate,
		}
	}
	for _, tc := range []struct {
		name string
		drop func(*remote.Options)
	}{
		{"no backend", func(o *remote.Options) { o.Backend = nil }},
		{"no store", func(o *remote.Options) { o.Store = nil }},
		{"no durable consumer", func(o *remote.Options) { o.Consumer = nil }},
		{"no binder", func(o *remote.Options) { o.Bind = nil }},
		{"no invoker", func(o *remote.Options) { o.Invoke = nil }},
		{"no translator", func(o *remote.Options) { o.Translate = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := complete()
			tc.drop(&opts)
			runner, err := remote.New(opts)
			if err == nil {
				t.Fatal("New accepted an incomplete assembly")
			}
			if runner != nil {
				t.Error("New returned both a runner and an error")
			}
		})
	}
	if _, err := remote.New(complete()); err != nil {
		t.Errorf("New refused a complete assembly: %v", err)
	}
}

// Capabilities and readiness belong to the provider adapter, and the substrate
// reports them verbatim.
//
// The direction that matters is subtraction as much as addition: a substrate that
// quietly dropped Resume would make the orchestrator re-prompt a harness that
// could have continued, and one that added it would make it resume a harness that
// cannot. Neither is visible in a run's events, so it is asserted here.
func TestCapabilitiesAndReadinessAreTheAdaptersOwn(t *testing.T) {
	base := func() remote.Options {
		return remote.Options{
			Backend:   remotetest.New(testProfile),
			Store:     remotetest.NewMemStore(),
			Consumer:  remotetest.NewConsumer(),
			Bind:      func(core.RunSpec) (remote.Binding, error) { return remote.Binding{}, nil },
			Invoke:    func(core.RunSpec) (remote.Invocation, error) { return remote.Invocation{}, nil },
			Translate: remotetest.Translate,
		}
	}

	for _, want := range []core.Capabilities{
		{Resume: true, Usage: true},
		{},
	} {
		opts := base()
		opts.Capabilities = want
		runner, err := remote.New(opts)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if got := runner.Capabilities(); got != want {
			t.Errorf("Capabilities = %+v, want %+v — a substrate reports the adapter's, unchanged", got, want)
		}
	}

	// No probe of its own: this package reaches no network, and answering "not
	// ready" for a substrate with nothing to check would block a workflow on a
	// question nobody asked.
	runner, err := remote.New(base())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := runner.Ready(context.Background()); err != nil {
		t.Errorf("Ready with no probe = %v, want nil", err)
	}

	boom := errors.New("the harness is not installed in the profile")
	opts := base()
	opts.Ready = func(context.Context) error { return boom }
	runner, err = remote.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := runner.Ready(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Ready = %v, want the adapter's own refusal %v", err, boom)
	}
}

// An adapter that refuses to compose the run dispatches nothing and reserves
// nothing.
//
// Both halves matter, and the second is the one that decays. A refusal that had
// already written a record would leave a claim cycle carrying a reservation for a
// run that will never exist — and Start over that claim afterwards is a path that
// has to reason about a record nothing dispatched, when the honest state is no
// record at all.
func TestAnAdaptersRefusalReachesNoBackendAndNoStore(t *testing.T) {
	boom := errors.New("the adapter said no")
	for _, tc := range []struct {
		name string
		opts func(*remote.Options)
	}{
		{
			name: "the binder cannot resolve a workspace",
			opts: func(o *remote.Options) {
				o.Bind = func(core.RunSpec) (remote.Binding, error) { return remote.Binding{}, boom }
			},
		},
		{
			name: "the invoker cannot compose the harness invocation",
			opts: func(o *remote.Options) {
				o.Invoke = func(core.RunSpec) (remote.Invocation, error) { return remote.Invocation{}, boom }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t)
			id := rig.acquire(t)
			opts := remote.Options{
				Backend:   rig.backend,
				Store:     rig.store,
				Consumer:  rig.consumer,
				Translate: remotetest.Translate,
				Bind: func(core.RunSpec) (remote.Binding, error) {
					return remote.Binding{Identity: id, Run: rig.run}, nil
				},
				Invoke: func(core.RunSpec) (remote.Invocation, error) { return remote.Invocation{}, nil },
			}
			tc.opts(&opts)
			runner, err := remote.New(opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := runner.Start(context.Background(), testSpec()); !errors.Is(err, boom) {
				t.Fatalf("Start = %v, want the adapter's refusal %v", err, boom)
			}
			if got := rig.backend.StartCalls(); got != 0 {
				t.Errorf("backend saw %d dispatches although the adapter refused, want 0", got)
			}
			if rig.store.Has(rig.claim) {
				t.Error("a refused Start reserved a run identity that will never be dispatched")
			}
		})
	}
}

// A binding whose identity cannot be attached to later is refused before
// anything is written, for Reserve's reason: an address BEN cannot re-derive is
// how one claim ends up with two runs.
func TestStartRefusesAnIncompleteBinding(t *testing.T) {
	rig := newRig(t)
	runner, err := remote.New(remote.Options{
		Backend:   rig.backend,
		Store:     rig.store,
		Consumer:  rig.consumer,
		Translate: remotetest.Translate,
		Bind: func(core.RunSpec) (remote.Binding, error) {
			// Acquired nothing: no sandbox, no profile revision.
			return remote.Binding{Identity: remote.Identity{Claim: rig.claim}, Run: rig.run}, nil
		},
		Invoke: func(core.RunSpec) (remote.Invocation, error) { return remote.Invocation{}, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runner.Start(context.Background(), testSpec()); !errors.Is(err, remote.ErrIdentityMissing) {
		t.Fatalf("Start = %v, want %v", err, remote.ErrIdentityMissing)
	}
	if got := rig.backend.StartCalls(); got != 0 {
		t.Errorf("backend saw %d dispatches over an incomplete identity, want 0", got)
	}
}

// A durable record that is present and unreadable is not an absent one.
//
// The distinction §9.10 draws everywhere, at the point where getting it wrong is
// a duplicate dispatch: absence is a fact that authorizes a fresh start, and a
// store that would not answer is not that fact. A Start that treated the two
// alike would dispatch a second run into a workspace whose first one it can no
// longer name.
func TestAnUnreadableRecordIsNotAnAbsentOne(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	boom := errors.New("the state directory is not readable")

	runner, err := remote.New(remote.Options{
		Backend:   rig.backend,
		Store:     failingLoad{Store: rig.store, err: boom},
		Consumer:  rig.consumer,
		Translate: remotetest.Translate,
		Bind: func(core.RunSpec) (remote.Binding, error) {
			return remote.Binding{Identity: id, Run: rig.run}, nil
		},
		Invoke: func(core.RunSpec) (remote.Invocation, error) { return remote.Invocation{}, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runner.Start(context.Background(), testSpec()); !errors.Is(err, boom) {
		t.Fatalf("Start over an unreadable record = %v, want %v", err, boom)
	}
	if got := rig.backend.StartCalls(); got != 0 {
		t.Errorf("backend saw %d dispatches although the record could not be read, want 0", got)
	}
}

// failingLoad is a Store whose Load always refuses. Save and Delete are the
// real ones, so a test using it fails only where it means to.
type failingLoad struct {
	remote.Store
	err error
}

func (f failingLoad) Load(remote.Claim) (remote.Record, error) { return remote.Record{}, f.err }

// What the invoker composed is what the backend is asked to run, unchanged.
//
// The substrate is not a second place a credential can enter a child (SPEC §7.6):
// it neither adds to the environment nor edits the argv, so the §7.6 audit the
// provider adapter passes is still a statement about what actually launches. The
// workspace identity travels with it, because a backend given argv and no
// workspace would run the attempt somewhere of its own choosing.
func TestTheInvocationReachesTheBackendUnchanged(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })

	spec, ok := rig.backend.Spec(rig.run)
	if !ok {
		t.Fatal("the backend holds no spec for the dispatched run")
	}
	if !slices.Equal(spec.Argv, []string{"claude", "--print"}) {
		t.Errorf("argv = %q, want the invoker's own", spec.Argv)
	}
	if len(spec.Env) != 1 || spec.Env["BEN_RUN_ID"] != "42-1" {
		t.Errorf("env = %v, want exactly the invoker's; the substrate composes none of its own", spec.Env)
	}
	if string(spec.Stdin) != testSpec().Prompt {
		t.Errorf("stdin = %q, want the prompt %q", spec.Stdin, testSpec().Prompt)
	}
	if spec.Identity != id {
		t.Errorf("identity = %+v, want the acquired workspace %+v", spec.Identity, id)
	}
	if spec.Limits != testSpec().Limits {
		t.Errorf("limits = %+v, want %+v — the backend must enforce them while BEN is disconnected",
			spec.Limits, testSpec().Limits)
	}
}

// Disposal ends the durable record, and only the caller that knows the claim
// cycle is over may do it.
//
// Journal.Close is deliberately not reached by the run ending: a record removed
// while a run may still be live is a run nothing can attach to, and only the
// orchestrator knows the difference between "this attempt finished" and "this
// claim is done".
func TestTheDisposalPathEndsTheRecord(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession), remotetest.Success())
	rig.backend.Quiet(rig.run)
	collect(t, h)

	journal, err := rig.runner(t, id).Journal(rig.claim)
	if err != nil {
		t.Fatalf("Journal: %v", err)
	}
	if got := journal.Claim(); got != rig.claim {
		t.Errorf("Claim = %s, want %s", got, rig.claim)
	}
	if rec := journal.Record(); rec.RunID != rig.run || !rec.Dispatched {
		t.Errorf("Record = %+v, want the dispatched run %s", rec, rig.run)
	}

	// The run is quiet, so the claim cycle may end.
	if got := h.Probe(context.Background()); got != core.TerminationConfirmed {
		t.Fatalf("Probe = %v, want %v before disposal", got, core.TerminationConfirmed)
	}
	if err := remote.Dispose(context.Background(), rig.backend.Workspaces(), rig.claim, quietStatus()); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rig.store.Has(rig.claim) {
		t.Error("the durable record outlived the claim cycle")
	}
	if !rig.backend.Deleted(rig.claim) {
		t.Error("the workspace outlived the claim cycle")
	}

	// And a journal nobody opened refuses rather than acting on a zero record.
	if _, err := rig.runner(t, id).Journal(rig.claim); !errors.Is(err, remote.ErrNoRecord) {
		t.Errorf("Journal after Close = %v, want %v", err, remote.ErrNoRecord)
	}
}

func quietStatus() remote.Status {
	return remote.Status{
		Phase: remote.PhaseQuiet, Stream: remote.StreamStateSealed,
		Process: remote.ProcessStateReaped, Domain: remote.DomainStateQuiet, Reachable: true,
	}
}

// The stop modes a run was asked for are recorded in order.
//
// `interrupt` and `discard` mean different things to the orchestrator — one asks
// an agent to wind up, the other throws the attempt away — and a substrate that
// collapsed them would make the distinction unobservable exactly where a test
// needs to see it (fake.Handle.Stops exists for the same reason).
func TestStopsRecordsTheModesInOrder(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession))

	attempt, ok := h.(*remote.Attempt)
	if !ok {
		t.Fatal("the handle is not a *remote.Attempt")
	}
	if got := attempt.Stops(); len(got) != 0 {
		t.Errorf("Stops before any stop = %v, want none", got)
	}
	attempt.Stop(context.Background(), core.StopInterrupt)
	attempt.Stop(context.Background(), core.StopDiscard)
	if got := attempt.Stops(); !slices.Equal(got, []core.StopMode{core.StopInterrupt, core.StopDiscard}) {
		t.Errorf("Stops = %v, want interrupt then discard", got)
	}
}

func TestStopModesReachTheBackendWithDifferentSemantics(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.SetConfirmable(rig.run, false)
	if got := h.Stop(context.Background(), core.StopInterrupt); got != core.TerminationUnconfirmed {
		t.Fatalf("interrupt Stop = %v, want unconfirmed", got)
	}
	requests := rig.backend.StopRequests(rig.run)
	if len(requests) != 1 || requests[0].Mode != core.StopInterrupt || requests[0].Grace <= 0 {
		t.Fatalf("backend interrupt requests = %+v, want interrupt with a patient grace", requests)
	}

	// An unconfirmed interrupt remains attached so an agent that winds up can
	// still publish its final stream.
	rig.backend.Emit(rig.run, remotetest.Text("winding up"))
	if got := (<-h.Events()).Type; got != core.EventProgress {
		t.Fatalf("event after unconfirmed interrupt = %v, want progress", got)
	}

	rig.backend.SetConfirmable(rig.run, true)
	if got := h.Stop(context.Background(), core.StopDiscard); got != core.TerminationConfirmed {
		t.Fatalf("discard Stop = %v, want confirmed", got)
	}
	requests = rig.backend.StopRequests(rig.run)
	if len(requests) != 2 || requests[1].Mode != core.StopDiscard || requests[1].Grace != 0 {
		t.Fatalf("backend stop requests = %+v, want discard with no grace second", requests)
	}
	<-h.Done()
}

// Every closed enum spells itself, including a value outside the set.
//
// The out-of-range row is the one worth writing down. These strings reach
// refusals — remote.Reacquire names the phase it would not touch — and a %d
// fallback in an error an operator reads at three in the morning is the
// difference between a diagnosis and a bug report. A silent empty string would be
// worse still: the refusal would name nothing at all.
func TestClosedEnumsSpellThemselves(t *testing.T) {
	phases := map[remote.Phase]string{
		remote.PhaseUnknown:  "unknown",
		remote.PhaseStarting: "starting",
		remote.PhaseRunning:  "running",
		remote.PhaseSignaled: "signaled",
		remote.PhaseQuiet:    "quiet",
		remote.Phase(99):     "Phase(99)",
	}
	for p, want := range phases {
		if got := p.String(); got != want {
			t.Errorf("Phase(%d).String() = %q, want %q", uint8(p), got, want)
		}
	}

	streams := map[remote.Stream]string{
		remote.StreamControl: "control", remote.StreamStdout: "stdout",
		remote.StreamStderr: "stderr", remote.Stream(99): "Stream(99)",
	}
	for value, want := range streams {
		if got := value.String(); got != want {
			t.Errorf("Stream(%d).String() = %q, want %q", uint8(value), got, want)
		}
	}
	streamStates := map[remote.StreamState]string{
		remote.StreamStateUnknown: "unknown", remote.StreamStateOpen: "open",
		remote.StreamStateSealed: "sealed", remote.StreamState(99): "StreamState(99)",
	}
	for value, want := range streamStates {
		if got := value.String(); got != want {
			t.Errorf("StreamState(%d).String() = %q, want %q", uint8(value), got, want)
		}
	}
	processStates := map[remote.ProcessState]string{
		remote.ProcessStateUnknown: "unknown", remote.ProcessStateRunning: "running",
		remote.ProcessStateReaped: "reaped", remote.ProcessState(99): "ProcessState(99)",
	}
	for value, want := range processStates {
		if got := value.String(); got != want {
			t.Errorf("ProcessState(%d).String() = %q, want %q", uint8(value), got, want)
		}
	}
	domainStates := map[remote.DomainState]string{
		remote.DomainStateUnknown: "unknown", remote.DomainStateActive: "active",
		remote.DomainStateQuiet: "quiet", remote.DomainState(99): "DomainState(99)",
	}
	for value, want := range domainStates {
		if got := value.String(); got != want {
			t.Errorf("DomainState(%d).String() = %q, want %q", uint8(value), got, want)
		}
	}
	hookStates := map[remote.HookState]string{
		remote.HookStateUnknown: "unknown", remote.HookStateRunning: "running",
		remote.HookStateFinished: "finished", remote.HookState(99): "HookState(99)",
	}
	for value, want := range hookStates {
		if got := value.String(); got != want {
			t.Errorf("HookState(%d).String() = %q, want %q", uint8(value), got, want)
		}
	}

	leases := map[remote.LeaseState]string{
		remote.LeaseNone:      "none",
		remote.LeaseHeld:      "held",
		remote.LeaseRunning:   "running",
		remote.LeaseState(99): "LeaseState(99)",
	}
	for l, want := range leases {
		if got := l.String(); got != want {
			t.Errorf("LeaseState(%d).String() = %q, want %q", uint8(l), got, want)
		}
	}
}
