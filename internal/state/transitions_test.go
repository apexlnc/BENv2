package state

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

var logStart = time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)

// appendAll writes entries through a writer it opens and closes, so every test
// that reads is reading a log some *closed* writer produced — the restart case,
// not a handle the test still holds.
func appendAll(t *testing.T, d Dir, entries ...Transition) {
	t.Helper()
	w, err := d.AppendTransitions()
	if err != nil {
		t.Fatalf("AppendTransitions: %v", err)
	}
	for i, e := range entries {
		if err := w.Append(e); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func entry(n int, issue, from, to string) Transition {
	return Transition{
		TS: logStart.Add(time.Duration(n) * time.Minute), Issue: issue,
		From: from, To: to, Actor: "host/wf", Reason: fmt.Sprintf("edge %d", n),
	}
}

func TestTransitionsSurviveTheWriterAndReadInOrder(t *testing.T) {
	d := At(t.TempDir())
	want := []Transition{
		entry(1, "#7", "queued", "claimed"),
		entry(2, "#7", "claimed", "preparing"),
		entry(3, "#8", "queued", "claimed"),
	}
	// Two writers in sequence: the log has to survive the first being closed,
	// which is the restart the §9.10 step 6 reader depends on.
	appendAll(t, d, want[:2]...)
	appendAll(t, d, want[2])

	got, _, err := d.ReadTransitions().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Issue != want[i].Issue || got[i].To != want[i].To || !got[i].TS.Equal(want[i].TS) {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestTailBound(t *testing.T) {
	d := At(t.TempDir())
	var all []Transition
	for i := 1; i <= 10; i++ {
		all = append(all, entry(i, "#7", "backoff", "preparing"))
	}
	appendAll(t, d, all...)

	got, total, err := d.ReadTransitions().Tail(3)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Tail(3) returned %d entries", len(got))
	}
	// The count is of the whole log, not of the window. A renderer that cannot
	// say "10" has no way to state that it showed 3 — and learning it by asking
	// for everything is the thing the bound exists to prevent.
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	// The *last* three, oldest first: a tail that returned the head would be a
	// status surface reporting a daemon's first three minutes forever.
	for i, want := range []int{8, 9, 10} {
		if got[i].Reason != fmt.Sprintf("edge %d", want) {
			t.Errorf("tail[%d] = %q, want edge %d", i, got[i].Reason, want)
		}
	}
}

func TestAbsentLogIsErrNoState(t *testing.T) {
	r := At(t.TempDir()).ReadTransitions()
	if _, _, err := r.Tail(0); !errors.Is(err, ErrNoState) {
		t.Errorf("Tail err = %v, want ErrNoState", err)
	}
	// The distinction this whole seam exists for: no log at all is "we could not
	// ask", never "this issue has no failure" (SPEC §9.10 step 6).
	_, found, err := r.LastFailure("#7")
	if !errors.Is(err, ErrNoState) {
		t.Errorf("LastFailure err = %v, want ErrNoState", err)
	}
	if found {
		t.Error("LastFailure found = true against a log that does not exist")
	}
}

func TestTrailingPartialRecordIsDroppedAndTheRestIsRead(t *testing.T) {
	d := At(t.TempDir())
	appendAll(t, d,
		entry(1, "#7", "queued", "claimed"),
		failure(2, "#7", core.FailureCrashed, "the agent exited without a terminal event"),
	)
	// The writer caught between its write and our read — or a crash that took
	// the newline with it.
	f, err := os.OpenFile(d.TransitionsPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"ts":"2026-08-13T09:03:00Z","issue":"#7","fr`); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close() //nolint:errcheck // the write is what mattered

	got, _, err := d.ReadTransitions().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d entries, want the 2 complete ones", len(got))
	}
	// And the fact behind the complete records is still answerable.
	fail, found, err := d.ReadTransitions().LastFailure("#7")
	if err != nil || !found {
		t.Fatalf("LastFailure = (%+v, %v, %v), want the crashed record", fail, found, err)
	}
	if fail.Reason != core.FailureCrashed {
		t.Errorf("reason = %q, want %q", fail.Reason, core.FailureCrashed)
	}
}

func TestMalformedCompleteRecordIsLoud(t *testing.T) {
	d := At(t.TempDir())
	appendAll(t, d, failure(1, "#7", core.FailureCrashed, "first"))
	f, err := os.OpenFile(d.TransitionsPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Complete — it has its newline — and not a record. That is corruption, and
	// a log with a hole in it must not quietly answer questions about the rest.
	if _, err := f.WriteString("this is not json\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close() //nolint:errcheck // the write is what mattered

	if _, _, err := d.ReadTransitions().Tail(0); err == nil {
		t.Error("Tail accepted a corrupt log")
	}
	fail, found, err := d.ReadTransitions().LastFailure("#7")
	if err == nil {
		t.Fatalf("LastFailure = (%+v, %v), nil error; corruption must not read as a surviving reason", fail, found)
	}
	if found {
		t.Error("LastFailure reported found alongside an error")
	}
}

func failure(n int, issue string, reason core.FailureReason, detail string) Transition {
	return Transition{
		TS: logStart.Add(time.Duration(n) * time.Minute), Issue: issue,
		From: "running", To: failedState, Actor: "host/wf",
		Reason: detail, RunID: fmt.Sprintf("%s-a1", issue), FailureReason: reason,
	}
}

func TestLastFailure(t *testing.T) {
	backoff := func(n int, issue string, reason core.FailureReason) Transition {
		e := failure(n, issue, reason, "retryable")
		e.To = "backoff"
		return e
	}

	for _, tc := range []struct {
		name    string
		log     []Transition
		want    core.FailureReason
		wantDet string
		found   bool
	}{
		{
			name: "the most recent failed edge wins",
			log: []Transition{
				failure(1, "#7", core.FailureLaunchError, "starting the agent: exec format error"),
				failure(2, "#7", core.FailureTimeout, "attempt timeout exceeded"),
			},
			want: core.FailureTimeout, wantDet: "attempt timeout exceeded", found: true,
		},
		{
			// The retry track's edges carry a §7.3 reason too, and they are not
			// what step 6 asks for: the row that reads this log is the one where
			// `ben:failed` is standing.
			name: "a later backoff does not displace the failure",
			log: []Transition{
				failure(1, "#7", core.FailureAuth, "auth"),
				backoff(2, "#7", core.FailureCrashed),
			},
			want: core.FailureAuth, wantDet: "auth", found: true,
		},
		{
			name: "another issue's failure is not ours",
			log: []Transition{
				failure(1, "#8", core.FailureCrashed, "not ours"),
				entry(2, "#7", "queued", "claimed"),
			},
			found: false,
		},
		{
			// §9.10 step 6's blessed degradation: a log was read, and this
			// issue's reason is not in it.
			name:  "a log with no failure for this issue is found=false, not an error",
			log:   []Transition{entry(1, "#7", "queued", "claimed")},
			found: false,
		},
		{
			// A `failed` edge the loop reached without a §7.3 verdict carries
			// none. Reading the trigger text back as a reason would be inventing
			// one, which step 6 forbids in as many words.
			name: "a failed edge with no taxonomy is not a reason",
			log: []Transition{{
				TS: logStart, Issue: "#7", From: "preparing", To: failedState,
				Actor: "host/wf", Reason: "attempts exhausted",
			}},
			found: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := At(t.TempDir())
			appendAll(t, d, tc.log...)
			got, found, err := d.ReadTransitions().LastFailure("#7")
			if err != nil {
				t.Fatalf("LastFailure: %v", err)
			}
			if found != tc.found {
				t.Fatalf("found = %v, want %v (got %+v)", found, tc.found, got)
			}
			if !tc.found {
				if got != (core.RunFailure{}) {
					t.Errorf("not found, but returned %+v", got)
				}
				return
			}
			if got.Reason != tc.want {
				t.Errorf("reason = %q, want %q", got.Reason, tc.want)
			}
			if got.Detail != tc.wantDet {
				t.Errorf("detail = %q, want %q", got.Detail, tc.wantDet)
			}
			if got.At.IsZero() {
				t.Error("At is zero — a consumer scoping to a claim cycle has nothing to compare")
			}
		})
	}
}

// The writer's bound and the reader's bound are one contract. If the writer
// could emit a record the reader refuses, the log would only fail during
// recovery — the one moment it exists for.
func TestOversizeRecordIsRefusedOnBothSides(t *testing.T) {
	d := At(t.TempDir())
	w, err := d.AppendTransitions()
	if err != nil {
		t.Fatalf("AppendTransitions: %v", err)
	}
	defer w.Close() //nolint:errcheck // closed again below where it matters

	huge := entry(1, "#7", "running", "failed")
	huge.Reason = strings.Repeat("x", maxRecordBytes)
	if err := w.Append(huge); err == nil {
		t.Fatal("Append accepted a record over the size limit")
	}
	// And nothing was written: a refused record must not leave a fragment.
	if _, err := os.Stat(d.TransitionsPath()); err == nil {
		if got, _, err := d.ReadTransitions().Tail(0); err != nil || len(got) != 0 {
			t.Errorf("after a refused append: %d entries, err %v", len(got), err)
		}
	}

	// The reader's half, against a line only something else could have written.
	if err := os.WriteFile(d.TransitionsPath(), append([]byte(strings.Repeat("x", maxRecordBytes+1)), '\n'), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := d.ReadTransitions().Tail(0); err == nil {
		t.Error("Tail accepted a line over the size limit")
	}
}

func TestCloseIsIdempotentAndAppendAfterCloseRefuses(t *testing.T) {
	d := At(t.TempDir())
	w, err := d.AppendTransitions()
	if err != nil {
		t.Fatalf("AppendTransitions: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v — shutdown paths call it more than once", err)
	}
	if err := w.Append(entry(1, "#7", "queued", "claimed")); err == nil {
		t.Error("Append succeeded after Close")
	}
}

// `ben status` reads while `ben run` appends (SPEC §11).
func TestReadingWhileAppending(t *testing.T) {
	d := At(t.TempDir())
	w, err := d.AppendTransitions()
	if err != nil {
		t.Fatalf("AppendTransitions: %v", err)
	}
	defer w.Close() //nolint:errcheck // the test's failure signal is the reads

	const n = 300
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= n; i++ {
			if err := w.Append(failure(i, "#7", core.FailureCrashed, strings.Repeat("d", i))); err != nil {
				t.Errorf("Append #%d: %v", i, err)
				return
			}
		}
	}()

	r := d.ReadTransitions()
	var last int
	for {
		select {
		case <-done:
			if last == 0 {
				t.Fatal("the reader never saw a record")
			}
			return
		default:
		}
		got, _, err := r.Tail(0)
		if errors.Is(err, ErrNoState) {
			continue
		}
		if err != nil {
			t.Fatalf("Tail: %v", err)
		}
		if len(got) < last {
			t.Fatalf("the log went backwards: %d entries after %d", len(got), last)
		}
		last = len(got)
	}
}

// A crash mid-append leaves a fragment with no newline. The reader tolerates
// one; a *writer* cannot, because O_APPEND starts the next record at the
// fragment's end and the two become a line that parses as neither — and that
// line is permanent, so one crash costs the whole log rather than the record it
// was writing. This is the regression for that.
func TestATornTailIsRepairedBeforeTheNextAppend(t *testing.T) {
	d := At(t.TempDir())
	appendAll(t, d,
		entry(1, "#7", "queued", "claimed"),
		failure(2, "#7", core.FailureCrashed, "the agent exited without a terminal event"),
	)
	fragment := `{"ts":"2026-08-13T09:03:00Z","issue":"#7","fr`
	f, err := os.OpenFile(d.TransitionsPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(fragment); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close() //nolint:errcheck // the write is what mattered

	// The restart.
	w, err := d.AppendTransitions()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if w.Repaired != int64(len(fragment)) {
		t.Errorf("Repaired = %d, want %d — the discard must be reported, not swallowed", w.Repaired, len(fragment))
	}
	if err := w.Append(entry(3, "#7", "claimed", "preparing")); err != nil {
		t.Fatalf("Append after repair: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, _, err := d.ReadTransitions().Tail(0)
	if err != nil {
		t.Fatalf("the log is unreadable after a torn restart: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d entries, want the 2 that survived plus the new one: %+v", len(got), got)
	}
	if got[2].To != "preparing" {
		t.Errorf("the post-restart entry is %+v", got[2])
	}
	// And the fact the whole seam exists for still answers.
	fail, found, err := d.ReadTransitions().LastFailure("#7")
	if err != nil || !found || fail.Reason != core.FailureCrashed {
		t.Errorf("LastFailure = (%+v, %v, %v) after a torn restart", fail, found, err)
	}
}

// The repair is a no-op on the ordinary case, and must not eat the last record.
func TestRepairLeavesACompleteLogAlone(t *testing.T) {
	d := At(t.TempDir())
	appendAll(t, d, entry(1, "#7", "queued", "claimed"), entry(2, "#7", "claimed", "preparing"))

	w, err := d.AppendTransitions()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if w.Repaired != 0 {
		t.Errorf("Repaired = %d on a complete log", w.Repaired)
	}
	w.Close() //nolint:errcheck // nothing was written

	got, _, err := d.ReadTransitions().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("read %d entries, want 2 — the repair ate a complete record", len(got))
	}
}

// A file whose tail runs longer than any record this package writes is not a
// fragment of one. Refusing beats guessing how much to discard: truncation
// would compound whatever is actually wrong, and moving the file aside is an
// operator's decision.
func TestRepairRefusesAFileItCannotBound(t *testing.T) {
	d := At(t.TempDir())
	if err := d.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	junk := append([]byte("{\"ts\":\"x\"}\n"), bytes.Repeat([]byte("x"), maxRecordBytes+1)...)
	if err := os.WriteFile(d.TransitionsPath(), junk, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := d.AppendTransitions(); err == nil {
		t.Error("opened a log whose tail has no record boundary")
	}
	// And it left the file alone.
	raw, err := os.ReadFile(d.TransitionsPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(raw) != len(junk) {
		t.Errorf("the refusal truncated the file anyway: %d bytes, was %d", len(raw), len(junk))
	}
}

// A file that is nothing but a fragment truncates to empty rather than refusing:
// the whole of it is one incomplete record, which is what a crash during the
// very first append leaves.
func TestRepairEmptiesAFileThatIsOnlyAFragment(t *testing.T) {
	d := At(t.TempDir())
	if err := d.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := os.WriteFile(d.TransitionsPath(), []byte(`{"ts":"2026-08-13T09:0`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	w, err := d.AppendTransitions()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := w.Append(entry(1, "#7", "queued", "claimed")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	w.Close() //nolint:errcheck // the append is what mattered

	got, _, err := d.ReadTransitions().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d entries, want 1: %+v", len(got), got)
	}
}

// Review finding: the caller retries a failed Append, so "failed" has to mean
// "not appended". A write that fails partway, or a Sync that fails over bytes
// already down, would otherwise turn the retry into either the glued-fragment
// corruption AppendTransitions repairs — recreated mid-process, where no reopen
// will find it — or a transition recorded twice.
func TestAFailedAppendLeavesNothingBehind(t *testing.T) {
	d := At(t.TempDir())
	w, err := d.AppendTransitions()
	if err != nil {
		t.Fatalf("AppendTransitions: %v", err)
	}
	if err := w.Append(entry(1, "#7", "queued", "claimed")); err != nil {
		t.Fatalf("first Append: %v", err)
	}

	// The bytes are written and then the commit is refused — a Sync failure, or
	// a short write reported after the fact.
	w.failAfterWrite = func() error { return errors.New("no space left on device") }
	failed := failure(2, "#7", core.FailureCrashed, "the record that must not survive")
	if err := w.Append(failed); err == nil {
		t.Fatal("Append reported success while its commit failed")
	}

	// The retry, once the disk comes back. This is the caller's behaviour, and
	// it is the reason the rollback exists.
	w.failAfterWrite = nil
	if err := w.Append(failed); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, _, err := d.ReadTransitions().Tail(0)
	if err != nil {
		t.Fatalf("the log is unreadable after a failed append: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d entries, want 2 — the failed append was neither rolled back nor written once", len(got))
	}
	if got[1].Reason != failed.Reason {
		t.Errorf("entry 1 = %+v, want the retried record", got[1])
	}
	// And specifically not twice, which is what a Sync failure over durable
	// bytes would produce without the rollback.
	for i, e := range got {
		if i > 0 && e == got[i-1] {
			t.Errorf("entry %d duplicates its predecessor: %+v", i, e)
		}
	}
}

// If the rollback itself cannot complete there is a fragment in the file and no
// way to remove it, so the writer refuses everything afterwards rather than
// appending past it. Retrying that is a spin, so the error says so in a way the
// caller can test for.
func TestAnUnrollableFailureLatchesTheWriterShut(t *testing.T) {
	d := At(t.TempDir())
	w, err := d.AppendTransitions()
	if err != nil {
		t.Fatalf("AppendTransitions: %v", err)
	}
	// Closing the file under the writer makes both the append and its rollback
	// fail, which is the shape being tested rather than the cause.
	w.failAfterWrite = func() error { return errors.New("i/o error") }
	w.f.Close() //nolint:errcheck // deliberately breaking the handle

	first := w.Append(entry(1, "#7", "queued", "claimed"))
	if !errors.Is(first, ErrLogUnwritable) {
		t.Fatalf("err = %v, want ErrLogUnwritable", first)
	}
	if second := w.Append(entry(2, "#7", "claimed", "preparing")); !errors.Is(second, ErrLogUnwritable) {
		t.Errorf("a later Append returned %v; the writer must stay shut", second)
	}
}
