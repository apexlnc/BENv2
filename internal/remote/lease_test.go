package remote_test

import (
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// The v2 concurrency count, over the two facts it is a function of: what the
// durable record says, and what the last status observed said.
//
// The two rows that differ from v1's reading are the point of the table. A
// suspended workspace with no run still costs, because the reservation is what
// the backend charges for; and a dispatched run whose termination is unconfirmed
// still costs, which is v1's "stop counting once the domain is confirmed quiet"
// read on a substrate where the proof is domain quiet rather than ESRCH.
func TestLeaseStateIsWhatTheCapCounts(t *testing.T) {
	reserved := remote.Record{Identity: testIdentity()}
	dispatched := remote.Record{Identity: testIdentity(), Dispatched: true}
	unacquired := remote.Record{Identity: remote.Identity{Claim: testClaim()}}

	for _, tc := range []struct {
		name   string
		rec    remote.Record
		status remote.Status
		want   remote.LeaseState
		cost   int
	}{
		{
			name: "a claim with no sandbox holds nothing",
			rec:  unacquired,
			want: remote.LeaseNone,
			cost: 0,
		},
		{
			name: "a reserved workspace with no dispatch holds the lease",
			rec:  reserved,
			want: remote.LeaseHeld,
			cost: 1,
		},
		{
			name:   "a suspended workspace still holds the lease",
			rec:    reserved,
			status: remote.Status{Phase: remote.PhaseQuiet, Domain: remote.DomainStateQuiet, Reachable: true},
			want:   remote.LeaseHeld,
			cost:   1,
		},
		{
			name:   "a running dispatch counts as a run",
			rec:    dispatched,
			status: remote.Status{Phase: remote.PhaseRunning, Reachable: true},
			want:   remote.LeaseRunning,
			cost:   1,
		},
		{
			name:   "a signaled dispatch still counts: delivery is not termination",
			rec:    dispatched,
			status: remote.Status{Phase: remote.PhaseSignaled, Reachable: true},
			want:   remote.LeaseRunning,
			cost:   1,
		},
		{
			name:   "an unreachable backend still counts: nothing was proven",
			rec:    dispatched,
			status: remote.Status{},
			want:   remote.LeaseRunning,
			cost:   1,
		},
		{
			name:   "a confirmed-quiet dispatch releases the run and keeps the lease",
			rec:    dispatched,
			status: remote.Status{Phase: remote.PhaseQuiet, Domain: remote.DomainStateQuiet, Reachable: true},
			want:   remote.LeaseHeld,
			cost:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := remote.LeaseStateOf(tc.rec, tc.status)
			if got != tc.want {
				t.Errorf("LeaseStateOf = %v, want %v", got, tc.want)
			}
			if got.Cost() != tc.cost {
				t.Errorf("%v.Cost() = %d, want %d", got, got.Cost(), tc.cost)
			}
		})
	}
}

// The zero LeaseState frees no capacity, which is the direction that matters: a
// state nobody computed must not read as "there is room".
func TestTheZeroLeaseStateIsNone(t *testing.T) {
	var s remote.LeaseState
	if s != remote.LeaseNone {
		t.Fatalf("the zero LeaseState is %v, want %v", s, remote.LeaseNone)
	}
	if s.Cost() != 0 {
		t.Errorf("LeaseNone.Cost() = %d, want 0", s.Cost())
	}
	// And the sum over an empty set is zero rather than something a caller has
	// to special-case.
	if got := remote.Active(nil); got != 0 {
		t.Errorf("Active(nil) = %d, want 0", got)
	}
}

func TestActiveSumsTheCosts(t *testing.T) {
	got := remote.Active([]remote.LeaseState{
		remote.LeaseNone, remote.LeaseHeld, remote.LeaseRunning, remote.LeaseRunning,
	})
	if got != 3 {
		t.Errorf("Active = %d, want 3", got)
	}
}
