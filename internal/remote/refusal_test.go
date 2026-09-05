package remote_test

import (
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// A backend that refuses to admit the body has answered, and the answer is
// "nothing was started" (#284). The runner must hand that to the orchestrator
// as a launch that never happened — not as an ambiguity-preserving handle over
// a run that does not exist, and not as a replay of a request the backend has
// already refused.
func TestARefusedStartIsALaunchThatNeverHappened(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	rig.backend.SetStartRefusal("payload_too_large", "inline stdin exceeds the profile's limit", 65536)

	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if h != nil {
		t.Fatalf("Start returned a handle %v over a run the backend refused to create", h)
	}
	if !errors.Is(err, remote.ErrProcessRefused) || !errors.Is(err, remote.ErrNoProcess) {
		t.Fatalf("Start = %v, want a refusal that also reads as never accepted", err)
	}
	var refused *remote.ProcessRefusal
	if !errors.As(err, &refused) || refused.Code != "payload_too_large" || refused.LimitBytes != 65536 {
		t.Fatalf("refusal = %+v, want the backend's code and limit", refused)
	}
	if got := rig.backend.StartCalls(); got != 1 {
		t.Fatalf("Start calls = %d, want one: a definite refusal is never replayed", got)
	}
	if got := rig.backend.RunCreations(); got != 0 {
		t.Fatalf("run creations = %d, want none", got)
	}
	rec, loadErr := rig.store.Load(rig.claim)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if !rec.Dispatched || rec.BackendRunID != "" {
		t.Fatalf("journal after a refusal = %+v; want the attempt marked and no backend id", rec)
	}

	// A later Start for the same run identity re-offers the exact request and
	// gets the same answer — from the backend's own record, creating nothing —
	// and still no handle, so nothing can be mistaken for a live run.
	h, err = rig.runner(t, id).Start(context.Background(), testSpec())
	if h != nil || !errors.Is(err, remote.ErrProcessRefused) {
		t.Fatalf("second Start = (%v, %v), want no handle and the same refusal", h, err)
	}
	if got := rig.backend.RunCreations(); got != 0 {
		t.Fatalf("run creations after the re-offer = %d, want none", got)
	}
}
