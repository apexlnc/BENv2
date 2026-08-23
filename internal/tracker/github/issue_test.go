package github

import (
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The §8.3 dispatchability matrix. Every clause gets a row that flips only it.
func TestDispatchable(t *testing.T) {
	required := []string{"ben-queue", "approved"}
	active := []string{"open"}

	base := core.Issue{
		Labels: []string{"ben-queue", "approved", "bug"},
		State:  "open",
	}
	with := func(mutate func(*core.Issue)) core.Issue {
		out := base
		out.Labels = append([]string(nil), base.Labels...)
		mutate(&out)
		return out
	}

	tests := []struct {
		name  string
		issue core.Issue
		want  bool
	}{
		{"all clauses satisfied", base, true},
		{"required label missing", with(func(i *core.Issue) { i.Labels = []string{"ben-queue"} }), false},
		{"labels match case-insensitively", with(func(i *core.Issue) { i.Labels = []string{"BEN-Queue", "Approved"} }), true},
		{"state not active", with(func(i *core.Issue) { i.State = "closed" }), false},
		{"state matches case-insensitively", with(func(i *core.Issue) { i.State = "OPEN" }), true},
		// A human called dibs (SPEC §8.4).
		{"assigned to another party", with(func(i *core.Issue) { i.Assignees = []string{"someone"} }), false},
		// Our own retained claim on a published issue (SPEC §9.10 step 3).
		{"assigned to the claim principal", with(func(i *core.Issue) { i.Assignees = []string{testLogin} }), false},
		// BUILD.md decision 8.
		{"carries a ben state label", with(func(i *core.Issue) { i.Labels = append(i.Labels, "ben:failed") }), false},
		{"carries a needs-review label", with(func(i *core.Issue) { i.Labels = append(i.Labels, "ben:needs-review") }), false},
		{"open blocker", with(func(i *core.Issue) {
			i.Blockers = []core.Blocker{{Identifier: "4", State: "open", Open: true}}
		}), false},
		{"only closed blockers", with(func(i *core.Issue) {
			i.Blockers = []core.Blocker{{Identifier: "4", State: "closed"}}
		}), true},
		{"one open blocker among closed", with(func(i *core.Issue) {
			i.Blockers = []core.Blocker{{Identifier: "4", State: "closed"}, {Identifier: "5", State: "open", Open: true}}
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dispatchable(tt.issue, required, active); got != tt.want {
				t.Errorf("dispatchable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The label projection is closed (SPEC §9.3): four names, plus "no label".
func TestStateLabelName(t *testing.T) {
	tests := []struct {
		label   core.StateLabel
		want    string
		wantErr error
	}{
		{core.StateLabelNone, "", nil},
		{core.StateLabelClaimed, "ben:claimed", nil},
		{core.StateLabelRunning, "ben:running", nil},
		{core.StateLabelNeedsReview, "ben:needs-review", nil},
		{core.StateLabelFailed, "ben:failed", nil},
		{core.StateLabel("done"), "", ErrUnknownStateLabel},
		{core.StateLabel("ben:claimed"), "", ErrUnknownStateLabel},
	}
	for _, tt := range tests {
		t.Run(string(tt.label), func(t *testing.T) {
			got, err := stateLabelName(tt.label)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("stateLabelName(%q) = %q, want %q", tt.label, got, tt.want)
			}
		})
	}
}

func TestIsStateLabel(t *testing.T) {
	for _, name := range []string{"ben:claimed", "BEN:Failed", "ben:anything"} {
		if !isStateLabel(name) {
			t.Errorf("%q should be recognized as a state label", name)
		}
	}
	for _, name := range []string{"ben-queue", "bug", "benign", ""} {
		if isStateLabel(name) {
			t.Errorf("%q is not in the ben: namespace", name)
		}
	}
}
