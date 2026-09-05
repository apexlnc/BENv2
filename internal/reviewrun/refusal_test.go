package reviewrun

import (
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

// A refusal is the fourth thing a start can come back with, after a status, a
// lost response, and an error (#284). It is the one that will not change by
// itself, so it is neither replayed nor retained as unresolved: it is
// recorded, returned as itself, re-offered cheaply, and superseded the moment
// the request or its delivery changes.

func refusal() *RefusedError {
	return &RefusedError{Reason: "payload_too_large", Detail: "inline stdin exceeds the profile's limit"}
}

func TestADefiniteRefusalIsRecordedAndNotReplayed(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	f.refuse = refusal()
	s, store := newSession(t, f, remotely(f))
	sub := testSubject()

	_, err := s.Review(context.Background(), sub)
	if !errors.Is(err, ErrRunRefused) {
		t.Fatalf("Review = %v, want ErrRunRefused", err)
	}
	if errors.Is(err, ErrRunUnresolved) {
		t.Fatalf("a definite refusal read as an unresolved dispatch: %v", err)
	}
	if got, ok := RefusalOf(err); !ok || got.Reason != "payload_too_large" || got.Detail == "" {
		t.Fatalf("RefusalOf = (%+v, %v), want the executor's reason and detail", got, ok)
	}
	if f.starts != 1 {
		t.Fatalf("starts = %d, want one: a refusal is an answer, not a lost response to replay", f.starts)
	}
	run, _ := sub.RunID()
	if got := f.created(run); got != 0 {
		t.Fatalf("a refused start created %d runs", got)
	}
	rec, err := store.Load(run)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Dispatched || rec.BackendRunID != "" || !rec.Refused() || rec.Refusal.Reason != "payload_too_large" || !rec.Quiet {
		t.Fatalf("record after a refusal = %+v; want dispatched, no backend id, the refusal, and quiet", rec)
	}

	// The next sweep re-offers the exact request. The executor answers from its
	// record without creating anything, and the record still says refused.
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunRefused) {
		t.Fatalf("second Review = %v, want ErrRunRefused", err)
	}
	if f.starts != 2 || f.created(run) != 0 {
		t.Fatalf("after the re-offer: starts = %d, runs = %d; want 2 and 0", f.starts, f.created(run))
	}
	rec, _ = store.Load(run)
	if !rec.Refused() {
		t.Fatalf("the re-offer lost the refusal: %+v", rec)
	}
}

func TestARefusedRunStartsOnceTheExecutorCanDeliverIt(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	f.refuse = refusal()
	s, store := newSession(t, f, remotely(f))
	sub := testSubject()
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunRefused) {
		t.Fatalf("Review = %v, want ErrRunRefused", err)
	}

	// Same request, same address; the executor can now deliver it — a daemon
	// that learned to stream, say. No operator step is needed to find out.
	f.admit = true
	report, err := s.Review(context.Background(), sub)
	if err != nil {
		t.Fatalf("Review once admitted: %v", err)
	}
	if report.Verdict != review.VerdictClean {
		t.Fatalf("verdict = %q", report.Verdict)
	}
	run, _ := sub.RunID()
	if got := f.created(run); got != 1 {
		t.Fatalf("runs = %d, want exactly one", got)
	}
	rec, _ := store.Load(run)
	if rec.Refused() || rec.BackendRunID == "" || rec.Verdict != "clean" {
		t.Fatalf("record after admission = %+v; want the refusal cleared and the run's verdict", rec)
	}
}

func TestARecomposedRequestSupersedesARefusedRun(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	f.refuse = refusal()
	s, store := newSession(t, f, remotely(f))
	sub := testSubject()
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunRefused) {
		t.Fatalf("Review = %v, want ErrRunRefused", err)
	}
	run, _ := sub.RunID()
	before, _ := store.Load(run)

	// The operator lowers the bound: the same subject composes a different
	// request. A dispatched run would refuse the mismatch; a refused one holds
	// no run to confuse with the new request, so it is superseded.
	f.refuse = nil
	next, err := New(Options{
		Executor: f, Store: store, Sandbox: placementOf(f), Logger: testLogger(t), Sleep: boundedSleep(2),
		Compose: func(sub Subject) (Request, error) {
			return Request{Argv: []string{"codex", "exec"}, Stdin: []byte(sub.Diff[:4] + "\n*** truncated ***")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := next.Review(context.Background(), sub)
	if err != nil {
		t.Fatalf("Review with a recomposed request: %v", err)
	}
	if report.Verdict != review.VerdictClean {
		t.Fatalf("verdict = %q", report.Verdict)
	}
	after, _ := store.Load(run)
	if after.Digest == before.Digest || after.Refused() || after.BackendRunID == "" {
		t.Fatalf("record after supersession = %+v; want the new digest, no refusal, a run", after)
	}

	// The same mismatch over a *dispatched* record is still refused: supersession
	// is a property of refusals, not of records.
	again, err := New(Options{
		Executor: f, Store: store, Sandbox: placementOf(f), Logger: testLogger(t), Sleep: boundedSleep(2),
		Compose: func(sub Subject) (Request, error) {
			return Request{Argv: []string{"codex", "exec", "--other"}, Stdin: []byte(sub.Diff)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.Review(context.Background(), sub); !errors.Is(err, ErrRunMismatch) {
		t.Fatalf("a recomposed request over a dispatched run = %v, want ErrRunMismatch", err)
	}
}

func TestARefusedRunDoesNotHoldTheWorkspaceCycle(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	f.refuse = refusal()
	s, _ := newSession(t, f, remotely(f))
	first := testSubject()
	if _, err := s.Review(context.Background(), first); !errors.Is(err, ErrRunRefused) {
		t.Fatalf("Review = %v, want ErrRunRefused", err)
	}

	// A new head in the same cycle. Nothing is executing in the sandbox, so the
	// gate lets the new run in rather than refusing it as not quiet.
	f.refuse = nil
	second := first
	second.Head = "2222222222222222222222222222222222222222"
	second.Occurrence++
	report, err := s.Review(context.Background(), second)
	if err != nil {
		t.Fatalf("Review of the next head after a refusal = %v; want no ErrNotQuiet", err)
	}
	if report.Verdict != review.VerdictClean {
		t.Fatalf("verdict = %q", report.Verdict)
	}
}

// The record the incident left behind: dispatched, no backend id, replayed on
// every sweep. A daemon whose executor now answers the replay with a refusal
// settles it rather than replaying it forever.
func TestAnUnresolvedRecordWhoseReplayIsRefusedSettles(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	f.failStarts = 2 // the start and its replay both fail before creating anything
	s, store := newSession(t, f, remotely(f))
	sub := testSubject()
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunUnresolved) {
		t.Fatalf("Review = %v, want ErrRunUnresolved", err)
	}
	run, _ := sub.RunID()
	if rec, _ := store.Load(run); !rec.Dispatched || rec.BackendRunID != "" || rec.Refused() {
		t.Fatalf("record = %+v; want the incident's shape", rec)
	}

	f.refuse = refusal()
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunRefused) {
		t.Fatalf("Review once the replay is refused = %v, want ErrRunRefused", err)
	}
	rec, _ := store.Load(run)
	if !rec.Refused() || !rec.Quiet || f.created(run) != 0 {
		t.Fatalf("record after the refused replay = %+v (runs %d); want refused, quiet, no run", rec, f.created(run))
	}
	if _, err := s.Review(context.Background(), sub); errors.Is(err, ErrRunUnresolved) {
		t.Fatalf("a settled refusal went back to unresolved: %v", err)
	}
}

func TestReconcileReportsARefusedRunQuiet(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	f.refuse = refusal()
	s, _ := newSession(t, f, remotely(f))
	if _, err := s.Review(context.Background(), testSubject()); !errors.Is(err, ErrRunRefused) {
		t.Fatalf("Review = %v, want ErrRunRefused", err)
	}
	states, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || !states[0].Quiet || !states[0].Refused() || states[0].BackendRunID != "" {
		t.Fatalf("a refused run surveyed as %+v; want exactly one, quiet, refused, unbound", states)
	}
	if f.statuses != 0 {
		t.Fatalf("the survey asked the executor about a run that was never started (%d status reads)", f.statuses)
	}
}

// The remote executor restates the backend's refusal in this package's
// vocabulary and leaves every other error alone.
func TestRemoteTranslatesABackendRefusal(t *testing.T) {
	backend := remotetest.New("profile-1")
	backend.SetStartRefusal("env_rejected", "one or more environment keys are reserved or forbidden", 0)
	exec, err := NewRemote(RemoteOptions{Backend: backend, GitRepository: "acme/widgets", Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	sub := testSubject()
	run, _ := sub.RunID()
	req := Request{Argv: []string{"codex", "exec"}, Stdin: []byte(sub.Diff)}
	ref := Ref{
		Run: run, Repository: sub.Repository, Issue: sub.Issue, Cycle: sub.Cycle,
		Branch: "ben/11", BaseSHA: base1, TargetBranch: "main", Sandbox: "sandbox-1", Profile: "profile-1",
	}
	if ref.Digest, err = exec.Digest(ref, req); err != nil {
		t.Fatal(err)
	}

	_, err = exec.Start(context.Background(), ref, req)
	if !errors.Is(err, ErrRunRefused) {
		t.Fatalf("Start = %v, want ErrRunRefused", err)
	}
	got, ok := RefusalOf(err)
	if !ok || got.Reason != "env_rejected" || got.Detail != "one or more environment keys are reserved or forbidden" {
		t.Fatalf("RefusalOf = (%+v, %v)", got, ok)
	}
	if !errors.Is(err, remote.ErrProcessRefused) {
		t.Fatalf("the backend's own refusal was not retained as the cause: %v", err)
	}
	if got, ok := RefusalOf(errors.New("the control plane did not answer")); ok {
		t.Fatalf("RefusalOf read a transport error as a refusal: %+v", got)
	}
	if _, ok := RefusalOf(&RefusedError{}); ok {
		t.Fatal("RefusalOf accepted a refusal with no reason")
	}
}
