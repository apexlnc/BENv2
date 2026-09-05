package remote_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

func TestStreamProcessAndDomainLifecycleAreIndependent(t *testing.T) {
	t.Run("stream seals before process reap and domain quiet", func(t *testing.T) {
		rig := newRig(t)
		id := rig.acquire(t)
		h, err := rig.runner(t, id).Start(context.Background(), testSpec())
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		rig.backend.Emit(rig.run, remotetest.Success())
		if got := (<-h.Events()).Type; got != core.EventSucceeded {
			t.Fatalf("event = %v, want succeeded", got)
		}
		select {
		case _, ok := <-h.Events():
			if !ok {
				t.Fatal("event stream closed on terminal publication before the backend stream sealed")
			}
			t.Fatal("unexpected event after terminal outcome")
		default:
		}
		rig.backend.Complete(rig.run)
		for range h.Events() {
		}

		select {
		case <-h.Done():
			t.Fatal("Done closed when only the event stream was sealed")
		default:
		}
		if got := h.Probe(context.Background()); got != core.TerminationUnconfirmed {
			t.Fatalf("Probe before domain quiet = %v, want unconfirmed", got)
		}

		rig.backend.Reap(rig.run)
		<-h.Done()
		if got := h.Probe(context.Background()); got != core.TerminationUnconfirmed {
			t.Fatalf("Probe after process reap but before domain quiet = %v, want unconfirmed", got)
		}
		rig.backend.SetDomainQuiet(rig.run)
		if got := h.Probe(context.Background()); got != core.TerminationConfirmed {
			t.Fatalf("Probe after domain quiet = %v, want confirmed", got)
		}
	})

	t.Run("process reaps before stream seal", func(t *testing.T) {
		rig := newRig(t)
		id := rig.acquire(t)
		h, err := rig.runner(t, id).Start(context.Background(), testSpec())
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		rig.backend.Reap(rig.run)
		<-h.Done()
		select {
		case _, ok := <-h.Events():
			if !ok {
				t.Fatal("event stream closed merely because the direct process reaped")
			}
		default:
		}

		rig.backend.Emit(rig.run, remotetest.Success())
		rig.backend.Complete(rig.run)
		var got []core.EventType
		for ev := range h.Events() {
			got = append(got, ev.Type)
		}
		if !sameTypes(got, core.EventSucceeded) {
			t.Fatalf("events = %v, want succeeded after Done", got)
		}
	})
}

func TestRecoveryFailureAfterDispatchPreservesAControllableHandle(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	recoveryErr := errors.New("durable inbox unavailable")
	rig.consumer.SetRecoverFault(recoveryErr)

	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil || h == nil {
		t.Fatalf("Start = (%v, %v), want a non-nil degraded handle and nil error", h, err)
	}
	if got := rig.backend.RunCreations(); got != 1 {
		t.Fatalf("backend run creations = %d, want 1", got)
	}
	if !rig.backend.Live(rig.run) {
		t.Fatal("backend run is not live after attachment recovery failed")
	}
	attempt, ok := h.(*remote.Attempt)
	if !ok {
		t.Fatalf("handle type = %T, want *remote.Attempt", h)
	}
	if got := attempt.CommitErr(); !errors.Is(got, recoveryErr) {
		t.Fatalf("CommitErr = %v, want %v", got, recoveryErr)
	}
	if _, ok := <-h.Events(); ok {
		t.Fatal("degraded handle's event stream remained open without a recovery pump")
	}
	if got := h.Stop(context.Background(), core.StopDiscard); got != core.TerminationConfirmed {
		t.Fatalf("Stop = %v, want confirmed", got)
	}
	if got := rig.backend.StopCalls(rig.run); got != 1 {
		t.Fatalf("backend Stop calls = %d, want 1", got)
	}
}

func TestAmbiguousStartIsResolvedInsideStart(t *testing.T) {
	t.Run("response lost after dispatch returns a live handle", func(t *testing.T) {
		rig := newRig(t)
		id := rig.acquire(t)
		boom := errors.New("response lost")
		rig.backend.SetStartFault(boom, true)
		h, err := rig.runner(t, id).Start(context.Background(), testSpec())
		if err != nil || h == nil {
			t.Fatalf("Start = (%v, %v), want a live handle and nil error", h, err)
		}
		if got := rig.backend.StartCalls(); got != 2 {
			t.Fatalf("Start calls = %d, want the original request and its exact replay", got)
		}
		if got := rig.backend.AttachCalls(); got != 0 {
			t.Fatalf("Attach calls = %d, want 0: the missing response contained no backend run id", got)
		}
		rig.backend.Emit(rig.run, remotetest.Success())
		rig.backend.Quiet(rig.run)
		if got := types(collect(t, h)); !sameTypes(got, core.EventSucceeded) {
			t.Fatalf("events = %v, want succeeded", got)
		}
	})

	t.Run("response lost before creation is resolved by the same request", func(t *testing.T) {
		rig := newRig(t)
		id := rig.acquire(t)
		boom := errors.New("response lost before the first request committed")
		rig.backend.SetStartFault(boom, false)
		h, err := rig.runner(t, id).Start(context.Background(), testSpec())
		if err != nil || h == nil {
			t.Fatalf("Start = (%v, %v), want a handle from the exact replay", h, err)
		}
		rec, loadErr := rig.store.Load(rig.claim)
		if loadErr != nil {
			t.Fatalf("Load: %v", loadErr)
		}
		if !rec.Dispatched {
			t.Fatal("exact replay cleared the durable dispatch identity")
		}
		if got := rig.backend.StartCalls(); got != 2 {
			t.Fatalf("Start calls = %d, want one unknown request plus its exact replay", got)
		}
		if got := rig.backend.AttachCalls(); got != 0 {
			t.Fatalf("Attach calls = %d, want 0", got)
		}
		rig.backend.Quiet(rig.run)
		h.Stop(context.Background(), core.StopDiscard)
	})

	t.Run("unavailable replay never returns nil over a possibly live run", func(t *testing.T) {
		rig := newRig(t)
		id := rig.acquire(t)
		rig.backend.SetStartFault(errors.New("response lost"), true)
		rig.backend.SetStartFault(errors.New("replay unavailable"), false)
		h, err := rig.runner(t, id).Start(context.Background(), testSpec())
		if err != nil || h == nil {
			t.Fatalf("Start = (%v, %v), want a non-nil ambiguity-preserving handle", h, err)
		}
		rec, loadErr := rig.store.Load(rig.claim)
		if loadErr != nil {
			t.Fatalf("Load: %v", loadErr)
		}
		if !rec.Dispatched {
			t.Fatal("ambiguous replay cleared the dispatch mark")
		}
		if rec.BackendRunID != "" {
			t.Fatalf("ambiguous response recorded backend run id %q, want none", rec.BackendRunID)
		}
		if !rig.backend.Live(rig.run) {
			t.Fatal("the ambiguously started run was not retained")
		}
		if got := rig.backend.StartCalls(); got != 2 {
			t.Fatalf("Start calls = %d, want original plus unavailable replay", got)
		}
		if got := rig.backend.AttachCalls(); got != 0 {
			t.Fatalf("Attach calls = %d, want 0", got)
		}
		h.Stop(context.Background(), core.StopDiscard)
	})
}

func TestUnansweredStartCanReplayAfterRunnerRestart(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	firstLost := errors.New("first start response unavailable")
	replayLost := errors.New("same-process replay unavailable")
	rig.backend.SetStartFault(firstLost, false)
	rig.backend.SetStartFault(replayLost, false)

	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil || h == nil {
		t.Fatalf("Start = (%v, %v), want an ambiguity-preserving handle", h, err)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load after unanswered Start: %v", err)
	}
	if rec.Replay == nil || !rec.Dispatched || rec.BackendRunID != "" {
		t.Fatalf("unanswered Start record = %+v, want a durable replay seed and no backend id", rec)
	}

	// A new Runner has none of the first one's RunSpec in memory. ResolveStart
	// must rebuild the exact request from the encoded journal and receive the
	// one permanent id without minting a different BEN run identity.
	st, err := rig.runner(t, id).ResolveStart(context.Background(), rec.ProcessRef())
	if err != nil {
		t.Fatalf("ResolveStart after restart: %v", err)
	}
	if st.BackendRunID == "" || rig.backend.RunCreations() != 1 || rig.backend.StartCalls() != 3 {
		t.Fatalf("resolved status = %+v, creations = %d, starts = %d; want one effect across three attempts",
			st, rig.backend.RunCreations(), rig.backend.StartCalls())
	}
	resolved, err := rig.store.Load(rig.claim)
	if err != nil || resolved.BackendRunID != st.BackendRunID || resolved.Replay != nil {
		t.Fatalf("resolved journal = %+v, %v; want backend id %s and no replay seed",
			resolved, err, st.BackendRunID)
	}
	h.Stop(context.Background(), core.StopDiscard)
}

func TestRestartReplayRefusesChangedProviderInputWithoutPersistingIt(t *testing.T) {
	const firstSecret = "provider-secret-that-must-not-be-persisted"
	rig := newRig(t)
	id := rig.acquire(t)
	rig.backend.SetStartFault(errors.New("request did not reach backend"), false)
	rig.backend.SetStartFault(errors.New("same-process replay did not reach backend"), false)

	h, err := rig.runnerWithEnv(t, id, map[string]string{"AGENT_API_KEY": firstSecret}).Start(
		context.Background(), testSpec())
	if err != nil || h == nil {
		t.Fatalf("Start = (%v, %v), want an ambiguity-preserving handle", h, err)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatal(err)
	}
	body, err := remote.EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(firstSecret)) {
		t.Fatal("durable replay seed contains the provider invocation credential")
	}

	before := rig.backend.StartCalls()
	_, err = rig.runnerWithEnv(t, id, map[string]string{"AGENT_API_KEY": "rotated-secret"}).ResolveStart(
		context.Background(), rec.ProcessRef())
	if !errors.Is(err, remote.ErrProcessMismatch) {
		t.Fatalf("ResolveStart after provider rotation = %v, want %v", err, remote.ErrProcessMismatch)
	}
	if got := rig.backend.StartCalls(); got != before {
		t.Fatalf("mismatched replay made %d backend calls, want none", got-before)
	}
	// The handle over an unresolved address is still reading, and reading an
	// address the backend cannot resolve is a failure it reconnects from rather
	// than a verdict (#275). Stop is what ends it, here as in the cases above.
	h.Stop(context.Background(), core.StopDiscard)
}

func TestProcessReferencesAreSandboxScopedAndRequestExact(t *testing.T) {
	backend := remotetest.New(testProfile)
	makeRun := func(t *testing.T, claim remote.Claim, run remote.RunID, prompt string) (remote.ProcessRef, remote.ProcessSpec) {
		t.Helper()
		id, err := backend.Workspaces().Acquire(context.Background(), remote.AcquireRequest{
			Claim: claim, Branch: testBranch, BaseSHA: testBaseSHA,
		})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		spec := remote.ProcessSpec{Identity: id, Argv: []string{"agent"}, Stdin: []byte(prompt), Limits: testSpec().Limits}
		digest, err := remote.ProcessRequestDigest(spec)
		if err != nil {
			t.Fatalf("ProcessRequestDigest: %v", err)
		}
		return remote.ProcessRef{Identity: id, RunID: run, RequestDigest: digest}, spec
	}

	ref1, spec1 := makeRun(t, testClaim(), "same-run-id", "one")
	claim2 := remote.Claim{Repository: testRepo, Issue: "43", Epoch: testEpoch + 1}
	ref2, spec2 := makeRun(t, claim2, "same-run-id", "two")
	if _, err := backend.Start(context.Background(), ref1, spec1); err != nil {
		t.Fatalf("Start ref1: %v", err)
	}
	if _, err := backend.Start(context.Background(), ref2, spec2); err != nil {
		t.Fatalf("Start ref2: %v", err)
	}
	if ref1.Identity.SandboxID == ref2.Identity.SandboxID {
		t.Fatal("test did not establish two sandbox scopes")
	}
	changedSpec := spec1
	changedSpec.Stdin = []byte("different request under the old digest")
	if _, err := backend.Start(context.Background(), ref1, changedSpec); !errors.Is(err, remote.ErrProcessMismatch) {
		t.Fatalf("Start with same ref/different spec = %v, want %v", err, remote.ErrProcessMismatch)
	}

	wrong := ref1
	wrong.RequestDigest = ref2.RequestDigest
	for name, call := range map[string]func() error{
		"attach": func() error {
			_, err := backend.Attach(context.Background(), wrong, "backend-"+ref1.Identity.SandboxID+"-"+string(ref1.RunID))
			return err
		},
		"status": func() error { _, err := backend.Status(context.Background(), wrong); return err },
		"events": func() error { _, err := backend.Events(context.Background(), wrong, 0); return err },
		"stdin":  func() error { return backend.Stdin(context.Background(), wrong, []byte("x")) },
		"stop": func() error {
			_, err := backend.Stop(context.Background(), wrong, remote.StopRequest{Mode: core.StopDiscard})
			return err
		},
		"wait": func() error { _, err := backend.Wait(context.Background(), wrong); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, remote.ErrProcessMismatch) {
				t.Fatalf("%s with mismatched digest = %v, want %v", name, err, remote.ErrProcessMismatch)
			}
		})
	}
}
