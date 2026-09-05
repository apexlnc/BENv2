// Package reviewrun is the substrate-neutral reviewer-execution boundary
// behind BEN's review controller (#204, against [#46]).
//
// [#11] gave the controller a *reducer* (internal/review) and a *deployment*
// (a GitHub Actions workflow holding a repository credential). #204 keeps the
// first and replaces the second. The reducer, the marker vocabulary, the
// identity checks and the bounded routing rules are unchanged and stay the
// policy authority; what changes is where the model runs and who is trusted
// with what while it does.
//
// This package owns that half and nothing else: it turns one *already
// validated* review subject into one closed verdict, through a durable process
// on whichever substrate the daemon is configured for. It publishes nothing,
// reads no tracker, and holds no forge credential — the trusted controller
// (internal/reviewctl) does all of that on either side of a call to [Session.Review].
//
// # Two substrates, one policy
//
// Selecting local or Airlock execution changes execution and recovery
// capability, never reducer or forge semantics. So the seam is a single closed
// interface, [Executor], and both implementations answer the same four
// questions: start this exact request idempotently, attach to what a previous
// start produced, hand me the durable output after this cursor, and tell me
// what is true right now.
//
//   - [NewLocal] runs a credential-stripped child on the daemon's host, under a
//     composed environment and a home directory BEN owns rather than the
//     operator's (#241). It is the development and rollback path and claims
//     neither sandbox isolation nor cross-process restart durability — a
//     dispatched local run whose response was lost is [ErrRunUnresolved], never a
//     second child. [Local]'s own doc states exactly which isolation it does and
//     does not provide.
//   - [NewRemote] runs one durable Airlock process in the issue's
//     workspace-cycle sandbox, over the #192 seams. A lost start response is
//     resolved by replaying the same idempotency address, and a restart
//     reattaches by backend run id and committed cursor.
//
// # What is durable, and in which order
//
// The forge markers remain the durable *policy* record (docs/REVIEW.md); this
// package's [Record] is the durable *execution* record, and the two orderings
// are internal/remote's, for its reasons:
//
//   - **Identity before the act.** The derived run identity, the pinned sandbox
//     and profile revision, and the canonical request digest are on disk before
//     a start may be attempted, and the dispatch mark is written before the call
//     rather than after it. A process that dies inside the launch window cannot
//     report what happened, so the record has to already name the exact request
//     to replay.
//   - **The act before the position.** Accepted output bytes and the cursor
//     that admits them move in one replacement of one record. A reader that saw
//     the cursor advanced past bytes it had not retained would resume from a
//     position it cannot reconstruct the verdict from.
//
// # What authorizes nothing
//
// Everything ambiguous. A missing verdict, a malformed one, two of them, a
// sequence gap, a replayed sequence carrying different bytes, a profile
// revision that moved under a pinned run, a sandbox that is not the cycle's, a
// run whose execution domain is not positively observed quiet: each is a named
// error and each leaves the occurrence unrouted for the next sweep. None of
// them is a verdict, and in particular none of them is `clean`.
//
// [#11]: https://github.com/srhg-ai-7cef3f93/ben/issues/11
// [#46]: https://github.com/srhg-ai-7cef3f93/ben/issues/46
package reviewrun
