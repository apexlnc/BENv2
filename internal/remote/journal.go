package remote

import (
	"context"
	"fmt"
	"reflect"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Journal owns the two write orderings a restart depends on, and it exists as a
// type because they are the kind of rule that decays into a comment.
//
// They point in opposite directions, which is why neither can be derived from
// the other:
//
//   - **Identity before the act.** The sandbox-scoped ProcessRef and canonical
//     request digest are on disk *before* a start may be attempted, and
//     Dispatched is set *before* the call rather than after it. A process that
//     dies inside the launch window cannot report what happened, so the record
//     has to already identify the exact retry/attach request.
//
//   - **The act before the position.** An event cursor and decoder state are
//     committed only after DurableConsumer acknowledges the raw and normalized
//     event. A crash before the local checkpoint replays an idempotency key. A
//     cursor committed first would skip evidence still buffered in memory.
//
// Nothing here starts a goroutine or touches a clock. It is the durable half of
// the boundary and its whole behaviour is which write happens when.
type Journal struct {
	store Store
	claim Claim
	rec   Record
	// loaded says the in-memory record came from the store rather than from
	// Reserve. It is what makes Resume able to refuse a journal nobody has
	// opened, instead of attaching to a zero Record.
	loaded bool
}

// OpenJournal reads the durable record for a claim cycle, or reports ErrNoRecord
// when there is none. Absence is a fact here — nothing was ever dispatched for
// this claim — which is exactly why it is a named error rather than a zero value
// (SPEC §9.10).
func OpenJournal(store Store, claim Claim) (*Journal, error) {
	if !claim.Valid() {
		return nil, fmt.Errorf("%w: %s is not a complete claim cycle", ErrClaimMismatch, claim)
	}
	rec, err := store.Load(claim)
	if err != nil {
		return nil, err
	}
	if !rec.ProcessRef().Complete() {
		return nil, fmt.Errorf("%w: record for %s has an incomplete process reference", ErrProcessMismatch, claim)
	}
	return &Journal{store: store, claim: claim, rec: rec, loaded: true}, nil
}

// Reserve writes the run identity for a fresh attempt, before anything can be
// dispatched. It is the first durable act of a remote run and the reason a later
// process can name it.
//
// Refuses an incomplete identity (ErrIdentityMissing): a record whose sandbox or
// profile revision is missing can only be attached to by guessing, and guessing
// an address is how one claim gets two runs.
func Reserve(ctx context.Context, store Store, ref ProcessRef, meta Meta) (*Journal, error) {
	if !ref.Identity.Complete() {
		return nil, fmt.Errorf("%w: %s", ErrIdentityMissing, ref.Identity.Claim)
	}
	if ref.RunID == "" {
		return nil, ErrNoRunID
	}
	if ref.RequestDigest == "" {
		return nil, fmt.Errorf("%w: process request has no digest", ErrProcessMismatch)
	}
	j := &Journal{
		store: store,
		claim: ref.Identity.Claim,
		rec: Record{
			Version:          RecordVersion,
			Identity:         ref.Identity,
			RunID:            ref.RunID,
			RequestDigest:    ref.RequestDigest,
			TemplateRevision: meta.TemplateRevision,
			PromptDigest:     meta.PromptDigest,
			Provider:         meta.Provider,
			Model:            meta.Model,
			Transcript:       meta.Transcript,
			Replay:           cloneReplay(meta.replay),
		},
		loaded: true,
	}
	if err := j.save(ctx); err != nil {
		return nil, err
	}
	return j, nil
}

// Meta is the descriptive half of a Record: what this attempt was told and by
// which agent. replay is the one internal exception: Runner supplies the
// reconstruction seed Reserve must place before dispatch, while direct callers
// cannot accidentally persist provider credentials through it.
type Meta struct {
	TemplateRevision string
	PromptDigest     string
	Provider         string
	Model            string
	Transcript       string
	// replay is populated only by Runner. Reserve remains usable by lower-level
	// crash tests that deliberately construct a journal without an invocation.
	replay *ReplaySpec
}

// Record is the journal's current view. A copy: a caller that mutated the
// journal's record directly would be changing durable state without writing it.
func (j *Journal) Record() Record {
	r := j.rec
	r.DecoderTail = append([]byte(nil), j.rec.DecoderTail...)
	r.Replay = cloneReplay(j.rec.Replay)
	return r
}

// RecordReplay ensures the non-secret reconstruction input lands before a
// dispatch can be attempted. Existing records are immutable: changing the seed
// under one ProcessRef would make restart recovery depend on which process read
// it rather than on what was originally launched.
func (j *Journal) RecordReplay(ctx context.Context, spec core.RunSpec) error {
	if !j.loaded {
		return ErrNoRecord
	}
	replay := replaySpecOf(spec)
	if j.rec.Replay != nil {
		if !reflect.DeepEqual(j.rec.Replay, replay) {
			return fmt.Errorf("%w: claim %s has different replay input", ErrProcessMismatch, j.claim)
		}
		if j.rec.Version == RecordVersion {
			return nil
		}
	}
	previousVersion := j.rec.Version
	previousReplay := j.rec.Replay
	j.rec.Version = RecordVersion
	j.rec.Replay = replay
	if err := j.save(ctx); err != nil {
		j.rec.Version = previousVersion
		j.rec.Replay = previousReplay
		return err
	}
	return nil
}

// Claim is the cycle this journal belongs to.
func (j *Journal) Claim() Claim { return j.claim }

// Dispatch marks a start as attempted and then performs it, in that order.
//
// The order is the point, so the dispatch closure is a parameter rather than the
// caller's next statement: a caller that had to remember to persist first would
// eventually not, and the failure it buys is invisible until a restart. The mark
// is durable before the closure runs, so a crash anywhere inside the closure —
// including after the backend accepted the start — leaves a record that says a
// dispatch was attempted.
//
// A second call is ErrAlreadyStarted. That is not a convenience check: it is the
// no-duplicate-dispatch rule, and it holds across processes because Dispatched
// came off the store.
func (j *Journal) Dispatch(ctx context.Context, start func(context.Context, ProcessRef) (Status, error)) (Status, error) {
	if !j.loaded {
		return Status{}, ErrNoRecord
	}
	ref := j.rec.ProcessRef()
	if !ref.Complete() {
		return Status{}, ErrNoRunID
	}
	if j.rec.Dispatched {
		return Status{}, fmt.Errorf("%w: %s (run %s)", ErrAlreadyStarted, j.claim, j.rec.RunID)
	}
	j.rec.Dispatched = true
	if err := j.save(ctx); err != nil {
		// Nothing was dispatched: the mark did not land, so a later process
		// reads "not dispatched" and may start. Rolling the in-memory flag back
		// keeps this process's view equal to the store's, which is what makes a
		// retry in the same process legal.
		j.rec.Dispatched = false
		return Status{}, err
	}
	st, err := start(ctx, ref)
	if err == nil && st.BackendRunID != "" && st.BackendRunID != j.rec.BackendRunID {
		// The backend id is the durable attachment handle, but this write remains
		// best-effort here: failing Start after the backend created a live run
		// would falsely tell the caller that nothing is running (SPEC §7.4).
		// Runner retries the write through Observe before exposing the handle.
		previous := j.rec.BackendRunID
		previousReplay := j.rec.Replay
		j.rec.BackendRunID = st.BackendRunID
		j.rec.Replay = nil
		if saveErr := j.save(ctx); saveErr != nil {
			// Observe compares against the in-memory value before writing. Restore
			// the durable view so that comparison causes the promised retry.
			j.rec.BackendRunID = previous
			j.rec.Replay = previousReplay
		}
	}
	return st, err
}

// Resume is the restart path: it reports the exact request to recover, and
// refuses to invent one.
//
// The two answers are the two worlds a restart can be in, and conflating them is
// the duplicate dispatch this package exists to prevent. `attach` true means a
// start was attempted for this id and the backend is the only thing that knows
// how it went. False means nothing was ever dispatched, so a fresh start is
// legal — and it is legal precisely because the mark is written *before* the
// call, so "not dispatched" cannot be a start that landed.
func (j *Journal) Resume() (ProcessRef, bool, error) {
	if !j.loaded {
		return ProcessRef{}, false, ErrNoRecord
	}
	ref := j.rec.ProcessRef()
	if !ref.Complete() {
		return ProcessRef{}, false, ErrNoRunID
	}
	return ref, j.rec.Dispatched, nil
}

// CommitCheckpoint records that everything up to the checkpoint has been
// accepted by DurableConsumer.
//
// Cursor, decoder tail and terminal outcome move in one replacement. A no-op
// for a sequence below the committed one: moving backwards would replay work
// BEN's durable inbox has already accepted.
func (j *Journal) CommitCheckpoint(ctx context.Context, cp Checkpoint) error {
	if !j.loaded {
		return ErrNoRecord
	}
	seq := int64(cp.Cursor)
	if seq < j.rec.Cursor {
		return nil
	}
	if seq == j.rec.Cursor && string(cp.Tail) == string(j.rec.DecoderTail) && cp.Terminal == j.rec.Terminal {
		return nil
	}
	previousCursor := j.rec.Cursor
	previousTail := j.rec.DecoderTail
	previousTerminal := j.rec.Terminal
	j.rec.Cursor = seq
	j.rec.DecoderTail = append([]byte(nil), cp.Tail...)
	j.rec.Terminal = cp.Terminal
	if err := j.save(ctx); err != nil {
		j.rec.Cursor = previousCursor
		j.rec.DecoderTail = previousTail
		j.rec.Terminal = previousTerminal
		return err
	}
	return nil
}

// Checkpoint is the durably committed event and decoder position.
func (j *Journal) Checkpoint() Checkpoint {
	return Checkpoint{
		Cursor: Cursor(j.rec.Cursor), Tail: append([]byte(nil), j.rec.DecoderTail...),
		Terminal: j.rec.Terminal,
	}
}

// Observe retains the durable backend resource id learned during start or
// attachment recovery.
func (j *Journal) Observe(ctx context.Context, st Status) error {
	if st.BackendRunID == "" || (st.BackendRunID == j.rec.BackendRunID && j.rec.Replay == nil) {
		return nil
	}
	previous := j.rec.BackendRunID
	previousReplay := j.rec.Replay
	j.rec.BackendRunID = st.BackendRunID
	// Once the permanent resource id exists, exact creation replay is no longer
	// a naming mechanism. Drop the prompt-bearing seed before event checkpoints
	// begin rewriting this record.
	j.rec.Replay = nil
	if err := j.save(ctx); err != nil {
		j.rec.BackendRunID = previous
		j.rec.Replay = previousReplay
		return err
	}
	return nil
}

// Close removes the durable record. It is called once the claim cycle is over
// and the run's termination is confirmed — never before, because a record
// removed over a possibly-live run is a run nothing can attach to.
func (j *Journal) Close() error {
	if !j.loaded {
		return ErrNoRecord
	}
	return j.store.Delete(j.claim)
}

func (j *Journal) save(ctx context.Context) error {
	// The context is checked rather than plumbed into Store: a durable write is
	// not a thing to abandon halfway, and the interesting cancellation is the
	// one that happens before it starts.
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.store.Save(j.rec)
}
