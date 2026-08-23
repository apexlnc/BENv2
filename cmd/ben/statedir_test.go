package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

// The §10.3 state dir as a *live daemon* writes it. Everything here drives
// `ben run` and reads the files back through the same reader `ben status` and
// §9.10 step 6 use — never a hand-authored fixture, because a fixture asserts
// what this test believes the daemon writes rather than what it writes.

func testStateFiles(t *testing.T) *stateFiles {
	t.Helper()
	return openTestState(t, state.At(t.TempDir()), "wf")
}

func openTestState(t *testing.T, dir state.Dir, key string) *stateFiles {
	t.Helper()
	files, err := openStateFiles(dir, key, &runLookup{}, discardLog())
	if err != nil {
		t.Fatalf("openStateFiles: %v", err)
	}
	t.Cleanup(func() { files.close() }) //nolint:errcheck // the daemon closes it first
	return files
}

// The end-to-end pin behind SPEC §9.10 step 6: a §7.3 reason that a *later*
// process can read. It is deliberately not an assertion on the conversion in
// transitionSink — the file format is the §9.2 state *names*, so a rename of
// orchestrator.StateFailed would leave that conversion compiling and correct
// while breaking every reader. Only driving a real failure through a real
// daemon and asking the real reader for it catches that.
func TestAFailureReasonSurvivesTheDaemonThatRecordedIt(t *testing.T) {
	files := testStateFiles(t)
	d := startDaemonWith(t, files, func(d *daemonRun) {
		// auth is non-retryable (§7.3), so the run reaches `failed` in one
		// attempt rather than sitting in backoff.
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

	got, found, err := files.dir.ReadTransitions().LastFailure("7")
	if err != nil {
		t.Fatalf("LastFailure: %v", err)
	}
	if !found {
		entries, _, _ := files.dir.ReadTransitions().Tail(0)
		t.Fatalf("the reason did not survive; the log holds %d entries: %+v", len(entries), entries)
	}
	if got.Reason != core.FailureAuth {
		t.Errorf("reason = %q, want %q", got.Reason, core.FailureAuth)
	}
	if got.At.IsZero() {
		t.Error("At is zero")
	}
}

// The sticky-field trap: Record.FailureReason survives into the retry, so a log
// that stamped it on every edge would name a §7.3 reason on transitions no
// failure caused — and §9.10 step 6 would read one back and publish it.
func TestOnlyTheEdgesAFailureCausedCarryAReason(t *testing.T) {
	files := testStateFiles(t)
	d := startDaemonWith(t, files, func(d *daemonRun) {
		// crashed is retryable: attempt 1 fails, the record re-enters backoff
		// and prepares again, so the log holds edges on both sides of a failure.
		d.Runner.SetScript(func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return []core.Event{
					{Type: core.EventStarted, SessionID: "sess-1", Continuation: "sess-1"},
					{Type: core.EventFailed, Reason: core.FailureCrashed},
				}
			}
			return []core.Event{{Type: core.EventStarted, SessionID: "sess-2", Continuation: "sess-2"}}
		})
		// The failure has to be routed, and holdOutcome waits for the event
		// stream to close before it routes anything (SPEC §7.4).
		d.Runner.SetHangAfterScript(false)
	})
	d.waitTransition("7", "backoff")
	d.stopAndWait()

	entries, _, err := files.dir.ReadTransitions().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	var sawFailure bool
	for _, e := range entries {
		switch e.To {
		case "backoff", "failed", "needs-review":
			if e.To == "backoff" {
				sawFailure = true
				if e.FailureReason != core.FailureCrashed {
					t.Errorf("backoff edge reason = %q, want %q", e.FailureReason, core.FailureCrashed)
				}
			}
		default:
			if e.FailureReason != "" {
				t.Errorf("%s → %s carries reason %q; no failure caused that edge",
					e.From, e.To, e.FailureReason)
			}
		}
	}
	if !sawFailure {
		t.Fatalf("no backoff edge in the log: %+v", entries)
	}
}

// Every entry carries the attempt it belongs to, and the run record published
// alongside it agrees. Without that the two files describe the same run under
// two names and neither can be joined to the agent's own output.
func TestTheRunIDJoinsTheLogTheRecordAndTheChild(t *testing.T) {
	files := testStateFiles(t)
	var childRunID string
	d := startDaemonWith(t, files, func(d *daemonRun) {
		d.Runner.SetScript(func(spec core.RunSpec, _ int) []core.Event {
			childRunID = spec.Env["BEN_RUN_ID"]
			return []core.Event{{Type: core.EventStarted, SessionID: "sess-1", Continuation: "sess-1"}}
		})
	})
	d.waitLabel("7", core.StateLabelRunning)
	d.stopAndWait()

	if childRunID == "" {
		t.Fatal("the child was launched without BEN_RUN_ID, so its output cannot be correlated")
	}

	runs, err := files.dir.ReadRuns()
	if err != nil {
		t.Fatalf("ReadRuns: %v", err)
	}
	if len(runs.Records) != 1 {
		t.Fatalf("records = %d, want 1: %+v", len(runs.Records), runs.Records)
	}
	if got := runs.Records[0].RunID; got != childRunID {
		t.Errorf("run record run_id = %q, child saw %q", got, childRunID)
	}
	if got := runs.Records[0].SessionID; got != "sess-1" {
		t.Errorf("run record session_id = %q, want sess-1", got)
	}

	entries, _, err := files.dir.ReadTransitions().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	for _, e := range entries {
		if e.To == "running" && e.RunID != childRunID {
			t.Errorf("running edge run_id = %q, child saw %q", e.RunID, childRunID)
		}
	}
}

func TestRunsFileDescribesTheDaemonAndItsExit(t *testing.T) {
	files := testStateFiles(t)
	d := startDaemonWith(t, files)
	d.waitLabel("7", core.StateLabelRunning)

	// While it runs.
	runs, err := files.dir.ReadRuns()
	if err != nil {
		t.Fatalf("ReadRuns: %v", err)
	}
	if runs.Daemon.ID == "" || runs.Daemon.PID == 0 || runs.Daemon.Workflow != "wf" {
		t.Errorf("daemon = %+v, want it to name itself", runs.Daemon)
	}
	if runs.Daemon.HeartbeatMS <= 0 {
		t.Error("heartbeat_ms is unset, so a reader cannot tell a stale file from a quiet daemon")
	}
	if runs.Daemon.Stopped {
		t.Error("stopped is set on a running daemon")
	}
	if runs.Daemon.Stale(time.Now(), 30*time.Second) {
		t.Errorf("a running daemon reads as stale: written_at %v", runs.Daemon.WrittenAt)
	}

	d.stopAndWait()

	// And after it exits: positively stopped, rather than left to look stale.
	runs, err = files.dir.ReadRuns()
	if err != nil {
		t.Fatalf("ReadRuns after exit: %v", err)
	}
	if !runs.Daemon.Stopped {
		t.Error("the last write does not mark the daemon stopped, so a reader has to wait out the heartbeat to find out")
	}
}

// §10.3: every line about a run carries the issue, the run id and the session
// id. Asserted through the handler `ben run` actually builds, against a real
// daemon — a hand-driven logger would prove only that the wrapper compiles.
func TestLogLinesAboutARunCarryTheCorrelationAttrs(t *testing.T) {
	files := testStateFiles(t)
	var out lockedBuffer
	d := startDaemonWith(t, files, func(d *daemonRun) {
		d.logTo = &out
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

	var withIssue, correlated int
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["issue"] == nil {
			// A line about the daemon itself has no run to correlate to, and
			// inventing attributes for it would be worse than omitting them.
			if rec["run_id"] != nil {
				t.Errorf("a line naming no issue carries run_id: %q", line)
			}
			continue
		}
		withIssue++
		if rec["run_id"] != nil {
			correlated++
		}
	}
	if withIssue == 0 {
		t.Fatal("the daemon logged nothing about a run, so nothing was correlated")
	}
	if correlated == 0 {
		t.Errorf("%d lines name an issue and none carries run_id", withIssue)
	}
}

// The wrapper's own contract, at the two spellings the loop can reach it by.
func TestCorrelateEnrichesBothSpellings(t *testing.T) {
	lookup := func(issue string) (string, string, bool) {
		if issue != "#7" {
			return "", "", false
		}
		return "7-1.0", "sess-1", true
	}
	for _, tc := range []struct {
		name string
		emit func(*slog.Logger)
		want bool
	}{
		{"issue passed per call", func(l *slog.Logger) { l.Info("m", "issue", "#7") }, true},
		{"issue carried by With", func(l *slog.Logger) { l.With("issue", "#7").Info("m") }, true},
		{
			// The failure mode embedding a slog.Handler would have produced:
			// With returns the inner handler and correlation silently stops.
			name: "With on an unrelated key keeps correlating",
			emit: func(l *slog.Logger) { l.With("component", "loop").Info("m", "issue", "#7") },
			want: true,
		},
		{"an issue nobody is tracking", func(l *slog.Logger) { l.Info("m", "issue", "#99") }, false},
		{"no issue at all", func(l *slog.Logger) { l.Info("m") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.emit(slog.New(correlate{inner: slog.NewJSONHandler(&buf, nil), lookup: lookup}))
			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("not JSON: %q: %v", buf.String(), err)
			}
			gotRun, gotSession := rec["run_id"], rec["session_id"]
			if tc.want {
				if gotRun != "7-1.0" || gotSession != "sess-1" {
					t.Errorf("run_id = %v, session_id = %v, want the tracked run's: %q", gotRun, gotSession, buf.String())
				}
				return
			}
			if gotRun != nil || gotSession != nil {
				t.Errorf("correlated a line it could not resolve: %q", buf.String())
			}
		})
	}
}
