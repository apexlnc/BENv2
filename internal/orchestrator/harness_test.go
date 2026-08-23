package orchestrator

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

const workflowTemplate = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben-queue"]
agent:
  kind: claude-code
limits:
  max_concurrent_agents: %CONCURRENCY%
  max_turns: %TURNS%
  max_attempts: 3
%EXTRA%deployment:
  mode: attended
---
Work issue {{ issue.identifier }}: {{ issue.title }}.
{% if attempt %}Attempt {{ attempt }}.
{% if run.previous_outcome == "succeeded" %}previous outcome succeeded.
{% elsif run.previous_outcome %}previous outcome {{ run.previous_outcome }}.
{% else %}previous outcome unknown.{% endif %}
{% endif %}
`

// epoch is the fixed start time; issue ages are offsets from it, so FIFO
// ordering is stated rather than dependent on when the test runs.
var epoch = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func definition(t *testing.T, concurrency, extra string) *config.WorkflowDefinition {
	t.Helper()
	return definitionTurns(t, concurrency, "4", extra)
}

// definitionTurns is definition with max_turns named too. It is spelled in the
// template rather than appended through extra because the loader refuses a
// duplicate mapping key — so a test that "overrides" a limit the template
// already sets would fail to load rather than override anything.
func definitionTurns(t *testing.T, concurrency, turns, extra string) *config.WorkflowDefinition {
	t.Helper()
	body := strings.ReplaceAll(workflowTemplate, "%CONCURRENCY%", concurrency)
	body = strings.ReplaceAll(body, "%TURNS%", turns)
	return loadDefinition(t, strings.ReplaceAll(body, "%EXTRA%", extra))
}

// loadDefinition loads a whole WORKFLOW.md body through the real loader, for a
// test that has to move a field the template fixes — `tracker.active_states`
// cannot be appended through extra, since a second `tracker:` key is a
// duplicate the loader refuses.
func loadDefinition(t *testing.T, body string) *config.WorkflowDefinition {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	def, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading the test workflow: %v", err)
	}
	return def
}

// harness wires an orchestrator to the three fakes and a manual clock, runs
// it, and stops it on cleanup.
type harness struct {
	t          *testing.T
	o          *Orchestrator
	Tracker    *fake.Tracker
	Workspaces *fake.Workspaces
	Runner     *fake.Runner
	Clock      *fake.Clock
	// Hooked wraps Workspaces when the test asks for an after_run hook.
	Hooked *fake.HookedWorkspaces
	// def is the definition the orchestrator was built with, kept so a test can
	// name the pointer a reload is meant to replace.
	def *config.WorkflowDefinition
	// Bundle is the adapter set in force at startup, and Source is the cell the
	// loop reads it from — both exposed so a test can publish a reload and then
	// assert which generation of adapters the work went through.
	Bundle *Bundle
	Source *testSource

	cancel  context.CancelFunc
	done    chan error
	stopped bool
	// Logs captures what the loop logged, for the handful of guarantees whose whole
	// observable effect *is* a log line — a possibly-live workspace retained, a
	// capability the daemon does not have. Asserting on those is not testing the
	// wording: §9.10 requires the operator be told, and a line at the wrong level or
	// without the workspace in it does not tell them.
	Logs *logCapture
}

// logStore is the shared record list behind every derived logCapture.
//
// Shared store, per-handler attributes: slog derives a new handler per
// Logger.With, and every one of them must append to the same place under the same
// lock. A first version gave each derived handler its own mutex over a shared
// slice, which is a data race the detector found on the second run — two handlers
// serializing against different locks serialize against nothing.
type logStore struct {
	mu      sync.Mutex
	records []logRecord
}

type logCapture struct {
	store *logStore
	// with are the attributes bound by Logger.With, which every record made through
	// this handler carries. Accumulated rather than dropped: the loop binds `issue`
	// that way (applyRecovery), so a handler that discarded them would report every
	// line as attribute-less and let a test asserting on `issue` pass against code
	// that never set it. Immutable per handler, so it needs no lock.
	with []slog.Attr
}

type logRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

func newLogCapture() *logCapture { return &logCapture{store: &logStore{}} }

// teeHandler sends every record to both handlers.
//
// The two capture different things and both are wanted: logTo is a text sink for
// the tests that assert on wording, and logCapture keeps records so a test can
// assert on a *level* and an *attribute*, neither of which is reliably greppable
// out of formatted text.
type teeHandler struct{ a, b slog.Handler }

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.a.Enabled(ctx, l) || t.b.Enabled(ctx, l)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	_ = t.a.Handle(ctx, r.Clone())
	return t.b.Handle(ctx, r)
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{a: t.a.WithAttrs(attrs), b: t.b.WithAttrs(attrs)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{a: t.a.WithGroup(name), b: t.b.WithGroup(name)}
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{Level: r.Level, Msg: r.Message, Attrs: map[string]string{}}
	for _, a := range c.with {
		rec.Attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.String()
		return true
	})
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	c.store.records = append(c.store.records, rec)
	return nil
}

// WithAttrs returns a handler carrying the bound attributes and sharing the same
// store, so a derived logger's lines land in one place under one lock.
func (c *logCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logCapture{store: c.store, with: append(append([]slog.Attr(nil), c.with...), attrs...)}
}

func (c *logCapture) WithGroup(string) slog.Handler { return c }

// find returns the captured records whose message contains substr.
func (c *logCapture) find(substr string) []logRecord {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	var out []logRecord
	for _, r := range c.store.records {
		if strings.Contains(r.Msg, substr) {
			out = append(out, r)
		}
	}
	return out
}

type verifierFunc func(ctx context.Context, issue core.Issue, ws core.Workspace) (VerifyResult, error)

func (f verifierFunc) Verify(ctx context.Context, issue core.Issue, ws core.Workspace) (VerifyResult, error) {
	return f(ctx, issue, ws)
}

// alwaysPublished is the verifier for tests that are not about verification.
var alwaysPublished = verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
	return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
})

type harnessOpts struct {
	concurrency string
	// extraConfig is spliced in after the limits entries. Indented, it adds a
	// limit ("  max_cost_usd: 1\n"); at column 0 it adds a top-level block
	// ("polling:\n  interval_ms: 1000\n").
	extraConfig string
	verifier    Verifier
	blocked     func() error
	prepRetry   func(error) bool
	issues      []core.Issue
	workspaces  Workspaces
	// configureWorkspaces applies concrete fake-provider state before the first
	// tick. It is for cross-boundary cases whose bug is in the provider shape,
	// not an orchestrator-only scripted return.
	configureWorkspaces func(*fake.Workspaces)
	// script, hang and stopUnconfirmed are applied before the loop starts:
	// the first tick fires immediately, so a test that set them afterwards
	// would be racing its own dispatch.
	script func(spec core.RunSpec, attempt int) []core.Event
	hang   bool
	// linger keeps each run's process alive after its stream closes, which is
	// the gap between Events and Done the real adapter has (SPEC §7.4).
	linger bool
	// stopUnconfirmed puts every Stop on §9.8's claim-retention path. A boolean
	// rather than a core.Termination because the useful default here is the
	// opposite of the type's: unconfirmed is the safe zero value in production
	// (see core.Termination) and the wrong one for the many tests that are not
	// about retention and just need a stop that works.
	//
	// Pair it with `descendants` whenever the run *ends* during the test. Stop's
	// answer is this knob; Probe's is derived from the group (fake Handle.Probe),
	// so a run that reaches Done with nothing outliving it has a group Probe
	// rightly calls gone and a Stop that says otherwise — a world the harness
	// cannot produce, and one where the pre-Done probe routes the very outcome
	// the test is asserting was held (#100, #103's family). A run held open for
	// the whole test (`hang`) has no such gap: the process is alive, so both
	// answers agree without the pairing.
	stopUnconfirmed bool
	// holdDone keeps `done` open after the group has gone quiet — the state a real
	// harness is in while its transcript finishes writing (#79).
	holdDone bool
	// descendants makes each run's process group outlive its process, so Probe
	// answers unconfirmed even after Done and only a Stop clears it (#79).
	descendants bool
	// probeGate blocks inside Probe, for testing a decision that overtakes an
	// observation already out (#79).
	probeGate func()
	// eventGate blocks before the nth scripted event, for a decision that has to
	// land *between* two events of one run — a drain arriving mid-stream, say.
	eventGate func(int)
	// stopGate blocks inside Stop, for testing a decision that overtakes a
	// signal ladder already walking.
	stopGate func()
	// startGate blocks inside Start, for testing a decision that lands while a
	// launch is still out — including one that will answer with an error.
	startGate func()
	// beforeStart configures the tracker before the loop's first tick.
	beforeStart func(*fake.Tracker)
	// prepareGate blocks inside Prepare, for testing an exit that races
	// in-flight work.
	prepareGate func()
	// claimBaseGate and failBeginClaimBase control the durable pending write
	// between approval and ben:claimed.
	claimBaseGate      func()
	failBeginClaimBase error
	// revalidate overrides the §9.4 step 2 backstop.
	revalidate func(context.Context, *harness) config.Snapshot[*Bundle]
	// withHook gives the workspace provider an after_run hook.
	withHook bool
	// failStart makes every Start fail.
	failStart error
	// prepareErr makes Prepare fail while still returning a workspace, as
	// the real provider does when it keeps one for forensics.
	prepareErr error
	// prepareFacts is the local branch evidence captured before hooks during a
	// fresh record's first Prepare. Nil states the ordinary no-prior-work
	// fixture explicitly.
	prepareFacts func(core.Workspace) (core.LocalBranchFacts, error)
	// legacyBase installs pre-epoch outgoing evidence for each initial issue.
	// It is opt-in: a genuinely fresh claim has no base to derive a floor from.
	legacyBase string
	// skipRecover starts the loop without §9.10, for the one test that asserts
	// Run refuses to.
	skipRecover bool
	// recoverErr tolerates a failed recovery pass, for the fixtures that make the
	// candidate read fail on purpose (§6.4 warn and continue).
	recoverErr bool
	// runGone answers §9.10's "is that run's group gone" for an identified marker.
	// Nil is a daemon with no prober, which is what makes every identified marker
	// possibly-live.
	runGone func(core.RunEvidence) (bool, error)
	// failures is the §9.10 step 6 transition-log reader. Nil is the missing
	// capability the startup warning names.
	failures FailureReasonReader
	// attemptFacts is the git account a finished attempt left on its branch,
	// which the next attempt's prompt reports (SPEC §9.6, #61). Nil leaves the
	// fake's own answer — an attempt that committed nothing.
	attemptFacts func(core.Workspace) (core.AttemptFacts, error)
	// budget wraps the tracker with the §8.5 per-tick request budget, for a test
	// about the window the tick opens rather than about the work inside it.
	budget *budgetTracker
	// logs captures the operator log, for a test asserting on what the loop
	// reported rather than on what it did. It enables Debug output and is safe
	// to read while the loop writes.
	logs *syncBuffer
	// definition replaces the whole workflow, template included, for a test
	// whose subject is what the prompt renders rather than what the loop
	// decides. The default template emits `issue.title` and not `issue.body`,
	// which is enough for every test that only needs *a* prompt and not enough
	// for §9.5's, where a pin over one half of the content has to fail.
	definition *config.WorkflowDefinition
	// transitions is the durable half of the §9.11 log. Nil is production's own
	// degraded mode and every other test's: the log stays in memory.
	transitions TransitionSink
	// attempts is the durable half of the attempt-outcome log (#60), on the same
	// terms. The in-memory tail is what almost every test reads.
	attempts AttemptSink
	// logTo captures the loop's own output, for a test asserting on what it says
	// rather than on what it does.
	logTo io.Writer
}

// budgetTracker is a tracker that meters its API cost (core.RequestBudget), which
// the GitHub adapter does and fake.Tracker deliberately does not: an in-memory
// tracker has no requests to bound, and modelling a budget it does not have would
// let the loop come to depend on one every tracker need not offer.
type budgetTracker struct {
	Tracker
	// report is what each BeginTick answers with — the window that just closed.
	report core.RequestReport

	mu        sync.Mutex
	ticks     int
	intervals []time.Duration
}

func (b *budgetTracker) BeginTick(interval time.Duration) core.RequestReport {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ticks++
	b.intervals = append(b.intervals, interval)
	return b.report
}

func (b *budgetTracker) windows() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ticks
}

func (b *budgetTracker) cadence(n int) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.intervals[n]
}

// syncBuffer is a log sink safe to read while the loop writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testSource is a runtime source a test can publish into. The production cell is
// written only by the config watcher (SPEC §5.4), so a test driving reloads owns
// one of its own rather than reaching into it — and it reproduces the two rules
// the loop depends on: one acquisition for snapshot and wake, and a revision that
// advances only on a transition.
type testSource struct {
	mu   sync.Mutex
	snap config.Snapshot[*Bundle]
	wake chan struct{}
}

func newTestSource(def *config.WorkflowDefinition, b *Bundle) *testSource {
	s := &testSource{wake: make(chan struct{})}
	s.publish(def, b, nil)
	return s
}

func (s *testSource) Load() (config.Snapshot[*Bundle], <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap, s.wake
}

// publish installs a configuration, advancing the revision on a transition only.
func (s *testSource) publish(def *config.WorkflowDefinition, b *Bundle, blocked error) config.Snapshot[*Bundle] {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := config.Snapshot[*Bundle]{Definition: def, Runtime: b, Blocked: blocked, Revision: s.snap.Revision}
	if def == s.snap.Definition && b == s.snap.Runtime && (blocked == nil) == (s.snap.Blocked == nil) {
		s.snap = next
		return next
	}
	next.Revision++
	s.snap = next
	close(s.wake)
	s.wake = make(chan struct{})
	return next
}

// reload publishes a new definition against the bundle already in force — the
// shape of a reload that moved no adapter's configuration.
func (s *testSource) reload(def *config.WorkflowDefinition, blocked error) {
	snap, _ := s.Load()
	b := snap.Runtime
	if def != nil && b != nil && def != b.Definition {
		// A definition-only reload keeps the adapters but must not leave the
		// bundle claiming it was built from the old one: the pairing assertion
		// below reads Bundle.Definition.
		next := *b
		next.Definition = def
		b = &next
	}
	if def == nil {
		def = snap.Definition
	}
	s.publish(def, b, blocked)
}

func start(t *testing.T, opts harnessOpts) *harness {
	t.Helper()
	if opts.concurrency == "" {
		opts.concurrency = "3"
	}
	if opts.verifier == nil {
		opts.verifier = alwaysPublished
	}

	h := &harness{
		t:          t,
		Tracker:    fake.NewTracker(opts.issues...),
		Workspaces: fake.NewWorkspaces(),
		Runner:     fake.NewRunner(),
		Clock:      fake.NewClock(epoch),
		done:       make(chan error, 1),
		Logs:       newLogCapture(),
	}
	if opts.configureWorkspaces != nil {
		opts.configureWorkspaces(h.Workspaces)
	}
	if opts.script == nil {
		opts.script = func(core.RunSpec, int) []core.Event { return fake.Succeed("session-1") }
	}
	// The §9.10 marker upgrade, wired where the assembly wires it: between the
	// runner that launched the run and the provider that owns its workspace
	// (core.RunnerOptions.OnRun → workspace RecordRun).
	//
	// Without it every marker stays evidence-less, so a restart reads
	// unknown_launch and parks — and a test asserting an orphan resumes would be
	// asserting a park instead. The `issue-` prefix undoes the fake provider's own
	// path naming, which is the same round trip RecordRun does with keyForPath.
	h.Runner.SetEvidenceSink(func(spec core.RunSpec, e core.RunEvidence) {
		h.Workspaces.RecordRunEvidence(
			core.Issue{Identifier: strings.TrimPrefix(filepath.Base(spec.Workspace.Path), "issue-")}, e)
	}, nil)
	h.Runner.SetScript(opts.script)
	h.Runner.SetHangAfterScript(opts.hang)
	h.Runner.SetLingerAfterStream(opts.linger)
	if opts.stopUnconfirmed {
		h.Runner.SetStopTermination(core.TerminationUnconfirmed)
	}
	h.Runner.SetDescendants(opts.descendants)
	h.Runner.SetHoldDone(opts.holdDone)
	h.Runner.SetStopGate(opts.stopGate)
	h.Runner.SetStartGate(opts.startGate)
	h.Runner.SetProbeGate(opts.probeGate)
	h.Runner.SetEventGate(opts.eventGate)
	h.Runner.SetFailStart(opts.failStart)
	if opts.prepareErr != nil {
		h.Workspaces.SetPrepareErrorWithWorkspace(opts.prepareErr)
	}
	if opts.prepareFacts == nil {
		opts.prepareFacts = func(ws core.Workspace) (core.LocalBranchFacts, error) {
			return core.LocalBranchFacts{Head: ws.BaseSHA, DescendsBase: true}, nil
		}
	}
	h.Workspaces.SetPrepareFacts(opts.prepareFacts)
	if opts.legacyBase != "" {
		for _, issue := range opts.issues {
			h.Workspaces.SetLegacyBasePin(issue.Identifier, opts.legacyBase)
		}
	}
	if opts.attemptFacts != nil {
		h.Workspaces.SetAttemptFacts(opts.attemptFacts)
	}
	if opts.prepareGate != nil {
		h.Workspaces.SetGate(opts.prepareGate)
	}
	if opts.claimBaseGate != nil {
		h.Workspaces.SetClaimBaseGate(opts.claimBaseGate)
	}
	if opts.failBeginClaimBase != nil {
		h.Workspaces.SetFailBeginClaimBase(opts.failBeginClaimBase)
	}
	if opts.beforeStart != nil {
		opts.beforeStart(h.Tracker)
	}

	var workspaces Workspaces = h.Workspaces
	if opts.workspaces != nil {
		workspaces = opts.workspaces
	} else if opts.withHook {
		h.Hooked = fake.NewHookedWorkspaces(h.Workspaces)
		workspaces = h.Hooked
	}

	var tracker Tracker = h.Tracker
	if opts.budget != nil {
		opts.budget.Tracker = h.Tracker
		tracker = opts.budget
	}

	h.def = opts.definition
	if h.def == nil {
		h.def = definition(t, opts.concurrency, opts.extraConfig)
	}
	h.Bundle = &Bundle{
		Definition:     h.def,
		Tracker:        tracker,
		Workspaces:     workspaces,
		Runner:         h.Runner,
		Verifier:       opts.verifier,
		ClaimPrincipal: fake.DefaultPrincipal,
		// Stated for every harness, not only the telemetry tests: an outcome
		// record naming nothing would let a wiring change that dropped the
		// descriptor pass every test that is about something else.
		Agent: testAgent,
	}
	h.Source = newTestSource(h.def, h.Bundle)

	o, err := New(Config{
		Runtime:        h.Source,
		Revalidate:     revalidateFor(h, opts),
		PrepRetryable:  opts.prepRetry,
		RunGone:        opts.runGone,
		FailureReasons: opts.failures,
		Clock:          h.Clock,
		// Both sinks: harnessLogger is what the text-asserting tests read, and the
		// record capture is what the level-and-attribute assertions need (see
		// logCapture). Neither can serve the other's purpose.
		Log: slog.New(teeHandler{
			a: harnessLogger(opts.logs, opts.logTo).Handler(),
			b: h.Logs,
		}),
		DaemonID:    "test-host/test-key",
		Transitions: opts.transitions,
		Attempts:    opts.attempts,
		// Stated rather than derived from a clock, so run ids are assertable.
		Instance: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.o = o

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel

	// §9.10 before §9.1, as the daemon does it: Run refuses a loop that has not
	// recovered (ErrNotRecovered), because the first tick dispatches and dispatch
	// only skips issues a record already covers. In the ordinary fixture the
	// principal holds nothing, so this classifies no candidates — its job here is
	// that every test drives the same startup sequence the daemon does.
	if !opts.skipRecover {
		if err := o.Recover(ctx); err != nil && !opts.recoverErr {
			cancel()
			t.Fatalf("Recover: %v", err)
		}
	}
	go func() { h.done <- o.Run(ctx) }()
	t.Cleanup(h.Stop)

	// SPEC §9.4: the first tick fires immediately at startup. Settling it
	// here is what lets a test assert on the resulting dispatch instead of
	// racing it — and stops a later Tick from advancing the clock past a
	// backoff timer the first tick already armed.
	h.settle(0)
	return h
}

// prepareWorkspaceForTest establishes the positive claim-base precondition for
// fixtures that create a workspace directly rather than through the live claim
// pipeline. Recovery tests that need another epoch install it explicitly.
func prepareWorkspaceForTest(w *fake.Workspaces, ctx context.Context, issue core.Issue, attempt int) (core.Workspace, error) {
	state, err := w.ClaimBase(ctx, issue)
	if err != nil {
		return core.Workspace{}, err
	}
	epoch := state.Epoch
	if state.State == core.ClaimBaseAbsent {
		epoch = 1
		if err := w.BeginClaimBase(ctx, issue, epoch); err != nil {
			return core.Workspace{}, err
		}
	}
	ws, _, err := w.PrepareClaim(ctx, issue, attempt, epoch)
	return ws, err
}

// Stop cancels the loop and waits for Run to return. Idempotent, because a test
// that needs the loop *provably* finished before it asserts — anything about what
// the shutdown drain wrote — calls it explicitly, and Cleanup calls it again.
// Without the guard the second call waits forever on a channel the first already
// drained, and reports it as the loop hanging.
func (h *harness) Stop() {
	if h.stopped {
		return
	}
	h.stopped = true
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		h.t.Error("orchestrator did not stop within 2s")
	}
}

// owedAfterStop stops the loop and returns what a record still owes, by name.
//
// Reading loop-owned state is only safe once the goroutine that owns it has
// gone, which is what Stop waits for — so this is deliberately terminal, and a
// test calling it is done driving the loop. It exists because the owed queue has
// no external shadow: an effect appended and never executed is invisible to the
// fakes, and "how many cleanup sequences were queued" is exactly the question a
// record that stops being re-decided has to answer.
func (h *harness) owedAfterStop(identifier string) []string {
	h.t.Helper()
	h.Stop()
	r, ok := h.o.records[identifier]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(r.owed))
	for _, eff := range r.owed {
		out = append(out, eff.what)
	}
	return out
}

// Tick fires one further poll cycle and waits for it to be applied. The
// startup tick has already run by the time start returns, so this is only for
// tests that need a *second* look at the world.
//
// Advancing by the poll interval also fires any timer due sooner, which is
// what the real clock would do; a test that needs a backoff to stay armed
// should assert before ticking.
func (h *harness) Tick() {
	h.t.Helper()
	if !h.Clock.BlockUntilWaiters(1) {
		h.t.Fatal("the ticker never armed its timer")
	}
	before := h.o.Transitions.Entries()
	h.Clock.Advance(time.Duration(h.def.Config.Polling.IntervalMS) * time.Millisecond)
	// Give the tick's worker and the resulting transitions time to land.
	h.settle(len(before))
}

// settle waits for the transition log to stop growing, which is the closest
// thing to "the loop has nothing left to do".
func (h *harness) settle(from int) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	last := from
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		n := len(h.o.Transitions.Entries())
		if n == last {
			stable++
			if stable >= 20 {
				return
			}
			continue
		}
		last, stable = n, 0
	}
}

// tickUntil drives poll cycles until a condition holds.
//
// For the outcomes that need a *tick* to happen rather than just time to pass —
// a retry, a re-read after a reload — and where the number of ticks is not
// something the test should be asserting. A single Tick() is a claim about how many
// cycles the work takes, and a reload publishes a wake of its own, so that claim is
// a race: the cycle the test was counting on may already have been spent.
func (h *harness) tickUntil(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		if !h.Clock.BlockUntilWaiters(1) {
			h.t.Fatalf("waiting for %s: the poll ticker never armed", what)
		}
		h.Clock.Advance(time.Duration(h.def.Config.Polling.IntervalMS) * time.Millisecond)
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		h.t.Fatalf("timed out driving ticks until %s", what)
	}
}

// barrierBudget is how long the barriers below wait before failing the test.
//
// One value for all of them, because they all wait on the same thing: a loop
// whose I/O is the fakes'. That work is memory-speed, so a barrier still
// untripped after two seconds is waiting on something that is not going to
// happen. A barrier over *real* subprocesses is a different question and takes
// its bound from launchBudget instead (#139).
const barrierBudget = 2 * time.Second

// deadlineMargin is what launchBudget leaves between its own expiry and the
// test binary's: enough for the barrier to be the thing that fails — naming
// the issue, its state and its path — rather than the binary's panic, and for
// the rest of the package to still run afterwards.
const deadlineMargin = 30 * time.Second

// launchBudget bounds a barrier that is waiting on real subprocesses, which in
// this package means the one fixture composing the real worktree provider: a
// launch there is an init, a bare clone, a fetch, a worktree add and an
// after_create hook that itself commits.
//
// Taken from the test binary's own deadline rather than named here. A constant
// can only ever encode the load on the machine it was chosen on — this fixture
// had 10s, then 60s, each a fresh guess at the same unknowable number, and the
// first of them is what #139 was filed for. `-timeout` is the knob a slow
// machine already has, so the bound moves with it instead of being independent
// of it.
func launchBudget(t *testing.T) (time.Duration, bool) {
	t.Helper()
	deadline, ok := t.Deadline()
	return budgetUntil(deadline, ok, time.Now())
}

// budgetUntil is launchBudget's arithmetic, kept separate because t.Deadline is
// not something a test can set. The bool keeps an unbounded test distinct from
// a bounded test whose deadline has arrived: both have a zero-duration wait,
// but the former waits forever and the latter must fail immediately.
func budgetUntil(deadline time.Time, ok bool, now time.Time) (time.Duration, bool) {
	if !ok {
		// `-timeout 0`: the run was told to have no bound, so neither has this.
		return 0, false
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0, true
	}
	fixedMarginBudget := remaining - deadlineMargin
	proportionalBudget := remaining / 2
	if fixedMarginBudget > proportionalBudget {
		return fixedMarginBudget, true
	}
	// A short binary timeout cannot spare the fixed margin. Give real Git half
	// of what remains and reserve the other half for the failure and cleanup;
	// barrierBudget is for fake I/O and is not a cap on subprocess work (#172).
	return proportionalBudget, true
}

// WaitState blocks until an issue reaches a state, or fails the test.
func (h *harness) WaitState(identifier string, want State) {
	h.t.Helper()
	deadline := time.Now().Add(barrierBudget)
	var got State
	for time.Now().Before(deadline) {
		for _, s := range h.o.Status() {
			if s.Identifier == identifier {
				got = s.State
				if s.State == want {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("issue %s is %q, want %q (path: %v)", identifier, got, want, h.o.Transitions.Path(identifier))
}

// notLaunched is every state SPEC §9.2 lets `preparing` leave for other than
// the launch. Derived from the transition map rather than listed, so an exit
// added to §9.2 later joins it instead of becoming something WaitLaunch waits
// out — a missing entry breaks nothing loudly, it only turns an immediate,
// named failure back into a full-budget wait that reads as a slow machine.
//
// The kill edge is added by hand: Allowed grants *any non-terminal* → failed
// without an entry in the map, so reading the map alone would miss the one
// exit that is most obviously not a launch.
var notLaunched = func() map[State]bool {
	out := map[State]bool{StateFailed: true}
	for tr := range legalTransitions {
		if tr.from == StatePreparing && tr.to != StateRunning {
			out[tr.to] = true
		}
	}
	return out
}()

// launchVerdict is what a §9.11 path says about the launch: it happened, it is
// not going to, or neither yet.
type launchVerdict struct {
	launched bool
	// refused is the exit out of preparing that was taken instead, empty when
	// the path has not reached one.
	refused State
}

// readLaunch classifies a transition path against the launch.
//
// In order and stopping at the first state that decides it, which is what makes
// the two answers exclusive: `failed` after a launch is a run that ended, not a
// launch that never came, and only the position in the path separates them.
func readLaunch(path []State) launchVerdict {
	for _, s := range path {
		if s == StateRunning {
			return launchVerdict{launched: true}
		}
		if notLaunched[s] {
			return launchVerdict{refused: s}
		}
	}
	return launchVerdict{}
}

// WaitLaunch blocks until an issue has launched — reached `running`, the loop's
// own word for "the worktree is ready, before_run passed, and the agent
// started".
//
// It separates two failures a stopwatch cannot tell apart, because reporting
// the wrong one sends a reader after the wrong bug: the loop taking an exit out
// of `preparing` that is not the launch is a regression, and is named the
// instant it happens, while still being on the way when the budget runs out is
// this machine, and says so (#139).
//
// Read off the §9.11 path rather than the live record, because a record that
// settles releases its claim and leaves the machine: live state answers ""
// for a launch that was refused a moment ago, and the log does not forget.
func (h *harness) WaitLaunch(identifier string) {
	h.t.Helper()
	budget, bounded := launchBudget(h.t)
	deadline := time.Now().Add(budget)
	for {
		path := h.o.Transitions.Path(identifier)
		switch v := readLaunch(path); {
		case v.launched:
			return
		case v.refused != "":
			h.t.Fatalf("issue %s left preparing for %q rather than launching (path: %v)",
				identifier, v.refused, path)
		case bounded && !time.Now().Before(deadline):
			h.t.Fatalf("issue %s had still not launched after %s — it is on its way, not refused,"+
				" so this is a budget the machine outran and -timeout is the knob (path: %v)",
				identifier, budget, path)
		}
		time.Sleep(time.Millisecond)
	}
}

// WaitGone blocks until an issue is no longer tracked.
func (h *harness) WaitGone(identifier string) {
	h.t.Helper()
	deadline := time.Now().Add(barrierBudget)
	for time.Now().Before(deadline) {
		found := false
		for _, s := range h.o.Status() {
			if s.Identifier == identifier {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("issue %s is still tracked; path: %v", identifier, h.o.Transitions.Path(identifier))
}

// WaitEffects waits for the serial effect queue to drain past n writes.
func (h *harness) WaitEffects(n int) {
	h.t.Helper()
	deadline := time.Now().Add(barrierBudget)
	for time.Now().Before(deadline) {
		calls, _, _ := h.Tracker.Snapshot()
		if len(calls) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// issueFixture is a minimal issue for unit tests that never reach a tracker.
func issueFixture(identifier string) core.Issue {
	return core.Issue{Identifier: identifier, State: "open", Labels: []string{"ben-queue"}}
}

// stateOf reports an issue's current state, or "" if untracked.
func (h *harness) stateOf(identifier string) State {
	for _, s := range h.o.Status() {
		if s.Identifier == identifier {
			return s.State
		}
	}
	return ""
}

// issueAssignees reads the tracker's view of an issue's assignees.
func (h *harness) issueAssignees(identifier string) []string {
	iss, err := h.Tracker.Get(context.Background(), identifier)
	if err != nil || iss == nil {
		return nil
	}
	return iss.Assignees
}

// startedOnly emits just the started event, so the run stays alive until it
// is stopped (paired with harnessOpts.hang).
func startedOnly(core.RunSpec, int) []core.Event {
	return []core.Event{{Type: core.EventStarted, SessionID: "s", Continuation: "s"}}
}

// publishDef is what a revalidation does in production: install what it found,
// then report it. A nil definition keeps the standing one, which is the shape of a
// failed revalidation — it reports a block without replacing the last-known-good
// (SPEC §5.4).
func (h *harness) publishDef(def *config.WorkflowDefinition, blocked error) config.Snapshot[*Bundle] {
	snap, _ := h.Source.Load()
	b := snap.Runtime
	if def == nil {
		def = snap.Definition
	} else if def != b.Definition {
		next := *b
		next.Definition = def
		b = &next
	}
	return h.Source.publish(def, b, blocked)
}

func revalidateFor(h *harness, opts harnessOpts) func(context.Context) config.Snapshot[*Bundle] {
	if opts.revalidate != nil {
		return func(ctx context.Context) config.Snapshot[*Bundle] { return opts.revalidate(ctx, h) }
	}
	if opts.blocked == nil {
		return nil
	}
	// The real backstop publishes what it found before returning it, so a block it
	// raises is in force for every later reader — not just for this caller.
	return func(context.Context) config.Snapshot[*Bundle] {
		snap, _ := h.Source.Load()
		return h.Source.publish(snap.Definition, snap.Runtime, opts.blocked())
	}
}

// hasWait reports whether some outstanding timer has exactly d left to run.
//
// Counting waiters cannot answer this. The fake clock keeps a waiter until its
// deadline passes, including one whose channel the code under test has walked
// away from — a ticker woken by a reload abandons the wait it was in and arms a
// new one, leaving two — so "a waiter exists" is true before the re-arm as well
// as after it. The duration is what separates them.
func hasWait(c *fake.Clock, d time.Duration) bool {
	for _, got := range c.Waits() {
		if got == d {
			return true
		}
	}
	return false
}

// hasWaitWithin is hasWait for a jittered delay, which has no exact value to
// name — only a ceiling the configured limit puts on it.
func hasWaitWithin(c *fake.Clock, max time.Duration) bool {
	for _, got := range c.Waits() {
		if got > 0 && got <= max {
			return true
		}
	}
	return false
}

// waitFor polls a condition, failing the test rather than hanging.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(barrierBudget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// applied reports how many signals of a kind the loop has finished handling. It
// is the acknowledgement half of every barrier below; see Orchestrator.applied
// for why a fake's call counter cannot serve as one.
//
// "Handled" rather than "acted on": a signal the loop dropped as stale
// (deliverable) is counted too. That is the right reading for a barrier — the
// question is whether the loop is still going to do something about this result,
// and a dropped one means it is not.
func (h *harness) applied(kind sigKind) uint64 { return h.o.applied[kind].Load() }

// TestAppliedCountsHandledNotDequeued pins the one property every barrier built on
// Orchestrator.applied depends on: the count advances when a signal's handler has
// *returned*, not when the loop took it off the channel. Moving the increment
// ahead of `handle` leaves every other test in the package green — the window it
// opens is a few instructions wide — so without this the ordering is a comment
// rather than a fact (#106's review).
//
// `sigAdopt` is the vehicle because its handler is the only one a test can park
// inside: `adoption.commit` is supplied by the caller, and it runs on the
// authority goroutine (runCommit). Every other handler either returns
// immediately or hands its blocking work to a worker.
//
// The `== 0` assertion is a negative, and it is barriered rather than timed: it is
// read while the handler is *provably* still executing, having announced itself
// from inside the commit. As long as it is parked there, no legitimate
// implementation can have counted the signal yet.
func TestAppliedCountsHandledNotDequeued(t *testing.T) {
	h := start(t, harnessOpts{})

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// So a failure above the release does not leave the authority goroutine parked
	// in the commit while the harness waits for the loop to stop.
	t.Cleanup(unblock)

	// prev and next are the same runtime, so the identity has not moved and the
	// handler goes straight to the commit rather than refusing over outstanding
	// work.
	cur, _ := h.o.cfg.Runtime.Load()
	a := &adoption{
		ack:    make(chan error, 1),
		prev:   cur.Runtime,
		next:   cur.Runtime,
		commit: func() { close(entered); <-release },
	}
	h.o.send(context.Background(), signal{kind: sigAdopt, adopt: a})

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the adopt handler never reached its commit")
	}
	if got := h.applied(sigAdopt); got != 0 {
		t.Fatalf("applied(sigAdopt) = %d while its handler is still running; the count must mean handled, not dequeued — every barrier over it reads it that way", got)
	}

	unblock()
	if err := <-a.ack; err != nil {
		t.Fatalf("adoption: %v", err)
	}
	waitFor(t, "the adopt signal to be counted once its handler returned", func() bool {
		return h.applied(sigAdopt) == 1
	})
}

// waitStopApplied blocks until the loop has handled n stop results.
//
// The rule, which holds for *every* tick a test fires expecting a ladder to
// follow it, not just the first: acknowledge the previous ladder's answer before
// firing it. `StopCount()` cannot serve, because the fake counts the call at
// entry, and between those two facts `beginStop`'s single-slot guard turns the
// tick into a no-op — after which the unconfirmed answer clears the flag and
// schedules nothing, so with a manual clock there is no later tick to recover it
// and the test hangs to its deadline on a fact nothing will produce.
//
// Both ticks in the ordinary shape need it, one ladder apart: the tick that must
// *retry* the stop (n=1) and the tick that must ask again after the test flips
// the answer to confirmed (n=2). #106's review found the second one after the
// first was fixed, which is the argument for stating the rule here rather than
// per call site.
func (h *harness) waitStopApplied(n uint64) {
	h.t.Helper()
	waitFor(h.t, fmt.Sprintf("the loop to handle %d stop result(s)", n), func() bool {
		return h.applied(sigStopped) >= n
	})
}

// waitForGroupQuestion blocks until §7.5's question — is this run's process group
// gone — has been put to a run and **the post-`Done` Stop's answer has been
// handled**. It is the barrier the "nothing followed the run" negatives need.
//
// Two waits, so a timeout says which fact never arrived.
//
//   - **Asked**, in either of the forms #79 separated: a Probe before `Done`, a
//     Stop after it. A run whose group nothing ever asked about is a reaped
//     harness taken for a quiet workspace, so this carries the assertion the old
//     `StopCount() == 0` check made — by name, at the barrier.
//   - **The Stop answered.** Not "an answer of either kind", which is what this
//     helper waited for first and is too weak: in the runs where `confirmQuiet`
//     fires before the loop sees `Done`, the acknowledged answer is the *probe's*,
//     and the Stop that follows is still outstanding when the negatives run. They
//     then pass because nothing has been decided yet rather than because the code
//     refused to act — and a mutant routing on an unconfirmed *stop* survives them
//     (#106's review staged exactly that: 20/20 green until this wait moved ahead
//     of the assertions).
//
// A Stop is always eventually asked here, so this cannot deadlock: `Done` closes
// on every run, and post-`Done` §7.5 makes the question a Stop.
//
// What keeps the negatives sound *after* the barrier is the fixture, not the
// barrier: under descendants+stopUnconfirmed every answer is `unconfirmed`, so
// once one has been handled and refused, nothing can route until the test changes
// what Stop reports. They hold for as long as that stands rather than for an
// interval — and the same acknowledgement is what the tick after the flip needs,
// so it is not repeated there.
func (h *harness) waitForGroupQuestion(run *fake.Handle) {
	h.t.Helper()
	if run == nil {
		h.t.Fatal("no run was started, so nothing could have asked about its process group")
	}
	waitFor(h.t, "§7.5's group question to be put to the run (a Probe before Done, a Stop after it)", func() bool {
		return run.ProbeCount()+run.StopCount() > 0
	})
	h.waitStopApplied(1)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// harnessLogger discards by default and captures at Debug when a test asks, so
// an assertion on a report can see the quiet lines as well as the loud ones.
func harnessLogger(logs *syncBuffer, logTo io.Writer) *slog.Logger {
	if logs != nil {
		return slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(cmp.Or[io.Writer](logTo, io.Discard), nil))
}

// PollNow runs one tick without advancing the clock, for tests that need the
// tick's effects (a refreshed preflight verdict, a reconciliation pass)
// without also firing whatever timers the poll interval would sweep past.
func (h *harness) PollNow() {
	h.t.Helper()
	before := len(h.o.Transitions.Entries())
	h.o.send(context.Background(), signal{kind: sigTick})
	h.settle(before)
}
