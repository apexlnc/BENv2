package remote

import "errors"

// The named refusals of the v2 boundary (AGENTS.md conventions). Every one of
// them is a refusal a test asserts on rather than a message it matches.
var (
	// ErrNoWorkspace says the backend holds no workspace for this claim cycle.
	// It is the attach-side answer that lets a restart tell "nothing was ever
	// acquired" from "the backend would not answer", which are the two sides of
	// §9.10's absence rule: only the first is a fact.
	ErrNoWorkspace = errors.New("remote: no workspace for this claim cycle")

	// ErrNotQuiet refuses a workspace operation over a run whose termination is
	// unconfirmed. It is the boundary's whole fail-closed posture in one error:
	// a possibly-live foreign process must never share a workspace with a
	// replacement (SPEC §9.8), and "the backend did not answer" is not evidence
	// that it went away.
	ErrNotQuiet = errors.New("remote: run termination is unconfirmed; the workspace may not be touched")

	// ErrClaimMismatch refuses an operation whose claim cycle is not the one the
	// durable record was written for. Every backend operation is idempotent
	// *per claim cycle*, so an epoch that moved is a different subject, not a
	// retry of this one.
	ErrClaimMismatch = errors.New("remote: operation names a different claim cycle than the durable record")

	// ErrNoRunID refuses a start with no externally persisted run identity. The
	// identity has to exist before the launch can be attempted, or a crash in
	// the launch window leaves a run nothing can name (see Journal).
	ErrNoRunID = errors.New("remote: run identity must be persisted before a start is attempted")

	// ErrNoProcess is the backend's definitive statement that Start never
	// crossed its durable acceptance fence for an exact ProcessRef. This is the
	// only process absence that may be read as "never started"; it is distinct
	// from an unanswered Start and from a previously accepted run that can no
	// longer be reached.
	ErrNoProcess = errors.New("remote: backend has no process for this reference")

	// ErrProcessUnresolved says Start crossed the backend's durable acceptance
	// fence, but no permanent backend run id was learned. The request may or may
	// not have been accepted. Exact replay or a backend-authoritative resource
	// observation may resolve it; it is never termination or workspace-reuse
	// evidence by itself.
	ErrProcessUnresolved = errors.New("remote: process start outcome is unresolved")

	// ErrReplayUnavailable says an unanswered Start has no durable, non-secret
	// RunSpec from which the current adapter can reconstruct and verify the
	// exact request. It is an ambiguity-retaining refusal, never permission to
	// mint a replacement run.
	ErrReplayUnavailable = errors.New("remote: process start replay input is unavailable")

	// ErrProcessUnavailable says the backend previously accepted the process
	// and supplied its permanent id, but can no longer return the addressed
	// resource. Deletion, tombstoning and access hiding all have this shape, so
	// it says nothing about process reaping or domain quiet.
	ErrProcessUnavailable = errors.New("remote: accepted process is unavailable")

	// ErrProcessRefused is the backend's definitive statement that it refused
	// to admit this exact request before committing anything — malformed input,
	// a profile limit exceeded, a rejected environment key. Airlock treats
	// these as pre-claim outcomes: nothing is created and nothing is stored
	// under the idempotency key, so the same request would be refused again
	// and a different request under the same address is a fresh first use.
	// Carried by ProcessRefusal, which also satisfies ErrNoProcess: "never
	// accepted" is exactly what a refusal establishes, and it is the reading
	// that lets every consumer of a confirmed absence retire the dispatch
	// instead of holding it as possibly live.
	ErrProcessRefused = errors.New("remote: backend refused to admit the process; nothing was started")

	// ErrProcessMismatch refuses reuse of a sandbox/run address with a
	// different identity or canonical request.
	ErrProcessMismatch = errors.New("remote: process reference does not match the durable request")

	// ErrAlreadyStarted refuses a second low-level dispatch for one run identity.
	// Runner recovery either attaches by a known backend id or replays the exact
	// idempotent Start request; it never calls Dispatch again.
	ErrAlreadyStarted = errors.New("remote: this run identity has already been dispatched")

	// ErrEventGap refuses a batch that skips a backend sequence or contains a
	// durable envelope the backend adapter cannot decode. Both are missing
	// evidence, and a cursor advanced over either would commit BEN to an event it
	// never saw.
	ErrEventGap = errors.New("remote: backend event sequence has a gap")

	// ErrRetentionGap is a backend's measured statement that a range of its
	// event log expired under BEN's cursor. It is the one gap BEN may advance
	// over, and only by durably recording the range and failing the attempt in
	// the same act (RetentionGap, Attempt): the provider output in that range is
	// gone, so nothing read afterwards may be translated into success.
	ErrRetentionGap = errors.New("remote: backend event retention expired under the cursor")

	// ErrEventConflict refuses a replayed sequence whose payload differs from
	// the one already consumed. A replay is expected and deduplicated; a
	// *different* event under a sequence already committed means the backend's
	// log is not the log BEN read, and no dedupe rule can repair that.
	ErrEventConflict = errors.New("remote: backend replayed a sequence with a different payload")

	// ErrFrameTooLarge is the remote equivalent of the local harness scanner
	// ceiling: an unterminated provider record may not grow without bound.
	ErrFrameTooLarge = errors.New("remote: provider stream line exceeds the framing limit")

	// ErrIdentityMissing refuses a durable record missing any part of its claim,
	// branch, trusted base, sandbox, or immutable profile revision.
	ErrIdentityMissing = errors.New("remote: durable record has an incomplete workspace identity")

	// ErrHookFailed reports a lifecycle hook that failed where §5.2.6 makes the
	// failure fatal. It mirrors workspace.ErrHookFailed deliberately: the
	// containment rules are the workflow's, not the substrate's.
	ErrHookFailed = errors.New("remote: lifecycle hook failed")

	// ErrNoHook is the backend's definitive statement that it never accepted a
	// dispatch for an exact durable hook reference.
	ErrNoHook = errors.New("remote: backend has no hook process for this reference")

	// ErrHookMismatch refuses reuse of a hook id for a different workspace,
	// phase, attempt, script, or timeout.
	ErrHookMismatch = errors.New("remote: hook reference does not match the durable request")

	// ErrNoRecord says the durable store holds nothing for this claim cycle.
	ErrNoRecord = errors.New("remote: no durable record for this claim cycle")
)

// ProcessRefusal is ErrProcessRefused with the backend's reason attached: the
// stable code a client routes on, the sanitized message, and the byte limit
// when the code names one.
//
// It is the one absence that carries a *cause*, because it is the one a human
// has to act on: a lost response resolves itself and a tombstone is a fact
// about the past, but a refused request stays refused until somebody changes
// the request or the profile, and the message is what tells them which.
type ProcessRefusal struct {
	// Code is the backend's stable failure identifier — `payload_too_large`,
	// `env_rejected`, `invalid_request`.
	Code string
	// Message is the backend's sanitized statement. Airlock's contract binds
	// it to carry no stdin bytes, environment values or argv beyond argv[0],
	// so it is safe to persist and to publish.
	Message string
	// LimitBytes is the exceeded limit when the code names one, else zero.
	LimitBytes int64
	// Cause is the underlying backend error, retained for errors.As.
	Cause error
}

func (r *ProcessRefusal) Error() string {
	msg := "remote: backend refused to admit the process (" + r.Code + ")"
	if r.Message != "" {
		msg += ": " + r.Message
	}
	return msg
}

func (r *ProcessRefusal) Unwrap() error { return r.Cause }

// Is makes a refusal both ErrProcessRefused and ErrNoProcess. The second is
// deliberate and load-bearing: a refused request created nothing, so every
// path that reads ErrNoProcess as "retire the dispatch, nothing is live" is
// correct for it, while a caller that needs the narrower fact asks for
// ErrProcessRefused.
func (r *ProcessRefusal) Is(target error) bool {
	return target == ErrProcessRefused || target == ErrNoProcess
}
