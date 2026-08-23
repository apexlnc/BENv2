// Package bench is #62's adapter/model comparison: a fixed cohort of historical
// tasks with known-good outcomes, the predeclared adapter/model matrix, the join
// from one benchmark session's runs to those tasks, and the matched-case
// publish-and-check readout drawn from durable session evidence.
//
// Two runner adapters exist so there is a choice (SPEC §7.7), and until this
// package there was nothing that measured the choice.
//
// # Why a cohort and not the dogfood queue
//
// Dogfood transition logs are observational. Tickets differ in difficulty, so a
// rate computed over whatever happened to be in the queue compares the queue and
// not the adapters — and the obvious repair, running both adapters on one issue,
// is not available at any price: it produces two pushes to one branch, two pull
// requests, and a contested claim that SPEC §10.1's unique-principal rule exists
// to make impossible.
//
// So the unit of comparison is a *case*: an exact task text, pinned to the
// immutable commit its known-good solution was written against, with the checks
// that decide a pass stated in the case itself. Every adapter/model cell runs
// that case in its own canary repository, issue, branch and workspace: the
// repository boundary prevents one long-lived queue worker from claiming
// another run, including through GitHub repository casing variants. Each cell
// gets exactly one run per case; repeats are refused because v1 has no
// predeclared balanced trial count. A per-run definition fingerprint binds old
// evidence to the normalized source repository and exact case content.
// Canonical absolute state paths keep one attempt log from acquiring two join
// identities. Task-relevant inputs
// differ only in adapter and model. Agent-authored check commands execute in the
// credential-free container defined under scripts/benchmark, never in the
// controller process; only completed command verdicts become evidence, while
// checker infrastructure failures abort. docs/BENCH.md is the procedure; this
// package is the definition, join and arithmetic.
//
// # What this package is not
//
//   - **Not a dispatch mode.** Nothing here runs an agent, creates an issue, or
//     talks to a tracker. The comparison is a read of files a daemon already
//     wrote, and the procedure around it is an operator typing documented
//     commands.
//   - **Not reachable from the loop.** No package the daemon links may import
//     this one — enforced over the import graph in internal/arch/bench_test.go,
//     because "benchmark telemetry must not become a runtime decision" is a
//     property of the build, not a promise in a comment.
//   - **Not a pool.** Compare counts only attempts a manifest run claims, over
//     only the cases every declared cell ran, and states every attempt, run and
//     case it dropped on the way (Report.CellsWithoutRuns,
//     Report.UnjoinedAttempts, Report.RunsWithoutAttempts, Report.Coverage). A
//     benchmark number whose denominator is unstated is the failure mode this
//     package exists to avoid.
//
// # Layout
//
//	cohort.go     the checked-in cohort: types, strict load, and every refusal
//	manifest.go   one session's declared matrix, dispatch join and check evidence
//	compare.go    the matched-case arithmetic, grouped by adapter and model
//	cohort/v1/    the cohort itself: cohort.json plus one task file per case
//
// Test files that are not named for one of those:
//
//	embedded_test.go   holds the checked-in cohort to the rules, from the
//	                   directory rather than from its own declaration
//	checker_test.go    holds the untrusted-check container to its isolation and
//	                   verdict/infrastructure status boundary
package bench
