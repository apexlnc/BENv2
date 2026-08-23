package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

// The attempt-outcome log as a *live daemon* writes it, and as `ben status`
// renders it (#60). Everything here drives `ben run` and reads back through the
// reader the command uses, for statedir_test.go's reason: a hand-authored
// fixture asserts what this test believes the daemon writes.

// publishes is a verifier that reports the one verdict a finished ticket has.
type publishes struct{}

func (publishes) Verify(context.Context, core.Issue, core.Workspace) (orchestrator.VerifyResult, error) {
	return orchestrator.VerifyResult{Verdict: orchestrator.VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
}

// The end-to-end pin: an attempt a real daemon ran, read back by the reader
// `ben status` uses, carrying the two things #62 needs to group by.
func TestAnAttemptOutcomeSurvivesTheDaemonThatRecordedIt(t *testing.T) {
	files := testStateFiles(t)
	d := startDaemonWith(t, files, func(d *daemonRun) {
		d.Verifier = publishes{}
		d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "sess-1", Continuation: "sess-1"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 4321, OutputTokens: 210, CostUSD: 1.5}},
				{Type: core.EventSucceeded},
			}
		})
		d.Runner.SetHangAfterScript(false)
	})
	d.waitTransition("7", "done")
	d.stopAndWait()

	got, total, err := files.dir.ReadAttempts().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if total != 1 {
		t.Fatalf("the log holds %d records, want 1: %+v", total, got)
	}
	a := got[0]
	if a.Issue != "7" || a.Attempt != 1 || a.RunID == "" {
		t.Errorf("record = %+v, want it to identify the attempt", a)
	}
	// Resolved through the real registry from the real WORKFLOW.md the fixture
	// wrote (config.AgentDescriptor), not stated by the test.
	if a.Agent != "claude-code" || a.Model != "opus" {
		t.Errorf("agent/model = %q/%q, want the workflow's own", a.Agent, a.Model)
	}
	if !a.Published() {
		t.Errorf("verdict = %q, want %q", a.Verdict, state.VerdictPublished)
	}
	if a.FailureReason != "" {
		t.Errorf("failure_reason = %q on a published attempt", a.FailureReason)
	}
	if a.InputTokens != 4321 || a.OutputTokens != 210 || a.CostUSD != 1.5 {
		t.Errorf("usage = %d/%d/$%v, want what the run reported", a.InputTokens, a.OutputTokens, a.CostUSD)
	}
	if !a.Ran || a.Duration() < 0 {
		t.Errorf("ran = %v, duration = %s", a.Ran, a.Duration())
	}
}

// A workflow that names no model is the ordinary one — the harness picks its own
// default — and its runs have to stay in the comparison rather than falling out
// of it as a missing field. BEN cannot name that model (core.AgentDescriptor),
// so it records the absence and says what it means.
func TestADefaultModelRunIsStillRecordedAndNamed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := startDaemonFor(t, nil, []func(*workflowSpec){withModel("")}, func(d *daemonRun) {
		d.Verifier = publishes{}
		d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "sess-1", Continuation: "sess-1"},
				{Type: core.EventSucceeded},
			}
		})
		d.Runner.SetHangAfterScript(false)
	})
	d.waitTransition("7", "done")
	d.stopAndWait()

	got, _, err := d.files.dir.ReadAttempts().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("records = %+v, want 1", got)
	}
	if got[0].Agent != "claude-code" {
		t.Errorf("agent = %q, want the workflow's adapter", got[0].Agent)
	}
	if got[0].Model != "" {
		t.Errorf("model = %q; the block names none, so BEN must not invent one", got[0].Model)
	}

	// It is a row in the aggregate, named rather than dashed out.
	code, out, errs := statusOf(t, d.path)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errs)
	}
	if !strings.Contains(out, "(adapter default)") {
		t.Errorf("the aggregate does not name the default-model cohort:\n%s", out)
	}
	if !strings.Contains(out, "BY AGENT") || !strings.Contains(out, "claude-code") {
		t.Errorf("the default-model run fell out of the breakdown:\n%s", out)
	}
}

// The §7.3 taxonomy reaches the file as itself, which is what makes "which
// failure reason dominates" a query rather than prose-matching.
func TestAFailedAttemptRecordsItsTaxonomyReason(t *testing.T) {
	files := testStateFiles(t)
	d := startDaemonWith(t, files, func(d *daemonRun) {
		// auth is non-retryable (§7.3), so one attempt settles it.
		d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "sess-1", Continuation: "sess-1"},
				{Type: core.EventFailed, Reason: core.FailureAuth},
			}
		})
		d.Runner.SetHangAfterScript(false)
	})
	d.waitLabel("7", core.StateLabelFailed)
	d.stopAndWait()

	got, _, err := files.dir.ReadAttempts().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 1 || got[0].FailureReason != core.FailureAuth {
		t.Fatalf("records = %+v, want one carrying auth", got)
	}
	if got[0].Verdict != "" {
		t.Errorf("verdict = %q; verification never ran on a failed attempt", got[0].Verdict)
	}
}

// The verdict spelling is the *file format*, so the constant the reader compares
// against and the enum the writer renders have to be the same string. A numeric
// cast or a renamed constant would leave both sides compiling and every later
// aggregate reading zero published attempts.
func TestTheVerdictSpellingIsPinnedAcrossThePackages(t *testing.T) {
	if got := verdictName(orchestrator.VerdictPublished); got != state.VerdictPublished {
		t.Errorf("orchestrator.VerdictPublished renders as %q, the state dir reads %q", got, state.VerdictPublished)
	}
	// Unknown is deliberately not a spelling: an attempt that never reached
	// verification has no answer to record, and "unknown" would make it
	// indistinguishable from a check that ran and could not decide.
	if got := verdictName(orchestrator.VerdictUnknown); got != "" {
		t.Errorf("VerdictUnknown renders as %q, want nothing at all", got)
	}
	for _, v := range []orchestrator.Verdict{
		orchestrator.VerdictPublished, orchestrator.VerdictIncomplete, orchestrator.VerdictContradicted,
	} {
		if verdictName(v) == "" {
			t.Errorf("%s renders as nothing, which the reader takes as 'verification never ran'", v)
		}
	}
}

// `ben status` answers the ticket's four questions from the whole log, and says
// what each number is a fraction of.
func TestStatusRendersTheAttemptAggregate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := startDaemonWith(t, nil, func(d *daemonRun) {
		d.Verifier = publishes{}
		d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "sess-1", Continuation: "sess-1"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 2_500_000, OutputTokens: 1500, CostUSD: 2}},
				{Type: core.EventSucceeded},
			}
		})
		d.Runner.SetHangAfterScript(false)
	})
	d.waitTransition("7", "done")
	d.stopAndWait()

	code, out, errs := statusOf(t, d.path)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errs)
	}
	for _, want := range []string{
		"ATTEMPTS (all 1",
		"1 of 1 published issues landed on attempt 1",
		"$2.00 per published issue",
		"p50",
		"2.5M in",
		"BY AGENT",
		"claude-code",
		"opus",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output does not mention %q:\n%s", want, out)
		}
	}
}

// The same numbers on the JSON contract, which is what a script reads.
func TestStatusJSONCarriesTheAttemptAggregate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := startDaemonWith(t, nil, func(d *daemonRun) {
		d.Verifier = publishes{}
		d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "sess-1", Continuation: "sess-1"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.5}},
				{Type: core.EventSucceeded},
			}
		})
		d.Runner.SetHangAfterScript(false)
	})
	d.waitTransition("7", "done")
	d.stopAndWait()

	code, out, errs := statusOf(t, "--json", d.path)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errs)
	}
	var report struct {
		Attempts *state.Summary `json:"attempts"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("status --json does not parse: %v\n%s", err, out)
	}
	if report.Attempts == nil {
		t.Fatalf("the contract carries no attempt aggregate:\n%s", out)
	}
	s := report.Attempts
	if s.Attempts != 1 || s.PublishedIssues != 1 || s.FirstAttemptPublished != 1 {
		t.Errorf("summary = %+v, want one attempt that published first time", s)
	}
	if s.CostUSD != 0.5 {
		t.Errorf("cost = %v, want the run's own", s.CostUSD)
	}
	if len(s.Agents) != 1 || s.Agents[0].Agent != "claude-code" || s.Agents[0].Model != "opus" {
		t.Errorf("agents = %+v, want the workflow's adapter and model", s.Agents)
	}
}

// A state dir with no attempt log is an answer rather than a refusal — unlike a
// missing transition log, which is evidence something removed it. The
// asymmetry is deliberate: this command also reads dirs written by a daemon
// that predates the file, and refusing to render a status over a rolling
// upgrade is the wrong trade for telemetry.
func TestStatusReportsAnAbsentAttemptLogRatherThanRefusing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := startDaemonWith(t, nil)
	d.waitLabel("7", core.StateLabelRunning)
	d.stopAndWait()

	if err := os.Remove(d.files.dir.AttemptsPath()); err != nil {
		t.Fatalf("removing the log: %v", err)
	}
	code, out, errs := statusOf(t, d.path)
	if code != 0 {
		t.Fatalf("exit %d over an absent attempt log, stderr: %s", code, errs)
	}
	if !strings.Contains(out, "ATTEMPTS (none)") {
		t.Errorf("status does not say the aggregate is empty:\n%s", out)
	}

	code, out, errs = statusOf(t, "--json", d.path)
	if code != 0 {
		t.Fatalf("exit %d over an absent attempt log, stderr: %s", code, errs)
	}
	if strings.Contains(out, `"attempts"`) {
		t.Errorf("the contract carries an aggregate over a log that is not there:\n%s", out)
	}
}

// A daemon whose attempts have not finished has an empty log, and that must not
// read as an error either — it is the ordinary state of the first minute.
func TestStatusOverALogWithNoFinishedAttempts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := startDaemonWith(t, nil)
	d.waitLabel("7", core.StateLabelRunning)

	code, out, errs := statusOf(t, d.path)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errs)
	}
	if !strings.Contains(out, "ATTEMPTS (all 0") {
		t.Errorf("status does not render an empty aggregate:\n%s", out)
	}
	d.stopAndWait()
}
