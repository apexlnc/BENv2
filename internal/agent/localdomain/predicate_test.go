package localdomain

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestQuietPredicate(t *testing.T) {
	sentinel := errors.New("observation failed")
	cases := []struct {
		name string
		obs  observation
		want Termination
		err  error
	}{
		{name: "old boot overrides every cgroup view", obs: observation{supervisor: supervisorOldBoot, cgroup: cgroupPopulated}, want: TerminationConfirmed},
		{name: "live empty is not quiet", obs: observation{supervisor: supervisorLive, cgroup: cgroupEmpty}, want: TerminationUnconfirmed},
		{name: "live absent is not quiet", obs: observation{supervisor: supervisorLive, cgroup: cgroupAbsent}, want: TerminationUnconfirmed},
		{name: "unknown remains unknown", obs: observation{supervisor: supervisorUnknown, cgroup: cgroupEmpty, err: sentinel}, want: TerminationUnconfirmed, err: sentinel},
		{name: "exited empty is quiet", obs: observation{supervisor: supervisorExited, cgroup: cgroupEmpty}, want: TerminationConfirmed},
		{name: "exited absent is quiet", obs: observation{supervisor: supervisorExited, cgroup: cgroupAbsent}, want: TerminationConfirmed},
		{name: "exited replaced is quiet", obs: observation{supervisor: supervisorExited, cgroup: cgroupReplaced}, want: TerminationConfirmed},
		{name: "populated vetoes exit", obs: observation{supervisor: supervisorExited, cgroup: cgroupPopulated}, want: TerminationUnconfirmed, err: ErrContainment},
		{name: "failed cgroup observation is not quiet", obs: observation{supervisor: supervisorExited, cgroup: cgroupUnknown, err: sentinel}, want: TerminationUnconfirmed, err: sentinel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decide(tc.obs)
			if got != tc.want {
				t.Fatalf("decide = %v, want %v", got, tc.want)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
		})
	}
}

type scriptedHandleOps struct {
	mu           sync.Mutex
	observations []observation
	observeCalls int
	actions      []string
	termErr      error
	killErr      error
	afterKill    *observation
}

func (s *scriptedHandleOps) observe(context.Context) observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observeCalls++
	if len(s.observations) == 0 {
		return observation{supervisor: supervisorUnknown, cgroup: cgroupUnknown}
	}
	result := s.observations[0]
	if len(s.observations) > 1 {
		s.observations = s.observations[1:]
	}
	return result
}

func (s *scriptedHandleOps) term() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = append(s.actions, "term")
	return s.termErr
}

func (s *scriptedHandleOps) kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = append(s.actions, "kill")
	if s.afterKill != nil {
		s.observations = []observation{*s.afterKill}
	}
	return s.killErr
}

func testTimings() Timings {
	return Timings{
		InterruptGrace: 5 * time.Millisecond,
		DiscardGrace:   2 * time.Millisecond,
		KillGrace:      5 * time.Millisecond,
		PollInterval:   time.Millisecond,
	}
}

func TestProbeIsOneReadOnlyObservation(t *testing.T) {
	ops := &scriptedHandleOps{observations: []observation{{supervisor: supervisorExited, cgroup: cgroupEmpty}}}
	h := newHandle(ops, testTimings())
	got, err := h.Probe(context.Background())
	if err != nil || got != TerminationConfirmed {
		t.Fatalf("Probe = (%v, %v), want confirmed", got, err)
	}
	if ops.observeCalls != 1 || len(ops.actions) != 0 {
		t.Fatalf("calls = %d, actions = %v; Probe must perform one read only", ops.observeCalls, ops.actions)
	}
}

func TestCancelledProbeDoesNotObserve(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ops := &scriptedHandleOps{}
	got, err := newHandle(ops, testTimings()).Probe(ctx)
	if got != TerminationUnconfirmed || !errors.Is(err, context.Canceled) {
		t.Fatalf("Probe = (%v, %v)", got, err)
	}
	if ops.observeCalls != 0 || len(ops.actions) != 0 {
		t.Fatalf("calls = %d, actions = %v", ops.observeCalls, ops.actions)
	}
}

func TestStopNaturalExitDoesNotSignalOrWrite(t *testing.T) {
	ops := &scriptedHandleOps{observations: []observation{{supervisor: supervisorExited, cgroup: cgroupEmpty}}}
	got, err := newHandle(ops, testTimings()).Stop(context.Background(), StopInterrupt)
	if err != nil || got != TerminationConfirmed {
		t.Fatalf("Stop = (%v, %v), want confirmed", got, err)
	}
	if len(ops.actions) != 0 {
		t.Fatalf("actions = %v, want none", ops.actions)
	}
}

func TestStopUsesPidfdTermThenCgroupKill(t *testing.T) {
	live := observation{supervisor: supervisorLive, cgroup: cgroupPopulated}
	quiet := observation{supervisor: supervisorExited, cgroup: cgroupEmpty}
	ops := &scriptedHandleOps{observations: []observation{live}, afterKill: &quiet}
	got, err := newHandle(ops, testTimings()).Stop(context.Background(), StopDiscard)
	if err != nil || got != TerminationConfirmed {
		t.Fatalf("Stop = (%v, %v), want confirmed", got, err)
	}
	if want := "term,kill"; stringsJoin(ops.actions) != want {
		t.Fatalf("actions = %v, want %s", ops.actions, want)
	}
}

func TestFailedPidfdTermSkipsCooperativeGrace(t *testing.T) {
	live := observation{supervisor: supervisorLive, cgroup: cgroupPopulated}
	quiet := observation{supervisor: supervisorExited, cgroup: cgroupEmpty}
	ops := &scriptedHandleOps{
		observations: []observation{live, quiet},
		termErr:      errors.New("pidfd signal blocked"),
	}
	got, err := newHandle(ops, testTimings()).Stop(context.Background(), StopInterrupt)
	if err != nil || got != TerminationConfirmed {
		t.Fatalf("Stop = (%v, %v), want confirmed despite defensive TERM failure", got, err)
	}
	if ops.observeCalls != 2 {
		t.Fatalf("observations = %d, want initial plus post-kill", ops.observeCalls)
	}
	if want := "term,kill"; stringsJoin(ops.actions) != want {
		t.Fatalf("actions = %v, want %s", ops.actions, want)
	}
}

func stringsJoin(values []string) string {
	result := ""
	for i, value := range values {
		if i > 0 {
			result += ","
		}
		result += value
	}
	return result
}
