package integration

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
)

// epoch fixes the start of every scenario's clock, so nothing in this package
// depends on when it runs.
var epoch = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// barrierTimeout bounds every wait. It is generous on purpose: it is the
// deadline on a *deadlock*, never the mechanism by which a fact becomes true.
// Every barrier below waits on something the fixture produces, so on a working
// build the wait ends in microseconds however loaded the machine is, and this
// number only decides how long a broken one hangs before it reports what it was
// waiting for.
const barrierTimeout = 10 * time.Second

// The SHAs a scripted run leaves behind. Full-length because §9.7's detail
// lines abbreviate anything longer than 12 characters, and a fixture that was
// already short would hide a truncation bug in the operator-facing text.
const (
	agentCommitSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rewrittenSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// workflow is a scenario's WORKFLOW.md, rendered rather than pasted so that a
// scenario states only the limits it is about.
//
// It is rendered through the *real* loader, which is the point: these scenarios
// run under a configuration that has passed §5.3's strict decode and §5.7's
// registry check, not under a hand-built config.WorkflowDefinition. A limit that
// stopped being loadable would fail here as well as in internal/config.
type workflow struct {
	PollIntervalMS    int
	MaxConcurrent     int
	MaxTurns          int
	MaxAttempts       int
	MaxRetryBackoffMS int
	// MaxCostUSD nil omits the key, which is how §9.9's budget cap is disabled.
	MaxCostUSD *float64
	// Prompt is the template body. Empty takes defaultPrompt.
	Prompt string
	// TrackerProvider and AgentProvider are the two opaque blocks (§5.2.2,
	// §5.2.5), as YAML fragments indented four spaces.
	TrackerProvider string
	AgentProvider   string
}

// defaultPrompt is deliberately close to the dogfood WORKFLOW.md's shape: the
// issue fields, and the continuation branch that distinguishes a second turn
// from a first. A prompt with no template expressions would leave the Liquid
// layer unexercised by every scenario here.
const defaultPrompt = `Work issue {{ issue.identifier }}: {{ issue.title }}.
{% if attempt %}This is attempt {{ attempt }}.
{% if run.previous_outcome == "succeeded" %}Your previous session ended cleanly but without a published pull request — finish the remaining work and publish.
{% elsif run.previous_outcome %}Your previous session failed ({{ run.previous_outcome }}) — recover and continue.
{% endif %}{% endif %}`

func defaultWorkflow() workflow {
	return workflow{
		// Long enough that no scenario ever gets a tick it did not ask for: the
		// clock is manual, so this is the amount a scenario advances to take one
		// tick, not an amount of real time anything waits.
		PollIntervalMS:    30000,
		MaxConcurrent:     2,
		MaxTurns:          4,
		MaxAttempts:       3,
		MaxRetryBackoffMS: 60000,
		TrackerProvider:   "    repo: acme/widgets",
		AgentProvider:     "    permission_mode: bypassPermissions",
	}
}

func (w workflow) render() string {
	prompt := w.Prompt
	if prompt == "" {
		prompt = defaultPrompt
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("tracker:\n  kind: github\n  provider:\n")
	fmt.Fprintf(&b, "%s\n", w.TrackerProvider)
	b.WriteString("  required_labels: [\"ben-queue\"]\n")
	b.WriteString("agent:\n  kind: claude-code\n  provider:\n")
	fmt.Fprintf(&b, "%s\n", w.AgentProvider)
	fmt.Fprintf(&b, "polling:\n  interval_ms: %d\n", w.PollIntervalMS)
	b.WriteString("limits:\n")
	fmt.Fprintf(&b, "  max_concurrent_agents: %d\n", w.MaxConcurrent)
	fmt.Fprintf(&b, "  max_turns: %d\n", w.MaxTurns)
	fmt.Fprintf(&b, "  max_attempts: %d\n", w.MaxAttempts)
	fmt.Fprintf(&b, "  max_retry_backoff_ms: %d\n", w.MaxRetryBackoffMS)
	if w.MaxCostUSD != nil {
		fmt.Fprintf(&b, "  max_cost_usd: %g\n", *w.MaxCostUSD)
	}
	// SPEC §5.2.9: required, no default. The suite is not about §10.1, so it
	// states the mode that asserts nothing — a human is running the tests.
	b.WriteString("deployment:\n  mode: attended\n")
	b.WriteString("---\n")
	b.WriteString(prompt)
	b.WriteString("\n")
	return b.String()
}

// scenarioConfig is what a test states about its world before the loop starts.
// Everything scriptable is set here rather than after start, because a fixture
// seeded after the first tick is racing its own dispatch.
type scenarioConfig struct {
	// workflow is the configuration the scenario runs under. Nil takes
	// defaultWorkflow — a pointer rather than a value so that "unset" and "a
	// workflow with every limit at zero" cannot be the same thing, which is the
	// distinction that makes an omitted field a load refusal here instead of a
	// silently degenerate run.
	workflow *workflow
	// issues are seeded into the tracker before the loop runs. Empty means one
	// dispatchable issue "7".
	issues []core.Issue
	// facts is the §9.7 publish evidence a finished run left behind. Nil takes
	// pushedAndDescends, the shape whose verdict then turns on the tracker.
	facts func(core.Workspace) (core.PublishFacts, error)
	// prepareFacts is the pre-repin observation against an outgoing claim base.
	// Nil reports no movement for scenarios that have only one claim epoch.
	prepareFacts func(core.Workspace) (core.LocalBranchFacts, error)
	// script is what the agent does. Nil takes a clean exit claiming success.
	script func(core.RunSpec, int) []core.Event
	// verifier overrides the real §9.7 checker. Almost nothing should: the
	// chain from git facts to a routed verdict is what makes these integration
	// tests. It exists for the scenarios whose subject is upstream of
	// verification and which would otherwise have to script evidence they are
	// not about.
	verifier orchestrator.Verifier
	// runGone is §9.10's run probe: is the run this evidence identifies
	// confirmed gone? It answers for a run *this process never started*, which is
	// why it is a scenario's to state — a scenario says whether the agent died
	// with the daemon or outlived it, and nothing else can.
	//
	// Nil leaves orchestrator.Config.RunGone nil, which is a daemon that cannot
	// ask, and it is the right default for every scenario that never restarts:
	// such a scenario has no marker from a previous tenure to probe, and Recover
	// names the missing capability at startup rather than waiting forever on a
	// question nobody is asking.
	//
	// The scenario keeps it in a cell (scenario.Prober) so it can move while the
	// loop is running — "the domain has since become quiet" is a fact a restart scenario
	// has to be able to produce, and assigning Config.RunGone mid-run is a data
	// race the authority goroutine would lose.
	runGone func(core.RunEvidence) (bool, error)
	// before runs once the fakes exist and before the loop starts.
	before func(*scenario)
}

// definition is the workflow this scenario runs under.
func (cfg scenarioConfig) definition() workflow {
	if cfg.workflow == nil {
		return defaultWorkflow()
	}
	return *cfg.workflow
}

// scenario is one invariant's world: the three fakes, a manual clock, the real
// loader's definition, and the real loop assembled over them.
type scenario struct {
	t *testing.T

	Tracker    *fake.Tracker
	Workspaces *fake.Workspaces
	Runner     *fake.Runner
	Clock      *fake.Clock
	// Prober is §9.10's run probe in a cell the scenario can move mid-run. It
	// answers nothing until a scenario states a probe (scenarioConfig.runGone).
	Prober *prober
	// Logs is what the loop logged. See logCapture for the one kind of guarantee
	// that is read off it rather than off a fake.
	Logs *logCapture

	o   *orchestrator.Orchestrator
	def *config.WorkflowDefinition
	// cfg is what the scenario stated about its world, kept because a restart
	// rebuilds the whole loop from it.
	cfg scenarioConfig
	// watcher is set by startWatched, and nil otherwise.
	watcher *config.Watcher[*orchestrator.Bundle]
	// Path is the WORKFLOW.md on disk. Only the reload scenarios rewrite it.
	Path string

	pollInterval time.Duration
	done         chan error
	cancel       context.CancelFunc
	// revalidations counts §9.4 step-2 calls, which is what tick() waits on.
	// See tick for why this and not the candidate fetch.
	revalidations atomic.Int64
}

// pushedAndDescends is the evidence of a run that committed and pushed
// everything: all three git legs of §9.7 hold, so the verdict turns on whether
// the tracker has an open pull request.
func pushedAndDescends(core.Workspace) (core.PublishFacts, error) {
	return core.PublishFacts{
		Head:          agentCommitSHA,
		DescendsBase:  true,
		RemoteProbed:  true,
		RemoteHead:    agentCommitSHA,
		RemoteHasHead: true,
	}, nil
}

// start assembles and runs one scenario. The runtime source is fixed at one
// configuration; startWatched is the constructor for the reload scenarios.
func start(t *testing.T, cfg scenarioConfig) *scenario {
	t.Helper()
	h := build(t, cfg, writeWorkflow(t, cfg.definition().render()))
	if err := h.launch(h.fixedConfig()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return h
}

// fixedConfig is the loop's configuration for a scenario whose workflow never
// moves. restart rebuilds it, which is why it is a method rather than four lines
// inside start.
func (h *scenario) fixedConfig() orchestrator.Config {
	source := config.NewRuntimeSource(h.def, h.bundle(h.cfg))
	cfg := h.baseConfig()
	cfg.Runtime = source
	// §9.4 step 2 over a source that never moves: report what is in force, never
	// blocked, and — the part that matters — the *same* revision every time, since
	// the loop discards work whose revision was superseded and a hook that
	// re-versioned an unchanged configuration would invalidate reads nothing had
	// superseded, on every tick.
	cfg.Revalidate = func(context.Context) config.Snapshot[*orchestrator.Bundle] {
		h.revalidations.Add(1)
		snap, _ := source.Load()
		return snap
	}
	return cfg
}

// baseConfig is everything a scenario's loop is built with that does not depend
// on where its configuration comes from.
func (h *scenario) baseConfig() orchestrator.Config {
	cfg := orchestrator.Config{
		Clock:    h.Clock,
		Log:      slog.New(h.Logs),
		DaemonID: "integration/" + h.t.Name(),
	}
	if h.cfg.runGone != nil {
		// Installed only where a scenario states a probe, so that "this daemon
		// cannot ask" stays expressible — see scenarioConfig.runGone. The method,
		// not the func, because the answer moves.
		cfg.RunGone = h.Prober.probe
	}
	return cfg
}

// startWatched is start over a real config.Watch on a real file, so that a
// scenario can edit the workflow under a running daemon.
//
// Everything about the reload is genuine here — the fsnotify watch, the settling
// window, the strict re-load, the last-known-good that serves in-flight work
// while a broken file blocks new dispatch (SPEC §5.4). Only BuildRuntime is
// this package's: it hands back the same three fakes bound to the new
// definition, which is exactly what cmd/ben's builder does for adapters whose
// configuration did not move.
func startWatched(t *testing.T, cfg scenarioConfig) *scenario {
	t.Helper()
	path := writeWorkflow(t, cfg.definition().render())
	h := build(t, cfg, path)
	adapters := h.bundle(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	w, err := config.Watch(ctx, path, config.WatchOptions[*orchestrator.Bundle]{
		Logger: slog.New(h.Logs),
		// Short because the settling window's *duration* is internal/config's
		// subject and not this suite's; what matters here is that there is one
		// and that an editor's write-and-rename passes through it.
		Debounce: 10 * time.Millisecond,
		BuildRuntime: func(_ context.Context, def *config.WorkflowDefinition, _ *orchestrator.Bundle, _ config.AdapterChange) (*orchestrator.Bundle, error) {
			next := *adapters
			next.Definition = def
			return &next, nil
		},
	})
	if err != nil {
		t.Fatalf("config.Watch: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	h.watcher = w

	loop := h.baseConfig()
	loop.Runtime = w.RuntimeSource()
	loop.Revalidate = func(ctx context.Context) config.Snapshot[*orchestrator.Bundle] {
		h.revalidations.Add(1)
		return w.Revalidate(ctx)
	}
	if err := h.launch(loop); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return h
}

// build makes the fakes and loads the definition, stopping short of starting
// anything.
func build(t *testing.T, cfg scenarioConfig, path string) *scenario {
	t.Helper()

	issues := cfg.issues
	if issues == nil {
		issues = []core.Issue{fake.Issue("7", epoch)}
	}
	h := &scenario{
		t:          t,
		Tracker:    fake.NewTracker(issues...),
		Workspaces: fake.NewWorkspaces(),
		Runner:     fake.NewRunner(),
		Clock:      fake.NewClock(epoch),
		Prober:     &prober{fn: cfg.runGone},
		Logs:       newLogCapture(),
		cfg:        cfg,
		Path:       path,
	}

	// The §9.10 marker upgrade, wired where the assembly wires it: between the
	// runner that launched the run and the provider that owns its workspace
	// (cmd/ben runtime.go, core.RunnerOptions.OnRun → workspace RecordRun).
	//
	// Without it every marker a launch writes stays evidence-less, so a restart
	// reads unknown_launch and parks — and a scenario asserting that a killed
	// daemon's orphan resumes would be asserting a park instead. The `issue-`
	// prefix undoes the fake provider's own key naming, which is the same round
	// trip RecordRun does with keyForPath.
	h.Runner.SetEvidenceSink(func(spec core.RunSpec, e core.RunEvidence) {
		h.Workspaces.RecordRunEvidence(
			core.Issue{Identifier: strings.TrimPrefix(filepath.Base(spec.Workspace.Path), "issue-")}, e)
	}, nil)

	// The fresh-claim observation is local and precedes hooks and the run; the
	// publication facts describe what the scripted run leaves behind. Two
	// moments, independently scripted, as the real provider reports them.
	prepareFacts := cfg.prepareFacts
	if prepareFacts == nil {
		prepareFacts = func(ws core.Workspace) (core.LocalBranchFacts, error) {
			return core.LocalBranchFacts{Head: ws.BaseSHA, DescendsBase: true}, nil
		}
	}
	h.Workspaces.SetPrepareFacts(prepareFacts)
	facts := cfg.facts
	if facts == nil {
		facts = pushedAndDescends
	}
	h.Workspaces.SetFacts(facts)

	script := cfg.script
	if script == nil {
		script = func(core.RunSpec, int) []core.Event { return fake.Succeed("session-A") }
	}
	h.Runner.SetScript(script)

	def, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading the scenario workflow: %v", err)
	}
	h.def = def
	h.pollInterval = time.Duration(def.Config.Polling.IntervalMS) * time.Millisecond

	if cfg.before != nil {
		cfg.before(h)
	}
	return h
}

// bundle is the adapter set the loop runs against: the three fakes and either
// the real §9.7 checker or the scenario's override.
func (h *scenario) bundle(cfg scenarioConfig) *orchestrator.Bundle {
	h.t.Helper()
	verifier := cfg.verifier
	if verifier == nil {
		v, err := newVerifier(h.Workspaces, h.Tracker)
		if err != nil {
			h.t.Fatalf("assembling the publish-evidence checker: %v", err)
		}
		verifier = v
	}
	return &orchestrator.Bundle{
		Definition:     h.def,
		Tracker:        h.Tracker,
		Workspaces:     h.Workspaces,
		Runner:         h.Runner,
		Verifier:       verifier,
		ClaimPrincipal: fake.DefaultPrincipal,
	}
}

// launch starts the loop and registers the teardown that stops it.
//
// It returns Recover's error rather than failing on it. §6.4's startup failure is
// a soft one — the caller decides whether to start anyway — and restart is the
// caller that has to be able to see it.
func (h *scenario) launch(cfg orchestrator.Config) error {
	h.t.Helper()
	o, err := orchestrator.New(cfg)
	if err != nil {
		h.t.Fatalf("orchestrator.New: %v", err)
	}
	h.o = o

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)

	// §9.10 before §9.1, which is the order supervise uses: Run refuses a loop that
	// has not recovered, because the first tick dispatches and dispatch skips only
	// issues a local record already covers. Scenarios that seed a claim before this
	// point are recovering it deliberately; the rest hold nothing, and this
	// classifies nothing.
	recovered := o.Recover(ctx)
	go func() { h.done <- o.Run(ctx) }()
	h.t.Cleanup(h.stop)
	return recovered
}

// stop drops the loop and waits for it to return.
//
// Idempotent, because a scenario that restarted registered a teardown per loop
// and they all run: each stops the loop that is current when it fires, and the
// rest find nothing to do. Cleanups run last-registered-first, so that is the
// live one.
func (h *scenario) stop() {
	h.t.Helper()
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("the loop returned %v", err)
		}
	case <-time.After(barrierTimeout):
		h.t.Error("the loop did not stop after its context was cancelled")
	}
}

// restart models a kill and a fresh start on the same host.
//
// Nothing in memory survives: the records, the held claims, the armed timers and
// the run handles go with the process. Everything that was *written down* stays
// exactly as the dead daemon left it — the tracker's labels, assignees and change
// log, the workspaces and their claim-time pins, and the §9.10 run markers —
// because that is the whole of what recovery may classify from.
//
// Three details are load-bearing, and each is a way of getting this wrong:
//
//   - The clock is rebuilt at the same instant. Wall time did not reset, but every
//     timer the dead process armed is gone with it, and a clock carried over would
//     leave stale waiters that a later Advance fires into nobody.
//   - The workflow is re-read from the file, because a starting daemon reads it.
//   - The agent is **not** stopped. A kill reaches the daemon and nothing else:
//     every attempt runs in its own execution domain (SPEC §7.5), so whether the
//     previous run is still there is a question only the run prober answers. That
//     is exactly what §9.10's workspace precondition is about, and it is why a
//     scenario saying "the agent died too" says so through scenarioConfig.runGone
//     rather than by ending the run.
func (h *scenario) restart() error {
	h.t.Helper()
	h.stop()
	h.Clock = fake.NewClock(h.Clock.Now())

	def, err := config.Load(h.Path)
	if err != nil {
		h.t.Fatalf("re-loading the scenario workflow at startup: %v", err)
	}
	h.def = def
	h.pollInterval = time.Duration(def.Config.Polling.IntervalMS) * time.Millisecond
	return h.launch(h.fixedConfig())
}

// prober is §9.10's run probe in a cell a scenario can move while the loop is
// running.
//
// The cell is the point. orchestrator.Config is read by the authority goroutine,
// so assigning its RunGone field mid-run is a data race the detector rightly
// rejects — while "the domain has since become quiet" is precisely the fact a restart
// scenario has to be able to produce (orchestrator recover_test's switchableProber
// says the same about its own).
type prober struct {
	mu sync.Mutex
	fn func(core.RunEvidence) (bool, error)
}

func (p *prober) set(fn func(core.RunEvidence) (bool, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fn = fn
}

func (p *prober) probe(e core.RunEvidence) (bool, error) {
	p.mu.Lock()
	fn := p.fn
	p.mu.Unlock()
	if fn == nil {
		// Nothing scripted is not "gone". §9.10 admits one route to a free
		// workspace — positive proof of absence — and every other answer, this one
		// included, is possibly live.
		return false, nil
	}
	return fn(e)
}

// domainQuiet is the probe for a restart whose previous run really did die, which
// is the common case after a crash and the one §9.10 converges from with no
// human. domainLive is the opposite: the execution domain outlived the daemon, which
// a kill makes reachable because every attempt runs in its own group.
var (
	domainQuiet = func(core.RunEvidence) (bool, error) { return true, nil }
	domainLive  = func(core.RunEvidence) (bool, error) { return false, nil }
)

func writeWorkflow(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// rewrite replaces the workflow file the way an operator's editor does: write a
// temporary file beside it and rename over the target. §5.4's debounce exists
// for exactly this sequence, and a test that wrote in place would exercise a
// different one.
func (h *scenario) rewrite(content string) {
	h.t.Helper()
	tmp := h.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(tmp, h.Path); err != nil {
		h.t.Fatal(err)
	}
}

// ---- barriers -------------------------------------------------------------
//
// Everything below waits on a fact the fixture produces. Nothing sleeps for a
// duration and then asserts, which is the failure mode #100, #103 and #116 all
// share.

// waitFor blocks until cond holds, reporting what it was waiting for and the
// transition log rather than hanging the suite.
func (h *scenario) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(barrierTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s\nstatus: %+v", what, h.o.Status())
}

// waitState blocks until an issue reaches a state, reporting the path it took
// instead if it does not.
func (h *scenario) waitState(identifier string, want orchestrator.State) {
	h.t.Helper()
	deadline := time.Now().Add(barrierTimeout)
	var got orchestrator.State
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

// waitMilestone blocks until a milestone is the *latest* one posted for an
// issue.
//
// The latest, not merely present, and that is the whole reason this exists
// rather than a label wait. enterNeedsReview projects the label and then posts
// the comment, both on the loop's serial effects queue, so the label is visible
// first — and a test that waited on the label and then read the last comment got
// the previous milestone's empty Detail. That is #86, and it flaked in CI.
func (h *scenario) waitMilestone(identifier string, want core.Milestone) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("the %s milestone on issue %s", want, identifier), func() bool {
		posted := h.Tracker.Milestones(identifier)
		return len(posted) > 0 && posted[len(posted)-1] == want
	})
}

// waitPosted blocks until a milestone has been posted for an issue at all,
// wherever it now sits in the list.
//
// Deliberately not waitMilestone, and the difference is which question is being
// asked. That one waits for the *latest*, which is what a test about a comment's
// payload needs (#86). This one is for the milestones that are evidence a decision
// was taken and that a later one is expected to follow — a recovered `claimed`
// ahead of the `published` its resumed attempt will post — where waiting for it to
// be last would be waiting for the loop to stop working.
func (h *scenario) waitPosted(identifier string, want core.Milestone) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("the %s milestone on issue %s", want, identifier), func() bool {
		return h.posted(identifier, want)
	})
}

// posted reports whether a milestone was ever posted for an issue.
func (h *scenario) posted(identifier string, want core.Milestone) bool {
	return h.milestones(identifier, want) > 0
}

// milestones counts how many times a milestone was posted for an issue.
//
// The count, not the presence, is what says a re-issued comment stayed
// idempotent: §9.10 has every recovery verdict re-issue the comment for the state
// it lands in, and that is a repair rather than spam only because the adapter
// posts one per occurrence (SPEC §8.4).
func (h *scenario) milestones(identifier string, want core.Milestone) int {
	n := 0
	for _, m := range h.Tracker.Milestones(identifier) {
		if m == want {
			n++
		}
	}
	return n
}

// lastComment returns the most recent milestone comment posted for an issue.
// Pair it with waitMilestone, never with a label wait — see waitMilestone.
func (h *scenario) lastComment(identifier string) core.MilestoneComment {
	h.t.Helper()
	_, _, comments := h.Tracker.Snapshot()
	got := comments[identifier]
	if len(got) == 0 {
		h.t.Fatalf("no milestone comment was posted for issue %s", identifier)
	}
	return got[len(got)-1]
}

// tick advances the clock by one poll interval and blocks until the dispatch
// cycle that advance released has begun.
//
// The barrier is not the advance: Advance only delivers to the waiter, and
// everything after it is asynchronous, so a test that advanced and asserted
// would be racing the loop it just woke.
//
// It is the §9.4 step-2 revalidation and deliberately not the candidate fetch,
// which was the obvious choice and is wrong in the one scenario that most needs
// a barrier. A blocked configuration skips the fetch entirely
// (beginDispatchReads), so a fetch-counting tick hangs for its whole timeout
// exactly when a test is asserting that a blocked daemon dispatches nothing.
// Revalidation runs first and unconditionally, on every tick that is not already
// mid-cycle.
//
// What that buys is worth stating precisely, since every negative assertion in
// this package rests on it. Revalidation is reached only from onReconciled, on
// the authority goroutine, after that tick's reconcile and held-claim sweep have
// both run; and revalidation N+1 cannot begin until cycle N's result has been
// handled, because dispatchInFlight is cleared in onTickResult and gates the
// next read. So waiting for the Nth revalidation proves that cycles 1..N−1 —
// reconciliation, sweep and dispatch alike — were applied to completion, and
// only the Nth is still in the air. A scenario asserting a negative therefore
// takes one more tick than it needs facts from, which is what ticks(n) is for.
//
// It is an acknowledgement for the *tick*, and for nothing else. An effect
// ordered by some other decision — a stop's termination result, the disposal a
// `done` verdict owes — is on its own path, and a test that needs one waits for
// that fact rather than counting ticks past it. Both were got wrong here first
// (see the two barriers in invariants_test.go that say so).
//
// #106 gave the loop a stronger primitive for exactly this — Orchestrator.applied
// counts signals whose *handler has returned* — and it is unexported, so a suite
// outside the package cannot read it. This is the same argument arriving at the
// nearest thing an outsider has: a hook the loop calls itself, counted, rather
// than a fake's call counter. A fake counts a call at entry, deliberately, so
// that a test can catch a ladder standing in a gate — which makes it evidence
// that a question was asked and no evidence at all that its answer has landed.
// A tick that arrives while the previous cycle's reads are still out is
// deliberately a no-op — beginDispatchReads gates on dispatchInFlight so a slow
// tracker is not piled on — and on a manual clock nothing else will produce
// another. So the advance is repeated until a cycle actually begins, which
// costs at worst an idle tick and never a missed one. The window below is how
// long to give a released tick before supplying a second; it bounds nothing
// that is being asserted.
const tickSettle = 100 * time.Millisecond

func (h *scenario) tick() {
	h.t.Helper()
	before := h.revalidations.Load()
	deadline := time.Now().Add(barrierTimeout)
	for time.Now().Before(deadline) {
		if !h.Clock.BlockUntilWaiters(1) {
			h.t.Fatal("the poll ticker never armed")
		}
		h.Clock.Advance(h.pollInterval)

		settle := time.Now().Add(tickSettle)
		for time.Now().Before(settle) {
			if h.revalidations.Load() > before {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	h.t.Fatalf("timed out waiting for the next dispatch cycle to begin\nstatus: %+v", h.o.Status())
}

// ticks takes n poll intervals, which is how a scenario gives the loop every
// opportunity to do the thing it must not do. Two is the minimum that proves
// one whole cycle ran; three is what the negative assertions here use.
func (h *scenario) ticks(n int) {
	h.t.Helper()
	for range n {
		h.tick()
	}
}

// settle drives the loop forward one poll interval at a time until an issue
// reaches a state, and fails as a state assertion if it never does.
//
// It is for the routes that take several timer fires to complete — a
// continuation track walking to max_turns, a backoff that has to re-dispatch —
// where the number of fires is an implementation detail of the delay schedule
// rather than something the invariant is about. Advancing a *manual* clock
// cannot outrun the loop: each step's own barrier is a queue read that has
// already happened, so an extra advance costs an idle tick and never a missed
// state. Where the count is the point, a scenario fires the timers itself.
func (h *scenario) settle(identifier string, want orchestrator.State) {
	h.t.Helper()
	// Comfortably more fires than any limit this suite configures needs, and
	// bounded so a route that never settles reports the state it is stuck in.
	const maxAdvances = 24
	for range maxAdvances {
		for _, s := range h.o.Status() {
			if s.Identifier == identifier && s.State == want {
				return
			}
		}
		h.tick()
	}
	h.waitState(identifier, want)
}

// settleThrough drives the loop forward until an issue has *passed through* a
// state, rather than until it is sitting in one.
//
// It is the barrier for the exits. A record that leaves the machine is
// forgotten once the writes it owed have landed — `failed` releases the claim
// and then drops the record — so Status goes quiet and a wait on the current
// state would time out on a run that did exactly what it was supposed to. The
// append-only transition log is what still says it happened (SPEC §9.11).
func (h *scenario) settleThrough(identifier string, want orchestrator.State) {
	h.t.Helper()
	const maxAdvances = 24
	for range maxAdvances {
		if h.reached(identifier, want) {
			return
		}
		h.tick()
	}
	if !h.reached(identifier, want) {
		h.t.Fatalf("issue %s never reached %q (path: %v)", identifier, want, h.path(identifier))
	}
}

// waitRunning blocks until an issue's agent process is live, and returns its
// handle. It is the barrier for "there is a run to interrupt".
func (h *scenario) waitRunning(identifier string) *fake.Handle {
	h.t.Helper()
	h.waitState(identifier, orchestrator.StateRunning)
	h.waitFor("a handle for the run on issue "+identifier, func() bool {
		return h.Runner.LastHandle() != nil
	})
	return h.Runner.LastHandle()
}

// path is the states an issue passed through.
func (h *scenario) path(identifier string) []orchestrator.State {
	return h.o.Transitions.Path(identifier)
}

// reached reports whether an issue was ever in a state.
func (h *scenario) reached(identifier string, want orchestrator.State) bool {
	for _, s := range h.path(identifier) {
		if s == want {
			return true
		}
	}
	return false
}

// hasRecord reports whether the loop currently owns a record for an issue,
// which is what "was it dispatched?" means from outside.
func (h *scenario) hasRecord(identifier string) bool {
	for _, s := range h.o.Status() {
		if s.Identifier == identifier {
			return true
		}
	}
	return false
}

// dispatches counts the attempts prepared for an issue, which is what "was it
// dispatched again?" means per issue.
//
// Per issue, and read from the workspace provider rather than the runner,
// because the runner's recorded specs are a bare exported slice appended to
// from a worker goroutine — reading it would race the code under test, and the
// runner's locked accessors are all whole-fixture rather than per-issue.
// Preparing is also the earlier of the two events: a dispatch that prepared and
// then failed to launch is still a dispatch, and it is the one a "no second
// agent" assertion most needs to see.
func (h *scenario) dispatches(identifier string) int {
	return h.Workspaces.PrepareCount(identifier)
}

// ---- fixture verbs ---------------------------------------------------------

// assignees is who the tracker says holds an issue. It is a fixture check — "the
// assignment landed, and nothing after it did" — never a substitute for the
// adapter's own guarantee about what a release removes.
func (h *scenario) assignees(identifier string) []string {
	h.t.Helper()
	iss, err := h.Tracker.Get(context.Background(), identifier)
	if err != nil {
		h.t.Fatalf("reading issue %s: %v", identifier, err)
	}
	return iss.Assignees
}

// labels is the label set the tracker reports for an issue.
//
// The issue's own labels, deliberately, and not Tracker.Label — that one reports
// the last value *projected*, which is the fake's bookkeeping and cannot say
// whether an earlier label is still standing beside it. Add-before-remove is
// exactly the case where those two answers differ (SPEC §9.3, §9.10 step 3).
func (h *scenario) labels(identifier string) []string {
	h.t.Helper()
	iss, err := h.Tracker.Get(context.Background(), identifier)
	if err != nil {
		h.t.Fatalf("reading issue %s: %v", identifier, err)
	}
	return iss.Labels
}

// stateLabels is the `ben:*` half of a label set, which is the half §9.3 owns.
func stateLabels(labels []string) []string {
	var out []string
	for _, l := range labels {
		if strings.HasPrefix(strings.ToLower(l), "ben:") {
			out = append(out, strings.ToLower(l))
		}
	}
	return out
}

// claim writes an assignment the way §8.4's claim does, so that a scenario
// describing what a dead daemon left behind leaves the change log the adapter's
// own writes produce rather than a hand-authored one.
func (h *scenario) claim(issue core.Issue) {
	h.t.Helper()
	ok, err := h.Tracker.Claim(context.Background(), issue)
	if err != nil || !ok {
		h.t.Fatalf("claiming issue %s for the fixture: verified=%v err=%v", issue.Identifier, ok, err)
	}
}

// beginCurrentClaimBase supplies provider state for a recovery scenario whose
// subject is downstream of the epoch gate. It derives the epoch from the same
// ordered tracker history the loop uses; no scenario-local counter can stand in
// for the assignment event ID.
func (h *scenario) beginCurrentClaimBase(identifier string) {
	h.t.Helper()
	events, err := h.Tracker.ClaimHistory(context.Background(), core.Issue{Identifier: identifier})
	if err != nil {
		h.t.Fatalf("reading claim history for %s: %v", identifier, err)
	}
	var claimEpoch int64
	for _, event := range events {
		if !strings.EqualFold(event.Subject, fake.DefaultPrincipal) {
			continue
		}
		switch event.Kind {
		case core.ClaimEventAssigned:
			claimEpoch = event.ID
		case core.ClaimEventUnassigned:
			claimEpoch = 0
		}
	}
	if claimEpoch <= 0 {
		h.t.Fatalf("claim history for %s has no positive current epoch: %+v", identifier, events)
	}
	if err := h.Workspaces.BeginClaimBase(context.Background(), core.Issue{Identifier: identifier}, claimEpoch); err != nil {
		h.t.Fatalf("beginning claim epoch %d for %s: %v", claimEpoch, identifier, err)
	}
}

// ---- logs ------------------------------------------------------------------

// logCapture records what the loop logged.
//
// It exists for the one class of guarantee in this package whose whole observable
// effect *is* a log line. §9.10 gate 2's unorderable fallback parks the issue **and**
// raises an operator error, and the second half is not decoration: a park nobody is
// told about is indistinguishable from a daemon that quietly did nothing, and this
// verdict is reached precisely when BEN refuses to guess. Asserting on the level and
// the issue attribute is therefore not testing the wording.
//
// Shared store, per-handler attributes, both for the reasons the orchestrator's own
// capture gives: slog derives a new handler per Logger.With, so every one of them
// must append to the same place under the same lock, and a handler that dropped the
// bound attributes would report every line as attribute-less — which would let an
// assertion on `issue` pass against code that never set it.
type logStore struct {
	mu      sync.Mutex
	records []logRecord
}

type logCapture struct {
	store *logStore
	with  []slog.Attr
}

type logRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

func newLogCapture() *logCapture { return &logCapture{store: &logStore{}} }

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
