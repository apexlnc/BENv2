package remote

import (
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// A retention range authorizes a cursor advance over bytes BEN never saw, so
// recovery accepts it only with the same outcome the live path requires. This
// is independent of the writer: a syntactically valid but semantically corrupt
// durable log must fail closed before Sequencer.Restore sees the range.
func TestReconcileRecoveryRequiresTheRetentionOutcome(t *testing.T) {
	gap := EventGap{From: 1, To: 2}
	failed := core.Event{Type: core.EventFailed, Reason: core.FailureCrashed}
	succeeded := core.Event{Type: core.EventSucceeded}
	for _, tc := range []struct {
		name  string
		items []Consumption
	}{
		{
			name: "gap without an outcome",
			items: []Consumption{{
				ID: "gap", Checkpoint: Checkpoint{Cursor: 2}, Gap: &gap,
			}},
		},
		{
			name: "gap carrying success",
			items: []Consumption{{
				ID: "gap", Checkpoint: Checkpoint{Cursor: 2, Terminal: true},
				Gap: &gap, Events: []core.Event{succeeded},
			}},
		},
		{
			name: "gap after success",
			items: []Consumption{
				{ID: "success", Checkpoint: Checkpoint{Terminal: true}, Events: []core.Event{succeeded}},
				{ID: "gap", Checkpoint: Checkpoint{Cursor: 2, Terminal: true}, Gap: &gap},
			},
		},
		{
			name: "gap carrying the wrong failure",
			items: []Consumption{{
				ID: "gap", Checkpoint: Checkpoint{Cursor: 2, Terminal: true}, Gap: &gap,
				Events: []core.Event{{Type: core.EventFailed, Reason: core.FailureTimeout}},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := reconcileRecovery(Checkpoint{}, tc.items); !errors.Is(err, ErrEventConflict) {
				t.Fatalf("reconcileRecovery error = %v, want ErrEventConflict", err)
			}
		})
	}

	if _, _, gaps, err := reconcileRecovery(Checkpoint{}, []Consumption{{
		ID: "gap", Checkpoint: Checkpoint{Cursor: 2, Terminal: true},
		Gap: &gap, Events: []core.Event{failed},
	}}); err != nil || len(gaps) != 1 || gaps[0] != gap {
		t.Fatalf("valid gap recovery = (%v, %v), want one accepted range", gaps, err)
	}

	next := EventGap{From: 3, To: 4}
	if _, _, gaps, err := reconcileRecovery(Checkpoint{}, []Consumption{
		{
			ID: "gap-1", Checkpoint: Checkpoint{Cursor: 2, Terminal: true},
			Gap: &gap, Events: []core.Event{failed},
		},
		{
			ID: "gap-2", Checkpoint: Checkpoint{Cursor: 4, Terminal: true},
			Gap: &next,
		},
	}); err != nil || len(gaps) != 2 || gaps[0] != gap || gaps[1] != next {
		t.Fatalf("repeated valid gap recovery = (%v, %v), want both accepted ranges", gaps, err)
	}
}
