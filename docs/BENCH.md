# The adapter benchmark (#62)

Two runner adapters exist so there is a choice (SPEC §7.7). This is how the choice
gets measured: a **fixed cohort** of historical tasks with known-good outcomes, one
isolated canary repository and issue per adapter/model cell per case, and a
matched-case publish-and-check readout drawn from durable session evidence.

```sh
go run ./cmd/benchreport session.json           # the comparison
go run ./cmd/benchreport --json session.json    # the same numbers, for jq
```

Everything else on this page is how `session.json` comes to exist honestly.

## Why not just read the dogfood logs

Because they answer a different question. Dogfood transition and attempt records
are observational: tickets differ in difficulty far more than adapters differ in
ability, so a publish rate over whatever was in the queue compares the queues.

And the obvious repair is not available at any price. Running both adapters on one
issue means two agents pushing `ben/<issue>`, two pull requests, and a claim only
one principal can hold (SPEC §9.3, §10.1) — so the "experiment" would measure a
race. `internal/bench` refuses a session manifest that records it
(`ErrSharedIssue`), which is the prohibition stated where it can fail rather than
in a paragraph. The queue boundary is broader than the issue boundary: BEN watches
every `ben-queue` issue in one repository, so even different issues can be claimed
by the wrong run's still-live daemon. `ErrSharedQueue` therefore requires one
canary repository per run.

**Approved decision, 2026-08-19:** a fixed cohort. The live dogfood queue is never
randomized, benchmark work never enters it, and nothing BEN decides at runtime
reads any of this. That last one is structural: no package the daemon links may
import `internal/bench`, enforced over the import graph in
`internal/arch/bench_test.go`. The comparison is a separate command for exactly
that reason.

## The cohort

`internal/bench/cohort/v1/` — a `cohort.json` plus one task file per case,
compiled into the report command with `go:embed`. Every case pins:

| field | what it is |
|---|---|
| `id` | the stable join key. It never changes; different content is a different case |
| `title`, `task_file` | the canary issue's exact title and body. The body is a file so `gh issue create --body-file` can take it unaltered |
| `task_sha256` | the pin on those bytes. An edit fails `make check` until somebody records the new digest — which is the moment to ask whether it is a new cohort version |
| `definition_sha256` | the pin on normalized `source_repo` plus the complete case declaration, including `task_sha256`. Every run copies it, so retaining `v1` and an ID cannot reinterpret or relabel an old session after any criterion changes |
| `base_commit` | the immutable commit the task is worked from: the first parent of the commit that solved it historically. Refused unless it is a full-length lowercase object ID, because a branch, a tag or an abbreviation is a base that can move |
| `tier` | `easy`, `medium` or `hard`. Documentation for whoever reads the result, validated against the closed set |
| `known_good` | the historical solution — commit, pull request, and what it did. **Provenance, not the pass criterion**: success is never a diff against it |
| `checks` | the mechanically decidable conditions, each with the acceptance bullet it comes from and whether it fails at the base commit |

v1 carries three cases from BEN's own history, one per tier:
`ben-159-regression-split` (easy), `ben-134-notfound-classification` (medium),
`ben-58-request-budget` (hard). Each task file is the issue body **verbatim** —
transcribed from the tracker and diffed against it, not paraphrased.

### What a case's checks are, and are not

Each check runs by `bash -c` in the root of the published head and must exit 0.
Together with BEN's own publish verdict (SPEC §9.7) they decide the case.

They are **necessary conditions read off the ticket's acceptance list, not a
review.** A wrong solution that satisfies all of them counts as a pass, and no
check covers "the code is good". That is the price of deciding a cell without a
human in it, and it is stated here rather than discovered later. Two things follow:

- Strengthening a case's checks changes what a pass means, so it is a **new cohort
  version**, not an edit.
- A check that already passes at the case's own base commit measures nothing.
  `fails_at_base` records which do not, and a case with none of them refuses to
  load (`ErrInertCase`).

`fails_at_base` is declared, never detected — re-deriving it needs the checks run
against a historical checkout, which no offline unit test can do. Verify a claim
the same way it was made:

```sh
git worktree add --detach /tmp/case-base <base_commit>
cd /tmp/case-base && bash -c '<the check>'   # must be non-zero for fails_at_base
```

The v1 checks were measured that way against both ends. For the record, at
(base → known-good): `regression_test.go` present → absent, with the sorted set of
`Test*` names in `internal/orchestrator` 337 names at each end; `ErrIssueNotFound`
mentions in `internal/core/core.go` 3 → 6 and in the GitHub adapter 1 → 5 — while
`internal/fake`'s went 5 → 4, which is why no check counts the fake; and in
`internal/tracker/github`, `clear(c.entries)` and `maxCacheEntries = 512` present →
absent with adapter mentions of a budget 2 → 31.

### Adding a case

Adding one does not bump the version: `benchreport` compares matched cases, so an
older session simply matches fewer and says so. Write the task file, then:

```sh
# the digest for task_sha256
shasum -a 256 internal/bench/cohort/v1/<id>.task.md

# the base commit: the first parent of the commit that solved it
git rev-parse <solving-commit>^
```

`make check` validates the result — duplicate IDs, mutable or malformed
revisions, missing task/outcome/check fields, drifted task text, and a stale
`definition_sha256` are all load refusals with a named error. For a new or
deliberately changed definition, start that pin as 64 zeroes: the refusal prints
the computed digest to review and record. Changing an existing case still means
deciding whether to create a new cohort version; the fingerprint makes an
accidental in-place edit unable to rewrite completed sessions.

## The procedure

One run is one (adapter, model) × one case. Everything task-relevant except the
adapter and model is held constant, and the manifest records what was actually
observed so the holding is checked rather than remembered.

### Once per session

Declare the matrix in `session.json` **before** dispatching anything. Cells live
separately from runs so forgetting every run for one adapter cannot silently turn a
two-cell experiment into a one-cell report. At least two distinct cells are
required. There is exactly one run per (cell, case): `ErrDuplicateRun` refuses a
repeat rather than giving one cell more chances to pass. A future repeated-trial
benchmark would need to predeclare and balance its trial count in a new schema.

```json
{
  "cohort": "v1",
  "session": "2026-08-19-a",
  "cells": [
    {"agent": "claude-code", "model": "opus"},
    {"agent": "codex-exec", "model": ""}
  ],
  "runs": []
}
```

Run the whole session from one clean controller checkout and one machine. Keep its
absolute path: published heads start from historical commits and deliberately do
not contain this cohort definition.

```sh
CONTROL_ROOT=$(pwd)
SESSION_FILE="$CONTROL_ROOT/session.json"
CHECK_IMAGE="ben-bench-check:$(git -C "$CONTROL_ROOT" rev-parse --short=12 HEAD)"
env -u GH_TOKEN -u GITHUB_TOKEN docker build \
  --file "$CONTROL_ROOT/scripts/benchmark/check.Dockerfile" \
  --tag "$CHECK_IMAGE" "$CONTROL_ROOT"
```

The checker image is built only from the trusted controller checkout. Its pinned
Go image preloads v1's module graph and staticcheck while the build has network;
the agent-authored checkout is never a build context and runs later with no
network or credentials. Run the controller as an unprivileged account; the
checker wrapper refuses UID 0.

**Two credentials that differ** (SPEC §10.2) — the same split `make smoke` uses,
for the same reason: `GITHUB_TOKEN` for the daemon (issues, labels, assignment,
contents read) and `GH_TOKEN` for the agent (contents and pull-requests write, and
**not** issues). See [SMOKE.md](SMOKE.md). The `operator_gh` wrapper below removes
both run credentials so repository provisioning uses the operator's stored `gh`
identity. Issue creation removes only `GH_TOKEN`: `gh` otherwise prefers it and
would ask the agent identity to mutate the queue it cannot write.

### Per run

**A fresh canary repository per run.** BEN is a long-lived queue worker, not a
one-shot issue runner: every daemon watching one repository and the same
`ben-queue` label can claim every issue there. Sharing a per-case repository would
let an earlier adapter claim the next adapter's issue before its intended daemon
starts. A repository per run gives each daemon a one-issue queue while the read-back
base keeps the git tree identical across cells.

```sh
set -euo pipefail

SESSION=2026-08-19-a
CASE=ben-159-regression-split
AGENT=claude-code            # one of: claude-code, codex-exec
MODEL=opus                   # empty means the harness default
CELL_ID=claude-code-opus    # unique path label; include both adapter and model
CANARY_OWNER=acme
CANARY="$CANARY_OWNER/bench-$SESSION-$CASE-$CELL_ID"
COHORT_FILE="$CONTROL_ROOT/internal/bench/cohort/v1/cohort.json"
operator_gh() { env -u GH_TOKEN -u GITHUB_TOKEN gh "$@"; }

case "$AGENT:$MODEL" in
  claude-code:)  PROFILE="$CONTROL_ROOT/scripts/benchmark/claude-code-default.md" ;;
  claude-code:*) PROFILE="$CONTROL_ROOT/scripts/benchmark/claude-code-model.md" ;;
  codex-exec:)   PROFILE="$CONTROL_ROOT/scripts/benchmark/codex-exec-default.md" ;;
  codex-exec:*)  PROFILE="$CONTROL_ROOT/scripts/benchmark/codex-exec-model.md" ;;
  *) echo "unknown benchmark agent: $AGENT" >&2; exit 2 ;;
esac

# 1. a private, one-run mirror pinned to the case base
BASE=$(jq -r --arg case "$CASE" '.cases[]|select(.id==$case).base_commit' "$COHORT_FILE")
CASE_DEFINITION_SHA256=$(jq -r --arg case "$CASE" \
  '.cases[]|select(.id==$case).definition_sha256' "$COHORT_FILE")
operator_gh repo create "$CANARY" --private
PUSH_URL=$(operator_gh repo view "$CANARY" --json sshUrl --jq .sshUrl)
git -c gc.auto=0 -c maintenance.auto=false push "$PUSH_URL" "$BASE:refs/heads/bench-base"
operator_gh repo edit "$CANARY" --default-branch bench-base
OBSERVED_BASE=$(operator_gh api "repos/$CANARY/commits/bench-base" --jq .sha)
[ "$OBSERVED_BASE" = "$BASE" ]

operator_gh label create ben-queue --repo "$CANARY" --color 0e8a16 --description "Dispatchable to BEN"
for label in claimed running needs-review failed; do
  operator_gh label create "ben:$label" --repo "$CANARY" --color 5319e7 --description "BEN state projection"
done

# 2. the one issue: exact cohort title and pinned body, with nothing added
TITLE=$(jq -r --arg case "$CASE" '.cases[]|select(.id==$case).title' "$COHORT_FILE")
TASK_FILE=$(jq -r --arg case "$CASE" '.cases[]|select(.id==$case).task_file' "$COHORT_FILE")
ISSUE_URL=$(env -u GH_TOKEN gh issue create --repo "$CANARY" --title "$TITLE" \
  --body-file "$CONTROL_ROOT/internal/bench/cohort/v1/$TASK_FILE" --label ben-queue)
ISSUE=${ISSUE_URL##*/}

# 3. an immutable profile copied to run-specific workflow and state directories
CELL_ROOT="/var/lib/ben-bench/$SESSION/$CASE/$CELL_ID"
mkdir -p "$CELL_ROOT"
WORKFLOW="$CELL_ROOT/WORKFLOW.md"
BEN_BIN="$CELL_ROOT/ben"
cp "$PROFILE" "$WORKFLOW"
go build -o "$BEN_BIN" "$CONTROL_ROOT/cmd/ben"
export BEN_BENCH_REPO="$CANARY"
export BEN_BENCH_MODEL="$MODEL"
export BEN_BENCH_WORKSPACE="$CELL_ROOT/workspaces"
export XDG_STATE_HOME="$CELL_ROOT/state"
"$BEN_BIN" config effective "$WORKFLOW"  # refuses before agent spend

DAEMON_PID=
stop_daemon() {
  [ -n "${DAEMON_PID:-}" ] || return 0
  if kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill -TERM "$DAEMON_PID"
  fi
  wait "$DAEMON_PID" 2>/dev/null || true
  DAEMON_PID=
}
trap 'status=$?; trap - EXIT INT TERM; stop_daemon; exit "$status"' EXIT
trap 'trap - EXIT INT TERM; stop_daemon; exit 130' INT
trap 'trap - EXIT INT TERM; stop_daemon; exit 143' TERM

"$BEN_BIN" run "$WORKFLOW" &
DAEMON_PID=$!

# 4. the state directory, for the manifest
STATE=$("$BEN_BIN" status --json "$WORKFLOW" | jq -r .state_dir)
```

The four profiles are checked in under `scripts/benchmark/`, load-validated by
`make workflow-check`, and tested to carry byte-identical prompts and identical
non-agent configuration. The default profiles record an empty model; the named
profiles resolve `BEN_BENCH_MODEL`. `CELL_ID` is deliberately explicit so names
containing `/` never become paths accidentally, and it must be unique within the
session.

Then append the run to `session.json` — one object per run, written **as it is
dispatched** rather than reconstructed afterwards:

```sh
NEXT_SESSION=$(mktemp "$SESSION_FILE.next.XXXXXX")
jq --arg case "$CASE" --arg agent "$AGENT" --arg model "$MODEL" \
  --arg repo "$CANARY" --arg issue "$ISSUE" --arg base "$OBSERVED_BASE" \
  --arg definition "$CASE_DEFINITION_SHA256" --arg state "$STATE" \
  '.runs += [{case:$case, case_definition_sha256:$definition,
              agent:$agent, model:$model, repo:$repo,
              issue:$issue, base:$base, state_dir:$state}]' \
  "$SESSION_FILE" >"$NEXT_SESSION"
mv "$NEXT_SESSION" "$SESSION_FILE"
```

`base` is the SHA read back from the forge in step 1, not the branch name that
pointed at it. `case_definition_sha256` copies the normalized source repository
and complete case declaration pin at dispatch; `benchreport` refuses to
reinterpret or relabel the run if the same cohort version and case ID now name a
different repository, title, task, base, provenance, or checks.
`model` is empty when the agent block names none — an answer, not a gap, and the
value the attempt record carries for it. `state_dir` is part of the join: an issue
identifier is unique within one tracker scope, so (`state_dir`, `issue`) is what
identifies a run's records. It must be an absolute, lexically canonical path; a
relative path or aliases such as `/x/state` and `/x/state/.` are refused.

Wait for that issue's attempt to reach a terminal verdict, then stop and wait for
the daemon before starting another run:

```sh
stop_daemon
trap - EXIT INT TERM
```

The built binary is the background process, so `$DAEMON_PID` is BEN rather than a
`go run` parent that can exit while leaving its child alive. The EXIT/INT/TERM
handlers cover every early failure before the explicit stop. Repository isolation
is still the queue correctness boundary; cleanup also keeps a session from
accumulating idle workers.

### What must not vary between cells

The task text, prompt, every workflow limit, hooks, toolchain, machine, and case
base commit. The committed profiles and their parity test hold the prompt, limits,
and hooks; use one checkout and machine for the rest. The canary repository,
issue, branch and state/workspace paths differ deliberately: they are isolation
identities, not task inputs. `benchreport`
refuses a session whose recorded base is not the case's pinned base
(`ErrBaseMismatch`), whose definition fingerprint has changed
(`ErrCaseDefinitionMismatch`), or whose attempt records name a different adapter
or model than the manifest claims (`ErrCellMismatch`) — the record is the
evidence, the manifest is the claim (SPEC §3.5). GitHub repository names are
compared case-insensitively, so casing variants cannot bypass one-repository-per-run.

The remaining environment is a matter of discipline, and the honest statement is
that BEN cannot check it. Run a case's cells from one checkout, in one sitting.

### Running and recording the case's checks

Only a run with BEN's `published` verdict has a published head to check. Resolve
that run's PR and fetch its exact head into a temporary checkout. The cohort
definition comes from the controller checkout — the historical task commit does
not contain this benchmark code — but every command runs through the checked-in
container wrapper. Agent-authored code receives a read-only checkout, fresh
tmpfs home/cache, an empty environment, no network, no capabilities, and no
Docker socket or host home. The operator and run credentials exist only in the
controller that fetches the head and records the booleans.

```sh
jq -e --arg issue "$ISSUE" \
  'select(.issue == $issue and .verdict == "published")' \
  "$STATE/attempts.jsonl" >/dev/null

PR_JSON=$(operator_gh pr view "ben/$ISSUE" --repo "$CANARY" --json number,headRefOid)
PR=$(jq -r .number <<<"$PR_JSON")
FORGE_HEAD=$(jq -r .headRefOid <<<"$PR_JSON")

CHECKOUT=$(mktemp -d)
RESULTS=$(mktemp)
NEXT_SESSION=$(mktemp "$SESSION_FILE.next.XXXXXX")
trap 'rm -rf -- "$CHECKOUT"; rm -f -- "$RESULTS" "$NEXT_SESSION"' EXIT

git -c gc.auto=0 -c maintenance.auto=false clone "$PUSH_URL" "$CHECKOUT"
git -C "$CHECKOUT" -c gc.auto=0 -c maintenance.auto=false \
  fetch origin "refs/pull/$PR/head"
CHECKED_COMMIT=$(git -C "$CHECKOUT" rev-parse FETCH_HEAD)
[ "$CHECKED_COMMIT" = "$FORGE_HEAD" ]
git -C "$CHECKOUT" switch --detach "$CHECKED_COMMIT"

while IFS= read -r spec; do
  id=$(jq -r .id <<<"$spec")
  run=$(jq -r .run <<<"$spec")
  check_status=0
  "$CONTROL_ROOT/scripts/benchmark/check.sh" \
    "$CHECK_IMAGE" "$CHECKOUT" "$run" || check_status=$?
  case "$check_status" in
    0) passed=true;  echo "pass $id" ;;
    1) passed=false; echo "FAIL $id" ;;
    *) echo "checker infrastructure failed for $id (status $check_status)" >&2
       exit "$check_status" ;;
  esac
  jq -nc --arg id "$id" --argjson passed "$passed" \
    '{id:$id, passed:$passed}' >>"$RESULTS"
done < <(jq -c --arg case "$CASE" \
  '.cases[] | select(.id == $case) | .checks[] | {id, run}' "$COHORT_FILE")

AFTER_HEAD=$(operator_gh pr view "ben/$ISSUE" --repo "$CANARY" \
  --json headRefOid --jq .headRefOid)
[ "$AFTER_HEAD" = "$CHECKED_COMMIT" ]

MATCHES=$(jq --arg repo "$CANARY" --arg issue "$ISSUE" \
  '[.runs[] | select(.repo == $repo and .issue == $issue)] | length' \
  "$SESSION_FILE")
[ "$MATCHES" -eq 1 ]
jq --slurpfile results "$RESULTS" --arg repo "$CANARY" --arg issue "$ISSUE" \
  --arg commit "$CHECKED_COMMIT" \
  '(.runs[] | select(.repo == $repo and .issue == $issue)) +=
     {checked_commit:$commit, check_results:$results}' \
  "$SESSION_FILE" >"$NEXT_SESSION"
mv "$NEXT_SESSION" "$SESSION_FILE"
trap - EXIT
rm -rf -- "$CHECKOUT"
rm -f -- "$RESULTS" "$NEXT_SESSION"
```

`check.sh` has a closed status protocol: 0 is a completed passing command, 1 is
a completed failing command, and every other status aborts the session. It keeps
the stopped container long enough to inspect its runtime state, so setup
refusals, Docker errors, cleanup failures, signals, and OOM kills cannot be
recorded as adapter failures.

`benchreport` validates the recorded check IDs exactly against the case: an
unknown, missing or duplicate result refuses the report, as does check evidence
for a run whose attempt log has no published verdict. A published run with no
check evidence is shown as `unchecked` and never counts as a pass. This keeps the
interesting outcome — published but mechanically wrong — in the durable report
instead of in an operator's notes.

## Reading the report

```
benchmark comparison — cohort v1 (srhg-ai-7cef3f93/ben), session 2026-08-19-a

declared cells (2): claude-code (opus), codex-exec (default model)
matched cases: 2 of 3
  ben-159-regression-split         (easy)    claude-code (opus), codex-exec (default model)
  ben-134-notfound-classification  (medium)  claude-code (opus), codex-exec (default model)
  ben-58-request-budget            (hard)    claude-code (opus)  — excluded: not run by every declared cell

cell                        cases  published  passed   check-fail  unchecked  runs  attempts  ran  p50    p95     input  output  cost   unpriced
claude-code (opus)          2      2 (100%)   2 (100%)  0           0          2     3         3    20m0s  30m0s   3.0k   300     $3.75  0
codex-exec (default model)  2      1 (50%)    0 (0%)    1           0          2     2         2    50m0s  1h0m0s  2.0k   200     $0.00  2

per matched case — published / checks / passed / attempts / wall clock / last verdict
case                             claude-code (opus)                          codex-exec (default model)
ben-159-regression-split         yes / passed / yes / 2 / 30m0s / published  yes / failed / no / 1 / 50m0s / published
ben-134-notfound-classification  yes / passed / yes / 1 / 30m0s / published  no / not-published / no / 1 / 1h0m0s / incomplete

claude-code (opus) — failures: none; verdicts: published 2, incomplete 1; check failures: none
codex-exec (default model) — failures: none; verdicts: incomplete 1, published 1; check failures: landfill-gone 1

attempts read 7, joined 6; 1 record belongs to no run in this session and was not counted
runs with no attempt record (1):
  ben-58-request-budget  codex-exec (default model)  acme/codex-three#23

passed means BEN published the run and every cohort check passed at its recorded immutable head; unchecked publishes never pass.
```

Six things about that output are load-bearing:

- **`declared cells (2)`.** The matrix comes from the manifest's top-level
  `cells`, not from whichever runs happened to leave records. If a declared cell
  has no run at all, a separate `declared cells with no run` block names it.
- **`matched cases: 2 of 3`.** Rates are over the cases *every* declared cell ran,
  and the excluded case is named with the cells that did run it. A cell's win on a
  case its rival never attempted is not in any number here.
- **`passed`, `check-fail`, `unchecked`.** Publish is BEN's §9.7 verdict; pass also
  requires every cohort check at the recorded head. The three columns prevent an
  unchecked or mechanically failing pull request from inheriting a publish win.
- **`unpriced`.** codex-exec quotes no price (`core.Usage`), so it reports `$0.00`
  with real tokens. The cost column without this one beside it reads as the cheaper
  adapter.
- **`attempts read … joined …`.** Only attempts a manifest run claims are counted;
  anything else in the same log — dogfood work sharing a host — is reported and
  excluded, never pooled.
- **`runs with no attempt record`.** A run that produced nothing is a hole in a
  denominator: still running, never dispatched, or a mistyped state directory.
  It is printed rather than quietly reducing the matrix.

`p50`/`p95` are attempt wall clock, dispatch to outcome, over every attempt
including those that never started a process — a clone that hung for six minutes
cost exactly that (`state.Attempt.Ran`). The percentile is nearest-rank and shared
with `ben status` (`state.Percentile`), so the two readouts of one log cannot
disagree.

## What this does not measure

- **Quality.** A published pull request that satisfies the checks counts, whatever
  a reviewer would have said.
- **Anything about a task not in the cohort.** Three cases is a spread, not a
  sample. Report the denominator, which is why every number here comes with one.
- **The model each harness actually resolved.** `model` is what the workflow block
  selected; claude-code announces its resolved model on its `system/init` line and
  codex-exec announces none, and carrying that would mean a field on the closed
  §7.2 event model — out of scope for #62 by decision.
- **A regression over time.** Nothing schedules this and nothing gates on it. It is
  a question you ask when the answer would change something: which adapter to point
  a queue at, or whether a new model is worth its price.
