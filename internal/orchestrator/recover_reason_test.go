package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// SPEC §9.10 step 6's failure reason: what a recovered `failed` comment is
// allowed to say about why the previous attempt failed. The reason lives in the
// §9.11 transition log rather than in the tracker projection, so recovery has to
// read it — and every test here is about a case where the honest answer is "it
// did not survive" rather than a reason.
//
// Three of those cases are indistinguishable from carrying a reason unless they
// are kept apart deliberately: no reader configured at all, a log that could not
// be read, and a last `failed` edge that belongs to a previous claim cycle.
// Collapsing any of them publishes an invented reason, which step 6 forbids in
// the same breath as it forbids skipping the comment.
//
// Split out of recover_test.go by #160; translog_test.go is the same log's write
// path (§9.11), which is a different subject. The §9.10 fixtures these share
// with the rest of the family — `harness.restart`, `stubFailures`,
// `newFailures`, `harness.waitComment`, `lastComment` — stay in
// recover_test.go, which owns them; `datedFailure` is reason-only and moved here.

// §9.10 step 6 and its loud cap. The comment is identical in both cases, which
// is exactly why the *capability* absence has to be reported separately — here,
// by asserting that a configured reader names the reason and an absent one says
// it did not survive.
func TestARecoveredFailedCommentNamesTheReasonOrSaysItDidNot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reader  FailureReasonReader
		wantRsn core.FailureReason
		wantNA  bool
	}{
		{
			name:    "the log survived, so the reason is named",
			reader:  stubFailures{reason: core.FailureStalled, ok: true},
			wantRsn: core.FailureStalled,
		},
		{
			name:   "the log was read and does not carry it: the blessed degraded path",
			reader: stubFailures{},
			wantNA: true,
		},
		{
			name:   "no reader at all: the same comment, plus the startup warning",
			reader: nil,
			wantNA: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, harnessOpts{runGone: groupGone})
			issue := fake.Issue("1", epoch)
			h.Tracker.Set(issue)
			if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			// ben:failed standing with the claim still assigned: `failed` releases
			// the claim (§9.2), so a standing assignment means the release never
			// landed.
			if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelFailed); err != nil {
				t.Fatalf("SetStateLabels: %v", err)
			}
			pinClaimBaseForRecovery(t, h, "1")

			if err := h.restart(harnessOpts{runGone: groupGone, failures: tc.reader}); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			// §9.10 forbids skipping this comment: a ben:failed label with no
			// explanation is worse than an honest one. waitComment fails the test if it
			// never arrives, which is that requirement stated as a barrier.
			comment := h.waitComment("1", core.MilestoneFailed)
			if comment.Reason != tc.wantRsn {
				t.Errorf("reason = %q, want %q", comment.Reason, tc.wantRsn)
			}
			if comment.ReasonUnavailable != tc.wantNA {
				t.Errorf("ReasonUnavailable = %v, want %v", comment.ReasonUnavailable, tc.wantNA)
			}
			// Setting both, or neither, is a refusal (core.MilestoneComment).
			if (comment.Reason != "") == comment.ReasonUnavailable {
				t.Errorf("comment sets Reason=%q and ReasonUnavailable=%v; exactly one is allowed",
					comment.Reason, comment.ReasonUnavailable)
			}
		})
	}
}

// A transition log that cannot be *read* is not a log that carries no reason. The
// comment is identical, so collapsing the two would publish "the reason did not
// survive" on a host where it was sitting right there.
func TestAnUnreadableTransitionLogRetainsRatherThanClaimingTheReasonIsGone(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelFailed); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	pinClaimBaseForRecovery(t, h, "1")

	corrupt := errors.New("transition log is truncated")
	failures := newFailures(stubFailures{err: corrupt})
	if err := h.restart(harnessOpts{runGone: groupGone, failures: failures}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if got, posted := lastComment(h.Tracker.CommentsFor("1"), core.MilestoneFailed); posted {
		t.Errorf("posted a failed comment (%+v) from a log that could not be read", got)
	}
	if _, tracked := h.o.records["1"]; !tracked {
		t.Fatal("the candidate was dropped; one with no record is dispatchable")
	}
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times before the comment §9.10 requires could be written", got)
	}

	// The log becomes readable and the verdict lands, reason and all.
	failures.set(stubFailures{reason: core.FailureStalled, ok: true})
	h.Tick()
	got := h.waitComment("1", core.MilestoneFailed)
	if got.Reason != core.FailureStalled || got.ReasonUnavailable {
		t.Errorf("comment = %+v, want the reason named once the log could be read", got)
	}
}

// A transition log that cannot be read blocks only the verdict that needs it.
//
// Every other row — terminal cleanup, an orphan resuming, a needs-review repair —
// says nothing about a failure reason, and a corrupt state file is a bad reason to
// stop releasing merged issues.
func TestAnUnreadableTransitionLogDoesNotBlockOtherVerdicts(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)
	// Merged and closed while the daemon was down: gate 1, which never reads the log.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })

	if err := h.restart(harnessOpts{
		runGone:  groupGone,
		verifier: incompleteEvidence,
		failures: stubFailures{err: errors.New("transition log is truncated")},
	}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	h.WaitGone("1")
	if got := h.Tracker.ReleaseCount("1"); got != 1 {
		t.Errorf("released %d times, want 1: gate 1 does not consult the transition log", got)
	}
}

// §9.10 step 2: evidence means evidence dated after the claim-establishing event.
//
// The transition log has no notion of a claim cycle — state.TransitionReader says
// so and returns the timestamp for exactly this — so its last `failed` edge can
// belong to a previous tenure: an issue that failed, was re-queued by a human, ran
// again, and failed again with that second edge never persisted. Publishing the
// first reason as this failure's is inventing one, which §9.10 step 6 forbids in
// the same breath as it forbids skipping the comment.
func TestAFailureReasonFromAPreviousClaimCycleIsNotThisOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		// failedAt is when the surviving `failed` edge happened, relative to the
		// claim-establishing assignment.
		before bool
		// same puts it at exactly the anchor's timestamp.
		same    bool
		wantRsn core.FailureReason
		wantNA  bool
	}{
		{name: "a failure inside this cycle is named", wantRsn: core.FailureStalled},
		{name: "a failure from a previous cycle is not survived", before: true, wantNA: true},
		{
			// §9.10 step 2 says evidence means evidence dated *after* the
			// claim-establishing event, and equality is not that. Tracker timestamps
			// are second-granularity (§8.4), so a failure sharing a second with the
			// assignment could have happened on either side of it — and the reading
			// that calls it evidence is the one that publishes a previous cycle's
			// reason as this failure's.
			name: "a failure sharing a second with the assignment cannot be ordered against it",
			same: true, wantNA: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, harnessOpts{runGone: groupGone})
			issue := fake.Issue("1", epoch)
			h.Tracker.Set(issue)

			// The claim cycle begins now; the fixture dates the failure either side.
			// Relative to the issue's creation, because that is the floor the fake
			// stamps from: the labels it was filed with are dated at CreatedAt, and no
			// later event is stamped before them.
			cycleBegan := epoch.Add(24 * time.Hour)
			h.Tracker.SetNow(cycleBegan)
			if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelFailed); err != nil {
				t.Fatalf("SetStateLabels: %v", err)
			}
			pinClaimBaseForRecovery(t, h, "1")

			failedAt := cycleBegan.Add(time.Hour)
			switch {
			case tc.before:
				failedAt = cycleBegan.Add(-time.Hour)
			case tc.same:
				failedAt = cycleBegan
			}
			if err := h.restart(harnessOpts{
				runGone:  groupGone,
				failures: datedFailure{at: failedAt, reason: core.FailureStalled},
			}); err != nil {
				t.Fatalf("Recover: %v", err)
			}

			got := h.waitComment("1", core.MilestoneFailed)
			if got.Reason != tc.wantRsn {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantRsn)
			}
			if got.ReasonUnavailable != tc.wantNA {
				t.Errorf("ReasonUnavailable = %v, want %v — a reason from a previous cycle is not this "+
					"failure's, and §9.10 forbids inventing one", got.ReasonUnavailable, tc.wantNA)
			}
		})
	}
}

// datedFailure is a §9.10 step 6 reader whose answer carries a specific time, so a
// test can put it either side of the claim-establishing assignment.
type datedFailure struct {
	at     time.Time
	reason core.FailureReason
}

func (d datedFailure) LastFailure(string) (core.RunFailure, bool, error) {
	return core.RunFailure{At: d.at, Reason: d.reason, Detail: "the agent stopped reporting"}, true, nil
}

// An anchor with no timestamp cannot date evidence against it, and an unapplied
// rule is not a passed one.
//
// §9.10 step 2 is written in terms of dates — "evidence means evidence dated after
// that event" — so a claim-establishing assignment the tracker returned without one
// leaves the cycle check inapplicable. Reporting the reason anyway would be
// applying a rule that never ran; the honest answer is that nothing established
// this failure as current.
func TestAnUndatedClaimAnchorReportsTheReasonAsNotSurvived(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	issue.Assignees = []string{fake.DefaultPrincipal}
	h.Tracker.Set(issue)
	// An assignment with no timestamp — event retention that dropped it, or a
	// tracker that does not date this entry — followed by the failed projection.
	h.Tracker.SetHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventAssigned, Actor: fake.DefaultPrincipal, Subject: fake.DefaultPrincipal, ID: 1,
	})
	if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelFailed); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	pinClaimBaseForRecovery(t, h, "1")

	if err := h.restart(harnessOpts{
		runGone:  groupGone,
		failures: datedFailure{at: epoch, reason: core.FailureStalled},
	}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	got := h.waitComment("1", core.MilestoneFailed)
	if !got.ReasonUnavailable || got.Reason != "" {
		t.Errorf("comment = %+v, want the reason reported as not survived: nothing dated it against this "+
			"claim cycle", got)
	}
	if len(h.Logs.find("no dated anchor")) == 0 {
		t.Error("nothing said why the reason was withheld; a rule that cannot be applied has to be visible")
	}
}
