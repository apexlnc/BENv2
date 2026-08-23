# BEN — Branch, Execute, Notify

A single-binary Go daemon for working GitHub Issues autonomously with coding agents. BEN
claims labeled issues, runs Claude Code or Codex in isolated git worktrees, verifies the
result from git facts (branch advanced, pushed, PR open), and hands it off for human review.
The issue closes when the PR merges, and no PR gets there without a human's approving review.

- **[SPEC.md](SPEC.md)** — the locked v1 specification: design invariants, the five component
  contracts, state machine, testing strategy, and deferred extensions.
- **[BUILD.md](BUILD.md)** — the build plan: twelve implementation tickets across three
  milestones.
- **[AGENTS.md](AGENTS.md)** — the repo guide for coding agents (and humans): canonical
  commands, import boundaries, and definition of done.
- **[WORKFLOW.md](WORKFLOW.md)** — BEN's own dogfood workflow; `ben run` can execute it, subject
  to the readiness gates below.
- **[docs/DEPLOY.md](docs/DEPLOY.md)** — the unattended-operation runbook: the account, the two
  credentials, the branch protection the review gate rests on, and risk-accepted mode.
- **[Dockerfile](Dockerfile)** — the BEN runtime image. Kubernetes desired state and workflow
  configuration for SRHG AI nonprod live in its Argo CD repository.
- **[docs/SMOKE.md](docs/SMOKE.md)** — the real-integration smoke profile: one issue end to end
  on a canary repo with a real harness, for the adapter drift CI cannot see.
- **[docs/BENCH.md](docs/BENCH.md)** — the adapter benchmark: a fixed cohort of historical tasks,
  one canary issue per adapter/model cell, and the matched-case comparison read off the attempt log.

## Status

**All twelve build tickets have landed.** `ben run` assembles the adapters, workspace, verifier,
watcher, and authority loop into a daemon, and `ben status` reads its white-box state while it runs
(B11). A restarted daemon reconstructs what its claim principal holds from the tracker, git and its
run markers before the first tick dispatches, so an interruption leaves claims and `ben:*` labels
standing *for recovery to reconstruct* rather than stranded (B10). The §12.3 invariant suite runs in
CI with no network and no subprocesses (B12).

The daemon is **not yet ready for unattended use**, and the reason is no longer an unfinished
ticket. BEN's own unattended-dogfood gate is tracked in
[issue #76](https://github.com/srhg-ai-7cef3f93/ben/issues/76), whose one open lane is the **forge
control** — §10.1 requirement 3, which is load-bearing in risk-accepted mode because it is the only
remaining gate there. Branch protection is applied through Terraform and independently read back
([#83](https://github.com/srhg-ai-7cef3f93/ben/issues/83)), but BEN publishes as whoever runs the
daemon, who is also the human reviewing the pull request — so an approval does not yet bind to an
identity distinct from the one that pushed.
[#155](https://github.com/srhg-ai-7cef3f93/ben/issues/155) and
[#156](https://github.com/srhg-ai-7cef3f93/ben/issues/156) are what close that. Until they do, use
`ben run` only for supervised development and the scripted smoke profile — and
`deploy/ben.service` stays blocked by its `ExecStartPre` gate.

Per-ticket state deliberately lives where it stays current rather than here: [BUILD.md](BUILD.md)
for the twelve tickets and their acceptance criteria, and the
[issue list](https://github.com/srhg-ai-7cef3f93/ben/issues) for which of them are done. This
section names the operational facts a reader needs before deploying it — whether restart recovery
works, and whether the unattended gate is complete.

## Try it

The configuration inspector needs no credentials, no network, and no installed agent harness:

```sh
go run ./cmd/ben config effective WORKFLOW.md
go run ./cmd/ben config effective --json WORKFLOW.md
```

Prints the fully-resolved workflow configuration with per-field provenance; secrets are
always redacted.

The CLI also includes `ben run [path]` and `ben status [--json] [path]`. Run
`go run ./cmd/ben --help` for their exact interface. Starting the daemon needs real credentials,
network access, and an installed harness, and remains subject to the **Status** caveat above.

## Running it unattended

**[docs/DEPLOY.md](docs/DEPLOY.md)** is the runbook: the dedicated account, the two credentials,
the branch protection BEN's review gate rests on, the isolation you can configure today, and
risk-accepted mode. Read it before standing BEN up with no human present.

Two of its rules do not survive being skimmed, because breaking either fails silently rather than
loudly:

- **The required approval must be one no agent credential can produce.** Requiring a review is not
  enough: a publish identity holds pull-request write, so it can approve *somebody else's* pull
  request, and that approval counts. Require review from **code owners** over a `CODEOWNERS`
  covering every path, with no agent listed directly or admitted through an owner team.
- **For BEN's own repository, Terraform is the only writer.** Change branch protection in
  [`terraform-srhg-github-live`](https://github.com/NYDIG/terraform-srhg-github-live) and let
  Atlantis apply it — never send a mutating `gh api` (`PUT`, `PATCH`, or `DELETE`) for that rule.
  Both the API and the provider replace the whole protection object, so a hand-applied rule is
  reverted, silently and at exit 0, by the next apply of a config that does not name its fields.
  BEN's own repository lost its review-binding controls exactly that way.

## Lineage

An inspired redesign of [OpenAI's Symphony](https://github.com/openai/symphony) — not a
conforming implementation. See [SPEC.md](SPEC.md) for the full lineage survey.
