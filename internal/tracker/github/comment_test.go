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
		core.FailureCredential, core.FailureOutputOverflow,
	} {
		if strings.Contains(body, string(reason)) {
			t.Errorf("body invents the §7.3 reason %q:\n%s", reason, body)
		}
	}
}

// The published milestone is an interface, not only a notification: #11's
// review controller reads the pull request link out of it, from the field line
// and never from the headline. That coupling has two halves, and this is the
// producing one — internal/review pins the parse, and neither test can see the
// other half drift on its own.
//
// The headline is deliberately *not* asserted here beyond its own test above:
// rewording it is allowed, and must stay allowed, precisely because nothing
// machine-readable depends on it.
func TestPublishedMilestoneCarriesTheLinkAsAField(t *testing.T) {
	const url = "https://github.com/acme/widgets/pull/9"
	body, err := renderMilestone(
		core.MilestoneComment{Milestone: core.MilestonePublished, PRURL: url},
		"build-box/ben-1a2b3c4d", testLogin)
	if err != nil {
		t.Fatalf("renderMilestone: %v", err)
	}

	// The exact line shape internal/review's publishedPRLink cuts on: a
	// `- pull request: ` prefix at the start of a line, and the URL alone
	// after it.
	const field = "- pull request: "
	var found int
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), field)
		if !ok {
			continue
		}
		found++
		if strings.TrimSpace(rest) != url {
			t.Errorf("the field line carries %q, want the bare URL %q", rest, url)
		}
	}
	if found != 1 {
		t.Fatalf("the body carries %d %q lines, want exactly one:\n%s", found, field, body)
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
