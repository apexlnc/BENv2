package reviewrun

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

// One review, one run, one verdict — the ordinary case, on both substrates.
func TestOneSubjectDrivesExactlyOneRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote bool
	}{
		{name: "local"},
		{name: "airlock", remote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeExec(envelope(`{"verdict":"changes_requested","findings":"line 3"}`))
			var opts []func(*Options)
			if tc.remote {
				opts = append(opts, remotely(f))
			}
			s, _ := newSession(t, f, opts...)
			sub := testSubject()

			report, err := s.Review(context.Background(), sub)
			if err != nil {
				t.Fatalf("Review: %v", err)
			}
			if report.Verdict != review.VerdictChangesRequested || report.Findings != "line 3" {
				t.Fatalf("report = %+v", report)
			}

			run, _ := sub.RunID()
			if got := f.created(run); got != 1 {
				t.Fatalf("the subject created %d runs, want exactly 1", got)
			}
			// A second sweep before the route completes must resolve the stated
			// verdict rather than dispatch again — the "terminal run with no
			// published review" row of the recovery table.
			again, err := s.Review(context.Background(), sub)
			if err != nil {
				t.Fatalf("resumed Review: %v", err)
			}
			if again != report {
				t.Fatalf("the resumed report = %+v, want the recorded %+v", again, report)
			}
			if f.starts != 1 || f.attaches != 0 {
				t.Fatalf("resuming a terminal run cost %d starts and %d attaches; want the executor untouched",
					f.starts, f.attaches)
			}
		})
	}
}

// A lost start response is resolved by replaying the same idempotency address.
// One underlying invocation, no second run, no invented outcome.
func TestALostStartResponseIsReplayedNotDuplicated(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	f.loseStarts = 1
	s, store := newSession(t, f, remotely(f))
	sub := testSubject()

	report, err := s.Review(context.Background(), sub)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if report.Verdict != review.VerdictClean {
		t.Fatalf("verdict = %q", report.Verdict)
	}
	run, _ := sub.RunID()
	if got := f.created(run); got != 1 {
		t.Fatalf("a lost response produced %d runs, want 1", got)
	}
	if f.starts != 2 {
		t.Fatalf("the lost response was resolved with %d starts, want the original plus one replay", f.starts)
	}
	rec, err := store.Load(run)
	if err != nil {
		t.Fatal(err)
	}
	if rec.BackendRunID != "backend-"+run {
		t.Fatalf("the replayed response was not retained: %+v", rec)
	}
}

// A daemon restart between the dispatch mark and the response resumes from the
// durable record: same address, same request, one run.
func TestABenRestartResumesTheSameRun(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	f.loseStarts = 1
	s, store := newSession(t, f, remotely(f))
	sub := testSubject()

	// The first process loses the response and cannot replay it either.
	f.failStarts = 1
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunUnresolved) {
		t.Fatalf("an unresolvable dispatch = %v, want ErrRunUnresolved", err)
	}
	run, _ := sub.RunID()
	rec, err := store.Load(run)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Dispatched || rec.BackendRunID != "" {
		t.Fatalf("record after an unresolved dispatch = %+v; want dispatched with no backend id", rec)
	}

	// A second process, over the same store, resolves it.
	next, err := New(Options{
		Executor: f, Store: store, Sandbox: placementOf(f), Logger: testLogger(t),
		Compose: func(sub Subject) (Request, error) {
			return Request{Argv: []string{"codex", "exec"}, Stdin: []byte(sub.Diff)}, nil
		},
		Sleep: boundedSleep(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := next.Review(context.Background(), sub)
	if err != nil {
		t.Fatalf("resumed Review: %v", err)
	}
	if report.Verdict != review.VerdictClean {
		t.Fatalf("verdict = %q", report.Verdict)
	}
	if got := f.created(run); got != 1 {
		t.Fatalf("a restart produced %d runs, want 1", got)
	}
}

// An Airlock API restart between the response and the events reattaches by run
// id and committed cursor, with no gap after deduplication.
func TestAnAirlockRestartReattachesByRunIDAndCursor(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"changes_requested","findings":"the second half"}`))
	f.chunk = 8
	f.sealAfter = 3 // three Events calls before the stream seals
	s, store := newSession(t, f, remotely(f))
	sub := testSubject()

	// The first pass runs out of poll budget with the stream still open.
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunIncomplete) {
		t.Fatalf("an unsealed run = %v, want ErrRunIncomplete", err)
	}
	run, _ := sub.RunID()
	partial, err := store.Load(run)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Cursor == 0 {
		t.Fatal("nothing was committed from an open stream")
	}
	if partial.Verdict != "" {
		t.Fatalf("an open stream stated a verdict: %+v", partial)
	}

	// Airlock restarts: the attach fails once, then the whole stream replays
	// from sequence 1 while the record's cursor is already past it.
	f.failAttaches = 1
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunUnresolved) {
		t.Fatalf("an unavailable attach = %v, want ErrRunUnresolved", err)
	}
	report, err := s.Review(context.Background(), sub)
	if err != nil {
		t.Fatalf("reattached Review: %v", err)
	}
	if report.Verdict != review.VerdictChangesRequested || !strings.Contains(report.Findings, "second half") {
		t.Fatalf("report = %+v", report)
	}
	if got := f.created(run); got != 1 {
		t.Fatalf("the reattach produced %d runs, want 1", got)
	}
	final, err := store.Load(run)
	if err != nil {
		t.Fatal(err)
	}
	// Replayed bytes were deduplicated rather than concatenated twice.
	if len(final.Output) != len(f.output) {
		t.Fatalf("admitted %d bytes for a %d byte stream; replay was not deduplicated",
			len(final.Output), len(f.output))
	}
}

// Fail closed on the things that make an answer untrustworthy. None of them is
// a verdict, and in particular none of them is `clean`.
func TestAmbiguityAuthorizesNoVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arrange runs *after* the cycle's placement has been captured, so it
		// moves the world under a run that is already pinned — which is the only
		// shape drift can actually take.
		arrange func(*fakeExec)
		want    error
	}{
		{
			name:    "an event gap",
			arrange: func(f *fakeExec) { f.gapAt = 2 },
			want:    ErrEventGap,
		},
		{
			name:    "the backend dropped output bytes",
			arrange: func(f *fakeExec) { f.truncateAt = 2 },
			want:    ErrOutputTruncated,
		},
		{
			name: "a replayed sequence carrying different bytes",
			arrange: func(f *fakeExec) {
				f.chunk = 8
				f.sealAfter = 2
				f.conflictAt = 1
			},
			want: ErrEventConflict,
		},
		{
			name:    "a profile revision that moved under the run",
			arrange: func(f *fakeExec) { f.profile = "profile-2" },
			want:    ErrProfileDrift,
		},
		{
			// The backend reports the run in a sandbox the cycle did not select:
			// the cross-cycle attach a revocation-and-reapproval must never make.
			name:    "a sandbox that is not this cycle's",
			arrange: func(f *fakeExec) { f.sandbox = "somebody-elses" },
			want:    ErrSandboxMismatch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeExec(envelope(`{"verdict":"clean"}`))
			pinned := Placement{Branch: "ben/11", BaseSHA: base1, TargetBranch: "main", Sandbox: f.sandbox, Profile: f.profile}
			tc.arrange(f)
			s, _ := newSession(t, f, func(o *Options) {
				o.Sandbox = func(context.Context, Subject) (Placement, error) { return pinned, nil }
			})
			report, err := s.Review(context.Background(), testSubject())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Review = (%+v, %v), want %v", report, err, tc.want)
			}
			if report.Verdict != "" {
				t.Fatalf("a refusal produced verdict %q", report.Verdict)
			}
		})
	}
}

// A sealed stream is complete output, not permission to reuse the sandbox.
// The verdict remains unusable until the backend separately attests that the
// whole execution domain is quiet.
func TestASealedStreamWaitsForDomainQuietBeforeReturningAVerdict(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"changes_requested","findings":"revise"}`))
	f.domainActive = true
	s, store := newSession(t, f, remotely(f))
	sub := testSubject()

	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunIncomplete) {
		t.Fatalf("Review while descendants remain active = %v, want ErrRunIncomplete", err)
	}
	run, _ := sub.RunID()
	rec, err := store.Load(run)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Sealed || rec.Quiet || rec.Verdict != "" {
		t.Fatalf("sealed but active record = %+v; no verdict may be usable yet", rec)
	}

	f.domainActive = false
	report, err := s.Review(context.Background(), sub)
	if err != nil {
		t.Fatalf("Review after domain quiet: %v", err)
	}
	if report.Verdict != review.VerdictChangesRequested {
		t.Fatalf("verdict = %q", report.Verdict)
	}
}

func TestOnlyStdoutCanBecomeReviewerOutput(t *testing.T) {
	controlVerdict := []byte(envelope(`{"verdict":"clean"}`))
	stderrVerdict := []byte(envelope(`{"verdict":"clean"}`))
	modelVerdict := []byte(envelope(`{"verdict":"changes_requested","findings":"real output"}`))
	rec := Record{Run: "review-control-test"}
	next, err := admit(rec, []Chunk{
		{Seq: 1, Stream: ChunkControl, Payload: controlVerdict},
		{Seq: 2, Stream: ChunkStderr, Payload: stderrVerdict},
		{Seq: 3, Stream: ChunkStdout, Payload: modelVerdict},
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Cursor != 3 || len(next.Admitted) != 3 {
		t.Fatalf("non-stdout events did not advance the durable cursor: %+v", next)
	}
	report, err := ExtractVerdict(next.Output)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != review.VerdictChangesRequested || report.Findings != "real output" {
		t.Fatalf("control or stderr payload entered the verdict text: %+v", report)
	}

	stderrOnly, err := admit(Record{Run: "review-stderr-only"}, []Chunk{
		{Seq: 1, Stream: ChunkStderr, Payload: stderrVerdict},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractVerdict(stderrOnly.Output); !errors.Is(err, ErrNoVerdictBlock) {
		t.Fatalf("stderr-only verdict = %v, want ErrNoVerdictBlock", err)
	}
}

// Process-local Start idempotence cannot resolve a durable mark written by a
// daemon that crashed inside synchronous Local.Start. Replaying it would launch
// a second child while the first may still exist.
func TestARestartNeverReplaysAnAmbiguousLocalStart(t *testing.T) {
	local, err := NewLocal(LocalOptions{Timeout: time.Minute, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	s, store := newSession(t, local, func(o *Options) {
		o.Compose = func(Subject) (Request, error) { return Request{Argv: helperArgv()}, nil }
	})
	sub := testSubject()
	run, _ := sub.RunID()
	subject, _ := sub.SubjectDigest()
	req, err := s.compose(sub)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := local.Digest(Ref{Run: run, Repository: sub.Repository, Issue: sub.Issue, Cycle: sub.Cycle}, req)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.open(context.Background(), sub, run, subject, digest, Placement{})
	if err != nil {
		t.Fatal(err)
	}
	rec.Dispatched = true
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunUnresolved) {
		t.Fatalf("Review after an ambiguous local start = %v, want ErrRunUnresolved", err)
	}
	local.mu.Lock()
	started := len(local.runs)
	local.mu.Unlock()
	if started != 0 {
		t.Fatalf("the restarted session launched %d replacement local reviewers", started)
	}
}

// A malformed or missing verdict is sealed and remembered as *silence*: the
// record must never be able to answer a later sweep with something it did not
// receive.
func TestAMalformedVerdictIsNeverCached(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   error
	}{
		{name: "nothing at all", output: "I had a look and it seems fine", want: ErrNoVerdictBlock},
		{name: "two envelopes", output: envelope(`{"verdict":"clean"}`) + envelope(`{"verdict":"clean"}`), want: ErrAmbiguousVerdict},
		{name: "a third word", output: envelope(`{"verdict":"looks-good"}`), want: review.ErrUnknownVerdict},
		{name: "a decorative extra key", output: envelope(`{"verdict":"clean","approve":true}`), want: review.ErrUnknownVerdict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeExec(tc.output)
			s, store := newSession(t, f, remotely(f))
			sub := testSubject()
			if _, err := s.Review(context.Background(), sub); !errors.Is(err, tc.want) {
				t.Fatalf("Review = %v, want %v", err, tc.want)
			}
			run, _ := sub.RunID()
			rec, err := store.Load(run)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Verdict != "" || rec.Terminal() {
				t.Fatalf("silence was cached as a verdict: %+v", rec)
			}
			// And a second sweep still refuses rather than resolving.
			if _, err := s.Review(context.Background(), sub); !errors.Is(err, tc.want) {
				t.Fatalf("resumed Review = %v, want %v", err, tc.want)
			}
		})
	}
}

// A new run in a workspace cycle waits for the previous one's execution domain
// to be positively observed quiet.
func TestANewRunWaitsForDomainQuiet(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"changes_requested"}`))
	f.sealAfter = 99 // never seals during this test
	s, store := newSession(t, f, remotely(f))

	first := testSubject()
	if _, err := s.Review(context.Background(), first); !errors.Is(err, ErrRunIncomplete) {
		t.Fatalf("an open run = %v, want ErrRunIncomplete", err)
	}

	// A new occurrence in the same approval cycle: a different address, the same
	// sandbox, and the previous run still live.
	second := testSubject()
	second.Occurrence = 801
	second.Head = "2222222222222222222222222222222222222222"
	if _, err := s.Review(context.Background(), second); !errors.Is(err, ErrNotQuiet) {
		t.Fatalf("dispatching into a live sandbox = %v, want ErrNotQuiet", err)
	}
	secondRun, _ := second.RunID()
	if _, err := store.Load(secondRun); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("a refused dispatch left a record: %v", err)
	}

	// Once the domain is quiet the previous run's execution record is retired
	// and the new one may start.
	f.sealAfter = 0
	if _, err := s.Review(context.Background(), second); err != nil {
		t.Fatalf("Review after quiet: %v", err)
	}
	firstRun, _ := first.RunID()
	if _, err := store.Load(firstRun); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("the finished run's execution record survived: %v", err)
	}
}

// The three runs of one approval cycle live in one sandbox at three addresses.
// A coding attempt, the review of what it produced, and the revision that
// follows must not contend for one idempotency key.
func TestDistinctRunIdentitiesInOneSandbox(t *testing.T) {
	base := testSubject()

	// The same approval, a later claim epoch, a new occurrence and a moved head:
	// the revision round's review.
	revision := base
	revision.Claim = 901
	revision.Occurrence = 801
	revision.Head = "3333333333333333333333333333333333333333"

	first, err := base.RunID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := revision.RunID()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two rounds of one approval share a run identity")
	}
	if base.CycleAddress() != revision.CycleAddress() {
		t.Fatalf("the rounds landed in different sandboxes: %q and %q", base.CycleAddress(), revision.CycleAddress())
	}
	// The role is in the identity, so a coding dispatch at the same cycle cannot
	// resolve this address.
	if !strings.HasPrefix(first, "review-") {
		t.Fatalf("the review run identity %q does not name its role", first)
	}
}

func TestTargetAndReviewerProfileSeparateRuns(t *testing.T) {
	baseline := testSubject()
	baselineRun, err := baseline.RunID()
	if err != nil {
		t.Fatal(err)
	}
	if baselineRun != "review-fb8b016b7a83481f167a38140ce81007" {
		t.Fatalf("baseline run identity changed to %q", baselineRun)
	}

	retargeted := baseline
	retargeted.TargetBranch = "release/v2"
	retargetedRun, _ := retargeted.RunID()
	if retargetedRun == baselineRun {
		t.Fatalf("targets main and release/v2 share run identity %s", baselineRun)
	}

	deep := baseline
	deep.ReviewerProfile = "deep"
	fast := baseline
	fast.ReviewerProfile = "fast"
	deepRun, _ := deep.RunID()
	fastRun, _ := fast.RunID()
	if deepRun == baselineRun || fastRun == baselineRun || deepRun == fastRun {
		t.Fatalf("profile run identities collide: baseline=%s deep=%s fast=%s", baselineRun, deepRun, fastRun)
	}
	deepSubject, _ := deep.SubjectDigest()
	fastSubject, _ := fast.SubjectDigest()
	if deepSubject == fastSubject {
		t.Fatal("two reviewer profiles share a whole-subject digest")
	}
}

func TestVersionOneExecutionRecordLoadsAsLegacyProfile(t *testing.T) {
	dir := NewDirStore(t.TempDir())
	rec := Record{
		Version: RecordVersion, Run: "review-legacy", Cycle: "acme/ben#11@700", Role: Role,
		Repository: "acme/ben", Issue: "11", Approval: 700, Occurrence: 800, Claim: 900,
		PR: 42, Base: base1, Head: head1, Digest: "sha256:request", Subject: "sha256:subject",
	}
	if err := dir.Save(rec); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(dir.Path(rec.Run))
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte(`"version": 2`), []byte(`"version": 1`), 1)
	if err := os.WriteFile(dir.Path(rec.Run), body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := dir.Load(rec.Run)
	if err != nil {
		t.Fatalf("Load v1 record: %v", err)
	}
	if got.Version != RecordVersion || got.ReviewerProfile != "" {
		t.Fatalf("migrated record = %+v, want version %d and legacy empty profile", got, RecordVersion)
	}
}

// Revocation and a later human reapproval are a new workspace cycle. Its runs
// are addressed elsewhere, so attaching the previous cycle's sandbox is not
// something this package can express.
func TestReapprovalCannotAttachThePreviousCycle(t *testing.T) {
	before := testSubject()
	after := testSubject()
	after.Cycle = 701 // a fresh approval-label event

	if before.CycleAddress() == after.CycleAddress() {
		t.Fatal("a reapproval reused the previous workspace-cycle address")
	}
	beforeRun, _ := before.RunID()
	afterRun, _ := after.RunID()
	if beforeRun == afterRun {
		t.Fatal("a reapproval reused the previous cycle's run identity")
	}

	// And the durable record refuses it a second way, so the property does not
	// rest on the derivation alone: a record naming the retained sandbox cannot
	// answer once the strategy reports the new cycle's own.
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	retained := f.sandbox
	s, _ := newSession(t, f, func(o *Options) {
		o.Sandbox = func(context.Context, Subject) (Placement, error) {
			return Placement{Branch: "ben/11", BaseSHA: base1, TargetBranch: "main", Sandbox: retained, Profile: f.profile}, nil
		}
	})
	if _, err := s.Review(context.Background(), after); err != nil {
		t.Fatal(err)
	}
	retained = "sandbox-2" // the sandbox the reapproved cycle actually selects
	f.sandbox = retained
	if _, err := s.Review(context.Background(), after); !errors.Is(err, ErrSandboxMismatch) {
		t.Fatalf("attaching a retained sandbox across cycles = %v, want ErrSandboxMismatch", err)
	}
}

// A subject that has moved is a different run, and the old record refuses to
// answer for it.
func TestAMovedSubjectIsADifferentRun(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	s, store := newSession(t, f, remotely(f))
	sub := testSubject()
	if _, err := s.Review(context.Background(), sub); err != nil {
		t.Fatal(err)
	}

	moved := sub
	moved.Head = "4444444444444444444444444444444444444444"
	movedRun, _ := moved.RunID()
	original, _ := sub.RunID()
	if movedRun == original {
		t.Fatal("a moved head reused the run identity")
	}

	// The same address with different bytes behind it is a refusal, not a resume.
	rec, err := store.Load(original)
	if err != nil {
		t.Fatal(err)
	}
	rec.Subject = "sha256:something-else"
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunMismatch) {
		t.Fatalf("a record judging another subject = %v, want ErrRunMismatch", err)
	}
}

// Nothing incompletely named ever reaches an executor.
func TestAnIncompleteSubjectIsRefusedBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Subject)
	}{
		{name: "no workspace cycle", mutate: func(s *Subject) { s.Cycle = 0 }},
		{name: "no occurrence", mutate: func(s *Subject) { s.Occurrence = 0 }},
		{name: "no claim epoch", mutate: func(s *Subject) { s.Claim = 0 }},
		{name: "an abbreviated head", mutate: func(s *Subject) { s.Head = head1[:12] }},
		{name: "no base", mutate: func(s *Subject) { s.Base = "" }},
		{name: "no repository", mutate: func(s *Subject) { s.Repository = "" }},
		{name: "invalid reviewer profile", mutate: func(s *Subject) { s.ReviewerProfile = "Deep!" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeExec(envelope(`{"verdict":"clean"}`))
			s, _ := newSession(t, f, remotely(f))
			sub := testSubject()
			tc.mutate(&sub)
			if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrSubject) {
				t.Fatalf("Review = %v, want ErrSubject", err)
			}
			if f.starts != 0 {
				t.Fatalf("an incomplete subject reached the executor %d times", f.starts)
			}
		})
	}
}

// Retire is refused while the run may still be executing, for the reason a
// journal is never closed over a live run: a record removed there is a run
// nothing can attach to.
func TestRetireRefusesALiveRun(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	f.sealAfter = 99
	s, store := newSession(t, f, remotely(f))
	sub := testSubject()
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunIncomplete) {
		t.Fatalf("an open run = %v", err)
	}
	if err := s.Retire(context.Background(), sub); !errors.Is(err, ErrNotQuiet) {
		t.Fatalf("Retire over a live run = %v, want ErrNotQuiet", err)
	}

	f.sealAfter = 0
	if _, err := s.Review(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if err := s.Retire(context.Background(), sub); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	run, _ := sub.RunID()
	if _, err := store.Load(run); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("Retire left the record behind: %v", err)
	}
}

// Startup reconciliation refreshes the one fact a restart cannot reconstruct,
// and reports rather than repairs the rest.
func TestReconcileRefreshesQuietBeforeDispatch(t *testing.T) {
	f := newFakeExec(envelope(`{"verdict":"clean"}`))
	f.sealAfter = 99
	s, _ := newSession(t, f, remotely(f))
	sub := testSubject()
	if _, err := s.Review(context.Background(), sub); !errors.Is(err, ErrRunIncomplete) {
		t.Fatalf("an open run = %v", err)
	}

	states, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Quiet {
		t.Fatalf("a live run surveyed as %+v; want exactly one, not quiet", states)
	}

	f.sealAfter = 0
	states, err = s.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || !states[0].Quiet {
		t.Fatalf("a finished run surveyed as %+v; want quiet", states)
	}
}

func placementOf(f *fakeExec) SandboxSource {
	return func(context.Context, Subject) (Placement, error) {
		return Placement{Branch: "ben/11", BaseSHA: base1, TargetBranch: "main", Sandbox: f.sandbox, Profile: f.profile}, nil
	}
}
