package orchestrator

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// approvedAtNoon is the approving instant every case below is measured against.
var approvedAtNoon = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// labeled builds one `labeled` event for a required label.
func labeled(label string, at time.Time) core.ClaimEvent {
	return core.ClaimEvent{Kind: core.ClaimEventLabeled, Actor: "a-labeler", Subject: label, At: at}
}

func unlabeled(label string, at time.Time) core.ClaimEvent {
	return core.ClaimEvent{Kind: core.ClaimEventUnlabeled, Actor: "a-labeler", Subject: label, At: at}
}

// SPEC §9.5's whole verdict, as a table. Each row is one shape of the two facts
// the tracker states, and the refusals outnumber the passes deliberately: the
// only ways through are a positively-stated "never edited" and an edit the
// tracker can order strictly before the approving label.
func TestCheckApprovalVerdicts(t *testing.T) {
	standing := []core.ClaimEvent{labeled("ben-queue", approvedAtNoon)}

	for _, tc := range []struct {
		name     string
		history  []core.ClaimEvent
		required []string
		edit     core.ContentEdit
		want     error
	}{
		{
			name:     "never edited passes",
			history:  standing,
			required: []string{"ben-queue"},
			edit:     core.ContentEdit{Status: core.ContentEditNever},
		},
		{
			name:     "edited before the approving label passes",
			history:  standing,
			required: []string{"ben-queue"},
			edit:     core.ContentEdit{Status: core.ContentEditAt, At: approvedAtNoon.Add(-time.Second)},
		},
		{
			name:     "edited after the approving label is drift",
			history:  standing,
			required: []string{"ben-queue"},
			edit:     core.ContentEdit{Status: core.ContentEditAt, At: approvedAtNoon.Add(time.Second)},
			want:     ErrContentDrift,
		},
		{
			// SPEC §8.4: tracker timestamps are second-granularity, so the two
			// cannot be ordered against each other. The change-log id does not
			// rescue it — that id orders events against *each other*, and a
			// content edit is not in the log at all.
			name:     "edited in the same second is unorderable",
			history:  standing,
			required: []string{"ben-queue"},
			edit:     core.ContentEdit{Status: core.ContentEditAt, At: approvedAtNoon},
			want:     ErrContentUnorderable,
		},
		{
			// BUILD.md decision 15. The zero ContentEdit is what a field nobody
			// filled and a tracker with no such capability both produce, and it
			// must not read as "never edited".
			name:     "an unknown edit time refuses",
			history:  standing,
			required: []string{"ben-queue"},
			edit:     core.ContentEdit{},
			want:     ErrContentEditUnknown,
		},
		{
			name:     "a required label the log never mentions has no approving instant",
			history:  []core.ClaimEvent{labeled("unrelated", approvedAtNoon)},
			required: []string{"ben-queue"},
			edit:     core.ContentEdit{Status: core.ContentEditNever},
			want:     ErrApprovalInstantUnknown,
		},
		{
			name:     "a label applied and since removed is not a standing approval",
			history:  []core.ClaimEvent{labeled("ben-queue", approvedAtNoon), unlabeled("ben-queue", approvedAtNoon.Add(time.Minute))},
			required: []string{"ben-queue"},
			edit:     core.ContentEdit{Status: core.ContentEditNever},
			want:     ErrApprovalInstantUnknown,
		},
		{
			// §6.7 makes applying a required label the approval act. With no
			// required label there is no act, so there is nothing to bind content
			// to — and the loader refuses an empty set for the neighbouring
			// reason (BUILD.md decision 9).
			name:     "no required labels means no approval to bind to",
			history:  standing,
			required: nil,
			edit:     core.ContentEdit{Status: core.ContentEditNever},
			want:     ErrApprovalInstantUnknown,
		},
		{
			// Missing *any* of them refuses: approval is not complete until the
			// set is.
			name:     "one required label of two is not approval",
			history:  standing,
			required: []string{"ben-queue", "ready"},
			edit:     core.ContentEdit{Status: core.ContentEditNever},
			want:     ErrApprovalInstantUnknown,
		},
		{
			// The instant is the *last* of them, so an edit between the two is
			// approved: the labeler who completed the set read this content.
			// Reject the mutation that takes the *first* label — it would call
			// this drift and park an issue a human had just approved.
			name: "an edit between two required labels is approved by the later one",
			history: []core.ClaimEvent{
				labeled("ben-queue", approvedAtNoon),
				labeled("ready", approvedAtNoon.Add(2*time.Minute)),
			},
			required: []string{"ben-queue", "ready"},
			edit:     core.ContentEdit{Status: core.ContentEditAt, At: approvedAtNoon.Add(time.Minute)},
		},
		{
			// And the same set with the edit after both: nobody has approved
			// this. Reject the mutation that takes the *earliest* standing label
			// — with the edit here it would still refuse, which is why the row
			// above is the one that catches it, and this row is what catches a
			// multi-label check that stops at the first match.
			name: "an edit after the last required label is drift",
			history: []core.ClaimEvent{
				labeled("ben-queue", approvedAtNoon),
				labeled("ready", approvedAtNoon.Add(2*time.Minute)),
			},
			required: []string{"ben-queue", "ready"},
			edit:     core.ContentEdit{Status: core.ContentEditAt, At: approvedAtNoon.Add(3 * time.Minute)},
			want:     ErrContentDrift,
		},
		{
			// §9.5's reapproval act: removing and re-applying a required label
			// moves the approving instant past the edit.
			name: "a re-applied label reapproves the edited content",
			history: []core.ClaimEvent{
				labeled("ben-queue", approvedAtNoon),
				unlabeled("ben-queue", approvedAtNoon.Add(2*time.Minute)),
				labeled("ben-queue", approvedAtNoon.Add(3*time.Minute)),
			},
			required: []string{"ben-queue"},
			edit:     core.ContentEdit{Status: core.ContentEditAt, At: approvedAtNoon.Add(time.Minute)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := core.ContentApproval{
				Content: core.IssueContent{Title: "approved title", Body: "approved body"},
				Edit:    tc.edit,
			}
			got, err := checkApproval(tc.history, tc.required, read)
			if !errors.Is(err, tc.want) {
				t.Fatalf("checkApproval = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				if got != (approvedContent{}) {
					t.Errorf("a refusal returned %+v; a refusal admits nothing, and the zero value is what cannot be pinned", got)
				}
				return
			}
			if got.at.IsZero() {
				t.Error("a pass returned no approving instant, so nothing can tell a later reapproval from this one")
			}
			if got.content != read.Content {
				t.Errorf("admitted content = %+v, want the content the check was made against", got.content)
			}
		})
	}
}

// The approving instant is the standing application of the last required label,
// asserted on its own because two of the table's rows above can only report that
// *some* refusal happened.
func TestApprovingInstantIsTheStandingLastLabel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		history  []core.ClaimEvent
		required []string
		want     time.Time
		wantOK   bool
	}{
		{
			name:     "one label",
			history:  []core.ClaimEvent{labeled("ben-queue", approvedAtNoon)},
			required: []string{"ben-queue"},
			want:     approvedAtNoon, wantOK: true,
		},
		{
			name: "the later of two, whatever order they appear in",
			history: []core.ClaimEvent{
				labeled("ready", approvedAtNoon.Add(time.Hour)),
				labeled("ben-queue", approvedAtNoon),
			},
			required: []string{"ben-queue", "ready"},
			want:     approvedAtNoon.Add(time.Hour), wantOK: true,
		},
		{
			// A re-application is a new approval, so the later event wins — not
			// the first one the log happens to carry.
			name: "the re-application, not the original",
			history: []core.ClaimEvent{
				labeled("ben-queue", approvedAtNoon),
				unlabeled("ben-queue", approvedAtNoon.Add(time.Hour)),
				labeled("ben-queue", approvedAtNoon.Add(2*time.Hour)),
			},
			required: []string{"ben-queue"},
			want:     approvedAtNoon.Add(2 * time.Hour), wantOK: true,
		},
		{
			// Labels are compared case-insensitively everywhere else in the loop
			// (routable, the §8.3 verdict); a second rule here would let an issue
			// dispatch under one spelling and fail to date its approval under the
			// other.
			name:     "case-insensitively",
			history:  []core.ClaimEvent{labeled("BEN-Queue", approvedAtNoon)},
			required: []string{"ben-queue"},
			want:     approvedAtNoon, wantOK: true,
		},
		{
			name:     "removed and not re-applied",
			history:  []core.ClaimEvent{labeled("ben-queue", approvedAtNoon), unlabeled("ben-queue", approvedAtNoon.Add(time.Hour))},
			required: []string{"ben-queue"},
		},
		{
			name:     "an empty log",
			required: []string{"ben-queue"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, ok := approvingInstant(tc.history, tc.required)
			if ok != tc.wantOK {
				t.Fatalf("approvingInstant ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !at.Equal(tc.want) {
				t.Errorf("approvingInstant = %s, want %s", at, tc.want)
			}
		})
	}
}

func TestApprovalCycleAnchorMovesOnSameAssignmentReapproval(t *testing.T) {
	approved := labeled("ben-queue", approvedAtNoon)
	approved.ID = 1
	assigned := core.ClaimEvent{
		Kind: core.ClaimEventAssigned, Actor: "a-controller", Subject: recoveryTestPrincipal,
		At: approvedAtNoon.Add(time.Minute), ID: 2,
	}
	history := []core.ClaimEvent{approved, assigned}
	original, ok := approvalCycleAnchor(history, []string{"ben-queue"})
	if !ok || original != approved.ID {
		t.Fatalf("original approval anchor = %d (ok=%v), want %d", original, ok, approved.ID)
	}
	assignment := claimCycleAnchor(history, recoveryTestPrincipal)

	removed := unlabeled("ben-queue", approvedAtNoon.Add(2*time.Minute))
	removed.ID = 3
	if anchor, ok := approvalCycleAnchor(append(history, removed), []string{"ben-queue"}); ok {
		t.Fatalf("removed approval returned anchor %d", anchor)
	}
	reapproved := labeled("ben-queue", approvedAtNoon.Add(3*time.Minute))
	reapproved.ID = 4
	history = append(history, removed, reapproved)
	current, ok := approvalCycleAnchor(history, []string{"ben-queue"})
	if !ok || current != reapproved.ID || current == original {
		t.Fatalf("reapproval anchor = %d (ok=%v), want new event %d", current, ok, reapproved.ID)
	}
	if got := claimCycleAnchor(history, recoveryTestPrincipal); got != assignment {
		t.Fatalf("assignment anchor moved from %d to %d during label-only reapproval", assignment, got)
	}
}

// Sub-second digits on one side of the comparison and not the other must not
// make an edit look safely older than the label it actually shares a second
// with (SPEC §8.4).
//
// The second row is the one that matters, and it is why the comparison truncates
// rather than comparing raw instants. Both facts arrive at whole seconds today —
// the label from the REST change log, the edit from GraphQL — so a raw
// comparison passes every fixture in this repository. The moment one transport
// starts carrying milliseconds and the other does not, a raw comparison reads
// 12:00:00.000 as strictly before 12:00:00.400 and dispatches an edit nobody can
// prove came first.
func TestSecondGranularityRefusesEitherOrdering(t *testing.T) {
	for _, tc := range []struct {
		name             string
		approved, edited time.Time
	}{
		{"the edit carries the sub-second digits", approvedAtNoon, approvedAtNoon.Add(400 * time.Millisecond)},
		{"the approving label carries them", approvedAtNoon.Add(400 * time.Millisecond), approvedAtNoon},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkApproval(
				[]core.ClaimEvent{labeled("ben-queue", tc.approved)},
				[]string{"ben-queue"},
				core.ContentApproval{Edit: core.ContentEdit{Status: core.ContentEditAt, At: tc.edited}})
			if !errors.Is(err, ErrContentUnorderable) {
				t.Errorf("checkApproval = %v, want %v: both instants fall in one second", err, ErrContentUnorderable)
			}
		})
	}
}

// A tracker that cannot answer §9.5's question has not said "never edited"; the
// absent capability is `unknown`, which refuses (SPEC §9.5's race matrix).
func TestATrackerWithoutTheContentSeamReportsUnknown(t *testing.T) {
	got, err := readApproval(t.Context(), seamlessTracker{}, core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("readApproval returned an error for an absent capability: %v", err)
	}
	if got.Edit.Status != core.ContentEditUnknown {
		t.Fatalf("edit status = %s, want unknown", got.Edit.Status)
	}
	if _, err := checkApproval([]core.ClaimEvent{labeled("ben-queue", approvedAtNoon)}, []string{"ben-queue"}, got); !errors.Is(err, ErrContentEditUnknown) {
		t.Errorf("checkApproval = %v, want %v", err, ErrContentEditUnknown)
	}
}

// seamlessTracker satisfies Tracker and deliberately not contentApprovalSource.
type seamlessTracker struct{ Tracker }

// Every field of core.Issue is classified, and adoptRouting carries exactly the
// routing ones (SPEC §9.5).
//
// Anchored at the type rather than at adoptRouting's own body, which is the
// point: a test driven by the function it checks would prove the fields it
// copies are copied and say nothing about a field nobody thought about. A new
// author-controlled member of core.Issue reaching the prompt is this ticket's
// defect returning through a different door, so the compiler cannot catch it and
// this has to.
func TestIssueFieldsAreClassified(t *testing.T) {
	// routing — the tracker's own answer, refreshed on every reconciliation
	// because §9.8's sweep rules read them.
	routing := []string{"State", "Labels", "Assignees", "Blockers", "URL", "UpdatedAt", "Revision", "Dispatchable"}
	// pinned — author-controlled (SPEC §5.6), bound to the approving instant.
	pinned := []string{"Title", "Body"}
	// identity — fixed for the life of an issue, so refreshing them is neither
	// needed nor harmful.
	identity := []string{"Identifier", "CreatedAt"}

	var declared []string
	declared = append(declared, routing...)
	declared = append(declared, pinned...)
	declared = append(declared, identity...)
	sort.Strings(declared)

	var actual []string
	for i := 0; i < reflect.TypeOf(core.Issue{}).NumField(); i++ {
		actual = append(actual, reflect.TypeOf(core.Issue{}).Field(i).Name)
	}
	sort.Strings(actual)

	if !reflect.DeepEqual(declared, actual) {
		t.Fatalf("core.Issue fields = %v, classified = %v.\nA new field must be classified routing, pinned, or identity: routing is refreshed by adoptRouting, pinned is bound to the approving label (SPEC §9.5), identity is neither.", actual, declared)
	}

	// And adoptRouting carries the routing set and nothing else. Two records
	// whose every field differs: what survives the refresh is the answer.
	r := &Record{Issue: core.Issue{
		Identifier: "1", Title: "pinned title", Body: "pinned body",
		State: "open", Labels: []string{"old"}, Assignees: []string{"old"},
		Blockers: []core.Blocker{{Identifier: "9", Open: true}}, URL: "old",
		CreatedAt: approvedAtNoon, UpdatedAt: approvedAtNoon, Revision: "old",
	}}
	fresh := core.Issue{
		Identifier: "1", Title: "edited title", Body: "edited body",
		State: "closed", Labels: []string{"new"}, Assignees: []string{"new"},
		Blockers: nil, URL: "new",
		CreatedAt: approvedAtNoon.Add(time.Hour), UpdatedAt: approvedAtNoon.Add(time.Hour),
		Revision: "new", Dispatchable: true,
	}
	r.adoptRouting(fresh)

	for _, f := range routing {
		if got, want := field(t, r.Issue, f), field(t, fresh, f); !reflect.DeepEqual(got, want) {
			t.Errorf("routing field %s = %v after adoptRouting, want the tracker's fresh %v", f, got, want)
		}
	}
	if r.Issue.Title != "pinned title" || r.Issue.Body != "pinned body" {
		t.Errorf("adoptRouting moved the pin: title %q, body %q", r.Issue.Title, r.Issue.Body)
	}
	if !r.Issue.CreatedAt.Equal(approvedAtNoon) {
		t.Errorf("adoptRouting moved CreatedAt to %s; an issue's filing time does not change", r.Issue.CreatedAt)
	}
}

func field(t *testing.T, issue core.Issue, name string) any {
	t.Helper()
	v := reflect.ValueOf(issue).FieldByName(name)
	if !v.IsValid() {
		t.Fatalf("core.Issue has no field %q", name)
	}
	return v.Interface()
}
