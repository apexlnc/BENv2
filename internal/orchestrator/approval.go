package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// SPEC §9.5, content-bound approval. Applying a `required_label` approves the
// issue *as it read at that moment* (§6.7), and this file is the pure half of
// making that true of the bytes an agent receives: which instant approved, and
// whether the content has moved since.
//
// Everything here is a function of facts the tracker stated. The I/O that
// gathers them is in pipeline.go; the routing of a refusal is there too. Split
// that way because the refusals are the part worth model-checking and the part a
// mutation has to be caught by — every branch below is reachable from a table
// test with no adapter, no clock, and no loop.

var (
	// ErrApprovalInstantUnknown is the change log failing to say when a
	// required label was applied. §6.7 makes the label the only approval act, so
	// a label whose application BEN cannot date is an approval BEN cannot bind
	// content to — which is a refusal, not a pass (SPEC §9.10).
	ErrApprovalInstantUnknown = errors.New("the tracker's change log does not say when a required label was applied, so there is no approving instant to bind content to (SPEC §9.5)")
	// ErrContentEditUnknown is the tracker failing to say whether the content
	// has been edited. Absence of edit evidence is not evidence of no edit.
	ErrContentEditUnknown = errors.New("the tracker cannot say when the issue's title or body was last edited (SPEC §9.5, §9.10)")
	// ErrContentDrift is an edit after the approving instant: the bytes on the
	// issue are not the bytes a trusted principal approved.
	ErrContentDrift = errors.New("the issue's title or body was edited after the approving label, so its content is not what a labeler approved (SPEC §9.5)")
	// ErrContentUnorderable is an edit BEN cannot order against the approval.
	// Tracker timestamps are second-granularity (SPEC §8.4), so an edit sharing
	// a second with the approving label may have landed on either side of it.
	// The change-log id does not rescue it: that id orders events against each
	// other, not against a content edit that is not in the log.
	ErrContentUnorderable = errors.New("the issue's title or body was edited in the same second as the approving label, so the two cannot be ordered (SPEC §9.5, §8.4)")
)

// approvalGranularity is the resolution at which two tracker timestamps can be
// ordered against each other (SPEC §8.4). Both sides of the comparison are
// truncated to it, so a transport that happens to carry sub-second digits on one
// side and not the other cannot make an edit look safely older than the label it
// actually shares a second with.
const approvalGranularity = time.Second

// approvedContent is content a passing §9.5 check admitted, with the approving
// instant it was admitted against.
//
// It is the pin's only currency, and that is the point. Its fields are
// unexported and checkApproval is the only thing that fills them, so
// `Record.pin` cannot be called by code that did not run the check — reified for
// the same reason `revisionProjection` is, because the alternative is a rule
// somebody has to keep. Every refresh site holds a freshly-read issue and could
// always have written `r.Issue.Body = fresh.Body`; what it cannot do is produce
// one of these.
//
// Mutation-tested: making the unpark re-pin is a compile error rather than a
// test failure, which is the strongest form of "no test can catch its removal"
// available here.
type approvedContent struct {
	content core.IssueContent
	at      time.Time
}

// checkApproval is §9.5's whole verdict: may this content be dispatched, and
// which instant approved it.
//
// It takes the tracker's whole content read rather than the edit fact alone,
// because the content it returns is admissible *because of* that fact — the two
// have to come from one response, and taking them apart is how they would come
// to be read at two different moments.
//
// Every refusal parks; none of them releases. The order of the two unknowns is
// not arbitrary: without an approving instant there is nothing for an edit time
// to be compared against, so that refusal comes first and names the fact that is
// actually missing.
func checkApproval(history []core.ClaimEvent, requiredLabels []string, read core.ContentApproval) (approvedContent, error) {
	approvedAt, ok := approvingInstant(history, requiredLabels)
	if !ok {
		return approvedContent{}, ErrApprovalInstantUnknown
	}
	admit := approvedContent{content: read.Content, at: approvedAt}
	switch read.Edit.Status {
	case core.ContentEditNever:
		// A stated fact, and the only one that admits content without a
		// comparison at all.
		return admit, nil
	case core.ContentEditAt:
		editedAt := read.Edit.At.Truncate(approvalGranularity)
		approvedSec := approvedAt.Truncate(approvalGranularity)
		switch {
		case editedAt.After(approvedSec):
			return approvedContent{}, fmt.Errorf("%w: edited %s, approved %s",
				ErrContentDrift, editedAt.UTC().Format(time.RFC3339), approvedSec.UTC().Format(time.RFC3339))
		case editedAt.Equal(approvedSec):
			return approvedContent{}, fmt.Errorf("%w: both at %s",
				ErrContentUnorderable, approvedSec.UTC().Format(time.RFC3339))
		default:
			return admit, nil
		}
	default:
		return approvedContent{}, ErrContentEditUnknown
	}
}

// approvingInstant is when the issue became approved: the standing `labeled`
// event of the **last** required label to be applied.
//
// The last, because approval is not complete until the set is — an earlier one
// approves an issue that was not yet dispatchable, and an edit between the two
// would then read as pre-approval. The *standing* application, because a label
// removed and re-applied is a new approval by the same act (§6.7): the later
// event is the one that approved the content as it now reads.
//
// Not-ok means no approving instant exists to bind to, which is a refusal. That
// covers an empty required set — approval by a label nobody has to apply is not
// approval — and a required label the change log never mentions.
func approvingInstant(history []core.ClaimEvent, requiredLabels []string) (time.Time, bool) {
	if len(requiredLabels) == 0 {
		return time.Time{}, false
	}
	var latest time.Time
	for _, want := range requiredLabels {
		at, ok := standingLabeledAt(history, want)
		if !ok {
			return time.Time{}, false
		}
		if at.After(latest) {
			latest = at
		}
	}
	return latest, true
}

// StandingApproval is approvingInstant's other half: the tracker-native *id* of
// the event that completed the current application of the required-label set, or
// zero when the set is not completely applied.
//
// The id rather than the timestamp, and that is the whole reason it is a second
// function. This one answers "which approval is this", not "when was it": two
// approvals a second apart must be distinguishable, and a second-granularity
// timestamp cannot say they differ (SPEC §8.4). It is what a workspace cycle is
// anchored to on a substrate whose sandbox outlives its claim (SPEC §6.7), which
// makes a change in it the statement that one cycle ended and another began — the
// remove-and-reapply that current labels alone cannot show (#252).
//
// Exported because the v2 assembly resolves the same anchor for the workspace
// strategy (cmd/ben's trackerCycles). Two spellings of "which approval selects
// the sandbox" would be two answers, and the one that disagreed would dispose
// somebody else's tree.
func StandingApproval(history []core.ClaimEvent, requiredLabels []string) int64 {
	if len(requiredLabels) == 0 {
		// Approval by a label nobody has to apply is not approval. The loader
		// refuses an empty required set, so this is a restatement rather than a
		// reachable branch — and the safe one, since the alternative reading is
		// "every issue is approved by the same nonexistent event".
		return 0
	}
	var anchor int64
	for _, want := range requiredLabels {
		id, ok := standingLabeledID(history, want)
		if !ok {
			return 0
		}
		if id > anchor {
			anchor = id
		}
	}
	return anchor
}

// standingLabeledID is standingLabeledAt by id: the event that began one label's
// *current* application, if it is applied at all.
func standingLabeledID(history []core.ClaimEvent, label string) (int64, bool) {
	var id int64
	var applied bool
	for _, ev := range history {
		if !strings.EqualFold(ev.Subject, label) {
			continue
		}
		switch ev.Kind {
		case core.ClaimEventLabeled:
			id, applied = ev.ID, true
		case core.ClaimEventUnlabeled:
			id, applied = 0, false
		}
	}
	if !applied || id <= 0 {
		return 0, false
	}
	return id, true
}

// standingLabeledAt replays the change log for one label and reports when its
// current application began, if it is applied at all.
//
// A replay rather than a search for the last `labeled` event: a label applied,
// removed, and never re-applied has a `labeled` event in the log and no standing
// approval, and taking that event's timestamp would date an approval that was
// withdrawn.
func standingLabeledAt(history []core.ClaimEvent, label string) (time.Time, bool) {
	event, ok := standingLabeledEvent(history, label)
	return event.At, ok
}

// approvalCycleAnchor is the tracker-native identity of the standing approval,
// as distinct from approvingInstant's timestamp. A remove-and-reapply can keep
// the same assignment and content while selecting a new remote workspace cycle;
// the event id is what makes those two approvals different even within one
// second. The last required label to be applied selects the cycle, exactly as it
// selects the approving instant.
func approvalCycleAnchor(history []core.ClaimEvent, requiredLabels []string) (int64, bool) {
	if len(requiredLabels) == 0 {
		return 0, false
	}
	var anchor int64
	for _, want := range requiredLabels {
		event, ok := standingLabeledEvent(history, want)
		if !ok || event.ID <= 0 {
			return 0, false
		}
		if event.ID > anchor {
			anchor = event.ID
		}
	}
	return anchor, true
}

func standingLabeledEvent(history []core.ClaimEvent, label string) (core.ClaimEvent, bool) {
	var event core.ClaimEvent
	var applied bool
	for _, ev := range history {
		if !strings.EqualFold(ev.Subject, label) {
			continue
		}
		switch ev.Kind {
		case core.ClaimEventLabeled:
			event, applied = ev, true
		case core.ClaimEventUnlabeled:
			event, applied = core.ClaimEvent{}, false
		}
	}
	return event, applied
}

// readApproval asks the tracker for §9.5's facts, or reports that it cannot.
//
// A tracker that does not implement the seam has not said "never edited"; it has
// said nothing. The zero core.ContentApproval carries ContentEditUnknown, which
// checkApproval refuses — so an adapter with no such capability parks the issue
// rather than passing it (SPEC §9.5's race matrix, BUILD.md decision 15). That
// is deliberately not an error: an error is a read that failed and will be
// retried, and this one never succeeds.
func readApproval(ctx context.Context, tracker Tracker, issue core.Issue) (core.ContentApproval, error) {
	src, ok := tracker.(contentApprovalSource)
	if !ok {
		return core.ContentApproval{}, nil
	}
	return src.ContentApproval(ctx, issue)
}
