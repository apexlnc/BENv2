package remote_test

import (
	"context"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// Exactly one input confirms a termination, and everything else — including the
// things that look like termination — does not.
//
// Table-driven over every phase plus the two ways of not having an answer,
// because the failure mode is a *near miss*: "not running" and "we sent it a
// SIGTERM" are each one step from correct and each fails open, which under
// SPEC §9.8 puts a second agent in a workspace the first may still hold.
func TestOnlyDomainQuietConfirmsATermination(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status remote.Status
		want   core.Termination
	}{
		{
			name:   "the zero status confirms nothing",
			status: remote.Status{},
			want:   core.TerminationUnconfirmed,
		},
		{
			name:   "an unreachable backend confirms nothing, whatever phase it last reported",
			status: remote.Status{Phase: remote.PhaseQuiet, Domain: remote.DomainStateQuiet},
			want:   core.TerminationUnconfirmed,
		},
		{
			name:   "unknown",
			status: remote.Status{Phase: remote.PhaseUnknown, Reachable: true},
			want:   core.TerminationUnconfirmed,
		},
		{
			name:   "starting",
			status: remote.Status{Phase: remote.PhaseStarting, Reachable: true},
			want:   core.TerminationUnconfirmed,
		},
		{
			name:   "running",
			status: remote.Status{Phase: remote.PhaseRunning, Reachable: true},
			want:   core.TerminationUnconfirmed,
		},
		{
			// The one a caller is most tempted to read as terminal. Delivery is
			// not termination — the local ladder makes the same distinction, and
			// a run that ignores its signals sits here indefinitely.
			name:   "signaled",
			status: remote.Status{Phase: remote.PhaseSignaled, Reachable: true},
			want:   core.TerminationUnconfirmed,
		},
		{
			name:   "quiet, from a backend that answered",
			status: remote.Status{Phase: remote.PhaseQuiet, Domain: remote.DomainStateQuiet, Reachable: true},
			want:   core.TerminationConfirmed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.status.Termination(); got != tc.want {
				t.Errorf("Termination = %v, want %v", got, tc.want)
			}
			if got, want := remote.MayReuse(tc.status), tc.want == core.TerminationConfirmed; got != want {
				t.Errorf("MayReuse = %v, want %v — the workspace gate and the verdict must not disagree", got, want)
			}
		})
	}
}

// The zero Phase and the zero Status both authorize nothing, which is the
// property core.Termination and core.ClaimBaseState are both arranged around.
//
// Asserted on the constant rather than only through the table, because the value
// of the ordering is that a field nobody filled cannot mean "free".
func TestTheZeroPhaseIsUnknown(t *testing.T) {
	var p remote.Phase
	if p != remote.PhaseUnknown {
		t.Errorf("the zero Phase is %v, want %v", p, remote.PhaseUnknown)
	}
	if p.String() != "unknown" {
		t.Errorf("PhaseUnknown.String() = %q, want %q", p.String(), "unknown")
	}
}

// An interrupt request may reach the signaled phase without ending the run —
// the same fact as the table above, established against the backend rather than
// asserted about a struct.
func TestInterruptDeliveryIsNotTermination(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })
	rig.backend.Emit(rig.run, remotetest.Init(testSession))
	<-h.Events()

	rig.backend.SetConfirmable(rig.run, false)
	if got := h.Stop(context.Background(), core.StopInterrupt); got != core.TerminationUnconfirmed {
		t.Fatalf("interrupt Stop = %v, want unconfirmed", got)
	}
	if got := h.Probe(context.Background()); got != core.TerminationUnconfirmed {
		t.Errorf("Probe after an unconfirmed interrupt = %v, want %v", got, core.TerminationUnconfirmed)
	}
	if !rig.backend.Live(rig.run) {
		t.Error("an unconfirmed interrupt ended the run in the fake")
	}
}

// An unreachable backend is the ambiguous case, and it parks: the run is not
// confirmed gone, so the claim is retained (SPEC §9.8).
func TestAnUnreachableBackendNeverConfirms(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession))
	<-h.Events()

	rig.backend.SetUnreachable(true)
	if got := h.Probe(context.Background()); got != core.TerminationUnconfirmed {
		t.Errorf("Probe of an unreachable backend = %v, want %v", got, core.TerminationUnconfirmed)
	}
	if got := h.Stop(context.Background(), core.StopDiscard); got != core.TerminationUnconfirmed {
		t.Errorf("Stop against an unreachable backend = %v, want %v", got, core.TerminationUnconfirmed)
	}
	drain(t, h)
	select {
	case <-h.Done():
		t.Error("Done closed without process-reaped evidence")
	default:
	}
}
