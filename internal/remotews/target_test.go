package remotews_test

import (
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
)

type refusingCycleStore struct {
	remotews.Store
	err error
}

func (s refusingCycleStore) SaveCycle(remotews.Cycle) error { return s.err }

func TestLegacyTargetlessCycleIsNonAuthorizingUntilALaterEpoch(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)
	cycle, err := r.store.LoadCycle(first.Key)
	if err != nil {
		t.Fatal(err)
	}
	cycle.Version = 1
	cycle.TargetBranch = ""
	if err := r.store.SaveCycle(cycle); err != nil {
		t.Fatal(err)
	}

	restarted := r.restart()
	legacyState, err := restarted.provider.ClaimBase(context.Background(), r.issue)
	if !errors.Is(err, remotews.ErrClaimTargetUnrecorded) {
		t.Fatalf("ClaimBase(legacy) = %v, want ErrClaimTargetUnrecorded", err)
	}
	if legacyState.State != core.ClaimBasePinned || legacyState.Epoch != 11 ||
		legacyState.BaseSHA != first.BaseSHA || legacyState.TargetBranch != "" {
		t.Fatalf("ClaimBase(legacy) state = %+v, want the non-authorizing epoch/base for upgrade", legacyState)
	}
	if _, err := restarted.prepare(2, 11); !errors.Is(err, remotews.ErrClaimTargetUnrecorded) {
		t.Fatalf("PrepareClaim(same legacy epoch) = %v, want ErrClaimTargetUnrecorded", err)
	}
	if err := restarted.begin(11); !errors.Is(err, remotews.ErrClaimTargetUnrecorded) {
		t.Fatalf("BeginClaimBase(same legacy epoch) = %v, want ErrClaimTargetUnrecorded", err)
	}
	boom := errors.New("cycle replacement unavailable")
	failed := restarted.restartWithStore(refusingCycleStore{Store: r.store, err: boom})
	if err := failed.begin(12); !errors.Is(err, boom) {
		t.Fatalf("BeginClaimBase(failed legacy upgrade) = %v, want %v", err, boom)
	}
	afterFailure, err := failed.provider.ClaimBase(context.Background(), r.issue)
	if !errors.Is(err, remotews.ErrClaimTargetUnrecorded) || afterFailure != legacyState {
		t.Fatalf("ClaimBase after failed legacy upgrade = %+v, %v; want unchanged %+v and ErrClaimTargetUnrecorded",
			afterFailure, err, legacyState)
	}

	if err := restarted.begin(12); err != nil {
		t.Fatalf("BeginClaimBase(later epoch): %v", err)
	}
	next, err := restarted.prepare(1, 12)
	if err != nil {
		t.Fatalf("PrepareClaim(later epoch): %v", err)
	}
	if next.TargetBranch != "main" || next.BaseSHA == "" {
		t.Fatalf("later workspace = %+v, want a complete main pin", next)
	}
	upgraded, err := restarted.store.LoadCycle(next.Key)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Version != remotews.CycleVersion || upgraded.TargetBranch != "main" {
		t.Fatalf("upgraded cycle = %+v", upgraded)
	}
}
