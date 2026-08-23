package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

// `ben status` against state a *live daemon* wrote, through the same argument
// handling `ben` dispatches — never a hand-authored fixture, which would assert
// what this test believes the daemon writes.

// statusOf runs the command the way the binary does and returns its streams.
func statusOf(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errs bytes.Buffer
	code = run(append([]string{"status"}, args...), &out, &errs)
	return code, out.String(), errs.String()
}

// The whole command, end to end: a daemon runs, `ben status` finds its state dir
// from the workflow path alone, and renders what it wrote. XDG_STATE_HOME is set
// so both halves resolve the same directory without either being told it.
func TestStatusRendersALiveDaemonsState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := startDaemonWith(t, nil, func(d *daemonRun) {
		d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "sess-1", Continuation: "sess-1"}}
		})
	})
	d.waitLabel("7", core.StateLabelRunning)
	d.waitRuns(1)

	// Read-only, and while the daemon runs (SPEC §11). Both halves matter: this
	// is the moment the file is being rewritten under the reader.
	code, out, errs := statusOf(t, d.path)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errs)
	}
	for _, want := range []string{"running", "7", "RUNS (1)", "TRANSITIONS"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NOT RUNNING") {
		t.Errorf("a live daemon rendered as not running:\n%s", out)
	}

	d.stopAndWait()

	// And after it exits: positively stopped, rather than left to look stale.
	code, out, errs = statusOf(t, d.path)
	if code != 0 {
		t.Fatalf("exit %d after shutdown, stderr: %s", code, errs)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("a stopped daemon does not say so:\n%s", out)
	}
}

// The §11 contract, asserted as a contract: named fields, stable meanings.
func TestStatusJSONIsAContract(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := startDaemonWith(t, nil, func(d *daemonRun) {
		d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "sess-1", Continuation: "sess-1"}}
		})
	})
	d.waitLabel("7", core.StateLabelRunning)
	d.waitRuns(1)
	d.stopAndWait()

	code, out, errs := statusOf(t, "--json", d.path)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errs)
	}
	var got statusReport
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("--json output does not decode as the contract: %v\n%s", err, out)
	}

	if got.Status != livenessStopped {
		t.Errorf("status = %q, want %q", got.Status, livenessStopped)
	}
	if got.Workflow == "" || got.StateDir == "" {
		t.Errorf("workflow/state_dir are empty: %+v", got)
	}
	if got.Daemon.ID == "" || got.Daemon.PID == 0 {
		t.Errorf("daemon does not name itself: %+v", got.Daemon)
	}
	if len(got.Runs) != 1 || got.Runs[0].Issue != "7" {
		t.Fatalf("runs = %+v, want one record for issue 7", got.Runs)
	}
	if got.Runs[0].State == "" || got.Runs[0].RunID == "" {
		t.Errorf("run record is missing its state or run id: %+v", got.Runs[0])
	}
	if !got.Runs[0].Resuming {
		t.Error("resuming = false for a run whose agent announced a session")
	}
	if got.TransitionsTotal == 0 || len(got.Transitions) == 0 {
		t.Errorf("no transitions in the contract: %+v", got)
	}
	// Oldest first, as the log is written.
	for i := 1; i < len(got.Transitions); i++ {
		if got.Transitions[i].TS.Before(got.Transitions[i-1].TS) {
			t.Errorf("transitions are out of order at %d: %v", i, got.Transitions)
			break
		}
	}
}

// Whatever `ben status` prints is a publication seam (#52). The credential in
// the workflow reaches the daemon and must reach neither surface.
func TestStatusNeverPrintsACredential(t *testing.T) {
	const secret = "ghp_statusmustnotprintthis"
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("BEN_TEST_TOKEN", secret)

	// The credential is in the configuration the daemon loads and runs under —
	// resolved from the environment, which is what makes it sensitive by
	// provenance (SPEC §5.8, #47). A test that never put one in the daemon would
	// be asserting that a string nobody supplied does not appear.
	d := startDaemonFor(t, nil,
		[]func(*workflowSpec){withTrackerProvider("    token: $BEN_TEST_TOKEN\n")},
		func(d *daemonRun) {
			d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
				return []core.Event{
					{Type: core.EventStarted, SessionID: "sess-1", Continuation: "sess-1"},
					// A failure, because the failure track is what carries free
					// operator-facing text into both the log and this command.
					{Type: core.EventFailed, Reason: core.FailureAuth},
				}
			})
			d.Runner.SetHangAfterScript(false)
		})
	d.waitLabel("7", core.StateLabelFailed)
	d.stopAndWait()

	// First, that the secret is genuinely in play. Without this the assertions
	// below would hold just as well for a string nobody ever supplied — the
	// vacuous shape #52 warns about, where a redaction test cannot see its own
	// input go missing. `config effective` resolves the same file and reports
	// the field as redacted, which is only possible if the value was resolved.
	var cfg bytes.Buffer
	if code := run([]string{"config", "effective", "--json", d.path}, &cfg, io.Discard); code != 0 {
		t.Fatalf("config effective exited %d", code)
	}
	if !strings.Contains(cfg.String(), "token") || !strings.Contains(cfg.String(), config.Redacted) {
		t.Fatalf("the fixture carries no redacted credential, so this test proves nothing:\n%s", cfg.String())
	}
	if strings.Contains(cfg.String(), secret) {
		t.Fatalf("config effective printed the credential; this test is about ben status, and that is a different leak:\n%s", cfg.String())
	}

	for _, args := range [][]string{{d.path}, {"--json", d.path}} {
		code, out, errs := statusOf(t, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d, stderr: %s", args, code, errs)
		}
		if strings.Contains(out+errs, secret) {
			t.Errorf("%v printed the credential:\n%s%s", args, out, errs)
		}
	}

	// And the file behind it, so the assertion is about the pipeline rather than
	// about the renderer having stripped something on the way out.
	raw, err := os.ReadFile(d.files.dir.RunsPath())
	if err != nil {
		t.Fatalf("reading runs.json: %v", err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Errorf("the credential is in runs.json:\n%s", raw)
	}
}

// A torn write must never render as truth — the run marker's rule, applied to
// the surface an operator actually reads (internal/workspace/marker.go).
func TestStatusRefusesATornStateFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	d := startDaemonWith(t, nil)
	d.waitLabel("7", core.StateLabelRunning)
	d.stopAndWait()

	// What a crash between the rename and the sync leaves: the name is
	// published and the bytes are not all there.
	if err := os.WriteFile(d.files.dir.RunsPath(), []byte(`{"daemon":{"id":"host/wf","pi`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, args := range [][]string{{d.path}, {"--json", d.path}} {
		code, out, errs := statusOf(t, args...)
		if code == 0 {
			t.Errorf("%v: exit 0 on a torn file; unreadable must not report as idle:\n%s", args, out)
		}
		if strings.Contains(out, "RUNS (0)") || strings.Contains(out, `"runs": []`) {
			t.Errorf("%v rendered a torn file as a daemon with nothing to do:\n%s", args, out)
		}
		if !strings.Contains(errs, "unreadable") {
			t.Errorf("%v does not say the file could not be read: %s", args, errs)
		}
	}
}

// A run-record file with no log beside it is news, not an empty log: the daemon
// creates the log before it does anything else, so one being gone means
// something removed it.
func TestStatusSeparatesAnEmptyLogFromAMissingOne(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := startDaemonWith(t, nil)
	d.waitLabel("7", core.StateLabelRunning)
	d.stopAndWait()

	if err := os.Remove(d.files.dir.TransitionsPath()); err != nil {
		t.Fatalf("removing the log: %v", err)
	}
	code, _, errs := statusOf(t, d.path)
	if code == 0 {
		t.Error("a missing transition log reported as success")
	}
	if !strings.Contains(errs, "transition log") {
		t.Errorf("the error does not name what is missing: %s", errs)
	}
}

// The transition log survives the daemon that wrote it, and a second daemon
// appends to the same one rather than replacing it.
func TestTheLogSurvivesARestart(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	first := startDaemonWith(t, nil)
	first.waitLabel("7", core.StateLabelRunning)
	first.stopAndWait()

	before, _, err := first.files.dir.ReadTransitions().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("the first daemon recorded no transitions")
	}

	// A second daemon over the same directory. It reaches its own transitions,
	// and the first daemon's are still in front of them.
	second := startDaemonWith(t, openTestState(t, first.files.dir, first.files.key))
	second.waitLabel("7", core.StateLabelRunning)
	second.stopAndWait()

	after, _, err := first.files.dir.ReadTransitions().Tail(0)
	if err != nil {
		t.Fatalf("Tail after restart: %v", err)
	}
	if len(after) <= len(before) {
		t.Fatalf("the second daemon appended nothing: %d entries, was %d", len(after), len(before))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("entry %d changed across the restart:\n was %+v\n now %+v", i, before[i], after[i])
		}
	}
	// And in order across the seam.
	for i := 1; i < len(after); i++ {
		if after[i].TS.Before(after[i-1].TS) {
			t.Errorf("entries are out of order at %d across the restart", i)
			break
		}
	}
}

func TestLivenessVerdict(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		d    state.Daemon
		want liveness
	}{
		{
			name: "a current heartbeat is running",
			d:    state.Daemon{HeartbeatMS: 2000, WrittenAt: now.Add(-time.Second)},
			want: livenessRunning,
		},
		{
			// A missed beat on a busy host. Crying "dead" at one is how a status
			// surface teaches people to ignore it.
			name: "one missed beat is still running",
			d:    state.Daemon{HeartbeatMS: 2000, WrittenAt: now.Add(-3 * time.Second)},
			want: livenessRunning,
		},
		{
			name: "long silence with no final record is stale",
			d:    state.Daemon{HeartbeatMS: 2000, WrittenAt: now.Add(-time.Hour)},
			want: livenessStale,
		},
		{
			// The final write beats the clock: a daemon that said it stopped is
			// stopped, however long ago.
			name: "a stopped daemon is stopped, not stale",
			d:    state.Daemon{HeartbeatMS: 2000, WrittenAt: now.Add(-time.Hour), Stopped: true},
			want: livenessStopped,
		},
		{
			// A file written by a daemon that declared no cadence promises no
			// heartbeat, so its silence is not evidence.
			name: "no declared interval falls back to the floor",
			d:    state.Daemon{WrittenAt: now.Add(-time.Second)},
			want: livenessRunning,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := livenessOf(tc.d, now); got != tc.want {
				t.Errorf("liveness = %q, want %q", got, tc.want)
			}
		})
	}
}

// The tail is a cap, and a cap this repo will not let be silent: the count it
// was taken from is printed beside it.
func TestTheTransitionTailSaysItIsATail(t *testing.T) {
	dir := state.At(t.TempDir())
	w, err := dir.AppendTransitions()
	if err != nil {
		t.Fatalf("AppendTransitions: %v", err)
	}
	base := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	total := transitionTail + 5
	for i := range total {
		if err := w.Append(state.Transition{
			TS: base.Add(time.Duration(i) * time.Minute), Issue: "7",
			From: "backoff", To: "preparing", Reason: "edge",
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	w.Close() //nolint:errcheck // the appends are what matter
	if err := dir.WriteRuns(state.Runs{Daemon: state.Daemon{ID: "host/wf", HeartbeatMS: 2000}}); err != nil {
		t.Fatalf("WriteRuns: %v", err)
	}

	report, err := readStatus(dir, "wf")
	if err != nil {
		t.Fatalf("readStatus: %v", err)
	}
	if len(report.Transitions) != transitionTail || report.TransitionsTotal != total {
		t.Fatalf("window = %d of %d, want %d of %d",
			len(report.Transitions), report.TransitionsTotal, transitionTail, total)
	}

	var out bytes.Buffer
	writeStatusText(&out, "WORKFLOW.md", report)
	if !strings.Contains(out.String(), "last 10 of 15") {
		t.Errorf("the tail does not say what it is a tail of:\n%s", out.String())
	}
}

// The review finding: `statusReport` embedded `state.Run`, so `--json` shipped
// the `continuation` field verbatim. It is an opaque resume token whose meaning
// belongs to whichever adapter minted it, and a stable API is a promise to keep
// publishing whatever it names.
//
// The file keeps it — at 0600, for the operator who needs it — and the command
// reports only that a session will be resumed.
func TestStatusPublishesThatASessionResumesAndNotTheToken(t *testing.T) {
	const token = "sess-01J9ZQ4S-must-not-be-published"
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := startDaemonWith(t, nil, func(d *daemonRun) {
		d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: token, Continuation: token}}
		})
	})
	d.waitLabel("7", core.StateLabelRunning)
	d.waitRuns(1)
	d.stopAndWait()

	// It really is in the state dir — otherwise this proves nothing.
	raw, err := os.ReadFile(d.files.dir.RunsPath())
	if err != nil {
		t.Fatalf("reading runs.json: %v", err)
	}
	if !bytes.Contains(raw, []byte(token)) {
		t.Fatalf("the token is not in runs.json, so this test asserts nothing about publishing it:\n%s", raw)
	}

	for _, args := range [][]string{{d.path}, {"--json", d.path}} {
		code, out, errs := statusOf(t, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d, stderr: %s", args, code, errs)
		}
		if strings.Contains(out+errs, token) {
			t.Errorf("%v published the resume token:\n%s%s", args, out, errs)
		}
	}

	// And the publishable substitute is there instead.
	_, js, _ := statusOf(t, "--json", d.path)
	var got statusReport
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Runs) != 1 || !got.Runs[0].Resuming {
		t.Errorf("resuming is not reported in place of the token: %+v", got.Runs)
	}
}

// The whole point of the presentation type is that a field added to the file
// does not become a published field.
//
// The first version of this test decoded the *whole* of runs.json into a struct
// holding only `records`, so it failed on the unrelated top-level `daemon` key
// and passed just as happily with `[]state.Run` in place of `[]runView`. It
// proved nothing about either. Decoding one record payload on its own is what
// actually puts the two types against each other — and the assertion on *why*
// the decode failed is what keeps it from passing for an unrelated reason.
func TestTheJSONContractIsNotTheFileFormat(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := startDaemonWith(t, nil, func(d *daemonRun) {
		d.Runner.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "s", Continuation: "s"}}
		})
	})
	d.waitLabel("7", core.StateLabelRunning)
	d.waitRuns(1)
	d.stopAndWait()

	raw, err := os.ReadFile(d.files.dir.RunsPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var file struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("runs.json does not parse: %v", err)
	}
	if len(file.Records) == 0 {
		t.Fatal("no records were written, so nothing is being compared")
	}

	dec := json.NewDecoder(bytes.NewReader(file.Records[0]))
	dec.DisallowUnknownFields()
	var published runView
	err = dec.Decode(&published)
	if err == nil {
		t.Fatalf("a run record decodes cleanly into the published type; the file format and the contract are the same thing again:\n%s", file.Records[0])
	}
	// And it fails for the right reason: a field the file carries and the
	// contract deliberately drops, not some unrelated shape mismatch.
	if !strings.Contains(err.Error(), "continuation") && !strings.Contains(err.Error(), "session_id") {
		t.Errorf("the decode failed on %q, not on a field the contract drops; this test would pass for the wrong reason", err)
	}
}
