package orchestrator

import (
	"context"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Credential-failure routing (SPEC §9.7, §9.8, §10.2).
//
// A credential source can fail at five places, and the class it reports —
// transient, permanent, or the inert zero — controls the route at exactly
// **two** of them: §9.8's automatic attempt retry at `Prepare` and `Start`, and
// §9.7's verification routing. At both, only an explicit
// core.CredentialTransient retries; unknown and permanent park.
//
// It controls **no routing** on the tracker's own paths. A poll retries next
// tick and an owed write stays owed whatever the class says, because those
// retries exist for reasons unrelated to why this particular credential failed:
// the queue must not lose a write, and the loop must keep reading. The class is
// still *read* there, for log severity — a non-transient failure logs at error
// with the authority named, so an operator reads a wrong trust policy off the
// log instead of inferring it from a silent stall. Reporting on a class is not
// routing by it.
//
// The three routes are named separately on purpose. §9.8's backoff covers
// `Prepare` and `Start` only; tracker retries ride the poll tick; verification
// retries ride the poll tick *inside* `verifying`. Folding them into one
// sentence is what would let a reader apply attempt backoff to a poll.

// credentialTransient reports whether err is a credential failure the loop may
// retry — an explicit transient classification and nothing else.
//
// The zero class is not transient, and that is the whole design: a class that
// defaulted to retryable would make every unclassified error retry by omission,
// and an omission is exactly what a new kind or a new error path produces.
func credentialTransient(err error) bool {
	class, ok := core.CredentialFailure(err)
	return ok && class == core.CredentialTransient
}

// credentialParks reports whether err is a credential failure that must park
// rather than spend the retry budget: unknown or permanent.
//
// A misconfigured trust policy fails identically every time, so retrying it
// spends three attempts to reach the same park with less to say about it.
func credentialParks(err error) bool {
	class, ok := core.CredentialFailure(err)
	return ok && class != core.CredentialTransient
}

// logCredentialFailure records a credential failure at the severity its class
// earns, naming the authority and never the token.
//
// Called from the routes that do *not* route on the class as well as the ones
// that do — the tracker's reads and its owed writes — because "a permanent
// credential error must not change what the tracker does" and "an operator must
// be able to see one" are different requirements.
func (o *Orchestrator) logCredentialFailure(msg, issue string, err error, attrs ...any) bool {
	class, ok := core.CredentialFailure(err)
	if !ok {
		return false
	}
	args := []any{"issue", issue, "authority", core.CredentialAuthority(err), "class", class.String(), "error", err}
	args = append(args, attrs...)
	if class == core.CredentialTransient {
		o.log.Warn(msg, args...)
		return true
	}
	o.log.Error(msg, args...)
	return true
}

// failLaunch routes a failure that ended an attempt before the agent reported
// anything: a workspace `Prepare`, or a `Start` refused before launch.
//
// Three routes, and the credential ones are the two new ones:
//
//   - a **transient** credential failure is `FailureCredential`, retryable, so
//     §9.8 backs off and retries;
//   - an **unknown or permanent** one parks `ben:needs-review` through §9.2's
//     `preparing → needs-review` edge, without spending the remaining automatic
//     retry budget;
//   - anything else keeps the routing it had — the provider's own retryable
//     classification, and `launch_error`.
//
// The attempt is *ended and recorded* on every one of them, park included: a
// dispatched preparation is an attempt in the §9.6 accounting, so parking must
// not un-count it. What a park does not do is advance `Attempt`, which is where
// the retry budget is spent (the backoff timer, not this).
func (o *Orchestrator) failLaunch(ctx context.Context, r *Record, err error, what string, retryable bool) {
	detail := what + ": " + err.Error()
	o.logCredentialFailure("credential failure while "+what, r.Issue.Identifier, err)
	switch {
	case credentialTransient(err):
		o.failAttempt(ctx, r, core.FailureCredential, true, detail)
	case credentialParks(err):
		o.parkCredential(ctx, r, detail)
	default:
		o.failAttempt(ctx, r, core.FailureLaunchError, retryable, detail)
	}
}

// parkCredential ends the attempt and parks the record for a human.
//
// It records the attempt first, and with no §7.3 reason: the agent did not fail
// — it never ran — and `credential` in that taxonomy means *transient*
// specifically (amendment 8), so labelling a permanent misconfiguration with it
// would make a park read as a retryable run failure in the outcome log.
func (o *Orchestrator) parkCredential(ctx context.Context, r *Record, detail string) {
	o.attemptEnded(ctx, r)
	o.recordAttempt(r, "", VerdictUnknown)
	// This dispatch is still an accounting attempt, but it has no run outcome:
	// the agent never started. Do not relabel an older attempt's outcome and
	// account with this attempt number, or carry that two-attempt-old history
	// into a human's re-queue as though it described the park.
	r.lastOutcome = ""
	r.forgetAccount()
	o.enterNeedsReview(ctx, r, detail, "")
}

// driveVerifyRetry re-issues a verification whose publish credential failed
// transiently, whose protected-file publication awaits approval, or whose
// mandatory tracker epoch read failed (SPEC §8.5, §9.7).
//
// Once per poll tick, from `verifying`, with the attempt neither ended nor
// recorded and no verdict routed. §9.7's fail-closed rule covers evidence that
// contradicts or cannot be established; a credential that could not be obtained
// establishes neither, and the evidence itself is unchanged on git and the
// tracker — so parking would spend a human's attention on weather.
//
// The record keeps its `ben:claimed` label and its claim throughout, which is
// what makes the retry free: §9.10 reads exactly that at the next start if the
// daemon dies mid-wait.
func (o *Orchestrator) driveVerifyRetry(ctx context.Context) {
	for _, r := range o.records {
		if r.State != StateVerifying || !r.verifyRetry || r.pending > 0 || r.suspended || r.exiting() {
			continue
		}
		r.verifyRetry = false
		o.beginVerify(ctx, r)
	}
}
