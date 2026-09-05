# Deterministic orchestration scenario lab

The scenario lab is a developer-only view of BEN's current authority loop. A strict JSON document supplies normalized facts and actions to the same configuration loader, orchestrator, publish verifier, conformant fakes, and manual clock used by `internal/integration`. The resulting text records production transitions and the effects observed at the fake boundaries.

It answers temporal questions such as “what did restart recover?” and “why did this evidence park the issue?” without a tracker, credentials, subprocesses, or using elapsed time to make a scenario fact true.

Run the committed corpus and print its traces with:

```sh
go test ./internal/integration -run '^TestScenarioFixtures$' -count=1 -v
```

The ordinary CI run replays the same corpus without printing successful traces.

## Document format

Documents live in `internal/integration/testdata/scenarios`. Schema version 1 has this shape:

```json
{
  "schema_version": 1,
  "name": "retry-to-success",
  "issue": {"identifier": "7"},
  "attempts": [
    {"outcome": "crashed", "session": "retry-1"},
    {"outcome": "succeeded", "session": "retry-2"}
  ],
  "publication": "complete",
  "steps": [
    {"action": "start", "until": "backoff"},
    {"action": "advance", "until": "done"}
  ]
}
```

Decoding is closed and strict. Unknown fields, enum values, unsupported versions, trailing JSON values, missing required values, and documents beyond the fixed attempt/step bounds are errors. The format uses the standard library's JSON decoder so `internal/config` remains the only owner of the repository's YAML dependency.

Version 1 supports:

- attempt outcomes `succeeded`, `crashed`, and `running`;
- publication facts `complete` and `rewritten`;
- actions `start`, `advance`, and `restart`;
- stable observation boundaries `running`, `backoff`, `done`, and `needs-review`;
- only the positive `prior_run: "gone"` fact for restart.

`attempts` scripts runner starts, not reconstructed orchestrator attempt numbers. Recovery deliberately adopts an attempt floor because it cannot know exactly how much work occurred before a crash. A restart trace can therefore show production attempt 3 on runner start 2; collapsing those numbers would hide the recovery rule the lab exists to explain.

`start` launches the loop and waits for its stated boundary. `advance` drives the manual clock until the boundary is reached. `restart` stops the daemon, ends the prior fake execution domain, verifies positive quiet, starts a fresh orchestrator over the retained tracker/workspace facts, and waits at backoff before any resumed run starts.

## Trace semantics

Each committed `.trace` file is a reviewed golden account with four sections per step:

- `observed` is the stable record, sorted label/assignee sets, milestones, counts, and any published-PR or blocker detail;
- `decisions` comes directly from the production transition log, including its production reasons;
- `effects` preserves tracker-write order and reports workspace, runner, marker, and disposal observations in fixed source order;
- `next` is diagnostic guidance derived from the resulting state, never an authorization decision.

Generated run IDs, temporary paths, daemon instance IDs, scheduler timing, and wall-clock timestamps are excluded. Fixed issue data, claim epochs, base SHAs, transition reasons, and ordered boundary calls remain because they carry useful semantics and are deterministic under the fakes.

Every fixture is executed twice and the bytes are compared before the golden comparison. The corpus filenames and their expected terminal properties are also anchored in Go rather than inferred from the documents. This prevents deleting a fixture or rewriting its expectation from making a self-consistent but weaker test pass.

## Trust boundary

The schema and renderer in `internal/scenario` describe input and output only. They do not import adapters, run the daemon, decide transitions, or expose a tracker-writing command. `internal/integration` is the sole execution binding, and it uses the existing fakes whose behavior is pinned separately against the real adapter control flow.

A document supplies facts; it does not state expected transitions or grant them authority. Golden traces detect behavior changes, while separate assertions establish the load-bearing properties:

- only complete publish evidence reaches the published milestone;
- a retry keeps its workspace owner and claim-time base;
- restart starts nothing while the previous run may still be live;
- rewritten history never reaches `done` or a published milestone.

The lab cannot contact GitHub, launch an agent, read credentials, replay production payloads, or mutate operator state.

## Extending the corpus

For a new current-loop regression:

1. Add a strict JSON document and its reviewed trace.
2. Add the filename and independent terminal expectations to `corpus` in `scenario_lab_test.go`.
3. Add an assertion outside the renderer for every authority or safety property the fixture claims.
4. Run the focused command twice, then `make check`.

Do not extend schema version 1 merely to mirror provider payloads or test plumbing. Add normalized boundary vocabulary only when a real interface already carries the fact. Stateful generation and minimization, future #197 review artifacts, and an operator-facing `explain` surface remain separate children of #213.
