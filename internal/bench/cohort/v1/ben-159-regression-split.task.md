**Spec:** — (repo hygiene; no spec change, no new dependency) · **Depends on:** —

`internal/orchestrator/regression_test.go` holds 53 tests in 2317 lines, spanning
claims, reloads, budgets, parking, timers, reconciliation, hooks, disposal and
prompt limits. "Regression" is not a topic — it is where a test lands when nobody
decides where it belongs, so the file grows monotonically and answers no question.
The package already has the topic files these belong in.

This is placement only. The tests themselves are good and their names are the best
documentation in the package (`AGoneIssueDiscardsTrackerWritesAndKeepsLocalWork`
says exactly what it holds). Nothing about a test body should change.

A starting taxonomy, from the current 53 names — refine it if the reading of a
test disagrees, but say so in the PR:

| target | tests |
|---|---|
| `reload_test.go` (new) | the ~10 `*Reload*`/`*Preflight*` tests, `ConfigurationBoundaryAcrossReadsAndEvents`, `EveryPolicySurfaceReadsTheReloadImmediately`, `APreflightReloadWakesTheTickerToo` |
| `claim_test.go` (new) | the refused/errored/lost/released-claim set, `NoAttemptStartsBeforeTheClaimLabelLands`, `RoutableChecksAssigneeIdentity`, `NewRequiresAClaimPrincipal`, `HumanReplacingTheClaimStopsTheRun` |
| `gone_test.go` (new) | the three `AGoneIssue*` tests plus `DeletedIssueIsNotTreatedAsATransientFailure` |
| `budget_test.go` (exists) | `BudgetParkedRecord*`, `UnconfirmedBudgetStopIsRetried`, `UnparkRestoresTheRunBudgets`, `RestoreBudgetsResetsTurnsAndTheAttemptBase` |
| `parked_test.go` (exists) | `ClosedParkedIssueIsCleanedUp`, `LateRunnerEventCannotUndoAPark`, `TheZeroVerifyResultParksInsteadOfPublishing` |
| `transitions_test.go` (exists) | `TheKillEdgeRefusesStatesTheMapDoesNotKnow` |
| `summary_test.go` (exists) | `RequeueKeepsThePriorFailureInThePrompt`, `TheFailureTrackDoesNotResumeTheSessionThatFailed` |

The timer, reconciliation, owed-write and hook groups have no obvious existing
home; propose one in the PR rather than inventing a second landfill.

`internal/orchestrator/doc.go` currently tells readers this file is a pile and to
prefer the topic file. Update that paragraph to describe what actually exists once
this lands.

**Verify the move, do not merely compile it.** A redistribution can silently drop
a test, and a green `make check` will not notice — a dropped test is a test that
does not run. Prove the set is preserved, not its size: a count cannot distinguish
one test lost and another added. #157 did this for a 270-line code move and it is
the same shape here.

```sh
# before and after must be byte-identical
grep -rho '^func Test[A-Za-z0-9_]*' internal/orchestrator/*_test.go | sort > /tmp/tests-before.txt
```

**Acceptance**

- [ ] `regression_test.go` is gone — deleted, not left as a stub or a shrunken pile
- [ ] The sorted set of `Test*` names in `internal/orchestrator` is byte-identical before and after (`diff /tmp/tests-before.txt /tmp/tests-after.txt` is empty), and the PR shows that diff being empty
- [ ] No test body, table case, or assertion changed: the diff is moves only, and the PR states how that was established rather than asserting it
- [ ] Every new file's name is a topic a reader could have guessed the test from
- [ ] `doc.go`'s note about `regression_test.go` reflects what the package now looks like
- [ ] `make check` green (evidence in the PR)
