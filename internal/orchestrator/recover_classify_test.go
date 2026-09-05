package orchestrator

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

const recoveryTestPrincipal = "ben-bot"

var recoveryTestTime = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

func TestRecoveryClaimBaseGateIsClosedAndFailClosed(t *testing.T) {
	const (
		anchor              = int64(41)
		claimBaseStateCount = 4 // independent boundary: unknown, absent, pending, pinned
	)
	markers := []struct {
		name string
		mark core.RunMarker
		err  error
	}{
		{name: "absent", mark: core.RunMarker{State: core.RunMarkerAbsent}},
		{name: "identified", mark: core.RunMarker{State: core.RunMarkerIdentified}},
		{name: "unknown launch", mark: core.RunMarker{State: core.RunMarkerUnknownLaunch}},
		{name: "unreadable", mark: core.RunMarker{State: core.RunMarkerUnreadable}, err: errors.New("marker unreadable")},
		{name: "unset without error", mark: core.RunMarker{State: core.RunMarkerUnreadable}},
	}

	seen := map[core.ClaimBaseState]bool{}
	for raw := 0; raw < claimBaseStateCount; raw++ {
		stateKind := core.ClaimBaseState(raw)
		seen[stateKind] = true
		state := core.ClaimBase{State: stateKind}
		switch stateKind {
		case core.ClaimBasePending:
			state.Epoch = anchor
		case core.ClaimBasePinned:
			state.Epoch, state.BaseSHA, state.TargetBranch = anchor, "base", "main"
		}

		for _, marker := range markers {
			t.Run(stateKind.String()+"/"+marker.name, func(t *testing.T) {
				got, resolved := classifyRecoveryClaimBase(anchor, state, nil, marker.mark, marker.err, false)
				switch stateKind {
				case core.ClaimBasePinned:
					if resolved {
						t.Fatalf("matching pinned state resolved at epoch gate: %+v", got)
					}
				case core.ClaimBasePending:
					switch marker.name {
					case "absent":
						if !resolved || got.action != recoveryActionApprove || got.epochFault {
							t.Fatalf("clean pending state = (%+v, %v), want approval resume", got, resolved)
						}
						return
					case "unreadable", "unset without error":
						if !resolved || got.action != recoveryActionBlocked || got.epochFault || got.project {
							t.Fatalf("pending with unreadable marker = (%+v, %v), want inert retry", got, resolved)
						}
						return
					}
					if !resolved || got.action != recoveryActionPark || !got.epochFault {
						t.Fatalf("pending with marker evidence = (%+v, %v), want epoch-fault park", got, resolved)
					}
				default:
					if !resolved || got.action != recoveryActionPark || !got.epochFault {
						t.Fatalf("non-authorizing state = (%+v, %v), want epoch-fault park", got, resolved)
					}
				}
			})
		}
	}

	for _, state := range []core.ClaimBaseState{
		core.ClaimBaseUnknown, core.ClaimBaseAbsent, core.ClaimBasePending, core.ClaimBasePinned,
	} {
		if !seen[state] {
			t.Errorf("closed claim-base state %s is outside the independent numeric boundary", state)
		}
	}
	if next := core.ClaimBaseState(claimBaseStateCount); next.String() != fmt.Sprintf("ClaimBaseState(%d)", claimBaseStateCount) {
		t.Errorf("claim-base enum gained a state at %d; extend the independent recovery matrix", claimBaseStateCount)
	}
}

func TestRecoveryClaimBaseGateRejectsMismatchAndHistoricalRunningEvidence(t *testing.T) {
	const anchor = int64(41)
	absent := core.RunMarker{State: core.RunMarkerAbsent}
	for _, tc := range []struct {
		name  string
		state core.ClaimBase
		err   error
	}{
		{name: "pending another epoch", state: core.ClaimBase{State: core.ClaimBasePending, Epoch: anchor - 1}},
		{name: "pinned another epoch", state: core.ClaimBase{State: core.ClaimBasePinned, Epoch: anchor - 1, BaseSHA: "base", TargetBranch: "main"}},
		{name: "pinned without base", state: core.ClaimBase{State: core.ClaimBasePinned, Epoch: anchor}},
		{name: "pinned without target", state: core.ClaimBase{State: core.ClaimBasePinned, Epoch: anchor, BaseSHA: "base"}},
		{name: "read failed", err: errors.New("bad record")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, resolved := classifyRecoveryClaimBase(anchor, tc.state, tc.err, absent, nil, false)
			if !resolved || !got.epochFault || got.action != recoveryActionPark {
				t.Fatalf("verdict = (%+v, %v), want epoch-fault park", got, resolved)
			}
		})
	}

	pending := core.ClaimBase{State: core.ClaimBasePending, Epoch: anchor}
	got, resolved := classifyRecoveryClaimBase(anchor, pending, nil, absent, nil, true)
	if !resolved || !got.epochFault || got.action != recoveryActionPark {
		t.Fatalf("pending plus historical running = (%+v, %v), want epoch-fault park", got, resolved)
	}
}

func TestClaimAuthorityRequiresAnExactEpochBaseTargetTuple(t *testing.T) {
	const epoch = int64(41)
	state := core.ClaimBase{
		State: core.ClaimBasePinned, Epoch: epoch, BaseSHA: "base", TargetBranch: "release/v2",
	}
	workspace := core.Workspace{
		ClaimEpoch: epoch, BaseSHA: "base", TargetBranch: "release/v2",
	}
	if !claimBasePinsEpoch(state, epoch) || !claimBaseAuthorizesWorkspace(state, workspace) {
		t.Fatal("the complete matching tuple did not authorize")
	}

	for _, tc := range []struct {
		name      string
		mutatePin func(*core.ClaimBase)
		mutateWS  func(*core.Workspace)
	}{
		{name: "pin missing target", mutatePin: func(v *core.ClaimBase) { v.TargetBranch = "" }},
		{name: "workspace missing target", mutateWS: func(v *core.Workspace) { v.TargetBranch = "" }},
		{name: "target mismatch", mutateWS: func(v *core.Workspace) { v.TargetBranch = "main" }},
		{name: "base mismatch", mutateWS: func(v *core.Workspace) { v.BaseSHA = "other" }},
		{name: "epoch mismatch", mutateWS: func(v *core.Workspace) { v.ClaimEpoch++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotState, gotWorkspace := state, workspace
			if tc.mutatePin != nil {
				tc.mutatePin(&gotState)
			}
			if tc.mutateWS != nil {
				tc.mutateWS(&gotWorkspace)
			}
			if claimBaseAuthorizesWorkspace(gotState, gotWorkspace) {
				t.Fatalf("mismatched authority was accepted: state=%+v workspace=%+v", gotState, gotWorkspace)
			}
		})
	}
}

func TestCurrentCycleRunningEvidenceSurvivesLabelRemoval(t *testing.T) {
	events := []core.ClaimEvent{
		recoveryEvent(5, core.ClaimEventLabeled, "ben:running"),
		recoveryEvent(10, core.ClaimEventAssigned, recoveryTestPrincipal),
		recoveryEvent(11, core.ClaimEventLabeled, "BEN:RUNNING"),
		recoveryEvent(12, core.ClaimEventUnlabeled, "ben:running"),
	}
	if !currentCycleRunningEvidence(events, 10) {
		t.Fatal("a removed current-cycle running label was forgotten")
	}
	if currentCycleRunningEvidence(events[:2], 10) {
		t.Fatal("running evidence from a prior cycle was admitted")
	}

	// ID is only the tie-breaker in ClaimHistory's (At, ID) order. A later
	// timestamp may carry a smaller provider ID and is still current-cycle
	// evidence because the normalized slice order is authoritative.
	nonMonotonicIDs := []core.ClaimEvent{
		{Kind: core.ClaimEventAssigned, Subject: recoveryTestPrincipal, At: recoveryTestTime, ID: 100},
		{Kind: core.ClaimEventLabeled, Subject: "ben:running", At: recoveryTestTime.Add(time.Second), ID: 1},
	}
	if !currentCycleRunningEvidence(nonMonotonicIDs, 100) {
		t.Fatal("ordered running evidence with a smaller ID was ignored")
	}
}

func TestRecoveryProjectionTable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		events   []core.ClaimEvent
		evidence Verdict
		want     recoveryVerdict
	}{
		{
			name: "failed standing completes failed and releases",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:running"),
				recoveryEvent(11, core.ClaimEventLabeled, "ben:failed"),
			),
			evidence: VerdictIncomplete,
			want: recoveryVerdict{
				action: recoveryActionReleaseKeep, project: true,
				stateLabel: core.StateLabelFailed, milestone: core.MilestoneFailed,
				cycleAnchor: 1,
			},
		},
		{
			name: "needs-review standing adopts parked",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:running"),
				recoveryEvent(11, core.ClaimEventLabeled, "ben:needs-review"),
			),
			evidence: VerdictPublished,
			want: recoveryVerdict{
				action: recoveryActionPark, project: true,
				stateLabel: core.StateLabelNeedsReview, milestone: core.MilestoneNeedsReview,
				cycleAnchor: 1,
			},
		},
		{
			name: "active standing with complete evidence finishes done",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:claimed"),
			),
			evidence: VerdictPublished,
			want: recoveryVerdict{
				action: recoveryActionHold, project: true,
				stateLabel: core.StateLabelNone, milestone: core.MilestonePublished,
				cycleAnchor: 1,
			},
		},
		{
			name: "active standing with incomplete evidence is an orphan",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:running"),
			),
			evidence: VerdictIncomplete,
			want: recoveryVerdict{
				action: recoveryActionBackoff, project: true,
				stateLabel: core.StateLabelClaimed, milestone: core.MilestoneClaimed,
				attemptFloor: 2, cycleAnchor: 1,
			},
		},
		{
			// An unprojected claim is adopted as an *unapproved* one: no label,
			// no milestone, attempt 1. That window is where §9.5's content check
			// sits, so announcing the claim here would announce a claim that may
			// be about to park for reapproval — and dispatching from here would
			// run content nobody established was approved.
			name:     "no projection in the cycle adopts an unapproved claim at attempt one",
			events:   recoveryHistory(),
			evidence: VerdictPublished,
			want: recoveryVerdict{
				action: recoveryActionApprove, attemptFloor: 1, cycleAnchor: 1,
			},
		},
		{
			name: "removed needs-review is a human re-queue",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:needs-review"),
				recoveryEvent(11, core.ClaimEventUnlabeled, "ben:needs-review"),
			),
			evidence: VerdictPublished,
			want: recoveryVerdict{
				action: recoveryActionBackoff, project: true,
				stateLabel: core.StateLabelClaimed, milestone: core.MilestoneClaimed,
				attemptFloor: 2, cycleAnchor: 1,
			},
		},
		{
			name: "removed failed means its release never landed",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:failed"),
				recoveryEvent(11, core.ClaimEventUnlabeled, "ben:failed"),
			),
			evidence: VerdictPublished,
			want:     recoveryVerdict{action: recoveryActionReleaseKeep, cycleAnchor: 1},
		},
		{
			name: "removed active with complete evidence is done",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:running"),
				recoveryEvent(11, core.ClaimEventUnlabeled, "ben:running"),
			),
			evidence: VerdictPublished,
			want: recoveryVerdict{
				action: recoveryActionHold, project: true,
				stateLabel: core.StateLabelNone, milestone: core.MilestonePublished,
				cycleAnchor: 1,
			},
		},
		{
			name: "removed active with less evidence is a contradiction",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:claimed"),
				recoveryEvent(11, core.ClaimEventUnlabeled, "ben:claimed"),
			),
			evidence: VerdictContradicted,
			want: recoveryVerdict{
				action: recoveryActionPark, project: true,
				stateLabel: core.StateLabelNeedsReview, milestone: core.MilestoneNeedsReview,
				cycleAnchor: 1,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRecovery(recoveryCandidateFor(recoveryTestPrincipal), tc.events, tc.evidence,
				recoveryMarkerFree, recoveryTestPrincipal)
			if got != tc.want {
				t.Errorf("verdict = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRecoveryAnythingLessThanCompleteEvidenceTakesTheConservativeRow(t *testing.T) {
	for _, evidence := range []Verdict{VerdictIncomplete, VerdictContradicted} {
		t.Run(evidence.String(), func(t *testing.T) {
			standing := recoveryHistory(recoveryEvent(10, core.ClaimEventLabeled, "ben:claimed"))
			if got := classifyRecovery(recoveryCandidateFor(recoveryTestPrincipal), standing, evidence,
				recoveryMarkerFree, recoveryTestPrincipal); got.action != recoveryActionBackoff {
				t.Errorf("standing active projection = action %v, want backoff", got.action)
			}

			cleared := recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:claimed"),
				recoveryEvent(11, core.ClaimEventUnlabeled, "ben:claimed"),
			)
			if got := classifyRecovery(recoveryCandidateFor(recoveryTestPrincipal), cleared, evidence,
				recoveryMarkerFree, recoveryTestPrincipal); got.action != recoveryActionPark {
				t.Errorf("cleared active projection = action %v, want needs-review", got.action)
			}
		})
	}
}

func TestRecoveryGatesPrecedeTheProjectionTable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate recoveryCandidate
		events    []core.ClaimEvent
		want      recoveryVerdict
	}{
		{
			name: "gate 1 terminal current state precedes every other gate",
			candidate: recoveryCandidate{
				issue:  core.Issue{Assignees: []string{recoveryTestPrincipal, "other"}},
				active: false, inPartition: false,
			},
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventLabeled, "ben:needs-review"),
			},
			want: recoveryVerdict{action: recoveryActionReleaseDispose},
		},
		{
			name: "gate 1 close in this cycle precedes a lost arbitration",
			candidate: recoveryCandidate{
				issue:  core.Issue{Assignees: []string{"other", recoveryTestPrincipal}},
				active: true, inPartition: true,
			},
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, "other"),
				recoveryEvent(2, core.ClaimEventAssigned, recoveryTestPrincipal),
				recoveryEvent(3, core.ClaimEventClosed, ""),
				recoveryEvent(4, core.ClaimEventReopened, ""),
			},
			want: recoveryVerdict{action: recoveryActionReleaseDispose, cycleAnchor: 2},
		},
		{
			name: "gate 2 another party first releases only ours",
			candidate: recoveryCandidate{
				issue:  core.Issue{Assignees: []string{"other", recoveryTestPrincipal}},
				active: true, inPartition: false,
			},
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, "other"),
				recoveryEvent(2, core.ClaimEventAssigned, recoveryTestPrincipal),
			},
			want: recoveryVerdict{action: recoveryActionReleaseKeep, cycleAnchor: 2},
		},
		{
			name: "gate 2 ours first continues into the table",
			candidate: recoveryCandidate{
				issue:  core.Issue{Assignees: []string{recoveryTestPrincipal, "other"}},
				active: true, inPartition: true,
			},
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, recoveryTestPrincipal),
				recoveryEvent(2, core.ClaimEventAssigned, "other"),
				recoveryEvent(3, core.ClaimEventLabeled, "ben:needs-review"),
			},
			want: recoveryVerdict{
				action: recoveryActionPark, project: true,
				stateLabel: core.StateLabelNeedsReview, milestone: core.MilestoneNeedsReview,
				cycleAnchor: 1,
			},
		},
		{
			name: "gate 2 unorderable fails closed before gate 4",
			candidate: recoveryCandidate{
				issue:  core.Issue{Assignees: []string{recoveryTestPrincipal, "never-in-log"}},
				active: true, inPartition: false,
			},
			events: recoveryHistory(),
			want: recoveryVerdict{
				action: recoveryActionPark, project: true,
				stateLabel: core.StateLabelNeedsReview, milestone: core.MilestoneNeedsReview,
				cycleAnchor: 1, operatorErr: true,
			},
		},
		{
			name: "gate 3 missing assignment fails closed before gate 4",
			candidate: recoveryCandidate{
				issue:  core.Issue{Assignees: []string{recoveryTestPrincipal}},
				active: true, inPartition: false,
			},
			events: nil,
			want: recoveryVerdict{
				action: recoveryActionPark, project: true,
				stateLabel: core.StateLabelNeedsReview, milestone: core.MilestoneNeedsReview,
				operatorErr: true,
			},
		},
		{
			name:      "gate 4 outside the partition precedes the table",
			candidate: recoveryCandidateFor(recoveryTestPrincipal),
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:needs-review"),
			),
			want: recoveryVerdict{action: recoveryActionReleaseKeep, cycleAnchor: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "gate 4 outside the partition precedes the table" {
				tc.candidate.inPartition = false
			}
			got := classifyRecovery(tc.candidate, tc.events, VerdictPublished,
				recoveryMarkerFree, recoveryTestPrincipal)
			if got != tc.want {
				t.Errorf("verdict = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRecoveryReadsOrderedEventsNotTheCurrentLabelSet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []core.ClaimEvent
		labels []string
		want   recoveryAction
	}{
		{
			name: "failed added after running is effective",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:running"),
				recoveryEvent(11, core.ClaimEventLabeled, "ben:failed"),
			),
			labels: []string{"ben:running"},
			want:   recoveryActionReleaseKeep,
		},
		{
			name: "running added after failed is effective",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:failed"),
				recoveryEvent(11, core.ClaimEventLabeled, "BEN:RUNNING"),
			),
			labels: []string{"ben:failed"},
			want:   recoveryActionBackoff,
		},
		{
			name: "needs-review added after running is effective",
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:running"),
				recoveryEvent(11, core.ClaimEventLabeled, "ben:needs-review"),
			),
			labels: []string{"ben:running"},
			want:   recoveryActionPark,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := recoveryCandidateFor(recoveryTestPrincipal)
			candidate.issue.Labels = tc.labels // deliberately contradict the ordered log
			got := classifyRecovery(candidate, tc.events, VerdictIncomplete,
				recoveryMarkerFree, recoveryTestPrincipal)
			if got.action != tc.want {
				t.Errorf("action = %v, want %v; current labels were read instead of event order", got.action, tc.want)
			}
		})
	}
}

func TestRecoveryScopesProjectionEvidenceToTheCurrentClaimCycle(t *testing.T) {
	events := []core.ClaimEvent{
		recoveryEvent(1, core.ClaimEventAssigned, recoveryTestPrincipal),
		recoveryEvent(2, core.ClaimEventLabeled, "ben:needs-review"),
		recoveryEvent(3, core.ClaimEventUnlabeled, "ben:needs-review"),
		recoveryEvent(4, core.ClaimEventUnassigned, recoveryTestPrincipal),
		recoveryEvent(5, core.ClaimEventAssigned, recoveryTestPrincipal),
	}
	got := classifyRecovery(recoveryCandidateFor(recoveryTestPrincipal), events, VerdictPublished,
		recoveryMarkerFree, recoveryTestPrincipal)
	if got.action != recoveryActionApprove || got.attemptFloor != 1 {
		t.Fatalf("verdict = %+v, want a fresh unprojected claim at attempt 1", got)
	}
}

func TestRecoveryRemovedNeedsReviewOutranksAStalePublishedPR(t *testing.T) {
	events := recoveryHistory(
		recoveryEvent(10, core.ClaimEventLabeled, "ben:needs-review"),
		recoveryEvent(11, core.ClaimEventUnlabeled, "ben:needs-review"),
	)
	got := classifyRecovery(recoveryCandidateFor(recoveryTestPrincipal), events, VerdictPublished,
		recoveryMarkerFree, recoveryTestPrincipal)
	if got.action != recoveryActionBackoff || got.attemptFloor != 2 {
		t.Fatalf("verdict = %+v, want human re-queue into backoff", got)
	}
}

func TestRecoveryArbitrationUsesStandingAssignmentOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []core.ClaimEvent
		want   recoveryAction
	}{
		{
			name: "same-second event id makes ours the winner",
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, recoveryTestPrincipal),
				recoveryEvent(2, core.ClaimEventAssigned, "other"),
			},
			want: recoveryActionApprove,
		},
		{
			name: "same-second event id makes the other party the winner",
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, "other"),
				recoveryEvent(2, core.ClaimEventAssigned, recoveryTestPrincipal),
			},
			want: recoveryActionReleaseKeep,
		},
		{
			name: "a withdrawn party is skipped even in a stale candidate",
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, "other"),
				recoveryEvent(2, core.ClaimEventAssigned, recoveryTestPrincipal),
				recoveryEvent(3, core.ClaimEventUnassigned, "other"),
			},
			want: recoveryActionApprove,
		},
		{
			name: "duplicate assignment keeps the race tenure but moves the claim anchor",
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, recoveryTestPrincipal),
				recoveryEvent(2, core.ClaimEventAssigned, "other"),
				recoveryEvent(3, core.ClaimEventAssigned, recoveryTestPrincipal),
			},
			want: recoveryActionApprove,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := recoveryCandidateFor(recoveryTestPrincipal)
			candidate.issue.Assignees = []string{recoveryTestPrincipal, "other"}
			got := classifyRecovery(candidate, tc.events, VerdictIncomplete,
				recoveryMarkerFree, recoveryTestPrincipal)
			if got.action != tc.want {
				t.Errorf("action = %v, want %v", got.action, tc.want)
			}
			if tc.name == "duplicate assignment keeps the race tenure but moves the claim anchor" && got.cycleAnchor != 3 {
				t.Errorf("cycle anchor = %d, want most recent assignment 3", got.cycleAnchor)
			}
		})
	}
}

func TestRecoveryArbitrationTieDefenseIgnoresAssigneeSliceOrder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		events    []core.ClaimEvent
		assignees [][]string
		want      recoveryArbitration
	}{
		{
			name: "a later duplicate order does not obscure the unique minimum",
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, recoveryTestPrincipal),
				recoveryEvent(2, core.ClaimEventAssigned, "later-a"),
				recoveryEvent(2, core.ClaimEventAssigned, "later-b"),
			},
			assignees: [][]string{
				{"later-a", "later-b", recoveryTestPrincipal},
				{recoveryTestPrincipal, "later-b", "later-a"},
				{recoveryTestPrincipal, recoveryTestPrincipal, "later-a", "later-b"},
			},
			want: recoveryArbitrationOurs,
		},
		{
			name: "a duplicate minimum is unorderable in either slice order",
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, recoveryTestPrincipal),
				recoveryEvent(1, core.ClaimEventAssigned, "other"),
				recoveryEvent(2, core.ClaimEventAssigned, "later"),
			},
			assignees: [][]string{
				{recoveryTestPrincipal, "other", "later"},
				{"later", "other", recoveryTestPrincipal},
			},
			want: recoveryArbitrationUnorderable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, assignees := range tc.assignees {
				if got := arbitrateRecovery(tc.events, assignees, recoveryTestPrincipal); got != tc.want {
					t.Errorf("assignees %v: arbitration = %v, want %v", assignees, got, tc.want)
				}
			}
		})
	}
}

func TestRecoveryMarkerPreconditionPreservesTrackerOnlyRepairs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate recoveryCandidate
		events    []core.ClaimEvent
		evidence  Verdict
	}{
		{
			name:      "failed projection",
			candidate: recoveryCandidateFor(recoveryTestPrincipal),
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:running"),
				recoveryEvent(11, core.ClaimEventLabeled, "ben:failed"),
			),
			evidence: VerdictIncomplete,
		},
		{
			name:      "done projection",
			candidate: recoveryCandidateFor(recoveryTestPrincipal),
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:running"),
			),
			evidence: VerdictPublished,
		},
		{
			name:      "orphan projection",
			candidate: recoveryCandidateFor(recoveryTestPrincipal),
			events: recoveryHistory(
				recoveryEvent(10, core.ClaimEventLabeled, "ben:running"),
			),
			evidence: VerdictIncomplete,
		},
		{
			name: "terminal gate",
			candidate: recoveryCandidate{
				issue:  core.Issue{Assignees: []string{recoveryTestPrincipal}},
				active: false, inPartition: true,
			},
			events: recoveryHistory(), evidence: VerdictPublished,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			free := classifyRecovery(tc.candidate, tc.events, tc.evidence,
				recoveryMarkerFree, recoveryTestPrincipal)
			live := classifyRecovery(tc.candidate, tc.events, tc.evidence,
				recoveryMarkerPossiblyLive, recoveryTestPrincipal)
			if live.action != recoveryActionWait {
				t.Fatalf("possibly-live action = %v, want wait", live.action)
			}
			live.action = free.action
			if live != free {
				t.Errorf("possibly-live verdict lost tracker repair:\n got  %+v\n want %+v except for action", live, free)
			}
		})
	}
}

func TestRecoveryUnknownLaunchParksAndAnUnresolvedMarkerAuthorizesNothing(t *testing.T) {
	candidate := recoveryCandidate{
		issue:  core.Issue{Assignees: []string{recoveryTestPrincipal}},
		active: false, inPartition: false,
	}

	unknownLaunch := classifyRecovery(candidate, nil, VerdictUnknown,
		recoveryMarkerUnknownLaunch, recoveryTestPrincipal)
	want := recoveryVerdict{
		action: recoveryActionPark, project: true,
		stateLabel: core.StateLabelNeedsReview, milestone: core.MilestoneNeedsReview,
	}
	if unknownLaunch != want {
		t.Errorf("unknown-launch verdict = %+v, want %+v", unknownLaunch, want)
	}

	for _, marker := range []recoveryMarkerState{recoveryMarkerUnresolved, recoveryMarkerState(99)} {
		got := classifyRecovery(candidate, nil, VerdictPublished, marker, recoveryTestPrincipal)
		if got.action != recoveryActionBlocked || got.project {
			t.Errorf("marker %d verdict = %+v, want blocked with no tracker writes", marker, got)
		}
	}
}

func TestRecoveryUnknownEvidenceIsNotIncompleteEvidence(t *testing.T) {
	active := recoveryHistory(recoveryEvent(10, core.ClaimEventLabeled, "ben:claimed"))
	for _, marker := range []recoveryMarkerState{recoveryMarkerFree, recoveryMarkerPossiblyLive} {
		got := classifyRecovery(recoveryCandidateFor(recoveryTestPrincipal), active, VerdictUnknown,
			marker, recoveryTestPrincipal)
		if got.action != recoveryActionBlocked || got.project {
			t.Errorf("marker %d unknown evidence verdict = %+v, want blocked with no inferred projection", marker, got)
		}
	}

	// Rows that do not ask about evidence still classify: the evidence read is
	// not allowed to reorder the gates or make ben:failed ambiguous.
	failed := recoveryHistory(recoveryEvent(10, core.ClaimEventLabeled, "ben:failed"))
	if got := classifyRecovery(recoveryCandidateFor(recoveryTestPrincipal), failed, VerdictUnknown,
		recoveryMarkerFree, recoveryTestPrincipal); got.action != recoveryActionReleaseKeep {
		t.Errorf("failed projection action = %v, want release", got.action)
	}
}

// The tracker adapter independently pins the emitted spellings in
// github.TestStateLabelName. This side pins the four literal names recovery
// accepts, so neither half can drift while a declaration-driven test follows it.
func TestRecoveryStateLabelSpellingIsIndependentlyAnchored(t *testing.T) {
	for _, tc := range []struct {
		name string
		want core.StateLabel
	}{
		{"ben:claimed", core.StateLabelClaimed},
		{"ben:running", core.StateLabelRunning},
		{"ben:needs-review", core.StateLabelNeedsReview},
		{"ben:failed", core.StateLabelFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := recoveryStateLabel(tc.name)
			if !known || got != tc.want {
				t.Errorf("recoveryStateLabel(%q) = (%q, %v), want (%q, true)", tc.name, got, known, tc.want)
			}
		})
	}
}

func TestRecoveryUnknownStateLabelFailsClosed(t *testing.T) {
	for _, events := range [][]core.ClaimEvent{
		recoveryHistory(recoveryEvent(10, core.ClaimEventLabeled, "ben:future-state")),
		recoveryHistory(
			recoveryEvent(10, core.ClaimEventLabeled, "ben:future-state"),
			recoveryEvent(11, core.ClaimEventUnlabeled, "ben:future-state"),
		),
	} {
		got := classifyRecovery(recoveryCandidateFor(recoveryTestPrincipal), events, VerdictPublished,
			recoveryMarkerFree, recoveryTestPrincipal)
		if got.action != recoveryActionPark || !got.operatorErr {
			t.Errorf("unknown state projection = %+v, want loud needs-review", got)
		}
	}
}

// TestRecoveryClassifierModelCheck crosses every gate condition with every
// projection-table shape, every evidence verdict, and every resolved
// marker state. The expected action is computed from the SPEC's precedence,
// independently of classifyRecovery. This is the exhaustiveness argument: no
// reachable point returns the zero action, the no-answer evidence value maps to
// an explicit blocked instruction, and the workspace precondition cannot erase
// the tracker-only half of a verdict.
func TestRecoveryClassifierModelCheck(t *testing.T) {
	type assignmentShape struct {
		name      string
		assignees []string
		events    []core.ClaimEvent
		gate      recoveryAction // zero means continue after gates 2 and 3
		hasAnchor bool
	}
	assignments := []assignmentShape{
		{
			name: "sole principal", assignees: []string{recoveryTestPrincipal},
			events:    []core.ClaimEvent{recoveryEvent(1, core.ClaimEventAssigned, recoveryTestPrincipal)},
			hasAnchor: true,
		},
		{
			name: "ours first", assignees: []string{recoveryTestPrincipal, "other"},
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, recoveryTestPrincipal),
				recoveryEvent(2, core.ClaimEventAssigned, "other"),
			},
			hasAnchor: true,
		},
		{
			name: "other first", assignees: []string{recoveryTestPrincipal, "other"},
			events: []core.ClaimEvent{
				recoveryEvent(1, core.ClaimEventAssigned, "other"),
				recoveryEvent(2, core.ClaimEventAssigned, recoveryTestPrincipal),
			},
			gate: recoveryActionReleaseKeep, hasAnchor: true,
		},
		{
			name: "unorderable", assignees: []string{recoveryTestPrincipal, "not-in-log"},
			events: []core.ClaimEvent{recoveryEvent(1, core.ClaimEventAssigned, recoveryTestPrincipal)},
			gate:   recoveryActionPark, hasAnchor: true,
		},
		{
			name: "missing principal event", assignees: []string{recoveryTestPrincipal},
			gate: recoveryActionPark,
		},
	}

	type projectionShape struct {
		name   string
		events []core.ClaimEvent
		want   func(Verdict) recoveryAction
	}
	constant := func(action recoveryAction) func(Verdict) recoveryAction {
		return func(Verdict) recoveryAction { return action }
	}
	activeStanding := func(evidence Verdict) recoveryAction {
		switch evidence {
		case VerdictPublished:
			return recoveryActionHold
		case VerdictIncomplete, VerdictContradicted:
			return recoveryActionBackoff
		default:
			return recoveryActionBlocked
		}
	}
	activeRemoved := func(evidence Verdict) recoveryAction {
		switch evidence {
		case VerdictPublished:
			return recoveryActionHold
		case VerdictIncomplete, VerdictContradicted:
			return recoveryActionPark
		default:
			return recoveryActionBlocked
		}
	}
	standingLabels := []struct {
		name  string
		label string
		want  func(Verdict) recoveryAction
	}{
		{name: "claimed", label: "ben:claimed", want: activeStanding},
		{name: "running", label: "ben:running", want: activeStanding},
		{name: "needs-review", label: "ben:needs-review", want: constant(recoveryActionPark)},
		{name: "failed", label: "ben:failed", want: constant(recoveryActionReleaseKeep)},
	}

	// Every non-empty standing-label subset, crossed with every member that can
	// be the most recently labeled effective projection. This is the complete
	// semantic state space of an interrupted add-before-remove projection: 32
	// shapes, not a sample of the two-label cases called out in BUILD.md.
	var projections []projectionShape
	for mask := 1; mask < 1<<len(standingLabels); mask++ {
		for effective := range standingLabels {
			if mask&(1<<effective) == 0 {
				continue
			}
			var names []string
			var events []core.ClaimEvent
			id := int64(100)
			for i, label := range standingLabels {
				if mask&(1<<i) == 0 {
					continue
				}
				names = append(names, label.name)
				if i == effective {
					continue
				}
				events = append(events, recoveryEvent(id, core.ClaimEventLabeled, label.label))
				id++
			}
			events = append(events, recoveryEvent(id, core.ClaimEventLabeled, standingLabels[effective].label))
			projections = append(projections, projectionShape{
				name:   fmt.Sprintf("standing=%v effective=%s", names, standingLabels[effective].name),
				events: events,
				want:   standingLabels[effective].want,
			})
		}
	}
	projections = append(projections,
		projectionShape{name: "none ever projected", want: constant(recoveryActionApprove)},
		projectionShape{name: "needs-review removed", events: recoveryLabelCycle(100, "ben:needs-review"), want: constant(recoveryActionBackoff)},
		projectionShape{name: "failed removed", events: recoveryLabelCycle(100, "ben:failed"), want: constant(recoveryActionReleaseKeep)},
		projectionShape{name: "claimed removed", events: recoveryLabelCycle(100, "ben:claimed"), want: activeRemoved},
		projectionShape{name: "running removed", events: recoveryLabelCycle(100, "ben:running"), want: activeRemoved},
	)

	evidenceVerdicts := []Verdict{VerdictUnknown, VerdictPublished, VerdictIncomplete, VerdictContradicted}
	markers := []recoveryMarkerState{
		recoveryMarkerFree,
		recoveryMarkerPossiblyLive,
		recoveryMarkerUnknownLaunch,
	}

	checked := 0
	for _, assignment := range assignments {
		for _, active := range []bool{false, true} {
			for _, closeInCycle := range []bool{false, true} {
				for _, inPartition := range []bool{false, true} {
					for _, projection := range projections {
						for _, evidence := range evidenceVerdicts {
							events := append([]core.ClaimEvent(nil), assignment.events...)
							if closeInCycle {
								events = append(events, recoveryEvent(50, core.ClaimEventClosed, ""))
							}
							events = append(events, projection.events...)
							candidate := recoveryCandidate{
								issue:  core.Issue{Assignees: assignment.assignees},
								active: active, inPartition: inPartition,
							}

							baseWant := projection.want(evidence)
							switch {
							case !active || (closeInCycle && assignment.hasAnchor):
								baseWant = recoveryActionReleaseDispose
							case assignment.gate != recoveryActionUnknown:
								baseWant = assignment.gate
							case !inPartition:
								baseWant = recoveryActionReleaseKeep
							}

							free := classifyRecovery(candidate, events, evidence,
								recoveryMarkerFree, recoveryTestPrincipal)
							if free.action != baseWant {
								t.Errorf("%s: free action = %v, want %v",
									modelPoint(assignment.name, active, closeInCycle, inPartition, projection.name, evidence, recoveryMarkerFree),
									free.action, baseWant)
							}

							for _, marker := range markers {
								checked++
								got := classifyRecovery(candidate, events, evidence, marker, recoveryTestPrincipal)
								wantAction := baseWant
								switch marker {
								case recoveryMarkerPossiblyLive:
									if baseWant != recoveryActionBlocked {
										wantAction = recoveryActionWait
									}
								case recoveryMarkerUnknownLaunch:
									wantAction = recoveryActionPark
								}
								point := modelPoint(assignment.name, active, closeInCycle, inPartition, projection.name, evidence, marker)
								if got.action != wantAction {
									t.Errorf("%s: action = %v, want %v", point, got.action, wantAction)
								}
								checkRecoveryVerdictShape(t, point, got)

								if marker == recoveryMarkerPossiblyLive {
									got.action = free.action
									if got != free {
										t.Errorf("%s: possibly-live marker changed tracker repair: got %+v, free %+v", point, got, free)
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// Canary for the dimensions above. If it moves, the new input space must
	// be read into both the oracle and checkRecoveryVerdictShape before repinning.
	const wantChecked = 17760
	if checked != wantChecked {
		t.Errorf("checked %d classifier points, want %d", checked, wantChecked)
	}
}

func recoveryCandidateFor(principal string) recoveryCandidate {
	return recoveryCandidate{
		issue:       core.Issue{Assignees: []string{principal}},
		active:      true,
		inPartition: true,
	}
}

func recoveryHistory(rest ...core.ClaimEvent) []core.ClaimEvent {
	events := []core.ClaimEvent{recoveryEvent(1, core.ClaimEventAssigned, recoveryTestPrincipal)}
	return append(events, rest...)
}

func recoveryEvent(id int64, kind core.ClaimEventKind, subject string) core.ClaimEvent {
	return core.ClaimEvent{Kind: kind, Subject: subject, At: recoveryTestTime, ID: id}
}

func recoveryLabelCycle(firstID int64, label string) []core.ClaimEvent {
	return []core.ClaimEvent{
		recoveryEvent(firstID, core.ClaimEventLabeled, label),
		recoveryEvent(firstID+1, core.ClaimEventUnlabeled, label),
	}
}

func modelPoint(assignment string, active, closed, partition bool, projection string, evidence Verdict, marker recoveryMarkerState) string {
	return fmt.Sprintf("assignment=%s active=%v closed=%v partition=%v projection=%s evidence=%s marker=%d",
		assignment, active, closed, partition, projection, evidence, marker)
}

// recoveryApprovalRoute is how a recovery verdict can end up with an agent
// running against issue content — the only thing §9.5 asks about a restart.
//
// Every action is classified, and the classification is what a new one has to
// come with: the exhaustive model sweep asserts the action it produced is in
// this map, so an action added without an entry fails there rather than
// silently acquiring whichever route its neighbour had.
type recoveryApprovalRoute uint8

const (
	// Nothing runs: the verdict releases, parks, holds, waits, or refuses.
	routeNoAgent recoveryApprovalRoute = iota
	// The record is adopted as an unapproved claim and beginApproval gates it
	// (SPEC §9.5). This is the restart shape of a claim whose check never ran.
	routeApprovalCheck
	// The record re-enters backoff, and the §9.6 re-fetch runs the check before
	// it can dispatch (onTimerFetched).
	routeRefetchCheck
)

var recoveryApprovalRoutes = map[recoveryAction]recoveryApprovalRoute{
	recoveryActionUnknown:        routeNoAgent,
	recoveryActionBlocked:        routeNoAgent,
	recoveryActionWait:           routeNoAgent,
	recoveryActionApprove:        routeApprovalCheck,
	recoveryActionBackoff:        routeRefetchCheck,
	recoveryActionPark:           routeNoAgent,
	recoveryActionHold:           routeNoAgent,
	recoveryActionReleaseKeep:    routeNoAgent,
	recoveryActionReleaseDispose: routeNoAgent,
}

// SPEC §9.5 has to survive a restart, and the window it occupies is exactly the
// one recovery classifies: a claim that landed and was never announced.
//
// Before #49 that window was claim-write → label-write, and it closed in one
// round trip. It now also holds the content check, which can span several ticks
// — a change-log or content read that failed is retried with the claim retained
// — so a crash inside it is materially more likely, and a recovery that
// dispatched from there would run an edit made after the approving label. Hence
// no verdict may reach an agent except through one of the two checks.
func TestNoRecoveryVerdictReachesAnAgentUnchecked(t *testing.T) {
	for action, route := range recoveryApprovalRoutes {
		if action == recoveryActionApprove || action == recoveryActionBackoff {
			if route == routeNoAgent {
				t.Errorf("action %v can reach an agent but is classified as reaching none", action)
			}
			continue
		}
		if route != routeNoAgent {
			t.Errorf("action %v is classified as reaching an agent; only the two dispatching actions may", action)
		}
	}

	// And the one this ticket moved: an unapproved claim announces nothing. A
	// projection here would commit the tracker to a claim §9.5 may still park.
	got := classifyRecovery(recoveryCandidateFor(recoveryTestPrincipal), recoveryHistory(),
		VerdictPublished, recoveryMarkerFree, recoveryTestPrincipal)
	if got.action != recoveryActionApprove {
		t.Fatalf("verdict = %+v, want an unapproved claim for an assignment that was never projected", got)
	}
	if got.project || got.stateLabel != core.StateLabelNone || got.milestone != "" {
		t.Errorf("verdict = %+v, want no projection before the §9.5 check", got)
	}
}

func checkRecoveryVerdictShape(t *testing.T, point string, got recoveryVerdict) {
	t.Helper()
	if _, ok := recoveryApprovalRoutes[got.action]; !ok {
		t.Errorf("%s: action %v has no §9.5 approval route classified; see recoveryApprovalRoutes", point, got.action)
	}
	switch got.action {
	case recoveryActionBlocked, recoveryActionWait, recoveryActionApprove, recoveryActionBackoff, recoveryActionPark,
		recoveryActionHold, recoveryActionReleaseKeep, recoveryActionReleaseDispose:
		// Named and classified.
	default:
		t.Errorf("%s: action %v is not one classified result", point, got.action)
	}
	if got.action == recoveryActionBlocked && (got.project || got.stateLabel != core.StateLabelNone ||
		got.milestone != "" || got.attemptFloor != 0) {
		t.Errorf("%s: blocked verdict inferred an effect from a missing fact: %+v", point, got)
	}

	if !got.project {
		if got.stateLabel != core.StateLabelNone || got.milestone != "" {
			t.Errorf("%s: no projection but label/comment set: %+v", point, got)
		}
	} else {
		wantMilestone := map[core.StateLabel]core.Milestone{
			core.StateLabelClaimed:     core.MilestoneClaimed,
			core.StateLabelNeedsReview: core.MilestoneNeedsReview,
			core.StateLabelFailed:      core.MilestoneFailed,
			core.StateLabelNone:        core.MilestonePublished,
		}[got.stateLabel]
		if wantMilestone == "" || got.milestone != wantMilestone {
			t.Errorf("%s: projection/comment pair is not one SPEC row: %+v", point, got)
		}
	}

	switch got.action {
	case recoveryActionApprove:
		// Attempt 1, and *nothing projected*: this claim has not passed §9.5's
		// content check, so it may still park for reapproval. Announcing
		// ben:claimed or posting the claimed milestone here would commit to a
		// dispatch the check has not authorized — and the ordinary path owes both
		// writes anyway, once it does.
		if got.attemptFloor != 1 {
			t.Errorf("%s: an unapproved claim does not say attempt 1: %+v", point, got)
		}
		if got.project || got.stateLabel != core.StateLabelNone || got.milestone != "" {
			t.Errorf("%s: an unapproved claim announced itself before the §9.5 check: %+v", point, got)
		}
	case recoveryActionBackoff:
		if got.attemptFloor != 2 || got.stateLabel != core.StateLabelClaimed {
			t.Errorf("%s: backoff does not say attempt >= 2 + claimed: %+v", point, got)
		}
	case recoveryActionPark:
		if !got.project || got.stateLabel != core.StateLabelNeedsReview {
			t.Errorf("%s: park is not projected needs-review: %+v", point, got)
		}
	case recoveryActionHold:
		if !got.project || got.stateLabel != core.StateLabelNone || got.milestone != core.MilestonePublished {
			t.Errorf("%s: held claim is not a completed done projection: %+v", point, got)
		}
	}
	// needs-review is the deliberate exception: gate 3 and unknown-launch park
	// precisely when no claim-cycle anchor can be recovered, and its comment is
	// anchored on the needs-review label transition itself. Every other emitted
	// projection comes from a positively anchored claim cycle.
	if got.project && got.milestone != core.MilestoneNeedsReview && got.cycleAnchor <= 0 {
		t.Errorf("%s: projection %q has no claim-cycle anchor: %+v", point, got.milestone, got)
	}
}
