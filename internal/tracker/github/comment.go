package github

import (
	"fmt"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// renderMilestone turns the structured payload into the comment body. The
// four headlines are fixed and the detail lines are a bounded list, so a
// milestone comment is recognizable machinery, not chat (SPEC §3.3, §8.4).
func renderMilestone(c core.MilestoneComment, daemon, principal string) (string, error) {
	var headline string
	lines := make([]string, 0, 4)

	switch c.Milestone {
	case core.MilestoneClaimed:
		headline = "**BEN claimed this issue.**"
		lines = append(lines, field("claim principal", "@"+principal))
	case core.MilestonePublished:
		// A published comment with no link is worse than no comment: it
		// asserts evidence it does not carry.
		if c.PRURL == "" {
			return "", fmt.Errorf("milestone %q requires a pull request URL", c.Milestone)
		}
		headline = "**BEN published a pull request.**"
		lines = append(lines, field("pull request", c.PRURL))
	case core.MilestoneFailed:
		// Exactly one of the two. A reason and a disclaimer together is
		// incoherent; neither is a comment that explains nothing.
		if (c.Reason == "") == !c.ReasonUnavailable {
			return "", fmt.Errorf("milestone %q needs either a §7.3 failure reason or ReasonUnavailable, not both and not neither", c.Milestone)
		}
		headline = "**BEN failed on this issue and released its claim.**"
		if c.ReasonUnavailable {
			// SPEC §9.10 step 6: the run record is local-only. Say so; never
			// invent a §7.3 value, never drop the comment.
			lines = append(lines, field("reason", "not recorded — the run record did not survive a restart"))
		} else {
			lines = append(lines, field("reason", "`"+string(c.Reason)+"`"))
		}
	case core.MilestoneNeedsReview:
		headline = "**BEN parked this issue for human review.**"
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownMilestone, c.Milestone)
	}

	if c.Detail != "" {
		lines = append(lines, field("detail", c.Detail))
	}
	lines = append(lines, field("daemon", "`"+daemon+"`"))

	return headline + "\n\n" + strings.Join(lines, "\n") + "\n", nil
}

func field(name, value string) string {
	return "- " + name + ": " + value
}
