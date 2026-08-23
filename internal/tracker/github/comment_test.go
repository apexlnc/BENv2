package github

import (
	"errors"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Exactly four milestones earn prose (SPEC §8.4); every body carries the
// daemon identity that makes multi-daemon setups legible.
func TestRenderMilestone(t *testing.T) {
	const daemon = "build-box/ben-1a2b3c4d"

	tests := []struct {
		name     string
		comment  core.MilestoneComment
		contains []string
	}{
		{
			name:     "claimed names the principal",
			comment:  core.MilestoneComment{Milestone: core.MilestoneClaimed},
			contains: []string{"BEN claimed this issue", "@" + testLogin, daemon},
		},
		{
			name:     "published links the pull request",
			comment:  core.MilestoneComment{Milestone: core.MilestonePublished, PRURL: "https://github.com/acme/widgets/pull/9"},
			contains: []string{"published a pull request", "/pull/9", daemon},
		},
		{
			name:     "failed states the reason",
			comment:  core.MilestoneComment{Milestone: core.MilestoneFailed, Reason: core.FailureStalled, Detail: "no events for 5m"},
			contains: []string{"failed on this issue", "`stalled`", "no events for 5m", daemon},
		},
		{
			name:     "needs review carries the detail",
			comment:  core.MilestoneComment{Milestone: core.MilestoneNeedsReview, Detail: "agent reported success with no commits"},
			contains: []string{"parked this issue for human review", "no commits", daemon},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := renderMilestone(tt.comment, daemon, testLogin)
			if err != nil {
				t.Fatalf("renderMilestone: %v", err)
			}
			for _, want := range tt.contains {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q:\n%s", want, body)
				}
			}
		})
	}
}

// A milestone that cannot carry its evidence is a refusal, not a comment.
func TestRenderMilestoneRequiresItsEvidence(t *testing.T) {
	for _, c := range []core.MilestoneComment{
		{Milestone: core.MilestonePublished},
		{Milestone: core.MilestoneFailed},
		// A reason and a disclaimer together is incoherent.
		{Milestone: core.MilestoneFailed, Reason: core.FailureStalled, ReasonUnavailable: true},
	} {
		if _, err := renderMilestone(c, "d", "p"); err == nil {
			t.Errorf("milestone %+v rendered without a coherent reason", c)
		}
	}
}

// SPEC §9.10 step 6: the §7.3 reason lives in the local run record, so a
// fresh-host recovery cannot always name it. It must say so — never invent a
// reason, never skip the comment, because a ben:failed label with no
// explanation is worse than an honest one.
func TestRenderMilestoneStatesAnUnavailableReason(t *testing.T) {
	body, err := renderMilestone(
		core.MilestoneComment{Milestone: core.MilestoneFailed, ReasonUnavailable: true},
		"build-box/ben-1a2b3c4d", testLogin)
	if err != nil {
		t.Fatalf("recovery must be able to report a failure it cannot explain: %v", err)
	}
	if !strings.Contains(body, "not recorded") || !strings.Contains(body, "did not survive a restart") {
		t.Errorf("body does not say the reason was lost:\n%s", body)
	}
	// Nothing may look like a §7.3 value.
	for _, reason := range []core.FailureReason{
		core.FailureCrashed, core.FailureStalled, core.FailureTimeout, core.FailureRateLimited,
		core.FailureAuth, core.FailureLaunchError, core.FailureKilled, core.FailureBudgetExceeded,
	} {
		if strings.Contains(body, string(reason)) {
			t.Errorf("body invents the §7.3 reason %q:\n%s", reason, body)
		}
	}
}

func TestRenderMilestoneRejectsUnknown(t *testing.T) {
	for _, m := range []core.Milestone{"", "started", "progress", "done"} {
		_, err := renderMilestone(core.MilestoneComment{Milestone: m}, "d", "p")
		if !errors.Is(err, ErrUnknownMilestone) {
			t.Errorf("milestone %q: error = %v, want ErrUnknownMilestone", m, err)
		}
	}
}
