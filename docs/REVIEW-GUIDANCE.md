# Reviewer guidance — what BEN's machine reviewer judges

This file is BEN's own answer to "what counts as a finding". It is named by
`review.guidance_file` in [WORKFLOW.md](../WORKFLOW.md) and appended verbatim to
the reviewer's prompt by trusted code (`internal/reviewctl`'s `Prompt`), *above*
the untrusted diff.

It is deliberately a file rather than a constant. The verdict contract — one
closed verdict, in one envelope, in the reviewer's output — belongs to
`internal/reviewrun` and is not configurable, because a prompt naming
delimiters the parser no longer looks for is a reviewer that runs to
completion, costs money and states nothing. What *is* configurable is the
standard a deployment wants applied, which is this.

Nothing here is a repository workflow, and installing anything is not a step.
The daemon reads this file at startup (`ben run`), renders it into every
reviewer invocation, and the reviewer holds no credential — see
[REVIEW.md](REVIEW.md).

---

This repository's own rules are the standard, and `AGENTS.md`, `SPEC.md` and
`BUILD.md` are the source of truth. In order of weight:

1. **Correctness** — does the change do what it claims, including on the error
   and boundary paths?
2. **Safety of the design invariants** — SPEC §3. Evidence over claims, closed
   enums at the boundaries, fail-closed refusals, no new authority granted by
   omission.
3. **Tests** — the acceptance criteria of the ticket exist as tests, and the
   tests would fail if the behaviour regressed. A test driven only by the
   declaration it checks is a gap worth naming (AGENTS.md, Conventions).
4. **Import boundaries and dependencies** — `internal/core` stays stdlib-only;
   a third-party boundary dependency has exactly one owner.
5. **Clarity** — comments state constraints the code cannot show, not
   narration.

Style preferences, naming taste and hypothetical future refactors are **not**
findings. Neither is anything you cannot point at a specific line for.

Choose `changes_requested` only for findings you would block a merge on. The
loop is bounded at three revision rounds; spending one on a nit wastes it.
Choose `clean` when the change is fit for a human reviewer — `clean` is not
"perfect", and remaining observations belong in `findings`, which is published
either way.
