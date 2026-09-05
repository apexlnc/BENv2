package registry

import (
	"slices"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The closed registered kind sets, asserted exactly. A kind added to a table
// without a decision lands here first, and the sets are what the loader's
// refusal message quotes.
func TestRegisteredKindsAreTheClosedSets(t *testing.T) {
	if got, want := TrackerNames(), []string{"github"}; !slices.Equal(got, want) {
		t.Errorf("TrackerNames() = %v, want %v", got, want)
	}
	if got, want := RunnerNames(), []string{"claude-code", "codex-exec"}; !slices.Equal(got, want) {
		t.Errorf("RunnerNames() = %v, want %v", got, want)
	}
	if got, want := SourceNames(), []string{"octo_sts", "projected_oidc", "static"}; !slices.Equal(got, want) {
		t.Errorf("SourceNames() = %v, want %v", got, want)
	}
}

// Every listed name resolves to a kind whose Structural answers here: in a test
// process with no credentials, no network, and no harness installed, which is
// the environment `config effective` and CI ask it in (SPEC §5.7, §5.8).
//
// The empty config is refused rather than merely answered. A registration
// pointing at a Structural that accepts everything is an adapter that cannot
// refuse a typo in its own opaque block — the gap this registry exists to close
// (#55) — and it would read as green here otherwise. A kind registered as a nil
// interface value panics.
func TestEveryListedNameResolvesToARefusingKind(t *testing.T) {
	for _, name := range TrackerNames() {
		kind, ok := Tracker(name)
		if !ok || kind == nil {
			t.Fatalf("Tracker(%q) = %v, %v — listed but not resolvable", name, kind, ok)
		}
		if err := kind.Structural(core.TrackerConfig{}); err == nil {
			t.Errorf("tracker kind %q accepted an empty config", name)
		}
	}
	for _, name := range RunnerNames() {
		kind, ok := Runner(name)
		if !ok || kind == nil {
			t.Fatalf("Runner(%q) = %v, %v — listed but not resolvable", name, kind, ok)
		}
		if err := kind.Structural(core.AgentConfig{Provider: map[string]any{}}); err == nil {
			t.Errorf("runner kind %q accepted an empty agent.provider block", name)
		}
	}
	for _, name := range SourceNames() {
		kind, ok := Source(name)
		if !ok || kind == nil {
			t.Fatalf("Source(%q) = %v, %v — listed but not resolvable", name, kind, ok)
		}
		if _, err := kind.Describe(map[string]any{"kind": name}); err == nil {
			t.Errorf("source kind %q accepted an empty block", name)
		}
	}
}

// An unsupported name yields a *nil* kind, not a zero value with the bool as the
// only warning: a caller that drops the bool must be left with nothing to call.
func TestUnknownKindResolvesToNil(t *testing.T) {
	for _, name := range []string{"", "gitlab", "GitHub", "claude", "claude-code "} {
		if kind, ok := Tracker(name); ok || kind != nil {
			t.Errorf("Tracker(%q) = %v, %v; want nil, false", name, kind, ok)
		}
		if kind, ok := Runner(name); ok || kind != nil {
			t.Errorf("Runner(%q) = %v, %v; want nil, false", name, kind, ok)
		}
		if kind, ok := Source(name); ok || kind != nil {
			t.Errorf("Source(%q) = %v, %v; want nil, false", name, kind, ok)
		}
	}
}

// The name lists are copies. A caller that sorts, truncates, or appends to what
// it got back must not be editing the closed kind set for everyone else.
func TestNamesAreFreshSlices(t *testing.T) {
	names := RunnerNames()
	if len(names) == 0 {
		t.Fatal("no runner kinds registered")
	}
	names[0] = "mutated"
	if again := RunnerNames(); slices.Contains(again, "mutated") {
		t.Errorf("RunnerNames() returned the table's own slice: %v", again)
	}

	trackers := TrackerNames()
	trackers[0] = "mutated"
	if again := TrackerNames(); slices.Contains(again, "mutated") {
		t.Errorf("TrackerNames() returned the table's own slice: %v", again)
	}

	sources := SourceNames()
	sources[0] = "mutated"
	if again := SourceNames(); slices.Contains(again, "mutated") {
		t.Errorf("SourceNames() returned the table's own slice: %v", again)
	}
}
