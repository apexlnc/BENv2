// Package remotews is SPEC §6.1's workspace strategy for a claim whose attempt
// runs on a v2 execution substrate: the concrete provider over
// remote.WorkspaceBackend that [#194] left to [#205], and the thing whose
// absence made `ben run` refuse a configured Airlock endpoint.
//
// It is the *sibling* of internal/workspace, not a layer over it. Both answer
// the same seam — the one the orchestrator declares — and neither imports the
// other's lifecycle. What differs is where the tree is and therefore what a fact
// about it is worth: internal/workspace reads a worktree the daemon created, and
// this package reads nothing at all inside the sandbox, because everything in
// there is authored by the thing being judged (SPEC §3.5).
//
// # Two clocks, and why there are two
//
// v1 has one claim-scoped identity: the tracker-native id of the assignment
// event, which pins the verification base (core.Workspace.ClaimEpoch, SPEC §6.2).
// A remote workspace needs a second one, and conflating them costs something in
// each direction:
//
//   - The **workspace cycle** is repository + issue + the standing human
//     approval-label event (SPEC §6.7's approval act). It selects the physical
//     sandbox. It has to outlive an assignment, because a controller-driven
//     unassignment and reassignment inside one approval is an ordinary revision
//     round — and a sandbox keyed by the assignment would throw away the tree
//     that round is about to revise.
//   - The **verification epoch** stays the assignment event. It pins the trusted
//     base a publication must descend from, daemon-side (#193), and it has to
//     move with the assignment, because that is the boundary §9.7's "advanced
//     past the base" is measured across.
//
// So a reassignment within one approval remints the base and reattaches the same
// sandbox at the same pinned profile revision, while a revocation followed by a
// *new* approval is a new workspace cycle: a different sandbox address, which is
// what makes attaching the retained one from the previous cycle unexpressible
// rather than merely discouraged.
//
// # What a prepare does
//
// In order, and the order is the contract:
//
//  1. Refuse while the previous attempt's execution domain is not positively
//     observed quiet (remote.MayReuse). A possibly-live foreign process must
//     never share a workspace with a replacement (SPEC §9.8).
//  2. Mint or read the claim's trusted base through the daemon-side fact source,
//     *before* anything can run — a base taken afterwards is a base the run may
//     already have moved (#193, core.RemoteClaim).
//  3. Acquire or reattach the sandbox for the workspace cycle.
//  4. Fire `after_create` when this prepare created the sandbox.
//  5. Restore the canonical branch to the independently observed remote head —
//     BEN's own script, not a workflow hook (remote.HookRestore). Whatever a
//     reviewer or a previous attempt left inside the sandbox is neither a
//     publication fact nor revision input; the only tree the next attempt may
//     build on is the one the daemon read from the canonical remote.
//  6. Fire `before_run`.
//
// # What it deliberately does not do
//
// It never reads a fact out of the sandbox, never treats a backend success as
// publication evidence (that is #193's daemon-side check, selected by
// verify.SelectPublication), and never composes this host's environment into a
// request (harness.RemoteEnviron).
//
// [#194]: https://github.com/srhg-ai-7cef3f93/ben/issues/194
// [#205]: https://github.com/srhg-ai-7cef3f93/ben/issues/205
package remotews
