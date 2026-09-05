package airlock

import (
	"context"
	"fmt"
	"time"
)

// idempotencyReplayWindow is Airlock's retention period for completed keyed
// results. BEN starts each fence before its request, conservatively: once this
// interval ends it cannot distinguish a replay from a second side effect.
const idempotencyReplayWindow = 24 * time.Hour

// prepareStart writes or reads the durable fence for one startRun address.
// The returned context bounds the client's internal retries as well as the
// caller's first attempt. A binding with RunID is permanent and needs no replay
// window; callers address that resource directly instead of using the context.
func prepareStart(
	ctx context.Context, client *client, store Store, substrate SubstrateBinding, address string,
) (StartBinding, keyedCredential, context.Context, context.CancelFunc, error) {
	auth, err := client.keyedAuth(ctx)
	if err != nil {
		return StartBinding{}, keyedCredential{}, nil, nil, err
	}
	binding, err := store.ReserveBinding(address, substrate, auth.principalBinding, time.Now().UTC())
	if err != nil {
		return StartBinding{}, keyedCredential{}, nil, nil, err
	}
	if binding.RunID != "" {
		return binding, keyedCredential{}, ctx, func() {}, nil
	}
	if binding.Refusal != nil {
		// A refused address holds no unanswered start to fence: the body was
		// refused before the key was claimed, so there is nothing to replay and
		// nothing that could become a second side effect. Start decides whether
		// to answer an unchanged body from the record or to renew the
		// reservation for a different one (Store.RenewStart).
		return binding, auth, ctx, func() {}, nil
	}
	if binding.StartAttemptedAt.IsZero() {
		return StartBinding{}, keyedCredential{}, nil, nil, fmt.Errorf("%w: %s has an old unanswered start with no replay fence",
			ErrStartReplayExpired, address)
	}
	deadline := binding.StartAttemptedAt.Add(idempotencyReplayWindow)
	if !time.Now().Before(deadline) {
		return StartBinding{}, keyedCredential{}, nil, nil, fmt.Errorf("%w: %s may no longer replay safely",
			ErrStartReplayExpired, address)
	}
	replayCtx, cancel := context.WithDeadline(ctx, deadline)
	return binding, auth, replayCtx, cancel, nil
}
