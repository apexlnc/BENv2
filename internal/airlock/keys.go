package airlock

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// Idempotency keys, and why every one of them is derived rather than generated.
//
// Airlock's guarantee is at-most-once *per key*, and its explicit instruction to
// a client that loses a response is to replay with the same key and never mint a
// new one. A generated key would therefore have to be persisted before the
// request it names — one more durable write, on the critical path, whose only
// job is to remember a random number.
//
// Deriving it from facts BEN has already persisted removes that write entirely.
// Every input below is either in remote.Record (written by Journal.Reserve
// before any dispatch) or in this package's own sandbox record (written before
// acquire), so the key is *recomputable* after a crash rather than recoverable.
// A daemon that lost every file it wrote would still not create a second
// sandbox for a claim whose record it can rebuild from the tracker.
//
// The key scope Airlock applies — tenant, subject, method, resolved path, key —
// is what lets these be short and readable: the sandbox path already
// distinguishes two runs, so a run key need only distinguish two dispatches
// within one sandbox.

// keyPrefix namespaces every key BEN mints, so a key in an Airlock audit trail
// is attributable to this daemon rather than to whoever else shares the tenant.
const keyPrefix = "ben"

// keyDigestHex is how many hex characters of SHA-256 a derived key carries. 32
// is 128 bits: this is a collision domain scoped to one tenant and subject, and
// the whole key must stay inside the contract's 255-character ceiling alongside
// a readable prefix.
const keyDigestHex = 32

// sandboxKey is the createSandbox idempotency key for one claim cycle.
//
// The claim cycle rather than the issue, for remote.Claim's reason: a workspace
// acquired under the previous claim carries the previous verification base, so a
// key scoped to the issue would hand a fresh claim a tree nobody re-approved.
// Branch, base and profile are inputs too, because they are in the request body
// — Airlock fingerprints the body and refuses the same key with a different one,
// so a key that ignored them would turn an edited workflow into
// `idempotency_key_conflict` instead of a fresh sandbox.
func sandboxKey(claim remote.Claim, branch, baseSHA, profile string) string {
	return derive("sbx", claim.String(), branch, baseSHA, profile)
}

// runKey is the startRun idempotency key for one exact dispatch.
//
// ProcessRef is already the durable address of one dispatch — sandbox, BEN's run
// id, and a canonical digest of the immutable ProcessSpec — so the key is a
// function of it and nothing else. That is what makes the lost-response replay
// in remote.Runner.dispatch land on the stored result: the same ref recomputes
// the same key, and a *different* request under the same BEN run id changes the
// digest and is therefore a different key rather than a silent attach.
func runKey(ref remote.ProcessRef) string {
	return derive("run", ref.Identity.SandboxID, string(ref.RunID), ref.RequestDigest)
}

// hookKey is the startRun idempotency key for one lifecycle-hook firing.
//
// Attempt is part of it because a hook fires once per attempt and the phases
// repeat across a retry; the request digest covers the script and its timeout,
// so an edited hook is a new firing rather than a replay of the old one.
func hookKey(ref remote.HookRef) string {
	return derive("hook", ref.Identity.SandboxID, string(ref.ID), string(ref.Phase),
		strconv.Itoa(ref.Attempt), ref.RequestDigest)
}

// signalKey is the signalRun idempotency key for one signal on one run.
//
// Keyed by the signal and not by the call, deliberately. BEN's stop ladder may
// ask for the same interrupt repeatedly — a tick that finds the run still
// unquiet asks again — and the contract's replay path turns that into "return
// the original signal_id, append nothing". A per-call key would instead append a
// durable signal record every tick and exhaust `max_signals_per_run`, which is
// the one quota a client can spend against itself.
func signalKey(runID string, sig Signal) string {
	return derive("sig", runID, string(sig))
}

// derive builds `ben.<kind>.<32 hex>` over NUL-joined inputs.
//
// NUL rather than a printable separator: every input here is an identifier or a
// digest, and a separator one of them could contain would let two distinct
// tuples flatten onto one key. The contract's key alphabet does not admit NUL,
// which is why it is hashed rather than spelled.
func derive(kind string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return keyPrefix + "." + kind + "." + hex.EncodeToString(sum[:])[:keyDigestHex]
}
