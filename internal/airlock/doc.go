// Package airlock is the foundation of BEN's opt-in v2 execution backend: the
// Airlock client that sits behind #192's substrate seams (#194, #46).
//
// # What this package is, and is not
//
// It is the *only* place in BEN that knows Airlock exists. Everything above it
// programs against internal/remote's closed BEN types — Identity, ProcessRef,
// Envelope, Status — and everything below it is HTTP, JSON and the frozen v2
// contract in the Airlock repository's docs/v2. A sandbox id and a profile
// revision cross that line as opaque strings; a Run, a RunEvent, an ErrorCode
// and an Idempotency-Key never do. SPEC §3 invariant 6 holds by construction:
// the orchestrator cannot name a wire type because no interface it reaches
// mentions one.
//
// It is not a second control loop, not a workspace strategy (SPEC §6.1), and not
// a verifier. Workspaces implements remote.WorkspaceBackend, Processes
// implements remote.ProcessBackend, and Hooks implements remote.HookExec; the
// composition into a core.AgentRunner is remote.New's, unchanged. #205 owns the
// workspace strategy and daemon routing that make this package dispatchable.
//
// # Dependency posture
//
// Stdlib only — net/http, encoding/json, crypto/tls — and internal/arch enforces
// it (noThirdParty). Importing Airlock as a Go module, or adding an HTTP or SSE
// dependency, is the AGENTS.md rule-3 sign-off this package deliberately makes
// visible as a failing test rather than a review comment.
//
// # The three durable facts this package owns
//
// #192's remote.Journal owns BEN's run identity and its event cursor. Three
// facts it deliberately does not own are Airlock's, and they are persisted here
// before the act they name (Store):
//
//   - Which canonical endpoint and non-secret credential-source binding the
//     addresses below belong to. Airlock scopes idempotency by endpoint, tenant
//     and subject; replay under another binding is a new side effect.
//   - Which sandbox a claim cycle acquired, and at which immutable profile
//     revision. remote.WorkspaceBackend.Attach is handed a Claim and nothing
//     else, and Airlock has no list-by-label route, so a daemon with no record
//     could only re-derive a sandbox by *creating* one — which is what Attach
//     must never do.
//   - Which Airlock run_id a ProcessRef resolved to. Airlock addresses a run by
//     its own durable handle, not by the client's idempotency key, and the key's
//     replay window is bounded (24h) while the run id is permanent.
//
// The binding and replay fence are written before each request whose response
// would otherwise be the only evidence, which is the same ordering rule Journal
// states: identity before the act.
//
// # Evidence
//
// Airlock's three termination-evidence fields map onto remote's three states
// unchanged, and nothing is inferred across them (statusOf). A 202 from
// signalRun is delivery acceptance, never termination; a run in `lost` reports
// domain quiet `unknown`, so remote.MayReuse answers false and the workspace is
// not touched. An Airlock success response is never evidence of a pushed branch:
// publication is verified daemon-side (#193, SPEC §9.7).
//
// docs/AIRLOCK.md is the operator-facing half; docs/REMOTE.md is the boundary
// this package lands against.
package airlock
