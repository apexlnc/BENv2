package review

import (
	"errors"
	"strings"
	"testing"
)

// The claim epoch is the prerequisite the whole loop rests on: it is what makes
// a reclaim remint SPEC §9.7's verification base, and therefore what stops a
// no-op reviser from satisfying `done` against an earlier claim's commits. The
// controller derives it independently of the daemon, from the same ordered log,
// so it has to agree with internal/tracker/github's replay — including on the
// rule that only the *current* streak counts.
func TestClaimEpoch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []Event
		want   int64
	}{
		{name: "never assigned"},
		{
			name:   "one standing assignment",
			events: []Event{{ID: 7001, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(1)}},
			want:   7001,
		},
		{
			name: "unassigned is no claim",
			events: []Event{
				{ID: 7001, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(1)},
				{ID: 7002, Type: EventUnassigned, Assignee: fxPrincipal, CreatedAt: at(2)},
			},
		},
		{
			name: "a reassignment is a new epoch",
			events: []Event{
				{ID: 7001, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(1)},
				{ID: 7002, Type: EventUnassigned, Assignee: fxPrincipal, CreatedAt: at(2)},
				{ID: 7003, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(3)},
			},
			want: 7003,
		},
		{
			name: "a redundant assign does not move a standing streak",
			events: []Event{
				{ID: 7001, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(1)},
				{ID: 7002, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(2)},
			},
			want: 7001,
		},
		{
			name: "somebody else's assignment is not the claim",
			events: []Event{
				{ID: 7001, Type: EventAssigned, Assignee: "a-human", CreatedAt: at(1)},
			},
		},
		{
			name: "a label transition is not an assignment",
			events: []Event{
				{ID: 7001, Type: EventLabeled, Label: fxQueue, CreatedAt: at(1)},
			},
		},
		{
			name: "out-of-order pages sort before replay",
			events: []Event{
				{ID: 7003, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(3)},
				{ID: 7001, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(1)},
				{ID: 7002, Type: EventUnassigned, Assignee: fxPrincipal, CreatedAt: at(2)},
			},
			want: 7003,
		},
		{
			name: "same instant, id breaks the tie",
			events: []Event{
				{ID: 7002, Type: EventUnassigned, Assignee: fxPrincipal, CreatedAt: at(1)},
				{ID: 7001, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(1)},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClaimEpoch(SortEvents(tc.events), fxPrincipal); got != tc.want {
				t.Errorf("ClaimEpoch = %d, want %d", got, tc.want)
			}
		})
	}
}

// A claim epoch and a milestone occurrence come from the same ordered log and
// are never interchangeable. Nothing in this package compares them; this holds
// the marker vocabulary to keeping them apart, which is the shape that makes
// the mistake impossible rather than merely absent.
func TestEpochAndOccurrenceAreSeparateFields(t *testing.T) {
	m := ReviewMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Base: base1, Verdict: VerdictClean}
	got, err := ParseReviewMarker(m.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Occurrence == got.Claim {
		t.Fatal("the fixture cannot detect a swap: choose distinct ids")
	}
	if got.Occurrence != occ1 || got.Claim != epoch1 {
		t.Errorf("marker = %+v, want occurrence %d and claim %d", got, occ1, epoch1)
	}
}

func TestCycleStartAndRecoveryPredicates(t *testing.T) {
	cfg := fxConfig()
	events := SortEvents([]Event{
		{ID: 1, Type: EventLabeled, Actor: "a-human", Label: fxQueue, CreatedAt: at(0)},
		{ID: 2, Type: EventAssigned, Actor: fxTracker, Assignee: fxPrincipal, CreatedAt: at(1)},
		// GitHub timestamps to one second. IDs provide the order within this
		// deliberately shared instant: publish, controller revoke, reapproval,
		// then unassignment.
		{ID: 3, Type: EventUnlabeled, Actor: fxTracker, Label: "ben:claimed", CreatedAt: at(20)},
		{ID: 4, Type: EventUnlabeled, Actor: fxController, Label: fxQueue, CreatedAt: at(20)},
		{ID: 5, Type: EventLabeled, Actor: "a-human", Label: fxQueue, CreatedAt: at(20)},
		{ID: 6, Type: EventUnassigned, Actor: fxController, Assignee: fxPrincipal, CreatedAt: at(20)},
		{ID: 8, Type: EventUnlabeled, Actor: fxTracker, Label: "ben:running", CreatedAt: at(30)},
		{ID: 7, Type: EventUnlabeled, Actor: "a-human", Label: fxQueue, CreatedAt: at(40)},
		{ID: 9, Type: EventUnlabeled, Actor: fxTracker, Label: "ben:running", CreatedAt: at(50)},
	})

	if got := cycleStartAtOccurrence(cfg, events, 3); !got.Equal(at(0)) {
		t.Errorf("cycleStartAtOccurrence(3) = %v, want the approval that produced it at %v", got, at(0))
	}
	if got := cycleStartAtOccurrence(cfg, events, 8); !got.Equal(at(20)) {
		t.Errorf("cycleStartAtOccurrence(8) = %v, want the reapproval at %v", got, at(20))
	}
	if !unassignedAfterClaim(events, fxPrincipal, 2) {
		t.Error("the same-second unassignment is ordered after claim event 2")
	}
	if unassignedAfterClaim(events, fxPrincipal, 6) {
		t.Error("an unassignment event is not an assignment anchor")
	}
	if got, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, 3); !ok || got != 1 {
		t.Errorf("approvalEpochAtOccurrence(3) = %d, %v; want 1, true", got, ok)
	}
	if got, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, 8); !ok || got != 5 {
		t.Errorf("approvalEpochAtOccurrence(8) = %d, %v; want 5, true", got, ok)
	}
	if got, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, 9); ok || got != 0 {
		t.Errorf("approvalEpochAtOccurrence(9) = %d, %v; want 0, false after withdrawal", got, ok)
	}
	if got, ok := labelEpochAtOccurrence(events, fxQueue, 3); !ok || got != 1 {
		t.Errorf("legacy labelEpochAtOccurrence(3) = %d, %v; want source epoch 1 despite later reapproval", got, ok)
	}
	if removed, got, valid := approvalChangesAfterEpoch(events, cfg.RequiredLabels, 1); !valid || !removed || got != 5 {
		t.Errorf("approvalChangesAfterEpoch(1) = %v, %d, %v; want true, 5, true", removed, got, valid)
	}
	if removed, got, valid := approvalChangesAfterEpoch(events, cfg.RequiredLabels, 5); !valid || !removed || got != 0 {
		t.Errorf("approvalChangesAfterEpoch(5) = %v, %d, %v; want true, 0, true", removed, got, valid)
	}
	if removed, got, valid := approvalChangesAfterEpoch(events, cfg.RequiredLabels, 3); valid || removed || got != 0 {
		t.Errorf("approvalChangesAfterEpoch(non-anchor event) = %v, %d, %v; want false, 0, false", removed, got, valid)
	}
}

func TestApprovalEpochUsesTheCompleteRequiredLabelSet(t *testing.T) {
	cfg := fxConfig()
	cfg.RequiredLabels = []string{fxQueue, "security-approved"}
	events := SortEvents([]Event{
		{ID: 10, Type: EventLabeled, Label: fxQueue, CreatedAt: at(0)},
		{ID: 20, Type: EventLabeled, Label: "security-approved", CreatedAt: at(1)},
		{ID: 30, Type: EventUnlabeled, Label: "ben:claimed", CreatedAt: at(2)},
		{ID: 40, Type: EventUnlabeled, Label: "SECURITY-APPROVED", CreatedAt: at(3)},
		{ID: 45, Type: EventUnlabeled, Label: "ben:running", CreatedAt: at(3)},
		{ID: 50, Type: EventLabeled, Label: "security-approved", CreatedAt: at(4)},
	})

	if got, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, 30); !ok || got != 20 {
		t.Fatalf("approval at occurrence 30 = %d, %v; want the last-applied required label 20", got, ok)
	}
	if got := cycleStartAtOccurrence(cfg, events, 30); !got.Equal(at(1)) {
		t.Errorf("cycle start = %v, want the complete-set event at %v", got, at(1))
	}
	if got, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, 45); ok || got != 0 {
		t.Errorf("approval while one required label is absent = %d, %v; want 0, false", got, ok)
	}
	if removed, newer, valid := approvalChangesAfterEpoch(events, cfg.RequiredLabels, 20); !valid || !removed || newer != 50 {
		t.Errorf("changes after anchor 20 = %v, %d, %v; want true, 50, true", removed, newer, valid)
	}
	if _, _, valid := approvalChangesAfterEpoch(events, cfg.RequiredLabels, 10); valid {
		t.Error("an individual label event was accepted as the complete-set anchor")
	}
}

func TestClaimEpochAtOccurrence(t *testing.T) {
	events := SortEvents([]Event{
		{ID: epoch1, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(1)},
		{ID: occ1, Type: EventUnlabeled, Label: "ben:claimed", CreatedAt: at(2)},
		{ID: 7100, Type: EventUnassigned, Assignee: fxPrincipal, CreatedAt: at(3)},
		{ID: epoch2, Type: EventAssigned, Assignee: fxPrincipal, CreatedAt: at(4)},
		{ID: occ2, Type: EventUnlabeled, Label: "ben:running", CreatedAt: at(5)},
	})
	for _, tc := range []struct {
		name       string
		occurrence int64
		want       int64
		ok         bool
	}{
		{name: "first publication keeps its source claim", occurrence: occ1, want: epoch1, ok: true},
		{name: "later publication uses the new claim", occurrence: occ2, want: epoch2, ok: true},
		{name: "missing occurrence", occurrence: 9999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := claimEpochAtOccurrence(events, fxPrincipal, tc.occurrence)
			if got != tc.want || ok != tc.ok {
				t.Errorf("claimEpochAtOccurrence = %d, %v; want %d, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestPublishedOccurrenceMustBeTheTrackerStateTransition(t *testing.T) {
	events := SortEvents([]Event{
		{ID: 1, Type: EventUnlabeled, Actor: fxTracker, Label: "ben:claimed", CreatedAt: at(1)},
		{ID: 2, Type: EventUnlabeled, Actor: fxTracker, Label: "ben:running", CreatedAt: at(2)},
		{ID: 3, Type: EventLabeled, Actor: fxTracker, Label: "ben:claimed", CreatedAt: at(3)},
		{ID: 4, Type: EventUnlabeled, Actor: "a-human", Label: "ben:claimed", CreatedAt: at(4)},
		{ID: 5, Type: EventUnlabeled, Actor: fxTracker, Label: fxQueue, CreatedAt: at(5)},
	})
	for _, tc := range []struct {
		name       string
		occurrence int64
		want       bool
	}{
		{name: "claimed projection cleared", occurrence: 1, want: true},
		{name: "running projection cleared", occurrence: 2, want: true},
		{name: "projection added", occurrence: 3},
		{name: "wrong actor", occurrence: 4},
		{name: "required label", occurrence: 5},
		{name: "missing event", occurrence: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := publishedOccurrenceOnLog(events, tc.occurrence, fxTracker); got != tc.want {
				t.Errorf("publishedOccurrenceOnLog = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{name: "the deployed shape", edit: func(*Config) {}},
		{name: "no principal", edit: func(c *Config) { c.Principal = "" }, want: "missing principal"},
		{name: "no tracker author", edit: func(c *Config) { c.TrackerAuthor = "" }, want: "missing tracker author"},
		{name: "no controller", edit: func(c *Config) { c.Controller = "" }, want: "missing controller"},
		{name: "no required labels", edit: func(c *Config) { c.RequiredLabels = nil }, want: "missing required labels"},
		{name: "a blank required label", edit: func(c *Config) { c.RequiredLabels = []string{fxQueue, " "} }, want: "is blank"},
		{name: "a repeated required label", edit: func(c *Config) { c.RequiredLabels = []string{fxQueue, "BEN-QUEUE"} }, want: "is repeated"},
		{name: "no queue label", edit: func(c *Config) { c.QueueLabel = "" }, want: "missing queue label"},
		{name: "queue label outside the complete set", edit: func(c *Config) { c.QueueLabel = "another-queue" }, want: "not in the complete required-label set"},
		{name: "a reserved state label", edit: func(c *Config) { c.QueueLabel = " BeN:needs-review " }, want: "reserved state-label namespace"},
		{name: "no issue", edit: func(c *Config) { c.Issue = 0 }, want: "is not positive"},
		{name: "no rounds", edit: func(c *Config) { c.RoundCap = 0 }, want: "leaves no round"},
		{name: "controller is the principal", edit: func(c *Config) { c.Controller = c.Principal }, want: "unassign itself"},
		{name: "controller is the milestone author", edit: func(c *Config) { c.Controller = c.TrackerAuthor }, want: "trigger its own rounds"},
		{name: "shared tracker and controller in an attended canary", edit: func(c *Config) {
			c.Controller = c.TrackerAuthor
			c.AllowSharedTrackerController = true
		}},
		{name: "shared controller still cannot be the principal", edit: func(c *Config) {
			c.Controller = c.Principal
			c.TrackerAuthor = c.Principal
			c.AllowSharedTrackerController = true
		}, want: "unassign itself"},
		{name: "the fixed informational label is the required one", edit: func(c *Config) { c.QueueLabel = HumanReviewLabel }, want: "would be an approval"},
		{name: "the fixed informational label is another required label", edit: func(c *Config) { c.RequiredLabels = append(c.RequiredLabels, HumanReviewLabel) }, want: "would be an approval"},
		{name: "no informational label is fine", edit: func(c *Config) { c.AddHumanReviewLabel = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fxConfig()
			tc.edit(&cfg)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestBranchIsCanonical(t *testing.T) {
	if got := fxConfig().Branch(); got != "ben/11" {
		t.Errorf("Branch = %q, want ben/11 — the ref SPEC §6.2 gives the workspace", got)
	}
	if n, ok := branchIssue("ben/11"); !ok || n != 11 {
		t.Errorf("branchIssue(ben/11) = %d, %v", n, ok)
	}
	for _, b := range []string{"main", "ben/", "ben/0", "ben/11/extra", "bens/11"} {
		if _, ok := branchIssue(b); ok {
			t.Errorf("branchIssue(%q) resolved an issue", b)
		}
	}
}
