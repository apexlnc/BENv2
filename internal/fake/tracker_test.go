package fake_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

var epoch = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// A fake that invents a guarantee the real adapter does not make is worse than a
// missing test (AGENTS.md). These are the three §9.5 guarantees the GitHub
// adapter makes, read off its own behavior.

// An issue's labels imply `labeled` events. On GitHub a label cannot stand
// without one, and §9.5's approving instant is read off exactly those — so a
// fake that installed labels silently would answer "no approving instant" for
// every fixture and park the whole suite.
func TestInstalledLabelsCarryTheirLabeledEvents(t *testing.T) {
	tr := fake.NewTracker(fake.Issue("1", epoch))

	events, err := tr.ClaimHistory(t.Context(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Kind == core.ClaimEventLabeled && ev.Subject == "ben-queue" {
			found = true
			if !ev.At.Equal(epoch) {
				t.Errorf("labeled event at %s, want the issue's creation time %s", ev.At, epoch)
			}
		}
	}
	if !found {
		t.Errorf("change log = %+v, want a labeled event for the required label", events)
	}
}

// SetHistory scripts what happened before the daemon looked; it does not script
// away the labels the issue carries. Replacing the whole log would model a world
// GitHub cannot produce — an issue carrying `ben-queue` with no record of it
// having been applied.
func TestSetHistoryKeepsTheLabeledEvents(t *testing.T) {
	tr := fake.NewTracker(fake.Issue("1", epoch))
	tr.SetHistory("1", core.ClaimEvent{Kind: core.ClaimEventClosed, ID: 20, At: epoch.Add(time.Hour)})

	events, err := tr.ClaimHistory(t.Context(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != core.ClaimEventLabeled || events[1].Kind != core.ClaimEventClosed {
		t.Fatalf("change log = %+v, want the implied labeled event followed by the scripted close", events)
	}
}

// The default edit fact is a positive "never edited", not the zero value.
//
// The distinction is the whole of BUILD.md decision 15 as it applies here: the
// zero core.ContentEdit is `unknown`, which §9.5 refuses. A fake that left it
// zero would park every fixture; a fake that reported `never` for a tracker that
// genuinely cannot answer would let unapproved content through. Both facts are
// reachable, and Edit moves the second.
func TestContentApprovalStatesTheEditFact(t *testing.T) {
	tr := fake.NewTracker(fake.Issue("1", epoch))

	got, err := tr.ContentApproval(t.Context(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Edit.Status != core.ContentEditNever {
		t.Errorf("a freshly installed issue reports %s, want a stated never-edited", got.Edit.Status)
	}
	if got.Content.Title != "Ticket 1" || got.Content.Body != "do the thing" {
		t.Errorf("content = %+v, want the issue's own title and body", got.Content)
	}

	// One act, both halves: the content moves and the fact dates it. Anything
	// else models a tracker that cannot exist.
	at := epoch.Add(time.Hour)
	tr.Edit("1", at, func(c *core.IssueContent) { c.Body = "do something else" })

	got, err = tr.ContentApproval(t.Context(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Edit.Status != core.ContentEditAt || !got.Edit.At.Equal(at) {
		t.Errorf("edit = %+v, want it dated at %s", got.Edit, at)
	}
	if got.Content.Body != "do something else" {
		t.Errorf("body = %q, want the edited content", got.Content.Body)
	}
}

// Every call that names an issue refuses when the tracker no longer has it,
// because the adapter does: each is a 404 from GitHub. And each refuses in the
// shape the adapter refuses in — classified for the three reads that ask about
// one issue by identifier, unclassified for the writes (#134,
// core.ErrIssueNotFound).
//
// Both halves are asserted, and the absent half is the load-bearing one. A fake
// that promised the classification on a write would let a caller key on it,
// pass here, and then classify nothing in production; a fake that withheld it on
// `ClaimHistory` would let a caller that never notices a deleted issue pass,
// which is exactly what #49's half-fix did. So the column is stated per row
// rather than derived from anything the fake itself does.
//
// Release is the row that mattered most, and the writes beside it are the reason
// fixing Release alone was not enough. An owed write that errors is retried at
// the head of the record's queue forever, so a label or comment left standing
// when an issue is deleted blocks the disposal and the forget behind it just as
// surely as a release does — and a fake that answered "written" for an issue it
// does not have hid that too.
func TestEveryCallRefusesForAnIssueTheTrackerDoesNotHave(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*fake.Tracker) error
		// classifies is whether this call's refusal carries
		// core.ErrIssueNotFound, per the adapter it stands in for.
		classifies bool
	}{
		// Idempotent while the issue exists — the adapter's RemoveAssignees is a
		// no-op for an assignee that is not there, and AddLabelsToIssue for a label
		// already on — so each row's refusal is about absence, not repetition.
		{name: "SetStateLabels", call: func(tr *fake.Tracker) error {
			return tr.SetStateLabels(t.Context(), core.Issue{Identifier: "1"}, core.StateLabelClaimed)
		}},
		{name: "Comment", call: func(tr *fake.Tracker) error {
			// A claim and its projection first, because a milestone comment is
			// anchored on the label transition that occasioned it (SPEC §8.4): the
			// adapter refuses one it cannot place in a claim cycle, and so does the
			// fake. Without this the write would not land on a *present* issue
			// either, and the case below could not tell absence from that refusal.
			if _, err := tr.Claim(t.Context(), core.Issue{Identifier: "1"}); err != nil {
				return err
			}
			if err := tr.SetStateLabels(t.Context(), core.Issue{Identifier: "1"},
				core.StateLabelClaimed); err != nil {
				return err
			}
			return tr.Comment(t.Context(), core.Issue{Identifier: "1"},
				core.MilestoneComment{Milestone: core.MilestoneClaimed})
		}},
		{name: "Release", call: func(tr *fake.Tracker) error {
			return tr.Release(t.Context(), core.Issue{Identifier: "1"})
		}},
		{name: "Claim", call: func(tr *fake.Tracker) error {
			// The write this fake used to answer `true, nil` for — a *verified
			// claim* on an issue that does not exist. Fetch answers from a list
			// the tracker may already have moved past, so an issue deleted
			// between the candidate read and the claim is a race the loop can
			// really lose, and a fake that verified the claim anyway hid whatever
			// the loop then did with the record.
			_, err := tr.Claim(t.Context(), core.Issue{Identifier: "1"})
			return err
		}},
		{name: "Get", classifies: true, call: func(tr *fake.Tracker) error {
			_, err := tr.Get(t.Context(), "1")
			return err
		}},
		{name: "ClaimHistory", classifies: true, call: func(tr *fake.Tracker) error {
			// The read a claimed record reaches first, and the one whose silence
			// held a vanished issue's claim and its §9.5 slot forever (#49). Its
			// endpoint names one issue, so the 404 is about that issue.
			_, err := tr.ClaimHistory(t.Context(), core.Issue{Identifier: "1"})
			return err
		}},
		{name: "ContentApproval", classifies: true, call: func(tr *fake.Tracker) error {
			_, err := tr.ContentApproval(t.Context(), core.Issue{Identifier: "1"})
			return err
		}},
		// FindPR is deliberately absent. It names a *branch* — the adapter asks
		// GitHub for open pull requests with that head — so a deleted issue is not
		// a refusal there at all: the answer is whatever the branch has, and
		// absence of a PR is an answer rather than an error (github FindPR).
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := fake.NewTracker(fake.Issue("1", epoch))
			if err := tc.call(tr); err != nil {
				t.Fatalf("%s on a present issue = %v, want it to land", tc.name, err)
			}
			tr.Delete("1")
			err := tc.call(tr)
			if err == nil {
				t.Fatalf("%s on a deleted issue succeeded", tc.name)
			}
			if got := errors.Is(err, core.ErrIssueNotFound); got != tc.classifies {
				if tc.classifies {
					t.Errorf("%s on a deleted issue = %v, which does not carry core.ErrIssueNotFound; this read classifies absence so the loop can forget the record", tc.name, err)
				} else {
					t.Errorf("%s on a deleted issue = %v, which carries core.ErrIssueNotFound; the adapter's writes do not classify a 404", tc.name, err)
				}
			}
		})
	}
}

// The table above covers the outcome a confirming `Get` reaches by *failing*.
// This is the rest of what §9.8's confirmation branches on, and the half a
// not-found table cannot state: a read that succeeds still answers "is this claim
// ours?", and it answers it from the assignee list alone (SPEC §8.3 — the
// assignment *is* the claim).
//
// #135 turns four rules on these four answers — forget, forget, retain, retain —
// so a fake that could not produce all four would test one branch four times. Each
// is produced the way a tracker produces it, never by scripting the answer:
// `Release` is what removes the principal, `ClaimBy` is what a human retaking the
// issue looks like, and the failure knobs are the two shapes a read failure comes
// in (one tracker-wide, one per issue — the second is what makes a rotation over a
// candidate set observable at all).
func TestGetStatesEveryOutcomeAHeldConfirmationBranchesOn(t *testing.T) {
	const me = fake.DefaultPrincipal
	flaky := errors.New("502 from the tracker")

	for _, tc := range []struct {
		name    string
		arrange func(*testing.T, *fake.Tracker)
		// wantErr is what the read fails with, if it fails; wantMine is whether
		// the principal is still among the assignees when it does not.
		wantErr  error
		wantMine bool
	}{
		{
			name:     "the claim is still ours",
			arrange:  func(*testing.T, *fake.Tracker) {},
			wantMine: true,
		},
		{
			name: "the principal was released",
			arrange: func(t *testing.T, tr *fake.Tracker) {
				if err := tr.Release(t.Context(), core.Issue{Identifier: "1"}); err != nil {
					t.Fatalf("Release: %v", err)
				}
			},
		},
		{
			// A human unassigning BEN and taking the issue. Not the same as the
			// row above to a reader of the change log, and not the same to §9.8:
			// there is an assignee, just not ours.
			name: "someone else took the assignment",
			arrange: func(_ *testing.T, tr *fake.Tracker) {
				tr.Mutate("1", func(i *core.Issue) { i.Assignees = nil })
				tr.ClaimBy("1", "a-human")
			},
		},
		{
			name:    "the read cannot be made at all",
			arrange: func(_ *testing.T, tr *fake.Tracker) { tr.SetFailGet(flaky) },
			wantErr: flaky,
		},
		{
			name: "the read cannot be made for this issue",
			arrange: func(_ *testing.T, tr *fake.Tracker) {
				tr.FailGetFor = func(identifier string) error {
					if identifier == "1" {
						return flaky
					}
					return nil
				}
			},
			wantErr: flaky,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := fake.NewTracker(fake.Issue("1", epoch))
			if _, err := tr.Claim(t.Context(), core.Issue{Identifier: "1"}); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			tc.arrange(t, tr)

			got, err := tr.Get(t.Context(), "1")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Get = (%+v, %v), want %v", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if mine := slices.Contains(got.Assignees, me); mine != tc.wantMine {
				t.Errorf("assigned to %s = %t, want %t (assignees %v)", me, mine, tc.wantMine, got.Assignees)
			}
		})
	}
}

// `Claim` owes a second answer beyond "it failed", and it is the one the caller
// acts on: whether an assignment may be standing.
//
// core.ErrClaimNotAttempted is the promise that nothing was written, and the
// adapter may carry it only where that holds. It does not hold here. The 404
// arrives from the assignment request itself, so the adapter's own reading is
// that a write was attempted and cannot be accounted for — it unwinds with a
// release and, when that release fails too, returns the joined error (github
// Claim). A fake that promised more would let a caller skip the unwinding
// release and pass, leaving assigned-with-no-state-label in production, which
// §9.10 step 3 reads as published-awaiting-review and never revisits.
func TestClaimingAnAbsentIssueDoesNotPromiseNothingWasWritten(t *testing.T) {
	tr := fake.NewTracker(fake.Issue("1", epoch))
	tr.Delete("1")

	verified, err := tr.Claim(t.Context(), core.Issue{Identifier: "1"})
	if verified {
		t.Fatal("Claim verified a claim on an issue the tracker does not have")
	}
	if err == nil {
		t.Fatal("Claim on a deleted issue returned false with no error, which is the adapter saying it released what it wrote")
	}
	if errors.Is(err, core.ErrClaimNotAttempted) {
		t.Errorf("Claim error = %v, which promises no assignment was written; the adapter's 404 comes from the assignment itself", err)
	}
}

// §8.4's claim is a sequence — refuse, write, read back — and each stage can
// only report what the stages before it reached. So the fixtures that script
// those stages have an order, and a fixture asked alone cannot show it: each of
// these rows passes on its own whichever way round the fake checks them. The
// crossing is the test.
//
// The read-back is the fixture that has to lose both crossings, because it is
// the one that cannot have happened. `false, nil` is the adapter stating it
// released whatever it wrote (SPEC §8.4), so a caller may forget the record with
// no release owed — right for a lost race, and fail-open for anything that never
// reached a read-back at all: an issue that is gone strands its claim, and a
// pre-write refusal reports capacity it never spent.
func TestAClaimAnswersFromTheEarliestStageThatRefuses(t *testing.T) {
	notAttempted := fmt.Errorf("%w: per-tick GitHub request budget spent", core.ErrClaimNotAttempted)

	for _, tc := range []struct {
		name string
		// setup crosses the read-back fixture with an earlier stage's.
		setup func(*fake.Tracker)
		// want is the error the earlier stage answers with; nil means "some
		// refusal that is not one of the sentinels" (the write's 404).
		want error
	}{
		{
			// The write's 404, which is where a deletion race between the
			// candidate read and the claim surfaces. It precedes the read-back
			// because there is no 201 to read back from.
			name:  "an issue that is gone outranks a lost read-back",
			setup: func(tr *fake.Tracker) { tr.Delete("1") },
		},
		{
			// A refusal the adapter reached before any request left the process
			// (a spent budget, a standing rate-limit refusal). It precedes both.
			name:  "a refusal before the write outranks a lost read-back",
			setup: func(tr *fake.Tracker) { tr.SetClaimError(notAttempted) },
			want:  core.ErrClaimNotAttempted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := fake.NewTracker(fake.Issue("1", epoch))
			tr.ClaimVerified = func(string) bool { return false }

			// While nothing else is scripted this is the read-back's answer, which
			// is what makes each row below a crossing rather than a restatement.
			if verified, err := tr.Claim(t.Context(), core.Issue{Identifier: "1"}); verified || err != nil {
				t.Fatalf("Claim = (%v, %v) for a claim that did not stick, want (false, nil)", verified, err)
			}

			tc.setup(tr)
			verified, err := tr.Claim(t.Context(), core.Issue{Identifier: "1"})
			if verified {
				t.Fatal("Claim verified a claim the fixture refuses")
			}
			if err == nil {
				t.Fatal("Claim answered (false, nil) — the read-back's answer, for a claim that never reached a read-back")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Claim error = %v, want %v from the stage that refused first", err, tc.want)
			}
			if tc.want == nil && errors.Is(err, core.ErrClaimNotAttempted) {
				t.Errorf("Claim error = %v, which promises no assignment was written; the 404 comes from the assignment itself", err)
			}
			if errors.Is(err, core.ErrIssueNotFound) {
				t.Errorf("Claim error = %v; a write's 404 does not classify absence (#134)", err)
			}
		})
	}
}

// Compile-time proof the fake satisfies the seam the orchestrator discovers on
// the tracker, and the one the adapter publishes.
var (
	_ core.ContentApprovalSource = (*fake.Tracker)(nil)
)
