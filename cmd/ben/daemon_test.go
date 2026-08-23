package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

// `ben run`'s two jobs are refusing to start and stopping without abandoning
// anything. The first is asserted against the real stages; the second is driven
// with a real SIGTERM, because the signal wiring — which context is cancelled
// when, and which is not — is the thing under test and a hand-called Shutdown
// would skip exactly it.

// The stages refuse distinguishably, because what an operator does next differs:
// an edit to WORKFLOW.md, a value the block allowed and the adapter did not, or
// the world not being set up.
func TestRunRefusesToStartAndNamesTheStage(t *testing.T) {
	boom := errors.New("boom")

	cases := []struct {
		name    string
		arrange func(*harness)
		want    string
	}{
		{"malformed block", func(h *harness) { h.tracker.structural = boom }, "configuration is malformed"},
		{"unconstructible adapter", func(h *harness) { h.tracker.construct = boom }, "could not be constructed"},
		{"adapter not ready", func(h *harness) { h.tracker.ready = boom }, "not ready to run here"},
		{"unknown kind", func(h *harness) {
			h.b.tracker = func(string) (core.TrackerKind, bool) { return nil, false }
		}, "no adapter for that kind"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.arrange(h)
			def := h.def() // writes the fixture WORKFLOW.md at h.path

			err := daemon(context.Background(), def.Path, discardLog(), h.b.build, testStateFiles(t))
			if err == nil {
				t.Fatal("daemon started on a runtime that could not be built")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the stage (%q)", err, tc.want)
			}
			if !strings.Contains(err.Error(), "refusing to start") {
				t.Errorf("error %q does not say it refused to start", err)
			}
		})
	}
}

// A config that does not load at all is still a refusal to start, and it says so
// rather than reporting a build stage that was never reached.
func TestRunRefusesAMissingWorkflow(t *testing.T) {
	h := newHarness(t)
	err := daemon(context.Background(), h.path, discardLog(), h.b.build, testStateFiles(t))
	if err == nil {
		t.Fatal("daemon started with no WORKFLOW.md")
	}
	for _, stage := range []string{"malformed", "constructed", "not ready"} {
		if strings.Contains(err.Error(), stage) {
			t.Errorf("error %q names build stage %q, which was never reached", err, stage)
		}
	}
}

// The acceptance criterion: SIGTERM during an active run interrupts the agent,
// the claim is handled per stop semantics, and the exit is clean.
//
// Driven through a real signal rather than by calling Shutdown, because the
// wiring is what is being tested: the loop's context must stay live across the
// signal — it is the one passed to AgentRunner.Start — and be cancelled only
// after the drain returns.
func TestSIGTERMDuringARunInterruptsItAndExitsCleanly(t *testing.T) {
	d := startDaemon(t)

	// The barrier is the tracker's own projection: `ben:running` is written by
	// the transition into running, so an agent is live by the time it appears.
	d.waitLabel("7", core.StateLabelRunning)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("raising SIGTERM: %v", err)
	}

	if err := d.wait(); err != nil {
		t.Fatalf("ben run exited with %v; a completed drain is a clean exit", err)
	}

	// The agent was interrupted, not discarded and not abandoned.
	handle := d.Runner.LastHandle()
	if handle == nil {
		t.Fatal("no run was ever started, so nothing was interrupted")
	}
	if got := handle.Stops(); len(got) != 1 || got[0] != core.StopInterrupt {
		t.Errorf("stops = %v, want exactly one StopInterrupt", got)
	}

	// And the claim and the label are standing, which is what §9.10 resumes
	// from (SPEC §9.8 as amended).
	if n := d.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; shutdown initiates no release", n)
	}
	if got := d.Tracker.Label("7"); got != core.StateLabelRunning {
		t.Errorf("state label = %q, want it left standing at %q", got, core.StateLabelRunning)
	}
}

// daemonRun is one `ben run` under test, wired to the fakes.
type daemonRun struct {
	t *testing.T
	// path is the WORKFLOW.md this daemon was started for — what `ben status`
	// is given, and all it is given.
	path       string
	files      *stateFiles
	logTo      io.Writer
	Tracker    *fake.Tracker
	Runner     *fake.Runner
	Workspaces *fake.Workspaces
	// Verifier defaults to neverAsked, which is right for every test whose run
	// is interrupted before it reports. A test whose run *completes* replaces
	// it, and the default's panic is what stops one from doing so by accident.
	Verifier orchestrator.Verifier
	exit     chan error
	// exited records that the daemon's return has already been collected.
	// Cleanup consults it rather than probing the channel: a probe cannot tell
	// "still running" from "already read", and raising a second SIGTERM against
	// a daemon that has finished restores the default disposition and kills the
	// test binary — which is exactly what this fixture did before.
	exited bool
}

// log is where the daemon's slog output goes: discarded unless a test asked
// for it, because the §10.3 correlation attributes are only assertable against
// the real handler this binary builds.
func (d *daemonRun) log() *slog.Logger {
	if d.logTo == nil {
		return discardLog()
	}
	return slog.New(correlate{inner: slog.NewJSONHandler(d.logTo, nil), lookup: d.files.lookup.find})
}

// wait collects the daemon's exit, once. Only the test goroutine calls it, and
// only ever before cleanup.
func (d *daemonRun) wait() error {
	d.t.Helper()
	select {
	case err := <-d.exit:
		d.exited = true
		return err
	case <-time.After(10 * time.Second):
		d.t.Fatal("ben run did not exit")
		return nil
	}
}

// startDaemon runs the shutdown fixture: an agent that starts and then hangs.
func startDaemon(t *testing.T) *daemonRun { return startDaemonWith(t, testStateFiles(t)) }

// startDaemonWith is the same daemon against a caller's state dir, so a test
// about the §10.3 files can read what this one wrote. arrange runs after the
// fakes are built and before the daemon starts.
//
// A nil files puts the daemon in the directory `ben status` would resolve for
// its own WORKFLOW.md. That is the only way to drive both halves of the command
// against one directory: the key is derived from the path and neither side is
// told it, which is the property worth testing rather than working around.
func startDaemonWith(t *testing.T, files *stateFiles, arrange ...func(*daemonRun)) *daemonRun {
	t.Helper()
	return startDaemonFor(t, files, nil, arrange...)
}

// startDaemonFor is startDaemonWith over a caller's WORKFLOW.md variations —
// for a test that needs something in the *configuration* the daemon runs under,
// which arrange runs too late to change.
func startDaemonFor(t *testing.T, files *stateFiles, defOpts []func(*workflowSpec), arrange ...func(*daemonRun)) *daemonRun {
	t.Helper()
	h := newHarness(t)
	def := h.def(defOpts...)
	if files == nil {
		key, err := config.KeyFor(def.Path)
		if err != nil {
			t.Fatalf("KeyFor: %v", err)
		}
		files = openTestState(t, state.For(key), key)
	}

	d := &daemonRun{
		path:       def.Path,
		files:      files,
		t:          t,
		Tracker:    fake.NewTracker(fake.Issue("7", time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))),
		Runner:     fake.NewRunner(),
		Workspaces: fake.NewWorkspaces(),
		Verifier:   neverAsked{},
		exit:       make(chan error, 1),
	}
	// This fixture is about shutdown, not a reclaimed branch. State the
	// ordinary first-claim evidence explicitly so #94's pre-launch check does
	// not have to invent a zero-value verdict.
	d.Workspaces.SetPrepareFacts(func(ws core.Workspace) (core.LocalBranchFacts, error) {
		return core.LocalBranchFacts{Head: ws.BaseSHA, DescendsBase: true}, nil
	})
	// `started` and then nothing: the agent is still running when the signal
	// arrives, which is the case the criterion is about.
	d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
		return []core.Event{{Type: core.EventStarted, SessionID: "s", Continuation: "s"}}
	})
	d.Runner.SetHangAfterScript(true)
	for _, fn := range arrange {
		fn(d)
	}

	build := func(_ context.Context, def *config.WorkflowDefinition, _ *orchestrator.Bundle, _ config.AdapterChange) (*orchestrator.Bundle, error) {
		return &orchestrator.Bundle{
			Definition: def,
			Tracker:    d.Tracker,
			Workspaces: d.Workspaces,
			Runner:     d.Runner,
			Verifier:   d.Verifier,
			// Resolved from the real definition through the real registry, as
			// `ben run` does: a hand-written descriptor here would prove only
			// that the field is carried (#60).
			Agent:          config.AgentDescriptor(def),
			ClaimPrincipal: fake.DefaultPrincipal,
		}, nil
	}

	go func() { d.exit <- daemon(context.Background(), def.Path, d.log(), build, files) }()
	t.Cleanup(func() {
		if d.exited {
			return
		}
		// The test failed before its own SIGTERM. Stop the daemon so it does not
		// outlive the test and leak its signal handler into the next one.
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-d.exit:
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not exit during cleanup")
		}
	})
	return d
}

// waitLabel blocks until the tracker carries a state label, which is the
// daemon's own projection and therefore a barrier rather than a guess.
func (d *daemonRun) waitLabel(identifier string, want core.StateLabel) {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got core.StateLabel
	for time.Now().Before(deadline) {
		got = d.Tracker.Label(identifier)
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	d.t.Fatalf("issue %s label = %q, want %q", identifier, got, want)
}

// neverAsked is a verifier the drain must not reach: the run is interrupted
// while it is still going, so no verdict is owed and asking for one would mean
// the outcome had been routed.
type neverAsked struct{}

func (neverAsked) Verify(context.Context, core.Issue, core.Workspace) (orchestrator.VerifyResult, error) {
	panic("shutdown routed a run's outcome to verification")
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stopAndWait raises the daemon's own SIGTERM and collects its exit. Tests
// about the §10.3 files need the daemon *finished* before they read: the last
// run-record write is taken after the drain, and a read that raced it would be
// asserting against a file the daemon had not written yet.
func (d *daemonRun) stopAndWait() {
	d.t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		d.t.Fatalf("raising SIGTERM: %v", err)
	}
	if err := d.wait(); err != nil {
		d.t.Fatalf("ben run exited with %v", err)
	}
	// What runDaemon's own deferred close does: stop publishing and take the
	// last write. Without it a test reads a file the daemon had not finished
	// writing, which is not the file an operator finds.
	if err := d.files.close(); err != nil {
		d.t.Errorf("closing the state files: %v", err)
	}
}

// waitTransition blocks until the §9.11 log records an edge into want. The log
// is the daemon's own record of the transition, so it is a barrier for states
// the tracker projection collapses — §9.3 maps backoff, preparing and verifying
// all onto ben:claimed, so waitLabel cannot see them apart.
func (d *daemonRun) waitTransition(issue, want string) {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, _, err := d.files.dir.ReadTransitions().Tail(0)
		if err == nil {
			for _, e := range entries {
				if e.Issue == issue && e.To == want {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	d.t.Fatalf("issue %s never transitioned to %q", issue, want)
}

// lockedBuffer is a bytes.Buffer a daemon goroutine writes and the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitRuns blocks until runs.json carries n records.
//
// A poll rather than a barrier, because there is nothing to synchronize on and
// nothing that should be: the file is refreshed on a cadence (see refresh), so
// it trails the loop by design and by a bounded amount. A test that read it the
// instant the tracker projected a label would be asserting that the bound is
// zero, which is not what the file promises.
func (d *daemonRun) waitRuns(n int) {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		runs, err := d.files.dir.ReadRuns()
		if err == nil {
			got = len(runs.Records)
			if got == n {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	d.t.Fatalf("runs.json holds %d records, want %d", got, n)
}

// SPEC §9.10's two capability seams are wired.
//
// Both absences are quiet in production and loud only in a log line: a nil RunGone
// leaves every workspace whose marker identifies a previous run possibly-live
// forever, so an issue whose agent really did die is retained and never resumed; a
// nil FailureReasons reports every recovered failure's reason as lost. Neither is a
// crash and neither is a refusal — they are a daemon that looks healthy and works
// less than it should, which is why "is it connected" is asserted here rather than
// read off the constructor.
func TestDaemonConfigWiresTheRecoveryCapabilities(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("the run prober is the harness's own", func(t *testing.T) {
		cfg := daemonConfig(nil, nil, log, nil)
		if cfg.RunGone == nil {
			t.Fatal("RunGone is nil: no workspace whose marker identifies a previous run could ever be freed")
		}
		// Behaviour rather than presence: a func field can be non-nil and answer
		// nothing useful. A boot identity that cannot describe a process running now
		// is §9.10's one signal-free proof, so it is the cheapest input with a known
		// answer — and it also proves this is the *harness* probe rather than a stub.
		gone, err := cfg.RunGone(core.RunEvidence{
			Scheme: core.RunEvidenceLocal,
			ID:     "4242",
			Boot:   "a-boot-identity-this-host-does-not-have",
		})
		if err != nil {
			t.Fatalf("probing evidence from another boot: %v", err)
		}
		if !gone {
			t.Error("evidence from another boot did not read as gone; recovery would never converge after a reboot")
		}
	})

	t.Run("the failure-reason reader is the state dir's, and reads what the sink wrote", func(t *testing.T) {
		// The two halves of §9.11 over one directory, which is the whole point: a
		// recovered `failed` comment names its reason only if this daemon reads the log
		// the previous one wrote. Asserted by writing through the sink and reading back
		// through the reader, rather than by comparing pointers — the seam is satisfied
		// structurally, so a mismatch would be a silent nil.
		files := openTestState(t, state.At(t.TempDir()), "wf")
		cfg := daemonConfig(nil, nil, log, files)
		if cfg.Transitions == nil {
			t.Fatal("Transitions is nil: the §9.11 log would not survive the process")
		}
		if cfg.FailureReasons == nil {
			t.Fatal("FailureReasons is nil: every recovered failure would report that its reason did not survive")
		}

		want := core.FailureStalled
		if err := cfg.Transitions.Append(orchestrator.TransitionEntry{
			TS: time.Now(), Issue: "7", From: orchestrator.StateRunning, To: orchestrator.StateFailed,
			Actor: "test", Reason: "the agent stopped reporting", FailureReason: want,
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}

		got, ok, err := cfg.FailureReasons.LastFailure("7")
		if err != nil {
			t.Fatalf("LastFailure: %v", err)
		}
		if !ok {
			t.Fatal("the reader could not find a failure the sink had just written")
		}
		if got.Reason != want {
			t.Errorf("reason = %q, want %q", got.Reason, want)
		}
		if got.Detail == "" {
			t.Error("no detail; §9.10's reconstructed comment is only as useful as what it can say")
		}
	})
}

// SPEC §10.1: BEN verifies none of the deployment properties, so the record is
// the whole of what "an explicit, recorded choice" gets from the program.
//
// `attended` and `risk-accepted` are Warn rather than Info deliberately — the
// first asserts a human BEN cannot see leave, the second says the agent holds
// this daemon's tracker authority. A line nobody sees is how either decays into
// the default it was written to prevent.
func TestTheDeploymentDeclarationIsRecordedAtStartup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		block   string
		level   string
		mustSay []string
	}{
		{
			name:    "attended says what it asserts",
			block:   "deployment:\n  mode: attended\n",
			level:   "WARN",
			mustSay: []string{"attended", "cannot detect them leaving"},
		},
		{
			name:    "risk-accepted carries its reason",
			block:   "deployment:\n  mode: risk-accepted\n  accepted_because: canary repo only\n",
			level:   "WARN",
			mustSay: []string{"risk-accepted", "canary repo only", "tracker authority"},
		},
		{
			name:    "protected is recorded without alarm",
			block:   "deployment:\n  mode: protected\n",
			level:   "INFO",
			mustSay: []string{"protected"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out lockedBuffer
			log := slog.New(slog.NewTextHandler(&out, nil))
			recordDeployment(log, mustDeployment(t, tc.block))

			line := out.String()
			if !strings.Contains(line, "level="+tc.level) {
				t.Errorf("logged at the wrong level:\n%s", line)
			}
			for _, want := range tc.mustSay {
				if !strings.Contains(line, want) {
					t.Errorf("record does not mention %q:\n%s", want, line)
				}
			}
		})
	}
}

// mustDeployment loads a declaration through the real loader rather than
// building the struct, so the test cannot assert on a shape the file could not
// produce.
func mustDeployment(t *testing.T, block string) config.DeploymentConfig {
	t.Helper()
	h := newHarness(t)
	def := h.def(withDeployment(block))
	return def.Config.Deployment

}
