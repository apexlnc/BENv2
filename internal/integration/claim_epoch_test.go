package integration

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
)

// SPEC §12.3-12: success evidence is scoped to one tracker assignment.
//
// E1 publishes H1 and leaves its PR open. An external controller then removes
// BEN's assignment; the next dispatch creates E2 and must pin H1 before any
// hook or agent runs. A no-op E2 therefore has no commit past its own base and
// cannot inherit E1's success. A later H2 descendant in the *same* E2 may use
// the still-open PR, because all three §9.7 legs are then current together.
func TestReclaimRepinsPriorPublishedHeadBeforeSuccessCanRepeat(t *testing.T) {
	var phase atomic.Int32
	h := start(t, scenarioConfig{
		prepareFacts: func(ws core.Workspace) (core.LocalBranchFacts, error) {
			// Called only for E2: the pending record carries E1's pin as the
			// outgoing comparison, while remote-first reattachment has found H1.
			return core.LocalBranchFacts{BaseSHA: ws.BaseSHA, Head: agentCommitSHA, DescendsBase: true}, nil
		},
		facts: func(ws core.Workspace) (core.PublishFacts, error) {
			head := agentCommitSHA
			if phase.Load() >= 3 {
				head = rewrittenSHA
			}
			facts := core.PublishFacts{Head: head, DescendsBase: true}
			if head != ws.BaseSHA {
				facts.RemoteProbed = true
				facts.RemoteHead = head
				facts.RemoteHasHead = true
			}
			return facts, nil
		},
		before: func(h *scenario) {
			h.Runner.SetScript(func(_ core.RunSpec, _ int) []core.Event {
				n := phase.Add(1)
				switch n {
				case 1:
					h.Workspaces.SetBranchHead("7", agentCommitSHA)
					h.Tracker.SetPR("ben/issue-7", core.PR{
						Number: 71, URL: "https://example.test/pull/71", State: "open", Branch: "ben/issue-7", BaseBranch: "main",
					})
				case 2:
					// Deliberate no-op reviser: H1 and PR1 are still present.
				case 3:
					h.Workspaces.SetBranchHead("7", rewrittenSHA)
				default:
					return fake.Fail(core.FailureCrashed)
				}
				return fake.Succeed("session")
			})
		},
	})

	// E1 publishes H1/PR1 and converts to a held claim.
	h.settle("7", orchestrator.StateDone)
	h.waitPosted("7", core.MilestonePublished)
	h.waitFor("E1 to become a held claim", func() bool { return h.o.HeldCount() == 1 })
	first := h.Workspaces.Prepares("7")
	if len(first) != 1 || first[0].Epoch <= 0 || first[0].BaseSHA == agentCommitSHA {
		t.Fatalf("E1 prepare = %+v, want a positive epoch pinned before H1", first)
	}
	e1 := first[0].Epoch

	// Forge/controller action: remove BEN's assignment. The issue is still open
	// and queued, so after the held record observes the loss BEN claims it again.
	h.Tracker.UnassignBy("7", fake.DefaultPrincipal)
	h.settle("7", orchestrator.StateNeedsReview)
	h.waitPosted("7", core.MilestoneNeedsReview)

	prepares := h.Workspaces.Prepares("7")
	if len(prepares) != 2 {
		t.Fatalf("prepares after E2 no-op = %+v, want E1 and E2", prepares)
	}
	e2 := prepares[1]
	if e2.Epoch <= 0 || e2.Epoch == e1 {
		t.Errorf("E2 epoch = %d, E1 = %d; reassignment must create a new tracker epoch", e2.Epoch, e1)
	}
	if e2.BaseSHA != agentCommitSHA {
		t.Errorf("E2 base = %q, want prior published head H1 %q", e2.BaseSHA, agentCommitSHA)
	}
	if got := publishedMilestones(h, "7"); got != 1 {
		t.Errorf("published milestones after E2 no-op = %d, want only E1's", got)
	}
	if got := h.Tracker.Label("7"); got != core.StateLabelNeedsReview {
		t.Errorf("label after no-op E2 = %q, want needs-review", got)
	}

	// Human re-queue within E2 retains its epoch/base. H2 advances from H1 and
	// the same open PR can now complete the current claim's evidence.
	h.Tracker.Mutate("7", func(issue *core.Issue) {
		labels := issue.Labels[:0]
		for _, label := range issue.Labels {
			if !strings.EqualFold(label, "ben:needs-review") {
				labels = append(labels, label)
			}
		}
		issue.Labels = labels
	})
	h.settle("7", orchestrator.StateDone)
	h.waitFor("E2's H2 publication", func() bool { return publishedMilestones(h, "7") == 2 })

	prepares = h.Workspaces.Prepares("7")
	if len(prepares) != 3 {
		t.Fatalf("prepares after H2 = %+v, want E1 plus two E2 attempts", prepares)
	}
	if got := prepares[2]; got.Epoch != e2.Epoch || got.BaseSHA != e2.BaseSHA {
		t.Errorf("E2 retry reminted its pair: first=%+v retry=%+v", e2, got)
	}
	if phase.Load() != 3 {
		t.Errorf("agent launches = %d, want E1, no-op E2, and H2 E2", phase.Load())
	}
}

func publishedMilestones(h *scenario, identifier string) int {
	n := 0
	for _, comment := range h.Tracker.CommentsFor(identifier) {
		if comment.Milestone == core.MilestonePublished {
			n++
		}
	}
	return n
}
