package orchestrator

import (
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// The fake tracker's fidelity to the adapter it stands in for.
//
// internal/fake is not test files (AGENTS.md, Conventions): the orchestrator's
// tests, cmd/ben's acceptance tests and B12 all read it, so a fake that invents a
// guarantee the real component does not make is worse than a missing test — it
// lets code depending on the invention pass. Asserted from this package because
// this is the package whose decisions the invention would flatter.

// The fake recomputes §8.3's verdict after a release, and it had omitted two of
// the five conditions in as many review rounds — first the label partition,
// then open blockers. Both omissions have one consequence: an issue the daemon
// has just decided is not its work, and released for that reason, is handed
// straight back as eligible and re-run.
//
// Enumerated rather than patched a third time. The adapter computes all five
// per read (github eligibleIgnoringBlockers + hasOpenBlocker), so a fake that
// answers from fewer is not a fake of it, and the next omission should fail a
// test rather than reach a review.
func TestTheFakeRecomputesEveryDispatchableCondition(t *testing.T) {
	blocker := func(open bool) core.Blocker {
		return core.Blocker{Identifier: "9", State: map[bool]string{true: "open", false: "closed"}[open], Open: open}
	}
	for _, tc := range []struct {
		name  string
		after func(*core.Issue)
		want  bool
	}{
		{name: "nothing in the way", after: func(*core.Issue) {}, want: true},
		{name: "a closed blocker is not in the way", after: func(i *core.Issue) {
			i.Blockers = []core.Blocker{blocker(false)}
		}, want: true},
		{name: "an open blocker", after: func(i *core.Issue) { i.Blockers = []core.Blocker{blocker(true)} }},
		{name: "another party still assigned", after: func(i *core.Issue) {
			i.Assignees = append(i.Assignees, "a-human")
		}},
		{name: "issue no longer open", after: func(i *core.Issue) { i.State = "closed" }},
		{name: "required labels gone", after: func(i *core.Issue) { i.Labels = nil }},
		{name: "a ben:* state label stands", after: func(i *core.Issue) {
			i.Labels = append(i.Labels, "ben:needs-review")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := fake.NewTracker(fake.Issue("1", epoch))
			// As the tracker looks mid-run: claimed by us, and then whatever
			// the case is about.
			tr.Mutate("1", func(i *core.Issue) {
				i.Assignees = []string{fake.DefaultPrincipal}
				i.Dispatchable = false
				tc.after(i)
			})
			if err := tr.Release(t.Context(), issueFixture("1")); err != nil {
				t.Fatalf("Release: %v", err)
			}

			got, err := tr.Fetch(t.Context())
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("fetched %d issues, want 1", len(got))
			}
			if got[0].Dispatchable != tc.want {
				t.Errorf("dispatchable = %v, want %v — the released issue's §8.3 verdict",
					got[0].Dispatchable, tc.want)
			}
		})
	}
}
