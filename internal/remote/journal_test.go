package remote_test

import (
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// The two durable orderings, each asserted by failing the write in the middle of
// it (#192).
//
// A crash test that never fails a write is a test of a happy path wearing a
// dramatic name: every ordering passes when every write succeeds. So the store
// here refuses on demand, and what each case reads afterwards is what a *later
// process* would read — which is the only reader either ordering exists for.

func testIdentity() remote.Identity {
	return remote.Identity{
		Claim:           testClaim(),
		Branch:          testBranch,
		BaseSHA:         testBaseSHA,
		SandboxID:       "sandbox-1",
		ProfileRevision: testProfile,
	}
}

// Identity before the act: a crash while the reservation is being written leaves
// nothing to attach to, and nothing was dispatched either.
//
// The pairing is the assertion. A reservation that failed and a dispatch that
// happened anyway would be the worst of the three states §9.10 names — a run
// nothing can name — so the refusal has to come back to the caller with the
// backend untouched.
func TestAFailedReservationDispatchesNothing(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	boom := errors.New("disk full")
	rig.store.SetSaveFault(func(remote.Record) error { return boom })

	_, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if !errors.Is(err, boom) {
		t.Fatalf("Start with a failing store = %v, want %v", err, boom)
	}
	if got := rig.backend.StartCalls(); got != 0 {
		t.Errorf("backend saw %d dispatches although the reservation never landed, want 0", got)
	}
	if rig.store.Has(rig.claim) {
		t.Error("a record exists although its write failed")
	}
}

// The dispatch mark lands before the backend is called, so a crash inside the
// launch window leaves a record a restart can attach to.
//
// Asserted by failing the *backend* rather than the store: the start does not
// return, and the question is what a later process reads. It must read
// "dispatched", because the alternative reading — "nothing happened" — dispatches
// a second run into a workspace that may already hold one.
func TestTheDispatchMarkSurvivesAFailedStart(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)

	journal, err := remote.Reserve(context.Background(), rig.store, testProcessRef(id, rig.run), remote.Meta{})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	boom := errors.New("the backend never answered")
	if _, err := journal.Dispatch(context.Background(), func(context.Context, remote.ProcessRef) (remote.Status, error) {
		return remote.Status{}, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Dispatch = %v, want %v", err, boom)
	}

	// What the next process reads.
	reopened, err := remote.OpenJournal(rig.store, rig.claim)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	run, dispatched, err := reopened.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if run.RunID != rig.run {
		t.Errorf("Resume run = %q, want %q", run.RunID, rig.run)
	}
	if !dispatched {
		t.Error("the record says nothing was dispatched, although a start was attempted: " +
			"a restart reading this would dispatch a second run")
	}
}

// A dispatch mark that could not be written is not a dispatch, and the in-memory
// view must agree with the store — or a retry in the same process would refuse
// itself over a mark nothing persisted.
func TestAFailedDispatchMarkLeavesTheJournalStartable(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)

	journal, err := remote.Reserve(context.Background(), rig.store, testProcessRef(id, rig.run), remote.Meta{})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	boom := errors.New("disk full")
	rig.store.SetSaveFault(func(remote.Record) error { return boom })
	called := 0
	if _, err := journal.Dispatch(context.Background(), func(context.Context, remote.ProcessRef) (remote.Status, error) {
		called++
		return remote.Status{}, nil
	}); !errors.Is(err, boom) {
		t.Fatalf("Dispatch = %v, want %v", err, boom)
	}
	if called != 0 {
		t.Fatalf("the dispatch closure ran %d times although the mark never landed", called)
	}

	rig.store.SetSaveFault(nil)
	if _, err := journal.Dispatch(context.Background(), func(context.Context, remote.ProcessRef) (remote.Status, error) {
		called++
		return remote.Status{BackendRunID: "backend-1"}, nil
	}); err != nil {
		t.Fatalf("retried Dispatch: %v", err)
	}
	if called != 1 {
		t.Errorf("the dispatch closure ran %d times, want 1", called)
	}
	if _, err := journal.Dispatch(context.Background(), func(context.Context, remote.ProcessRef) (remote.Status, error) {
		called++
		return remote.Status{}, nil
	}); !errors.Is(err, remote.ErrAlreadyStarted) {
		t.Errorf("a second Dispatch after one landed = %v, want %v", err, remote.ErrAlreadyStarted)
	}
}

func TestBackendRunIDWriteFailureLeavesObserveAbleToRetry(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	journal, err := remote.Reserve(context.Background(), rig.store, testProcessRef(id, rig.run), remote.Meta{})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	boom := errors.New("disk full")
	rig.store.SetSaveFault(func(r remote.Record) error {
		if r.BackendRunID != "" {
			return boom
		}
		return nil
	})
	st, err := journal.Dispatch(context.Background(), func(context.Context, remote.ProcessRef) (remote.Status, error) {
		return remote.Status{BackendRunID: "backend-1"}, nil
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got := journal.Record().BackendRunID; got != "" {
		t.Fatalf("in-memory backend run id after failed save = %q, want the durable empty value", got)
	}

	rig.store.SetSaveFault(nil)
	if err := journal.Observe(context.Background(), st); err != nil {
		t.Fatalf("Observe retry: %v", err)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := rec.BackendRunID; got != "backend-1" {
		t.Fatalf("durable backend run id after Observe retry = %q, want backend-1", got)
	}
}

// The act before the position, in both directions.
//
// A crash *before* the commit replays and is deduplicated — the cursor stays
// where it was and the events are delivered again to a filter that drops them. A
// crash *after* it does not replay, because the position is what "consumed"
// means. The pair is the whole rule: exactly one of them may lose work, and it is
// the one that costs a repeated translation rather than a missing event.
func TestTheCursorIsCommittedAfterTranslationAndNeverBefore(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)

	// A store that refuses every cursor write past the first event, which is
	// exactly a host that died between translating event 2 and recording it.
	rig.store.SetSaveFault(func(r remote.Record) error {
		if r.Cursor > 1 {
			return errors.New("disk full")
		}
		return nil
	})

	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run,
		remotetest.Init(testSession),
		remotetest.Text("second"),
		remotetest.Success())
	rig.backend.Quiet(rig.run)

	if got := types(collect(t, h)); !sameTypes(got, core.EventStarted, core.EventProgress) {
		t.Fatalf("events = %v, want the two events accepted before the attach checkpoint failed", got)
	}

	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("loading the record: %v", err)
	}
	if rec.Cursor != 1 {
		t.Errorf("durable cursor = %d, want 1 — the writes past it failed, so nothing may claim them", rec.Cursor)
	}

	// The degradation is reported rather than silent.
	if attempt, ok := h.(*remote.Attempt); !ok {
		t.Fatal("the handle is not a *remote.Attempt")
	} else if attempt.CommitErr() == nil {
		t.Error("a failed cursor write was not reported; an operator watching a run replay its " +
			"whole log after every restart has nothing to read")
	}

	entries := rig.consumer.Entries()
	if len(entries) < 2 || entries[1].ID != "event:2" ||
		!sameTypes(types(entries[1].Events), core.EventProgress) {
		t.Fatalf("durable consumer entries = %+v, want event 2 accepted before its cursor advanced", entries)
	}

	// A restart first reprojects the accepted durable history, then advances from
	// the reconciled checkpoint. Live delivery is at-least-once; durable consumer
	// acceptance, not an in-memory channel send, is the dedupe boundary.
	rig.store.SetSaveFault(nil)
	restarted := rig.runner(t, id)
	h2, err := restarted.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start after restart: %v", err)
	}
	if got := types(collect(t, h2)); !sameTypes(got, core.EventStarted, core.EventProgress, core.EventSucceeded) {
		t.Errorf("the recovered stream carried %v, want durable history then the new outcome", got)
	}
}

// Reserve refuses an identity that cannot be attached to later.
func TestReserveRefusesAnIncompleteIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   remote.Identity
		run  remote.RunID
		want error
	}{
		{
			name: "no sandbox",
			id: remote.Identity{
				Claim: testClaim(), Branch: testBranch, BaseSHA: testBaseSHA, ProfileRevision: testProfile,
			},
			run:  "run-1",
			want: remote.ErrIdentityMissing,
		},
		{
			name: "no profile revision",
			id: remote.Identity{
				Claim: testClaim(), Branch: testBranch, BaseSHA: testBaseSHA, SandboxID: "sandbox-1",
			},
			run:  "run-1",
			want: remote.ErrIdentityMissing,
		},
		{
			name: "no branch",
			id: remote.Identity{
				Claim: testClaim(), BaseSHA: testBaseSHA, SandboxID: "sandbox-1", ProfileRevision: testProfile,
			},
			run:  "run-1",
			want: remote.ErrIdentityMissing,
		},
		{
			name: "no trusted base",
			id: remote.Identity{
				Claim: testClaim(), Branch: testBranch, SandboxID: "sandbox-1", ProfileRevision: testProfile,
			},
			run:  "run-1",
			want: remote.ErrIdentityMissing,
		},
		{
			name: "no claim epoch",
			id: remote.Identity{
				Claim:     remote.Claim{Repository: testRepo, Issue: testIssue},
				SandboxID: "sandbox-1", ProfileRevision: testProfile,
			},
			run:  "run-1",
			want: remote.ErrIdentityMissing,
		},
		{
			name: "no run identity",
			id:   testIdentity(),
			run:  "",
			want: remote.ErrNoRunID,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := remotetest.NewMemStore()
			if _, err := remote.Reserve(context.Background(), store, testProcessRef(tc.id, tc.run), remote.Meta{}); !errors.Is(err, tc.want) {
				t.Errorf("Reserve = %v, want %v", err, tc.want)
			}
			if store.Saves() != 0 {
				t.Error("a refused reservation still wrote a record")
			}
		})
	}
}

// Opening a journal for a claim nothing was reserved for is ErrNoRecord — a fact,
// and the one that says a fresh start is legal.
func TestOpenJournalReportsAbsenceAsAFact(t *testing.T) {
	store := remotetest.NewMemStore()
	if _, err := remote.OpenJournal(store, testClaim()); !errors.Is(err, remote.ErrNoRecord) {
		t.Errorf("OpenJournal over an empty store = %v, want %v", err, remote.ErrNoRecord)
	}
	if _, err := remote.OpenJournal(store, remote.Claim{Repository: testRepo, Issue: testIssue}); !errors.Is(err, remote.ErrClaimMismatch) {
		t.Errorf("OpenJournal with no claim epoch = %v, want %v", err, remote.ErrClaimMismatch)
	}
}

// The descriptive half of the record survives the round trip: what the attempt
// was told, and which agent it was told to.
func TestReserveRecordsWhatTheAttemptWasTold(t *testing.T) {
	store := remotetest.NewMemStore()
	meta := remote.Meta{
		TemplateRevision: "tmpl-9",
		PromptDigest:     remote.PromptDigest("do the thing"),
		Provider:         "codex-exec",
		Model:            "gpt-5-codex",
		Transcript:       "transcripts/run-1.jsonl",
	}
	if _, err := remote.Reserve(context.Background(), store, testProcessRef(testIdentity(), "run-1"), meta); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	rec, err := store.Load(testClaim())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	switch {
	case rec.TemplateRevision != meta.TemplateRevision:
		t.Errorf("TemplateRevision = %q, want %q", rec.TemplateRevision, meta.TemplateRevision)
	case rec.PromptDigest != meta.PromptDigest:
		t.Errorf("PromptDigest = %q, want %q", rec.PromptDigest, meta.PromptDigest)
	case rec.Provider != meta.Provider:
		t.Errorf("Provider = %q, want %q", rec.Provider, meta.Provider)
	case rec.Model != meta.Model:
		t.Errorf("Model = %q, want %q", rec.Model, meta.Model)
	case rec.Transcript != meta.Transcript:
		t.Errorf("Transcript = %q, want %q", rec.Transcript, meta.Transcript)
	case rec.Identity != testIdentity():
		t.Errorf("Identity = %+v, want %+v", rec.Identity, testIdentity())
	}
	// The digest is over the prompt and nothing else, so an operator can prove a
	// retained prompt is the bytes this attempt sent.
	if remote.PromptDigest("do the thing") == remote.PromptDigest("do the other thing") {
		t.Error("PromptDigest collides on different prompts")
	}
}

func TestRecordingReplayInputUpgradesAnOlderJournal(t *testing.T) {
	store := remotetest.NewMemStore()
	ref := testProcessRef(testIdentity(), "run-1")
	if err := store.Save(remote.Record{
		Version: 2, Identity: ref.Identity, RunID: ref.RunID, RequestDigest: ref.RequestDigest,
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := remote.OpenJournal(store, testClaim())
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordReplay(context.Background(), testSpec()); err != nil {
		t.Fatalf("RecordReplay: %v", err)
	}
	rec, err := store.Load(testClaim())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Version != remote.RecordVersion || rec.Replay == nil {
		t.Fatalf("replay record = %+v, want version %d with a seed", rec, remote.RecordVersion)
	}
}
