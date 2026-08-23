package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	agentharness "github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
	"github.com/srhg-ai-7cef3f93/ben/internal/workspace"
)

// `ben run` — the daemon (SPEC §11). Everything it does that is not wiring is
// one of two things: refusing to start, and stopping without abandoning
// anything.

// runDaemon is `ben run`, factored out of the CLI so a test can drive the whole
// lifecycle without exec'ing a binary. It returns the process exit code.
func runDaemon(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(stderr, "ben: run takes at most one path argument; got %d\n", fs.NArg())
		return 2
	}
	path := "WORKFLOW.md"
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}

	key, err := config.KeyFor(path)
	if err != nil {
		fmt.Fprintf(stderr, "ben: %v\n", err)
		return 1
	}

	// slog JSON to stdout, wrapped so every line about a run carries the §10.3
	// correlation attributes; the supervisor owns sinks and rotation. The lookup
	// is a cell because this logger is built before the orchestrator exists —
	// config.Watch takes it and publishes revision 1 before returning.
	lookup := &runLookup{}
	log := slog.New(correlate{inner: slog.NewJSONHandler(stdout, nil), lookup: lookup.find})

	files, err := openStateFiles(state.For(key), key, lookup, log)
	if err != nil {
		fmt.Fprintf(stderr, "ben: %v\n", err)
		return 1
	}
	defer files.close() //nolint:errcheck // reported below through daemon's error

	if err := daemon(context.Background(), path, log, newBuilder(log, files.dir.Transcripts()).build, files); err != nil {
		// Already logged in structured form at the point it was known; this is
		// the line a human running it in a terminal reads.
		fmt.Fprintf(stderr, "ben: %v\n", err)
		return 1
	}
	return 0
}

// buildFunc is config.WatchOptions.BuildRuntime for this daemon's runtime type.
// Taken as a parameter rather than constructed inside daemon so the lifecycle
// can be driven without real adapters — the same reason builder's kind lookups
// are fields (see runtime.go).
type buildFunc func(context.Context, *config.WorkflowDefinition, *orchestrator.Bundle, config.AdapterChange) (*orchestrator.Bundle, error)

// daemon builds the runtime, starts the loop, and drains it on a signal.
func daemon(ctx context.Context, path string, log *slog.Logger, build buildFunc, files *stateFiles) error {
	// The hooks the watcher calls are supplied at Watch time, and Watch builds
	// revision 1 before it returns — so they exist before the loop they ask.
	// See orchestratorCell.
	cell := &orchestratorCell{}

	w, err := config.Watch(ctx, path, config.WatchOptions[*orchestrator.Bundle]{
		Logger:       log,
		BuildRuntime: build,
		Barrier:      cell.barrier,
		Quiescent:    cell.quiescent,
	})
	if err != nil {
		return startupRefusal(err)
	}
	defer w.Close() //nolint:errcheck // Close is idempotent and reports nothing actionable

	// SPEC §10.1: record the declared mode. BEN verifies none of it — §10.1 owes
	// verification to the deployment and endorses no mechanism — so this line is
	// the whole of what "an explicit, recorded choice" gets from the program, and
	// the journal the supervisor keeps is where it is recorded.
	snap, _ := w.RuntimeSource().Load()
	recordDeployment(log, snap.Definition.Config.Deployment)

	o, err := orchestrator.New(daemonConfig(w.RuntimeSource(), w.Revalidate, log, files))
	if err != nil {
		return err
	}
	cell.set(o)
	// Publishing starts only now, and stops before the loop's own goroutines
	// are gone — the last write is taken after supervise has drained, so the
	// file an operator finds after a shutdown describes the state it exited in
	// rather than the state it was in when the signal landed.
	files.attach(o, log)

	return supervise(o, log)
}

// daemonConfig is the orchestrator configuration `ben run` builds.
//
// Extracted from daemon so the §9.10 capability seams are assertable. Both are
// wiring whose absence is *quiet* — a nil RunGone retains every recovered
// workspace forever, a nil FailureReasons reports every failure reason as lost —
// so "is it connected" is a property worth a test rather than a reading of this
// function.
func daemonConfig(
	runtime orchestrator.RuntimeSource,
	revalidate func(context.Context) config.Snapshot[*orchestrator.Bundle],
	log *slog.Logger,
	files *stateFiles,
) orchestrator.Config {
	cfg := orchestrator.Config{
		Runtime:       runtime,
		Revalidate:    revalidate,
		PrepRetryable: prepRetryable,
		// SPEC §9.10's workspace precondition, asked across a restart. The harness
		// owns the local process substrate and this is its restart-side probe: only
		// ESRCH, or a boot identity that cannot describe a live process, frees a
		// workspace. Without it every marker identifying a previous run stays
		// possibly-live forever, so an issue whose agent really did die is retained
		// and never resumed.
		RunGone: agentharness.EvidenceGone,
		Log:     log,
	}
	if files != nil {
		// Both halves of the §9.11 log: the sink the loop appends to, and the reader
		// §9.10 step 6 asks for a failure reason that outlived the process that
		// produced it. The same directory on both ends, which is the point — a
		// recovered `failed` comment names the reason only if this daemon is reading
		// the log the previous one wrote.
		cfg.Transitions = files.sink()
		cfg.FailureReasons = files.failures()
		// The #60 attempt-outcome log, in the same directory and for the same reason.
		cfg.Attempts = files.attemptSink()
	}
	return cfg
}

// supervise runs the loop until a signal, then drains it.
//
// Two contexts, and keeping them apart is the whole of the graceful half. The
// signal context only *reports* the signal; the loop's own context is cancelled
// afterwards, and never before the drain returns — the context passed to
// AgentRunner.Start descends from it, so cancelling it on the signal would
// abandon exactly the processes shutdown exists to stop (SPEC §11, §9.8).
func supervise(o *orchestrator.Orchestrator, log *slog.Logger) error {
	sigCtx, stopNotify := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	// Deliberately deferred rather than called when the first signal lands. A
	// second SIGTERM must **not** kill a daemon mid-drain: BUILD.md's acceptance
	// forbids exiting while a process group is unconfirmed, and restoring the
	// default disposition would do exactly that. Repeat signals are absorbed by
	// the handler this keeps installed, and logged (see logRepeatSignals).
	defer stopNotify()

	loopCtx, cancelLoop := context.WithCancel(context.Background())
	defer cancelLoop()

	// §9.10 before §9.1. Run refuses a loop that has not recovered, because the
	// first tick dispatches and dispatch skips only issues a local record already
	// covers — so starting without this puts a second agent onto every issue this
	// principal already holds.
	//
	// A failed pass is warn-and-continue (SPEC §6.4), not a refusal to start: the
	// candidate read is a tracker request like any other, and a daemon that exited
	// because GitHub was briefly down would be less available than one that starts
	// and retries on later ticks. Recover has already held those candidates out of
	// dispatch by the only means that works — it created no records for issues it
	// could not read, and the adapter's dispatchable verdict excludes anything
	// assigned.
	if err := o.Recover(loopCtx); err != nil {
		log.Warn("recovery did not complete; starting anyway and retrying on later ticks (SPEC §6.4)", "error", err)
	}

	loop := make(chan error, 1)
	go func() { loop <- o.Run(loopCtx) }()

	select {
	case err := <-loop:
		// The loop returned on its own — an errgroup member failed. Nothing to
		// drain: it is already down.
		return err
	case <-sigCtx.Done():
	}

	log.Info("shutdown signal received; stopping dispatch and interrupting in-flight runs")
	stopRepeats := logRepeatSignals(log, o)
	defer stopRepeats()

	// Unbounded on purpose. The drain waits for a confirmed termination wherever
	// a handle exists (SPEC §9.8), and BEN does not invent a deadline for that:
	// exiting on one would mean leaving while a process may still hold a
	// worktree. The supervisor's TimeoutStopSec is the bound: deploy/ben.service
	// sets it and says why, and TestTheSampleUnitSaysWhatTheDaemonReliesOn holds
	// the two to each other — a citation nobody checks outlives the file it names.
	if err := o.Shutdown(context.Background()); err != nil {
		return fmt.Errorf("draining: %w", err)
	}
	log.Info("shutdown: drained; claims and state labels left standing for recovery")

	cancelLoop()
	if err := <-loop; err != nil {
		return err
	}
	return nil
}

// logRepeatSignals tells an operator why a second ^C did nothing. It is the
// only thing that stands between "the daemon is waiting on a process that will
// not die" and "the daemon is hung", and those look identical from outside.
//
// Registered after the first signal, so everything it sees is a repeat. A signal
// arriving inside that window is absorbed by the handler NotifyContext still has
// installed — the process is not killed, only the log line is missed.
func logRepeatSignals(log *slog.Logger, o *orchestrator.Orchestrator) func() {
	repeats := make(chan os.Signal, 1)
	signal.Notify(repeats, syscall.SIGTERM, os.Interrupt)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case s := <-repeats:
				log.Warn("already shutting down; waiting for every run's process group to be confirmed gone. "+
					"BEN does not bound this — the supervisor's TimeoutStopSec does (SPEC §9.8, deploy/ben.service)",
					"signal", s.String(), "runs_outstanding", len(o.Status()))
			}
		}
	}()
	return func() {
		signal.Stop(repeats)
		close(done)
	}
}

// orchestratorCell holds the loop for the watcher's hooks.
//
// The order is forced and cannot be rearranged: config.Watch builds revision 1
// and publishes it *before* it returns, and orchestrator.New needs the source
// Watch returns. So the hooks are supplied first and find the loop later, which
// leaves one window — and it is safe by construction rather than by assumption.
//
// **Before the loop exists** there is no authority goroutine, and the only two
// things that create work bound to an identity — beginClaim and the conversion
// to a held claim — run on it. So an empty cell means "no such work can exist",
// which is precisely the predicate AdoptIdentity tests. Publishing directly
// reaches the same verdict without a goroutine to ask.
//
// **Between the cell being filled and Run draining signals** the ordinary path
// applies: AdoptIdentity posts to a buffered channel and waits on a cap-1 ack,
// so at most one adoption is outstanding and it completes as soon as the loop
// starts. It is ctx-bounded regardless.
type orchestratorCell struct {
	mu sync.RWMutex
	o  *orchestrator.Orchestrator
}

func (c *orchestratorCell) set(o *orchestrator.Orchestrator) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.o = o
}

func (c *orchestratorCell) get() *orchestrator.Orchestrator {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.o
}

func (c *orchestratorCell) barrier(ctx context.Context, prev, next *orchestrator.Bundle, commit func()) error {
	if o := c.get(); o != nil {
		return o.AdoptIdentity(ctx, prev, next, commit)
	}
	commit()
	return nil
}

func (c *orchestratorCell) quiescent() bool {
	o := c.get()
	return o == nil || o.IdentityQuiescent()
}

// prepRetryable classifies a workspace Prepare failure for SPEC §9.2's two prep
// edges. Keeping it here is what keeps the loop provider-agnostic (§6.1).
//
// Exactly one failure retries, and the asymmetry is §6.6's: a hook is operator
// code and can fail for reasons that pass on the next attempt — a flaky network
// install, a lock another process held. Every other named refusal is the
// provider reporting state it will not guess about — a base repository pointing
// somewhere else, a worktree whose registration disagrees with the disk, a
// branch that diverged from origin — and retrying those spends attempts to
// re-derive the same answer, then reports `failed` with the *last* one as the
// reason. §6.6 fails closed on ambiguity, and an unrecognized error is
// ambiguous.
func prepRetryable(err error) bool {
	return errors.Is(err, workspace.ErrHookFailed)
}

// recordDeployment logs the §10.1 declaration once, at startup.
//
// `attended` is Warn rather than Info, and deliberately: it asserts a human is
// present for this process's whole lifetime, BEN cannot notice them leaving, and
// a line nobody sees is how that assertion decays into the default it was
// written to prevent.
func recordDeployment(log *slog.Logger, d config.DeploymentConfig) {
	switch d.Mode {
	case config.DeploymentRiskAccepted:
		log.Warn("deployment declares risk-accepted mode: the agent is trusted with this daemon's "+
			"tracker authority, the dispatch label is routing rather than a boundary, and PR review "+
			"is the only remaining gate (SPEC §10.1)",
			"mode", string(d.Mode), "accepted_because", d.AcceptedBecause)
	case config.DeploymentAttended:
		log.Warn("deployment declares attended mode: this asserts a human is present for the whole "+
			"lifetime of this process. BEN cannot detect them leaving, and the assertion stops being "+
			"true the moment they do (SPEC §10.1)", "mode", string(d.Mode))
	default:
		log.Info("deployment mode declared", "mode", string(d.Mode))
	}
}

// startupRefusal turns a build failure into the message BUILD.md's acceptance
// asks for: which stage refused, distinguishably.
//
// The stages differ in what an operator does next, which is the whole reason
// they are separate. A malformed block is an edit to WORKFLOW.md. An adapter
// that would not construct is usually a value the block's syntax allowed and the
// adapter did not. An adapter that is not ready is the world: a credential, a
// host, an installed harness, a base cache pointing elsewhere.
func startupRefusal(err error) error {
	switch {
	case errors.Is(err, ErrUnknownKind):
		return fmt.Errorf("refusing to start — no adapter for that kind: %w", err)
	case errors.Is(err, ErrStructural):
		return fmt.Errorf("refusing to start — the workflow configuration is malformed; fix WORKFLOW.md: %w", err)
	case errors.Is(err, ErrConstruct):
		return fmt.Errorf("refusing to start — an adapter could not be constructed from this configuration: %w", err)
	case errors.Is(err, ErrNotReady):
		return fmt.Errorf("refusing to start — an adapter is not ready to run here; check credentials, connectivity and the installed harness: %w", err)
	}
	// Not a build failure: the config did not load at all.
	return fmt.Errorf("refusing to start: %w", err)
}
