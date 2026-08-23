// Package integration is B12's §12.3 invariant suite: SPEC §3's design
// invariants as executable end-to-end scenarios.
//
// What makes these *integration* tests rather than a fourth copy of the
// orchestrator's package tests is what is real in them. The loop is real, and so
// is the strict configuration loader, the hot-reload watcher, the Liquid prompt
// layer the loop renders through, and the §9.7 publish-evidence checker with its
// three legs. What is faked is the world outside the process — the tracker, the
// worktree provider, and the agent runner — because §12.3 requires the whole
// suite to be green in CI with no network, no subprocesses, and no wall-clock
// waits. internal/fake supplies those three, and AGENTS.md's rule about them
// governs here more than anywhere else in the repo: a fake that invents a
// guarantee the real component does not make is worse than a missing test,
// because B12's entire surface is fakes.
//
// # Barriers, not durations
//
// Every wait in this package is a barrier on a fact the fixture can actually
// produce — a state reached, a milestone posted, a dispatch cycle begun. None is
// a sleep, and none asserts a negative over one. #100, #103 and #116 were each
// some form of "a test asserted a negative over a sleep" or "a fixture could not
// produce the fact it waited on", and an integration suite is the easiest place
// in the repo to grow more of them. Where an invariant *is* a negative — no
// second dispatch, no release, no `done` — the barrier is a positive fact that
// must land first (a further tick, a milestone, a confirmed stop), and the
// negative is asserted after it.
//
// # Coverage map for SPEC §12.3
//
// Not every invariant's strongest assertion belongs in this package, and the map
// says where each one lives so that "is §12.3 covered?" is answerable by reading
// one list rather than by grepping. An entry naming another package is a
// deliberate decision that the invariant is asserted better at that boundary,
// not an omission.
//
//  1. kill -9 → restart → converge
//     HERE, in two tests, because §12.3-1 asks for two things.
//     TestAKilledDaemonRecoversAndConverges is convergence and its condition:
//     both answers §9.10's run probe can give — the agent that died with the
//     daemon, and the one whose process group outlived it, retained and
//     dispatching nothing until it is confirmed gone.
//     TestARestartFinishesTheProjectionItDiedInsideOf is "any point": the three
//     windows §12.3-1 names between the writes of a multi-write projection —
//     after the assignment and before ben:claimed, mid-`done` with the labels
//     cleared and the publish comment unposted, and mid-`failed` with the label
//     set and the claim unreleased — plus the boundary *inside* one projection
//     that BUILD.md's acceptance adds, where add-before-remove leaves two
//     `ben:*` labels standing and the ordered events, not the label set, decide.
//     Each is driven by the loop's own writes, with only the one write that
//     defines the window made to fail — except the last, which no failing write
//     can produce, and where the fixture performs the half that landed.
//     The classifier half is model-checked in
//     orchestrator/recover_classify_test.go; these are the driver reaching those
//     verdicts through real writes.
//
//  2. two daemons race one issue
//     HERE — TestARestartArbitratesAClaimAnotherPartyAlsoHolds, over §9.10 gate
//     2's three verdicts: our claim retained, only our own assignment released,
//     and the race nothing can order, which parks with a loud operator error
//     rather than guessing. The *live*-claim race is asserted against the real
//     adapter in tracker/github/adapter_test.go —
//     TestClaimRaceHasExactlyOneWinner, TestReleaseNeverRemovesAnotherParty,
//     TestClaimYieldsWhenTheRaceCannotBeOrdered — because §8.4's
//     refuse-write-read-back sequence is the adapter's.
//
//  3. success claimed with no commits → needs-review, never done
//     HERE — TestAClaimOfSuccessNeverPublishesOnEvidenceShortOfComplete, over
//     every evidence shape short of §9.7-complete rather than the one the
//     acceptance criterion names.
//
//  4. retry preserves agent commits (never -B)
//     HERE — TestRetryPreservesTheWorkspaceAndItsClaimTimeBase. The -B refusal
//     itself is a git fact, asserted against real repositories in
//     workspace/workspace_test.go.
//
//  5. unconfirmed stop retains the claim; no workspace is ever shared
//     HERE — TestAnUnconfirmedStopRetainsTheClaimAndTheWorkspace and
//     TestALaggingQueueReadNeverDispatchesAnIssueTwice.
//
//  6. invalid hot-reload blocks dispatch, spares in-flight runs
//     HERE — TestAnInvalidReloadBlocksDispatchAndSparesTheRunInFlight, over a
//     real config.Watch on a real file.
//
//  7. kill from every non-terminal state → failed(killed)
//     orchestrator/transitions_test.go asserts the edge over all 81 state
//     pairs, against a restatement of §9.2 independent of the implementation's
//     map. v1 exposes no daemon-level kill, so there is no wider shape to
//     drive; what is integration-shaped is that a *drain* is not a kill, which
//     is HERE as TestShutdownSuspendsRatherThanFailing.
//
//  8. secrets never in argv or the child env beyond the allowlist
//     HERE — TestNoCredentialReachesTheRunSpecOrThePrompt, at the one boundary
//     the loop owns. The argv and child-env halves are the adapters',
//     asserted in agent/harness/redact_test.go, the block.go allowlist tests,
//     and both runners' conformance suites.
//
//  9. never closes an issue, never writes unstructured prose
//     HERE — TestTheOrchestratorWritesOnlyTheClosedSet, anchored at the seam's
//     method table and the comment payload's fields rather than at the calls a
//     scenario happened to make.
//
//  10. budget breach stops the run and parks it
//     HERE — TestABudgetBreachStopsTheRunAndParks.
//
//  11. a retained claim is released without a restart once the issue closes
//     HERE — TestAPublishedClaimIsReleasedWhenTheIssueClosesWithoutARestart.
//
//  12. a reclaimed issue cannot publish on the previous assignment's work
//     HERE — TestAReclaimScopesSuccessToTheNewAssignmentEpoch. The real
//     verifier consumes the fake provider's independently modelled epoch/base
//     pair across E1 publish, controller unassignment, E2 no-op, sticky human
//     unpark and E2 descendant commit. The pending/pinned crash matrix is
//     exhaustively table-driven in orchestrator/recover_classify_test.go, and
//     its atomic git-store boundary is asserted in workspace/claimbase_test.go.
//
// No row is OWED.
//
// The map is a comment rather than a set of skipped tests on purpose, and that
// stays true now that nothing is outstanding: a t.Skip reads as coverage in a
// test run and rots quietly, while a row here is a claim a reviewer can check
// against the suite — including the rows that deliberately point somewhere else.
package integration
