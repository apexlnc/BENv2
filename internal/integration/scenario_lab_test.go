package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	scenariolab "github.com/srhg-ai-7cef3f93/ben/internal/scenario"
)

// corpus is the independent boundary that makes omission or semantic drift in
// testdata visible. A test driven only by the documents it reads could lose a
// fixture, or turn "rewritten never publishes" into "complete publishes", and
// still prove every remaining declaration agrees with itself (AGENTS.md).
var corpus = map[string]corpusExpectation{
	"ordinary-success.json": {
		name: "ordinary-success", final: orchestrator.StateDone,
		starts: 1, published: true,
	},
	"retry-to-success.json": {
		name: "retry-to-success", final: orchestrator.StateDone,
		starts: 2, published: true, retry: true,
	},
	"restart-converges.json": {
		name: "restart-converges", final: orchestrator.StateDone,
		starts: 2, published: true, restart: true,
	},
	"rewritten-evidence.json": {
		name: "rewritten-evidence", final: orchestrator.StateNeedsReview,
		starts: 1, contradicted: true,
	},
}

type corpusExpectation struct {
	name         string
	final        orchestrator.State
	starts       int
	published    bool
	retry        bool
	restart      bool
	contradicted bool
}

func TestScenarioFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "scenarios", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	gotFiles := make([]string, 0, len(paths))
	for _, path := range paths {
		gotFiles = append(gotFiles, filepath.Base(path))
	}
	wantFiles := make([]string, 0, len(corpus))
	for name := range corpus {
		wantFiles = append(wantFiles, name)
	}
	sort.Strings(wantFiles)
	if !slices.Equal(gotFiles, wantFiles) {
		t.Fatalf("scenario corpus = %v, want exactly %v", gotFiles, wantFiles)
	}

	for _, path := range paths {
		filename := filepath.Base(path)
		expect := corpus[filename]
		doc := loadScenarioDocument(t, path)
		if doc.Name != expect.name {
			t.Fatalf("%s name = %q, want independently anchored %q", filename, doc.Name, expect.name)
		}

		t.Run(doc.Name, func(t *testing.T) {
			var first, repeated labResult
			t.Run("first", func(t *testing.T) { first = runScenarioDocument(t, doc) })
			t.Run("repeat", func(t *testing.T) { repeated = runScenarioDocument(t, doc) })

			if first.trace != repeated.trace {
				t.Fatalf("two replays produced different bytes\nfirst:\n%s\nrepeat:\n%s", first.trace, repeated.trace)
			}
			assertScenarioResult(t, expect, first)

			goldenPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".trace"
			golden, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("reading golden trace: %v\ncandidate:\n%s", err, first.trace)
			}
			if first.trace != string(golden) {
				t.Fatalf("trace differs from %s\ngot:\n%s\nwant:\n%s", goldenPath, first.trace, golden)
			}
			t.Logf("\n%s", first.trace)
		})
	}
}

func loadScenarioDocument(t *testing.T, path string) scenariolab.Document {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("closing %s: %v", path, err)
		}
	}()
	doc, err := scenariolab.Decode(f)
	if err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return doc
}

type labResult struct {
	trace                        string
	final                        orchestrator.State
	starts                       int
	milestones                   []core.Milestone
	prepares                     []fake.PrepareCall
	transitions                  []orchestrator.TransitionEntry
	restartSafe                  bool
	retrySecondStartSeen         bool
	retryUndisposedAtSecondStart bool
}

type labExecution struct {
	t   *testing.T
	doc scenariolab.Document
	h   *scenario

	trace scenariolab.Trace

	transitionCursor  int
	trackerCursor     int
	prepareCursor     int
	disposeCursor     int
	startCursor       int
	markerWriteCursor int
	markerClearCursor int

	transitions                  []orchestrator.TransitionEntry
	restartSafe                  bool
	retrySecondStartSeen         atomic.Bool
	retryUndisposedAtSecondStart atomic.Bool
}

func runScenarioDocument(t *testing.T, doc scenariolab.Document) labResult {
	t.Helper()
	exec := &labExecution{
		t: t, doc: doc,
		trace: scenariolab.Trace{Scenario: doc.Name, SchemaVersion: doc.SchemaVersion},
	}

	wf := defaultWorkflow()
	cfg := scenarioConfig{
		workflow: &wf,
		issues:   []core.Issue{fake.Issue(doc.Issue.Identifier, epoch)},
		script:   exec.script,
	}
	switch doc.Publication {
	case scenariolab.PublicationComplete:
		cfg.facts = pushedAndDescends
	case scenariolab.PublicationRewritten:
		cfg.facts = func(core.Workspace) (core.PublishFacts, error) {
			return core.PublishFacts{Head: rewrittenSHA}, nil
		}
	}
	if hasOutcome(doc, scenariolab.OutcomeRunning) {
		cfg.runGone = domainQuiet
	}

	exec.h = build(t, cfg, writeWorkflow(t, cfg.definition().render()))
	if hasOutcome(doc, scenariolab.OutcomeRunning) {
		exec.h.Runner.SetHangAfterScript(true)
	}
	for i, step := range doc.Steps {
		exec.apply(i+1, step)
	}
	exec.h.stop()

	status, ok := exec.h.o.StatusFor(doc.Issue.Identifier)
	if !ok {
		t.Fatalf("scenario ended without a record for issue %s", doc.Issue.Identifier)
	}
	return labResult{
		trace:                        exec.trace.Text(),
		final:                        status.State,
		starts:                       exec.h.Runner.StartCount(),
		milestones:                   exec.h.Tracker.Milestones(doc.Issue.Identifier),
		prepares:                     exec.h.Workspaces.Prepares(doc.Issue.Identifier),
		transitions:                  append([]orchestrator.TransitionEntry(nil), exec.transitions...),
		restartSafe:                  exec.restartSafe,
		retrySecondStartSeen:         exec.retrySecondStartSeen.Load(),
		retryUndisposedAtSecondStart: exec.retryUndisposedAtSecondStart.Load(),
	}
}

func (e *labExecution) script(_ core.RunSpec, attempt int) []core.Event {
	if attempt < 1 || attempt > len(e.doc.Attempts) {
		e.t.Errorf("scenario started undeclared attempt %d", attempt)
		return fake.Fail(core.FailureLaunchError)
	}
	if attempt == 2 && e.doc.Attempts[0].Outcome == scenariolab.OutcomeCrashed {
		// Observe retention when the retry has started but cannot yet have
		// completed. Prepare records alone cannot distinguish reuse from a
		// dispose-and-recreate cycle with the same identifier and base.
		e.retrySecondStartSeen.Store(true)
		e.retryUndisposedAtSecondStart.Store(len(e.h.Workspaces.Disposals(e.doc.Issue.Identifier)) == 0)
	}
	declared := e.doc.Attempts[attempt-1]
	switch declared.Outcome {
	case scenariolab.OutcomeSucceeded:
		e.h.Tracker.SetPR("ben/issue-"+e.doc.Issue.Identifier, core.PR{
			Number: 220, URL: "https://example.test/pull/220", State: "open",
			Branch: "ben/issue-" + e.doc.Issue.Identifier, BaseBranch: "main",
		})
		return []core.Event{
			{Type: core.EventStarted, SessionID: declared.Session, Continuation: declared.Session},
			{Type: core.EventSucceeded},
		}
	case scenariolab.OutcomeCrashed:
		return []core.Event{
			{Type: core.EventStarted, SessionID: declared.Session, Continuation: declared.Session},
			{Type: core.EventFailed, Reason: core.FailureCrashed},
		}
	case scenariolab.OutcomeRunning:
		return []core.Event{{Type: core.EventStarted, SessionID: declared.Session, Continuation: declared.Session}}
	default:
		e.t.Errorf("validated scenario carried unsupported outcome %q", declared.Outcome)
		return nil
	}
}

func (e *labExecution) apply(number int, step scenariolab.Step) {
	e.t.Helper()
	switch step.Action {
	case scenariolab.ActionStart:
		if err := e.h.launch(e.h.fixedConfig()); err != nil {
			e.t.Fatalf("Recover: %v", err)
		}
		e.reach(step.Until, false)
	case scenariolab.ActionAdvance:
		e.reach(step.Until, true)
	case scenariolab.ActionRestart:
		e.restart(step)
	default:
		e.t.Fatalf("validated scenario carried unsupported action %q", step.Action)
	}
	e.capture(number, step)
}

func (e *labExecution) restart(step scenariolab.Step) {
	e.t.Helper()
	prior := e.h.Runner.LastHandle()
	if prior == nil {
		e.t.Fatal("restart has no prior run to classify")
	}
	claimedCall := fmt.Sprintf("comment %s=%s", e.doc.Issue.Identifier, core.MilestoneClaimed)
	claimedBefore := trackerCallCount(e.h.Tracker, claimedCall)
	starts := e.h.Runner.StartCount()
	e.h.stop()
	endAgent(e.t, prior)
	confirmed := prior.Probe(context.Background()) == core.TerminationConfirmed
	if err := e.h.restart(); err != nil {
		e.t.Fatalf("Recover: %v", err)
	}
	// The new orchestrator owns a new in-memory transition log. Tracker,
	// workspace, and runner cursors remain monotonic across the restart.
	e.transitionCursor = 0
	e.h.Runner.SetHangAfterScript(false)
	e.reach(step.Until, false)
	e.h.waitFor("the recovered claimed comment effect", func() bool {
		return trackerCallCount(e.h.Tracker, claimedCall) > claimedBefore
	})
	e.restartSafe = confirmed && e.h.Runner.StartCount() == starts
}

func trackerCallCount(tracker *fake.Tracker, want string) int {
	calls, _, _ := tracker.Snapshot()
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}

func (e *labExecution) reach(state scenariolab.State, drive bool) {
	e.t.Helper()
	want := orchestrator.State(state)
	if drive {
		e.h.settle(e.doc.Issue.Identifier, want)
	} else {
		e.h.waitState(e.doc.Issue.Identifier, want)
	}

	id := e.doc.Issue.Identifier
	switch state {
	case scenariolab.StateRunning:
		e.h.waitFor("the running label and identified run marker", func() bool {
			marker, ok := e.h.Workspaces.RunMarkerFor(id)
			return e.h.Tracker.Label(id) == core.StateLabelRunning && ok && marker.State == core.RunMarkerIdentified
		})
	case scenariolab.StateBackoff:
		e.h.waitFor("the claimed projection after backoff", func() bool {
			return e.h.Tracker.Label(id) == core.StateLabelClaimed
		})
	case scenariolab.StateDone:
		e.h.waitMilestone(id, core.MilestonePublished)
		e.h.waitFor("the final workspace disposal", func() bool {
			return len(e.h.Workspaces.Disposals(id)) > 0
		})
		e.h.waitFor("the final run marker clear", func() bool {
			_, ok := e.h.Workspaces.RunMarkerFor(id)
			return !ok
		})
	case scenariolab.StateNeedsReview:
		e.h.waitMilestone(id, core.MilestoneNeedsReview)
	}
}

func (e *labExecution) capture(number int, step scenariolab.Step) {
	e.t.Helper()
	id := e.doc.Issue.Identifier
	entry := scenariolab.StepTrace{
		Number: number,
		Action: fmt.Sprintf("%s until=%s", step.Action, step.Until),
		Next:   nextFact(step.Until),
	}
	if step.PriorRun != "" {
		entry.Action += " prior_run=" + string(step.PriorRun)
	}

	status, ok := e.h.o.StatusFor(id)
	if ok {
		entry.Observed = append(entry.Observed,
			fmt.Sprintf("issue=%s state=%s attempt=%d turns=%d failure=%s", id, status.State, status.Attempt, status.Turns, valueOrNone(string(status.FailureReason))))
	} else {
		entry.Observed = append(entry.Observed, fmt.Sprintf("issue=%s state=absent", id))
	}
	labels := append([]string(nil), e.h.labels(id)...)
	assignees := append([]string(nil), e.h.assignees(id)...)
	sort.Strings(labels)
	sort.Strings(assignees)
	entry.Observed = append(entry.Observed,
		fmt.Sprintf("labels=%v", labels),
		fmt.Sprintf("assignees=%v", assignees),
		fmt.Sprintf("milestones=%v", e.h.Tracker.Milestones(id)),
		fmt.Sprintf("prepares=%d runs=%d releases=%d disposals=%d",
			e.h.Workspaces.PrepareCount(id), e.h.Runner.StartCount(), e.h.Tracker.ReleaseCount(id), len(e.h.Workspaces.Disposals(id))))
	comments := e.h.Tracker.CommentsFor(id)
	if len(comments) > 0 {
		last := comments[len(comments)-1]
		if last.Detail != "" {
			entry.Observed = append(entry.Observed, fmt.Sprintf("detail=%q", last.Detail))
		}
		if last.PRURL != "" {
			entry.Observed = append(entry.Observed, fmt.Sprintf("pull_request=%s", last.PRURL))
		}
	}

	transitions := e.h.o.Transitions.Entries()
	if e.transitionCursor > len(transitions) {
		e.t.Fatalf("transition log shrank from %d to %d without a restart", e.transitionCursor, len(transitions))
	}
	for _, transition := range transitions[e.transitionCursor:] {
		entry.Decisions = append(entry.Decisions,
			fmt.Sprintf("%s -> %s: %s", transition.From, transition.To, transition.Reason))
		e.transitions = append(e.transitions, transition)
	}
	e.transitionCursor = len(transitions)

	calls, _, _ := e.h.Tracker.Snapshot()
	if e.trackerCursor > len(calls) {
		e.t.Fatalf("tracker call log shrank from %d to %d", e.trackerCursor, len(calls))
	}
	for _, call := range calls[e.trackerCursor:] {
		entry.Effects = append(entry.Effects, "tracker: "+call)
	}
	e.trackerCursor = len(calls)

	prepares := e.h.Workspaces.Prepares(id)
	for _, prepare := range prepares[e.prepareCursor:] {
		entry.Effects = append(entry.Effects, fmt.Sprintf(
			"workspace: prepare issue=%s attempt=%d epoch=%d base=%s",
			prepare.Identifier, prepare.Attempt, prepare.Epoch, prepare.BaseSHA))
	}
	e.prepareCursor = len(prepares)

	starts := e.h.Runner.StartCount()
	for n := e.startCursor + 1; n <= starts; n++ {
		entry.Effects = append(entry.Effects, fmt.Sprintf("runner: start #%d", n))
	}
	e.startCursor = starts

	markerWrites := e.h.Workspaces.MarkerWriteCount()
	for n := e.markerWriteCursor + 1; n <= markerWrites; n++ {
		entry.Effects = append(entry.Effects, fmt.Sprintf("workspace: run marker write #%d", n))
	}
	e.markerWriteCursor = markerWrites
	markerClears := e.h.Workspaces.MarkerClearCount()
	for n := e.markerClearCursor + 1; n <= markerClears; n++ {
		entry.Effects = append(entry.Effects, fmt.Sprintf("workspace: run marker clear #%d", n))
	}
	e.markerClearCursor = markerClears

	disposals := e.h.Workspaces.Disposals(id)
	for _, disposal := range disposals[e.disposeCursor:] {
		entry.Effects = append(entry.Effects,
			fmt.Sprintf("workspace: dispose key=%s keep=%t", disposal.Key, disposal.Keep))
	}
	e.disposeCursor = len(disposals)
	e.trace.Steps = append(e.trace.Steps, entry)
}

func nextFact(state scenariolab.State) string {
	switch state {
	case scenariolab.StateRunning:
		return "positive run absence before restart or workspace reuse"
	case scenariolab.StateBackoff:
		return "advance the manual clock after the retry delay"
	case scenariolab.StateNeedsReview:
		return "human review or a new approval fact"
	case scenariolab.StateDone:
		return "issue merge or close"
	default:
		return ""
	}
}

func valueOrNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func hasOutcome(doc scenariolab.Document, want scenariolab.Outcome) bool {
	for _, attempt := range doc.Attempts {
		if attempt.Outcome == want {
			return true
		}
	}
	return false
}

func assertScenarioResult(t *testing.T, want corpusExpectation, got labResult) {
	t.Helper()
	if got.final != want.final {
		t.Errorf("final state = %s, want %s", got.final, want.final)
	}
	if got.starts != want.starts {
		t.Errorf("runner starts = %d, want %d", got.starts, want.starts)
	}
	published := 0
	for _, milestone := range got.milestones {
		if milestone == core.MilestonePublished {
			published++
		}
	}
	if want.published {
		if published != 1 {
			t.Errorf("published milestones = %d, want exactly 1", published)
		}
	} else if published != 0 {
		t.Errorf("published milestones = %d, want none", published)
	}

	if want.retry {
		if !got.retrySecondStartSeen {
			t.Error("retry never reached the independent second-start observation")
		} else if !got.retryUndisposedAtSecondStart {
			t.Error("workspace was disposed before the retry runner started")
		}
		if len(got.prepares) != 2 {
			t.Fatalf("retry prepares = %+v, want two", got.prepares)
		}
		if got.prepares[0].BaseSHA == "" || got.prepares[0].BaseSHA != got.prepares[1].BaseSHA {
			t.Errorf("retry moved claim-time base from %q to %q", got.prepares[0].BaseSHA, got.prepares[1].BaseSHA)
		}
		if got.prepares[0].Identifier != got.prepares[1].Identifier {
			t.Errorf("retry changed workspace owner from %q to %q", got.prepares[0].Identifier, got.prepares[1].Identifier)
		}
	}
	if want.restart && !got.restartSafe {
		t.Error("restart launched another run before the prior run was positively confirmed gone")
	}
	if want.contradicted {
		for _, transition := range got.transitions {
			if transition.To == orchestrator.StateDone {
				t.Errorf("contradicted evidence reached done through %+v", transition)
			}
		}
	}
}
