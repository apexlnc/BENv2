package airlock

import (
	"errors"
	"fmt"
	"time"
)

// The named refusals of the Airlock backend (AGENTS.md conventions). Each is a
// sentinel a test asserts on rather than a message it matches, and each names a
// *fact about the backend* rather than a transport failure — the distinction
// SPEC §9.10 rests on, since only a fact may be acted upon.
var (
	// ErrNotOwned refuses a sandbox this principal does not own. Airlock answers
	// 403 inside the owning tenant and 404 across tenants, deliberately, so
	// cross-tenant existence is unobservable; both are a durable record pointing
	// at something that is not ours, and both must park rather than retry.
	ErrNotOwned = errors.New("airlock: this principal does not own the recorded sandbox")

	// ErrProfileRevision refuses a sandbox whose immutable profile revision is
	// not the one the claim pinned. The sandbox id matching while the revision
	// moved is exactly the mutable-profile hazard remote.Identity.ProfileRevision
	// exists to close: the same id naming a different world.
	ErrProfileRevision = errors.New("airlock: sandbox profile revision does not match the pinned revision")

	// ErrSandboxUnusable refuses a sandbox that exists in a state no attempt can
	// run in — failed, deleting or deleted. A refusal rather than a wait: those
	// are verdicts, and polling one is how a client waits forever for an answer
	// it already has.
	ErrSandboxUnusable = errors.New("airlock: sandbox is in a state no run can be dispatched into")

	// ErrDeletionUnconfirmed reports a deleteSandbox that returned but whose
	// three evidence fields have not all reached `confirmed`. The durable record
	// is deliberately retained: until compute release, volume destruction and
	// record tombstoning are each confirmed, the caller may not assume the data
	// is gone, and a record removed here is a sandbox nothing can finish
	// deleting.
	ErrDeletionUnconfirmed = errors.New("airlock: sandbox deletion is not confirmed")

	// ErrCreateReplayExpired refuses to repeat an ambiguously answered
	// createSandbox after Airlock's 24-hour idempotency record may have expired.
	// Past that boundary the same key can create a second sandbox, so the
	// durable reservation is retained for an operator to reconcile.
	ErrCreateReplayExpired = errors.New("airlock: the sandbox create replay window has expired")

	// ErrStartReplayExpired refuses to repeat an ambiguously answered startRun
	// after Airlock's idempotency record may have expired. Hooks and agent runs
	// use the same route and the same bounded guarantee; both retain their
	// write-ahead start record for an operator to reconcile.
	ErrStartReplayExpired = errors.New("airlock: the run start replay window has expired")

	// ErrSubstrateBinding refuses to use durable state written for another
	// Airlock endpoint, credential definition or runtime principal. Idempotency
	// is scoped by the endpoint's tenant and subject, so replaying an old key
	// after either moves is a fresh side effect rather than recovery.
	ErrSubstrateBinding = errors.New("airlock: durable state belongs to a different substrate")

	// ErrNoSandboxRecord says this daemon holds no durable sandbox record for a
	// claim cycle. Distinct from remote.ErrNoWorkspace, which is the answer
	// Attach gives its caller: this one is the store's, and the translation
	// between them is Workspaces'.
	ErrNoSandboxRecord = errors.New("airlock: no durable sandbox record for this claim cycle")

	// ErrNoRunBinding is the store-local absence of any write-ahead start record.
	// Processes maps it to remote.ErrNoProcess at its public seam. An existing
	// binding with no RunID is instead remote.ErrProcessUnresolved: Start crossed
	// the write-ahead fence and may have committed remotely.
	ErrNoRunBinding = errors.New("airlock: no Airlock run is bound to this process reference")

	// ErrUnexpectedRun refuses a run whose sandbox or identifier is not the one
	// addressed. Airlock scopes every run under its sandbox precisely so one
	// consumer cannot attach to another's; this is the client-side half of that.
	ErrUnexpectedRun = errors.New("airlock: response names a different run or sandbox than the request")

	// ErrConfig refuses an unusable client configuration before any request is
	// made — a missing base URL, a non-HTTPS endpoint, credentials in the URL,
	// an absent auth source or profile.
	ErrConfig = errors.New("airlock: backend configuration is unusable")

	// ErrUnready reports a backend that cannot serve this workflow here and now:
	// an unreachable control plane, a rejected token, or a profile the tenant is
	// not approved for.
	ErrUnready = errors.New("airlock: backend is not ready to run here")
)

// APIError is one Airlock error response, carrying the code a client routes on.
//
// The message is included. The contract makes it unstable and human-readable
// but also binds it: it never contains prompt text, process output, argv beyond
// argv[0], environment values, or secrets. That is what makes it safe to log,
// and it is the only field that says *why* — so dropping it would leave an
// operator with a code and nothing else.
//
// Details is deliberately not carried whole. Its property set is closed by the
// schema, and the three entries BEN routes on are lifted into typed fields; a
// map of everything else is a place for an implementation to have put the
// offending env values, which is what the closed set exists to prevent.
type APIError struct {
	Status     int
	Code       ErrorCode
	Message    string
	RequestID  string
	RetryAfter time.Duration
	// ActiveRunID is details.active_run_id on run_conflict: the run already
	// holding the sandbox's single active slot.
	ActiveRunID string
	// RequestedAfter and OldestAvailableSeq are details.requested_after and
	// details.oldest_available_seq on cursor_too_old: the cursor the statement
	// is about, and the oldest sequence still retained.
	//
	// Pointers, because presence is what BEN routes on rather than value. The
	// pair is what turns an expiry into a *range* the daemon may durably accept
	// as data loss (remote.RetentionGap); a response that omits or mangles
	// either one has stated only that something is missing, which is an
	// unrepairable remote.ErrEventGap. Zero would be indistinguishable from a
	// backend that named sequence 0.
	RequestedAfter     *int64
	OldestAvailableSeq *int64
	// LimitBytes is details.limit_bytes on payload_too_large: the exceeded
	// limit, which is what makes the refusal actionable rather than a status.
	LimitBytes int64
	// ExpectedOffset is details.expected_offset on stdin_offset_mismatch: the
	// byte position the next stdin write must use. A pointer for
	// RequestedAfter's reason — presence is what the stdin writer routes on,
	// and zero is a legitimate offset.
	ExpectedOffset *int64
}

func (e *APIError) Error() string {
	return fmt.Sprintf("airlock: %s (HTTP %d, request %s): %s", e.Code, e.Status, e.RequestID, e.Message)
}

// Retryable reports whether re-sending the identical request — reusing the
// original Idempotency-Key where the route requires one — may eventually
// succeed.
//
// Read from the code, not from the response's `retryable` flag. The contract
// fixes the flag *from* the code by schema, so the code is the narrower and
// more trustworthy of the two, and a server that got the flag wrong cannot make
// this client hot-loop on a permanent refusal.
func (e *APIError) Retryable() bool {
	switch e.Code {
	case CodeIdempotencyKeyInFlight, CodeSandboxNotReady, CodeSandboxSuspended,
		CodeRunConflict, CodeRunNotReadyForStdin, CodeRateLimited, CodeQuotaExceeded,
		CodeInternal, CodeDependencyUnavailable:
		return true
	}
	return false
}

// codeOf returns the Airlock error code behind err, and whether err is an
// Airlock error at all. An error that is not one reports "" and false, never a
// code a caller could route on by accident — the same shape core.CredentialFailure
// uses, and for the same reason.
func codeOf(err error) (ErrorCode, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return "", false
	}
	return apiErr.Code, true
}

// hasCode reports whether err is an Airlock error with one of the given codes.
func hasCode(err error, codes ...ErrorCode) bool {
	code, ok := codeOf(err)
	if !ok {
		return false
	}
	for _, c := range codes {
		if code == c {
			return true
		}
	}
	return false
}

// retryable reports whether err is worth re-sending. A transport error is:
// every route in the contract is either naturally idempotent or keyed, so an
// unanswered request is always safe to repeat. An Airlock error is retryable
// only where its code says so.
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	return true
}
