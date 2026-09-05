package reviewrun

import "errors"

// The named refusals (AGENTS.md conventions). Every one of them fails closed:
// no verdict is produced, so the controller's occurrence stays unrouted and the
// daemon's next sweep looks again. None of them is ever reported as `clean`.
var (
	// ErrSubject refuses a review subject that is not completely named. The
	// subject is captured by trusted BEN code from the forge and revalidated
	// before and after publication; one that arrives here incomplete is a caller
	// that skipped that, and deriving a run identity from it would key a durable
	// record on facts nobody checked.
	ErrSubject = errors.New("reviewrun: the review subject is not completely named")

	// ErrNoRecord says the durable store holds no run for an identity. A fact —
	// nothing was ever dispatched — as against a store that would not answer.
	ErrNoRecord = errors.New("reviewrun: no durable record for this review run")

	// ErrRecordState refuses a record this package could not have written: an
	// unknown version, a key that does not match the file, a torn cursor. No
	// auto-repair, for remotews.ErrCycleState's reason — the record is an
	// address, and repairing an address dispatches into the wrong run.
	ErrRecordState = errors.New("reviewrun: the durable review-run record is in an unexpected state")

	// ErrRunMismatch refuses a durable identity whose recorded request digest,
	// sandbox or subject does not equal the one being asked for. Same address,
	// different request is a conflict rather than an accidental attach
	// (remote.ErrProcessMismatch, applied to a review run).
	ErrRunMismatch = errors.New("reviewrun: this review-run identity already names a different request")

	// ErrProfileDrift refuses a run whose backend profile revision has moved
	// under it. A sandbox id that still matches while the world it names has
	// changed is a different sandbox wearing the same id (remote.Identity).
	ErrProfileDrift = errors.New("reviewrun: the sandbox profile revision moved under this review run")

	// ErrSandboxMismatch refuses a run whose recorded sandbox is not the one the
	// issue's current workspace cycle selects. It is the cross-cycle attach a
	// revocation-and-reapproval must never make: the previous cycle's tree is
	// not this cycle's subject.
	ErrSandboxMismatch = errors.New("reviewrun: the recorded sandbox is not this workspace cycle's")

	// ErrRunUnresolved is a dispatch whose outcome nobody can establish: a lost
	// start response that replay could not recover, or a local run this process
	// did not launch. The identity and the dispatch mark are retained; a later
	// sweep replays the same request rather than minting a second run.
	ErrRunUnresolved = errors.New("reviewrun: this review run was dispatched and its outcome cannot be established")

	// ErrRunRefused is a dispatch the executor definitively declined to admit —
	// a prompt over the substrate's stdin bound, a rejected environment key, a
	// malformed request. Nothing was started and the same request would be
	// refused again, so it is neither replayed as ErrRunUnresolved nor read as
	// a verdict. The record retains the refusal; a later sweep re-offers the
	// exact request, and the executor answers without a dispatch unless the
	// request or the way it delivers one has changed (#284).
	ErrRunRefused = errors.New("reviewrun: the executor refused to admit this reviewer invocation; nothing was started")

	// ErrRunIncomplete is the ordinary "not yet": the run is live and its output
	// stream is not sealed. Not a failure and not a verdict.
	ErrRunIncomplete = errors.New("reviewrun: the review run has not finished")

	// ErrNotQuiet refuses a new review run while an earlier run in the same
	// workspace cycle has not been positively observed quiet. Two agents in one
	// sandbox is the hazard SPEC §9.8 exists for, and "the phase is not running"
	// is one step from correct and fails open (remote.MayReuse).
	ErrNotQuiet = errors.New("reviewrun: an earlier run in this workspace cycle is not confirmed quiet")

	// ErrEventGap refuses admitting output beyond the next expected sequence.
	// Advancing across missing evidence commits BEN to bytes it never saw, and
	// the verdict is extracted from exactly those bytes.
	ErrEventGap = errors.New("reviewrun: the backend event stream has a gap")

	// ErrEventConflict refuses a replayed sequence whose payload differs from
	// the one already admitted. Replay is how recovery works; replay that says
	// something else is a different run's stream.
	ErrEventConflict = errors.New("reviewrun: a replayed backend event carries different bytes")

	// ErrOutputOverflow refuses a run whose output exceeds what a record may
	// retain. Truncated bytes cannot be proven to contain the sole verdict, so a
	// bounded reader that quietly kept the tail would be guessing.
	ErrOutputOverflow = errors.New("reviewrun: the review run produced more output than one record may retain")

	// ErrOutputTruncated refuses a backend stream that explicitly says bytes
	// were dropped. A retained prefix can contain exactly one valid envelope and
	// still be followed by a second one in the missing suffix.
	ErrOutputTruncated = errors.New("reviewrun: the backend truncated the review run's output")

	// ErrNoVerdictBlock is a sealed run whose output carries no machine verdict.
	// Silence from a reviewer is not a clean bill (review.ErrNoVerdict).
	ErrNoVerdictBlock = errors.New("reviewrun: the reviewer stated no machine verdict")

	// ErrAmbiguousVerdict is output carrying more than one verdict block.
	// Picking either would make the route depend on which the model wrote first,
	// and the diff it read is attacker-influenced text.
	ErrAmbiguousVerdict = errors.New("reviewrun: the reviewer stated more than one machine verdict")

	// ErrCredentialLeak refuses an invocation whose environment names a
	// credential the reviewer must never hold. Refused at composition, so the
	// request that would have carried it is never serialized.
	ErrCredentialLeak = errors.New("reviewrun: refusing to hand the reviewer a credential it must not hold")

	// ErrOwnedEnv refuses a local passthrough naming a child variable the
	// executor composes itself — HOME, the XDG directories, git's global
	// configuration. The reviewer's home is BEN's per run (#241), so a value
	// named here would be silently overwritten, and the operator who named it
	// wrote it expecting the opposite.
	ErrOwnedEnv = errors.New("reviewrun: refusing a passthrough that names a variable the local reviewer composes")

	// ErrNoRun is an executor asked to attach to something it does not have.
	// Returned by the local executor across a process boundary, where "attach"
	// has no meaning at all.
	ErrNoRun = errors.New("reviewrun: the executor holds no such run")
)

// RefusedError is ErrRunRefused with the executor's reason attached: a short
// stable code and a sanitized statement, both safe to persist in the record
// and to publish on the issue. It is the shape an executor returns; the session
// reads it back with RefusalOf.
type RefusedError struct {
	Reason string
	Detail string
	Cause  error
}

func (e *RefusedError) Error() string {
	msg := ErrRunRefused.Error() + " (" + e.Reason + ")"
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *RefusedError) Unwrap() error { return e.Cause }

func (e *RefusedError) Is(target error) bool { return target == ErrRunRefused }

// RefusalOf reads the reason out of a refusal. The second result is false for
// every other error, including a bare ErrRunRefused with no reason attached —
// a refusal nobody can act on is not one worth recording.
func RefusalOf(err error) (Refusal, bool) {
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason == "" {
		return Refusal{}, false
	}
	return Refusal{Reason: refused.Reason, Detail: refused.Detail}, true
}
