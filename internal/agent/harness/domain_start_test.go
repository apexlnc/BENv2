package harness_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/agenttest"
	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

type recordingDomain struct{ run *recordingDomainRun }

func (d *recordingDomain) Ready(context.Context) error { return nil }

func (d *recordingDomain) Start(_ context.Context, launch harness.DomainLaunch) (harness.DomainRun, error) {
	if launch.OnDomain != nil {
		if err := launch.OnDomain(core.RunEvidence{Scheme: "recording-domain-v1", ID: "run-1"}); err != nil {
			return nil, err
		}
	}
	_, _ = launch.Stdout.WriteString("ok\n")
	_ = launch.Stdout.Close()
	_ = launch.Stderr.Close()
	d.run.finish()
	return d.run, nil
}

type recordingDomainRun struct {
	done      chan struct{}
	doneOnce  sync.Once
	probes    atomic.Int32
	stops     atomic.Int32
	probeTerm core.Termination
	stopTerm  core.Termination

	mu    sync.Mutex
	modes []core.StopMode
}

func (r *recordingDomainRun) finish() { r.doneOnce.Do(func() { close(r.done) }) }

func (r *recordingDomainRun) DirectDone() <-chan struct{} { return r.done }

func (r *recordingDomainRun) Probe(context.Context) core.Termination {
	r.probes.Add(1)
	return r.probeTerm
}

func (r *recordingDomainRun) Stop(_ context.Context, mode core.StopMode) core.Termination {
	r.stops.Add(1)
	r.mu.Lock()
	r.modes = append(r.modes, mode)
	r.mu.Unlock()
	return r.stopTerm
}

func (r *recordingDomainRun) stopModes() []core.StopMode {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.StopMode(nil), r.modes...)
}

func TestDomainEvidencePrecedesProviderPublication(t *testing.T) {
	var evidence core.RunEvidence
	h, err := start(t, []string{"/bin/sh", "-c", "printf 'ok\\n'"}, func(e core.RunEvidence) error {
		evidence = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	if evidence.Scheme == "" || evidence.ID == "" {
		t.Fatalf("evidence = %+v, want domain identity", evidence)
	}
	if len(events) != 1 || events[0].Type != core.EventSucceeded {
		t.Fatalf("events = %+v, want succeeded", events)
	}
}

func TestDomainEvidenceFailureReturnsNoUnownedHandle(t *testing.T) {
	h, err := start(t, []string{"/bin/sh", "-c", "sleep 30"}, func(core.RunEvidence) error {
		return errors.New("marker upgrade failed")
	})
	if err == nil || h != nil {
		t.Fatalf("Start = (%v, %v), want nil handle and error after trusted teardown", h, err)
	}
}

func TestNilEvidenceSinkLaunchesNormally(t *testing.T) {
	h, err := start(t, []string{"/bin/sh", "-c", "printf 'ok\\n'"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if events := collect(t, h); len(events) != 1 || events[0].Type != core.EventSucceeded {
		t.Fatalf("events = %+v, want succeeded", events)
	}
}

func TestSlowEvidenceSinkDoesNotLoseTerminalOutput(t *testing.T) {
	h, err := start(t, []string{"/bin/sh", "-c", "printf 'ok\\n'"}, func(core.RunEvidence) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if events := collect(t, h); len(events) != 1 || events[0].Type != core.EventSucceeded {
		t.Fatalf("events = %+v, want succeeded", events)
	}
}

func TestRunHandleDelegatesQuietAuthorityToTheExecutionDomain(t *testing.T) {
	run := &recordingDomainRun{
		done:      make(chan struct{}),
		probeTerm: core.TerminationUnconfirmed,
		stopTerm:  core.TerminationConfirmed,
	}
	domain := &recordingDomain{run: run}

	h, err := harness.Start(context.Background(), harness.Launch{
		Name:   "delegation-test",
		Argv:   []string{"provider"},
		Env:    []string{"PATH=/usr/bin:/bin"},
		Dir:    t.TempDir(),
		Prompt: "do it",
		Translate: func([]byte) []core.Event {
			return []core.Event{{Type: core.EventSucceeded}}
		},
		Timings: harness.Timings{
			StopGrace: 100 * time.Millisecond, PostExitDrain: 100 * time.Millisecond,
		},
		Domain: domain,
	})
	if err != nil {
		t.Fatal(err)
	}
	if events := collect(t, h); len(events) != 1 || events[0].Type != core.EventSucceeded {
		t.Fatalf("events = %+v, want succeeded", events)
	}
	if got := run.probes.Load(); got != 0 {
		t.Fatalf("natural completion made %d implicit Probe call(s)", got)
	}
	if got := run.stops.Load(); got != 0 {
		t.Fatalf("natural completion made %d implicit Stop call(s)", got)
	}

	if got := h.Probe(context.Background()); got != core.TerminationUnconfirmed {
		t.Fatalf("Probe = %v, want the domain's unconfirmed verdict", got)
	}
	if got := run.probes.Load(); got != 1 {
		t.Fatalf("domain Probe calls = %d, want 1", got)
	}
	if got := run.stops.Load(); got != 0 {
		t.Fatalf("Probe performed %d domain Stop call(s)", got)
	}

	if got := h.Stop(context.Background(), core.StopDiscard); got != core.TerminationConfirmed {
		t.Fatalf("Stop = %v, want the domain's confirmed verdict", got)
	}
	if got := run.stops.Load(); got != 1 {
		t.Fatalf("domain Stop calls = %d, want 1", got)
	}
	if got := run.stopModes(); len(got) != 1 || got[0] != core.StopDiscard {
		t.Fatalf("domain Stop modes = %v, want [discard]", got)
	}
}

func start(t *testing.T, argv []string, sink func(core.RunEvidence) error) (core.RunHandle, error) {
	t.Helper()
	return harness.Start(context.Background(), harness.Launch{
		Name:   "domain-test",
		Argv:   argv,
		Env:    []string{"PATH=/usr/bin:/bin"},
		Dir:    t.TempDir(),
		Prompt: "do it",
		Translate: func([]byte) []core.Event {
			return []core.Event{{Type: core.EventSucceeded}}
		},
		Timings: harness.Timings{
			StopGrace: 100 * time.Millisecond, PostExitDrain: 100 * time.Millisecond,
		},
		Domain: agenttest.Domain(),
		OnRun:  sink,
	})
}

func collect(t *testing.T, h core.RunHandle) []core.Event {
	t.Helper()
	var events []core.Event
	for event := range h.Events() {
		events = append(events, event)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not close")
	}
	return events
}
