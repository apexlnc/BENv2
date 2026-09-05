// Package reviewctl is BEN's trusted review controller: the half of the
// bounded `code → machine review → revise` loop that holds credentials and
// writes to the forge.
//
// It was `cmd/benreview` under [#11] and is a package under [#204], because the
// controller now has two callers rather than one. The daemon runs it on its
// ordinary poll/sweep lifecycle — that is the availability mechanism — and an
// operator command drives one issue for a dry run or a manual reconcile. Both
// use the same reducer, the same forge client and the same driver, which is the
// point of it being a package: a recovery path an operator exercises and a
// recovery path production uses must not be two implementations.
//
// # What did not change
//
// Everything policy. internal/review remains the authority: its reducer, its
// `ben:review` / `ben:route-intent` / `ben:route` markers, its identity
// separation, its round cap, its exact-head and exact-base rules, and its
// closed [review.StepKind] set — which is the permission model written where
// the code has to obey it. [Forge] is still six reads and five writes with no
// approve, no merge, no close, no required-label addition and no `ben:*` write,
// because there is still no method for one.
//
// # What changed
//
// Where the model runs. #11 ran it as a child of a GitHub Actions job that held
// a repository credential; #204 runs it through BEN's own process backend
// (internal/reviewrun) — a credential-stripped local child, or one durable
// Airlock run in the issue's workspace-cycle sandbox. There is no repository
// workflow, no `workflow_dispatch`, and no model process on a GitHub runner.
//
// Two consequences are worth stating because they are what the split buys:
//
//   - The controller **captures** the subject. It resolves and revalidates the
//     issue, the approval event, the occurrence, the claim epoch, the canonical
//     pull request and both diff endpoints, fetches the exact three-dot diff
//     itself, and hands the reviewer bounded, opaque input. The reviewer is never
//     given a forge credential and never asked which pull request is current.
//   - The controller **validates** the answer. One closed verdict, or nothing is
//     published. Missing, malformed, ambiguous or untrusted results authorize no
//     forge mutation at all.
//
// [#11]: https://github.com/srhg-ai-7cef3f93/ben/issues/11
// [#204]: https://github.com/srhg-ai-7cef3f93/ben/issues/204
package reviewctl
