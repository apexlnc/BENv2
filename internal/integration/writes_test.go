package integration

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
)

// SPEC §12.3-9: the orchestrator never closes an issue and never writes
// unstructured prose.
//
// Asserted at two boundaries, because the behavioural half alone cannot say it.
// A scenario can only show that the writes it *provoked* were in the set; the
// write nobody provoked is exactly the one this invariant is about. So the
// closed set is anchored at the seam's method table — a tracker the loop
// programs against has no way to spell "close", and no way to post a body of
// text — and the scenario then shows that the four writes it does have are the
// four it used.
func TestTheOrchestratorWritesOnlyTheClosedSet(t *testing.T) {
	t.Run("the seam has no other write", func(t *testing.T) {
		// SPEC §8.2 restated independently of orchestrator/deps.go. Iterating
		// the interface and comparing it to itself would pass whatever it said;
		// this is the list §8.2 names, written out here so that a method added
		// to the seam has to be added here too, deliberately.
		want := []string{
			// Reads. ClaimedByPrincipal is §9.10 step 1's recovery candidate read,
			// added to the seam by B10 boundary 3 (#8) — a read, so it changes nothing
			// about the closed *write* set this test exists to pin.
			"ClaimHistory", "ClaimedByPrincipal", "Fetch", "Get", "HeldClaims",
			// The closed write set (SPEC §8.1, §8.4).
			"Claim", "Comment", "Release", "SetStateLabels",
		}
		slices.Sort(want)

		seam := reflect.TypeOf((*orchestrator.Tracker)(nil)).Elem()
		got := make([]string, 0, seam.NumMethod())
		for i := range seam.NumMethod() {
			got = append(got, seam.Method(i).Name)
		}
		slices.Sort(got)

		if !slices.Equal(got, want) {
			t.Errorf("orchestrator.Tracker = %v, want %v — §8.1 forbids the orchestrator closing an issue or writing free text, and the seam is where that is made unspellable rather than merely unused",
				got, want)
		}
	})

	t.Run("the comment payload is structured", func(t *testing.T) {
		// The other half of "never writes unstructured prose": a milestone
		// comment is a *record*, not a body. The adapter renders it; the loop
		// cannot hand over text of its own. Restated here so that adding a free
		// `Body string` to the payload fails this test rather than passing
		// silently through every scenario below.
		want := []string{"Detail", "Milestone", "PRURL", "Reason", "ReasonUnavailable"}
		slices.Sort(want)

		payload := reflect.TypeOf(core.MilestoneComment{})
		got := make([]string, 0, payload.NumField())
		for i := range payload.NumField() {
			got = append(got, payload.Field(i).Name)
		}
		slices.Sort(got)

		if !slices.Equal(got, want) {
			t.Errorf("core.MilestoneComment fields = %v, want %v", got, want)
		}
	})

	t.Run("a run that fails uses all four writes and no others", func(t *testing.T) {
		// A non-retryable failure is the outcome that exercises the whole set:
		// claim, the ben:claimed → ben:running → ben:failed projection, the
		// failed milestone, and the release §9.2 performs at `failed`.
		h := start(t, scenarioConfig{
			script: func(core.RunSpec, int) []core.Event {
				return fake.Fail(core.FailureAuth)
			},
		})
		h.settleThrough("7", orchestrator.StateFailed)
		h.waitMilestone("7", core.MilestoneFailed)
		h.waitFor("the claim to be released at `failed`", func() bool { return h.Tracker.ReleaseCount("7") == 1 })

		calls, _, comments := h.Tracker.Snapshot()
		writes := []string{"claim", "comment", "label", "release"}
		for _, call := range calls {
			verb, _, _ := strings.Cut(call, " ")
			if !slices.Contains(writes, verb) {
				t.Errorf("the orchestrator made the write %q; §8.1's write set is %v", call, writes)
			}
		}
		// Every write the outcome owes was actually made, so the loop above is
		// checking a populated set rather than an empty one.
		for _, want := range writes {
			if !slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, want+" ") }) {
				t.Errorf("no %s write was recorded; calls = %v", want, calls)
			}
		}

		milestones := []core.Milestone{
			core.MilestoneClaimed, core.MilestoneFailed,
			core.MilestoneNeedsReview, core.MilestonePublished,
		}
		for _, c := range comments["7"] {
			if !slices.Contains(milestones, c.Milestone) {
				t.Errorf("posted milestone %q, which is not one of §8.4's four", c.Milestone)
			}
		}
	})
}

// SPEC §12.3-8, at the boundary the loop owns: nothing of the daemon's own
// environment crosses into a run.
//
// The argv and child-environment halves of this invariant belong to the
// adapters and are asserted there — harness/redact_test.go over every value the
// harness handles, block.go's allowlist over what may be inherited at all, and
// both runners' conformance suites over what they exec. What only this boundary
// can say is that the *orchestrator* contributes nothing: RunSpec is the one
// value that crosses from the loop to an adapter, §7.6 reserves `BEN_` to the
// orchestrator and permits nothing else in it, and the prompt is the other thing
// a run is handed.
//
// The secret here is planted in the daemon's own environment, which is where a
// tracker PAT or an agent API key actually lives on a host. A leak would be a
// value the loop had to have gone and fetched — which is the point: it never
// reads one.
func TestNoCredentialReachesTheRunSpecOrThePrompt(t *testing.T) {
	const secret = "ghp_notarealtokenbutshapedlikeone"
	t.Setenv("GITHUB_TOKEN", secret)
	t.Setenv("ANTHROPIC_API_KEY", secret)

	h := start(t, scenarioConfig{
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "s", Continuation: "s"}}
		},
		before: func(h *scenario) { h.Runner.SetHangAfterScript(true) },
	})
	h.waitRunning("7")

	if n := h.Runner.StartCount(); n != 1 {
		t.Fatalf("started %d runs; this scenario reads the one spec that crossed the boundary", n)
	}
	spec, ok := h.Runner.LastSpec()
	if !ok {
		t.Fatal("no RunSpec was recorded")
	}

	// SPEC §7.6: `BEN_` is reserved to the orchestrator and RunSpec.Env may
	// carry nothing else. Anything unprefixed here is the daemon's environment
	// leaking through the one channel that is supposed to be closed.
	for k, v := range spec.Env {
		if !strings.HasPrefix(k, core.EnvPrefix) {
			t.Errorf("RunSpec.Env carries %q, which is outside the reserved %s namespace (SPEC §7.6)", k, core.EnvPrefix)
		}
		if strings.Contains(v, secret) {
			t.Errorf("RunSpec.Env[%q] carries the daemon's credential", k)
		}
	}
	if strings.Contains(spec.Prompt, secret) {
		t.Errorf("the rendered prompt carries the daemon's credential:\n%s", spec.Prompt)
	}
	if strings.Contains(spec.Workspace.Path, secret) || strings.Contains(spec.Continuation, secret) {
		t.Error("the run's workspace path or continuation token carries the daemon's credential")
	}

	// And nothing wrote one back to the tracker either, which is the other
	// direction a value that never should have been read could escape.
	calls, _, comments := h.Tracker.Snapshot()
	for _, c := range calls {
		if strings.Contains(c, secret) {
			t.Errorf("a tracker write carries the daemon's credential: %q", c)
		}
	}
	for _, c := range comments["7"] {
		if strings.Contains(c.Detail, secret) || strings.Contains(c.PRURL, secret) {
			t.Error("a milestone comment carries the daemon's credential")
		}
	}
}
