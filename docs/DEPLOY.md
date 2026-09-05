# Running BEN unattended (SPEC §10.1, §10.2)

The deployment procedure for a BEN daemon that dispatches with no human present: the account it
runs as, the credentials it holds, the branch protection its review gate rests on, and the
isolation you can and cannot configure today.

SPEC §10.1 is the normative topology and §10.2 the credential model — what any unattended
deployment must be true of. This document is the procedure for *one* deployment: how to satisfy
those requirements on a host, how to prove you have, and what BEN's own deployment does. The two
are complementary; read §10 for what is required, and this for how.

**These are the requirements for an unattended deployment, not a claim that any deployment has
been approved for one.** The readiness gate, #76, closed on 2026-08-20; what remains is the
deployment-mode decision this document describes and, for public input, the containment
qualification in #195. See the [README's Status section](../README.md#status). Until a deployment
records that decision, use `ben run` only for supervised development and the scripted smoke profile
([SMOKE.md](SMOKE.md)) — and `deploy/ben.service` stays blocked by its `ExecStartPre` gate.

**If you are here to change BEN's own branch protection: Terraform is the only writer.** A
hand-applied `gh api` rule is reverted silently, at exit 0, by the next Atlantis apply — read
[For BEN, Terraform is the only writer](#for-ben-terraform-is-the-only-writer) before you send
anything mutating.

## What §10.1 requires

BEN dispatches an agent that runs permissively: the worktree bounds *edits to the repository*
and nothing else. Absent the boundary §10.1 requires below, a run that follows a hostile
instruction reaches everything the daemon's OS account reaches — its filesystem and `$HOME`,
its network, and every credential it holds. BEN assumes untrusted issue authors and trusted
labelers: anyone may file, but only a principal your tracker authorizes to label can dispatch,
so the label is the approval act (SPEC §6.7).

SPEC §10.1 requires, for any daemon dispatching with no human present:

- **A dedicated OS account**, whose `$HOME` holds no credentials beyond that daemon's own — no
  personal SSH keys, no cloud credentials, no second checkout. BEN's own data lives there too
  (`~/.local/share/ben` for workspaces, `~/.local/state/ben` for run records and transcripts),
  which is fine; what must not be there is anyone else's secrets.
- **Not root.** Claude Code refuses its headless permission mode when running as root outside
  a recognized sandbox, and an unprompted agent with root can rewrite the host.
- **A run that cannot reach the daemon.** The agent and the daemon are distinct principals even
  when they share a UID, and BEN cannot guarantee the daemon's tracker credential is
  unreachable from a process running under that same identity. An unattended run must be unable
  to read or use that credential, or to inspect or control the daemon process. §10.1 states this
  as a property, not a mechanism — a separate run identity, a process sandbox, or a container
  can each satisfy it, and you must verify yours does on your platform.
- **Two credential classes, minimally scoped** (§10.2): the tracker PAT the daemon claims with,
  and the agent's — its harness API key or stored login, plus the `publish` block's credential
  (§5.2.8), which is where you state the identity BEN pushes as. Keep the tracker
  credential away from the agent: a run holding it can rewrite the queue that dispatched it.
- **A label your untrusted principals cannot apply.** On GitHub, applying a label needs triage
  permission or above; if your repository settings widen that, do not run unattended.
- **A forge that actually enforces review.** BEN's whole gate is that a pushed branch and an
  open PR are inert until a human **approves** them — and on GitHub the credential that pushes a
  branch is ordinarily enough to merge it, approved or not. The agent completing the merge
  afterwards is intended, not a gap; what must not happen is the agent merging what no human
  approved. Protect every branch a workflow can select as its target — the repository default
  when `workspace.base_branch` is omitted, or the configured branch when it is written — and get
  all three parts:

  1. **Require a review**, and **dismiss it on any later push** so the approval binds to the
     commit that merges.
  2. **Require an approval the agent cannot supply.** Do not rely on GitHub forbidding an author
     from approving their own pull request — that constrains the agent only on pull requests the
     agent opened. Your publish identity needs pull-request write to open a PR at all, and that
     same permission lets it approve *somebody else's*: a human's PR, or a second agent's. Its
     approval counts, and "approval of the most recent reviewable push" is satisfied too, since
     it was not the pusher. Require review from **code owners** over a `CODEOWNERS` covering
     every path, with no agent listed directly or admitted through an owner team, and the
     approval becomes one no agent credential can produce.
  3. **Give the publish identity no capability that exempts it.** Branch protection does not
     apply to administrators *or to custom roles carrying "bypass branch protections"* — check
     the capability, not the role name. "Not an admin" is the easy shorthand and it is not the
     same statement.

  All of it names principals, not tokens, so none of it separates agent from human until the
  agent publishes as its own identity.

## Provisioning the account

```sh
sudo useradd --system --create-home --home-dir /var/lib/ben --shell /usr/sbin/nologin ben
sudo install -d -o ben -g ben -m 0700 /var/lib/ben/.local/share/ben /var/lib/ben/.local/state/ben
```

### Upgrading into claim-scoped bases and targets

When upgrading from a build without claim epochs, stop the old daemon and first prove that its
claim principal has **no assigned issue in the repository** — running, parked, or published and
awaiting review. For GitHub, an operator can make the assignee-filtered check directly:

```sh
gh issue list --repo <owner>/<repo> --assignee <claim-assignee> --state all --limit 1000
```

The result must be empty before the epoch-aware daemon starts. Resolve or deliberately unassign
every standing claim; do not treat an idle process, an empty `ben status`, or deletion of the
workspace root as equivalent. A pre-upgrade assignment has no trustworthy historical pinning
instant and the new daemon deliberately parks it. An old `refs/ben/base/*` pin can serve only as
the outgoing comparison fact after a **new** assignment event; it is never retrofitted onto the
assignment already standing (SPEC §9.10).

The same empty-principal drain is mandatory when upgrading from a build that recorded claim bases
but not targets. Old local claim-base, daemon-side mirror-claim, and remote workspace-cycle records
remain readable only as non-authorizing legacy state for their existing assignment. BEN parks that
epoch rather than filling its target from today's workflow or repository default. Only a later
assignment epoch may carry a valid outgoing base or cycle identity forward and write a complete
base/target record before any hook or agent starts.

Start from the shipped [sample systemd unit](../deploy/ben.service), not a hand-written fragment.
It runs under the dedicated account and deliberately contains `ExecStartPre=/bin/false`; removing
that line is the sample's explicit acknowledgement of risk-accepted mode. Do not remove it while
the README's **Status** gates remain open.

For local execution, the unit's `Delegate=yes` is required, and its explicit
`RestrictNamespaces=no` must not be narrowed. systemd rejects every `clone3` call when any namespace
restriction is active because seccomp cannot inspect `clone3`'s pointer argument; that also rejects
BEN's mandatory atomic cgroup placement. BEN's readiness canary must prove unified cgroup v2 with
`nsdelegate`, PID/mount/cgroup containment, pidfds,
`cgroup.kill`, and cleanup under the unit's actual policy. A host that lacks or blocks any of those
facts refuses the local adapter; do not work around that refusal with a process-group launcher.
Remote/Airlock execution remains available. The service retains `KillMode=mixed`: BEN owns each
domain's bounded graceful teardown, while systemd owns the outer `TimeoutStopSec` deadline.

The destructive real-host proof is intentionally outside ordinary CI. On a disposable Linux
systemd host with a `ben` account, run `scripts/test-systemd-localdomain.sh`. It compiles the
package test, installs short-lived copies of the checked-in unit under `/run/systemd/system` with
only executable/workflow/state paths and the documented startup gate adjusted, and uses no drop-in.
It proves refusal without `Delegate=yes`, clean completion, a surviving `setsid` descendant, an
abrupt `MainPID` restart, durable recovery identity, and startup cleanup. The test logs the installed
systemd and kernel versions and removes its generated units and `/var/lib/ben-234-systemd-proof-*`
state. Use a disposable host because it deliberately SIGKILLs its own fixture service.

That execution domain accounts for descendants and hides parent/sibling cgroup controls, but it
does not by itself give you **a run that cannot reach the daemon's credentials** — the agent still
runs as `ben`, so files and sockets readable by that UID remain readable — nor **a forge that
enforces review**, which no unit file can supply.
The first needs a second identity for agent processes or a whole-process sandbox; the second is
protection on every selected target branch. [Issue #83](https://github.com/srhg-ai-7cef3f93/ben/issues/83)
records BEN's applied protection half; [#155](https://github.com/srhg-ai-7cef3f93/ben/issues/155)
and [#156](https://github.com/srhg-ai-7cef3f93/ben/issues/156) built the publish-identity half,
which **Credential sources** below is how you configure. Whatever mechanism you use, prove it with
the agent's actual credential rather than by
reading the configuration back, and test **three** things for every selected target — a direct push to the branch is
refused; a pull request the agent opened cannot be merged with no approving review; and a pull
request opened by *someone else* cannot be merged on the **agent's own** approval. The third is the
one a configuration read-back will never tell you about.

### Kubernetes canary

The root [`Dockerfile`](../Dockerfile) builds the BEN runtime image. The live SRHG AI nonprod pod
contract and workflow are intentionally not duplicated here: GitOps owns them in
`srhg-engineering-e98049f/argocd-srhg-ai-nonprod`, under `apps/aoa.yaml` and
`values/ben.values.yaml`. Operators stage and scale `Application/ben-srhg-ai-nonprod` by merging
that repository; direct `kubectl apply` and `kubectl scale` are reverted by Argo self-heal.

The image runs `tini` as PID 1, directly in front of BEN. It reaps agent descendants orphaned to
the container init process and forwards Kubernetes' SIGTERM to BEN so §9.8's ordered drain still
owns shutdown. Do not override the entrypoint with BEN itself or a shell wrapper: the former does
not reap adopted descendants, and the latter does not preserve the supervisor signal contract.

Before dispatching a canary issue, verify the running pod against
[SMOKE.md's runtime preflight](SMOKE.md#kubernetes-canary-runtime-preflight): the image digest, the
PID 1 contract above, the harness the active workflow names, the publishing Git identity, and the
daemon's heartbeat and startup recovery. There is no script to check those first, which is what the
`make smoke` profile has and a label-dispatched canary does not.

The canary is `attended` and uses a separate `ben-kube-canary` queue label. Scaling it is a supervised
smoke action, not an unattended deployment. The daemon and its agent still share the pod's process
identity; putting both in one container does not establish protected mode's run/daemon boundary.
The Argo Deployment therefore cannot be left at one replica after the operator leaves, and #76's
closure does not relabel it: #156 and #76 closed on 2026-08-19 and 2026-08-20 with the four
behavioural forge-control probes waived on record as GitHub's own enforcement, and the shared-process
pod still does not establish protected mode's run/daemon boundary. Relabelling it `risk-accepted` is
an explicit decision with its own `accepted_because`, not a consequence of the gate closing.

Build and smoke the image locally before publishing a multi-architecture index:

```sh
docker build --build-arg VCS_REF="$(git rev-parse HEAD)" -t ben:canary .
docker run --rm ben:canary --help
```

For every commit merged to `main`,
[the daemon image workflow](../.github/workflows/publish-daemon-image.yml) smoke-tests the image
before publishing a Linux AMD64/ARM64 index to
`844479804508.dkr.ecr.us-east-2.amazonaws.com/ben/daemon:<full-commit-sha>`. Tags are immutable.
The ECR repository and the workflow's main-only `ben-w` GitHub OIDC role are managed in
`NYDIG/terraform-srhg-cicd-nonprod` and must exist before the first publication.

## The review controller

Optional, and off until you turn it on. [`REVIEW.md`](REVIEW.md) is its runbook: the three
identities it needs, the markers it reads its own state back out of, and the deployment gate.

Two things about it belong here, in the §10.1 runbook, because they are properties of the
*deployment* rather than of the controller:

- **It never approves and never applies a required label.** It publishes advisory `COMMENT`
  reviews, hands BEN's claim back, or removes `ben-queue`. The branch rule below is therefore
  untouched by it: a human code owner's approval is still what merges a pull request, and adding
  the controller does not create a second path to one.
- **Its token is a fourth identity**, distinct from the tracker, base-fetch and publish credentials
  of **Credential sources** below. It needs `issues: write` and `pull-requests: write` on this
  repository and nothing else — in particular no `contents: write`, since it never pushes.

## The forge control: branch protection

Requirement 3 in full: what to set on every branch selectable by `workspace.base_branch`, what
each field buys, and how to read it back afterwards. Omission selects the repository default;
an explicit value selects that branch. Repeat the rule and the proof for each value used by a
deployed workflow. One rule comes first because meeting it after the fact costs you a control —
it is specific to BEN's own repository, and the reason it exists is not.

### For BEN, Terraform is the only writer

**Do not send mutating `gh api` requests (`PUT`, `PATCH`, or `DELETE`) for BEN's own branch
protection rule.** Change it in
[`terraform-srhg-github-live`](https://github.com/NYDIG/terraform-srhg-github-live) and let Atlantis
apply it. The `PUT` below documents the protection object for a deployment that has no declarative
manager; it is not an operating command for this repository. The hand-applied change in issue #83
worked because the caller currently had administrator access; the deployment must not depend on a
future operator — and especially not BEN's publish identity — holding that authority.

This is also the drift boundary. Both the API `PUT` and the Terraform provider replace the whole
protection object. A hand-applied rule is reverted, silently and at exit 0, by the next `apply` of
a config that does not name these fields: the provider does not merge with what it finds, it states
the whole object, and a field the config omits is written as the provider's default rather than
left alone. BEN's own repository lost its review-binding controls exactly this way (issue #83
applied them by hand; issue #104 later brought the branch under Terraform, whose module set
`dismiss_stale_reviews: false` and had no field at all for `require_last_push_approval`).

So a hand-applied rule is worse than no rule: it reads as a control on every read-back that names
its fields, right up until an unrelated apply removes it and tells nobody.

### The rule

```sh
# 1. every path has human owners, and no agent is one directly or through a team
printf '* @your-org/reviewers\n' > CODEOWNERS

# 2. the branch rule — for a deployment with no declarative manager. For BEN's own
#    repository this is Terraform's to write; see the warning above.
gh api -X PUT repos/<owner>/<repo>/branches/<target-branch>/protection --input - <<'JSON'
{
  "required_pull_request_reviews": {
    "required_approving_review_count": 1,
    "dismiss_stale_reviews": true,
    "require_last_push_approval": true,
    "require_code_owner_reviews": true
  },
  "restrictions": null,
  "required_status_checks": null,
  "enforce_admins": false,
  "allow_force_pushes": false,
  "allow_deletions": false
}
JSON
```

`require_code_owner_reviews` with a comprehensive `CODEOWNERS` is what makes the required approval
one the agent cannot produce. Without it the other flags are all satisfiable by the agent alone on a
pull request it did not open, and the merge is legitimate on every remaining rule. The two
review-binding flags are the load-bearing pair for the *other* half: without them an approval of one
tree survives a later push and can be spent merging a different one.

`restrictions` — a push allowlist, org-owned repositories only — is deliberately `null`, because it
would block the agent from *merging* an approved PR and that is the intended workflow (SPEC §10.1).

**It is not a substitute for the code-owner rule, and setting it does not let you skip that rule.**
A push allowlist governs *who performs the merge*; it says nothing about *whose approval counts*. The
agent can still approve a human's pull request, and an allowlisted human can then press merge — the
approving review on record is the agent's, and no human reviewed anything. The two controls answer
different questions, so the code-owner requirement stays mandatory whether or not you add a push
restriction on top of it.

Every listed owner — and every member of a listed owner team — is part of this security boundary.
Keep owner teams human-only: adding the agent indirectly gives it an approval that satisfies the
rule just as surely as listing its account here. With more than one human owner, each owner's pull
requests can receive the required approval without administrator bypass.

`enforce_admins` is the honest part of the trade: left false, administrators can still push
directly and merge with no review at all, so the rule binds the agent — a write principal holding no
exemption — rather than everyone. **That is what makes the agent's identity load-bearing rather than
tidy:** an agent publishing as an exempt principal walks past every line of the JSON above, and the
more of the merge you let it perform, the more of your gate rests on it holding no exemption. Check
that as a *capability*, not a role name — branch protection is waived for administrators **and for
custom roles carrying "bypass branch protections"**, so "it is not an admin" does not establish it.
Turning `enforce_admins` on costs you the ability to merge your own work anywhere the reviewer
count exceeds the reviewers available, since no one may approve their own pull request.

### Use the API only as a read-only observer

Read back the fields you set after Atlantis applies, not a summary of the branch. The apply that
reverted BEN's controls was checked — by a read-back projecting review count, admin enforcement,
status checks, force-push and deletion, all of which were genuinely unchanged. It could not report
the fields it did not name, so the check passed and the controls were gone. Ask for the fields whose
absence is the failure:

```sh
gh api repos/<owner>/<repo>/branches/<target-branch>/protection -q '{
  reviews:            .required_pull_request_reviews.required_approving_review_count,
  dismiss_stale:      .required_pull_request_reviews.dismiss_stale_reviews,
  last_push_approval: .required_pull_request_reviews.require_last_push_approval,
  code_owner_review:  .required_pull_request_reviews.require_code_owner_reviews
}'
```

Every one of those is a field whose *silent* flip to `false` costs you a control and changes nothing
you would notice. Be wary of `-q` paths generally, for a related reason: a path through an **absent**
key yields `null`, which renders indistinguishably from a present-and-empty value. Where the
distinction matters — `has("restrictions")` versus reading `.restrictions.users` on a branch that
carries no push allowlist — ask with `has()` rather than reading through.

## Isolation you can configure today, and what you can't

- **Adapter-owned sandbox postures** are configured through the provider block (SPEC §5.2.5).
  `codex-exec` takes `sandbox_mode` for its command sandbox. `claude-code` takes
  `sandbox_mode: srt`, which wraps the whole harness in `@anthropic-ai/sandbox-runtime` under a
  BEN-composed filesystem and network policy. It requires an isolated config directory, an
  environment-authenticated harness, a publish block and a git identity; readiness refuses when
  the host cannot deliver that posture.
  The implementation landed in #149; [issue #81](https://github.com/srhg-ai-7cef3f93/ben/issues/81)
  closed on 2026-09-03 with the real-agent proof supplied by the Airlock canaries (#195), where the
  substrate composes the sandbox-runtime wrapper. The adapter's own `sandbox_mode: srt` path has not
  yet carried a real agent on a compatible host.
- **No adapter setting proves protected mode by itself.** §10.1 states an outcome: the deployment
  must verify that the run cannot reach the tracker credential or daemon process on its platform.
- **A container** is a workspace strategy, not an adapter setting. It is deferred (SPEC §6.1,
  §13) — the `WorkspaceProvider` seam exists so it lands without touching the orchestrator.

A domain allowlist enforced without terminating TLS is defence in depth, not a boundary.

## Credential sources

§10.2 asks for two credentials that are not the same credential. Naming a PAT in `$GITHUB_TOKEN`
satisfies that only as far as an operator's discipline goes: the token is long-lived, it sits in
the daemon's environment for the life of the process, and rewriting `/etc/ben/env` changes nothing
until a restart. `credential_sources` (SPEC §5.2.10) is the alternative — each credential minted at
the moment it is needed, from a source that states a deadline BEN will not use it past.

```yaml
credential_sources:
  tracker:
    kind: octo_sts
    url: https://octo.srhg-nonprod.cloud
    scope: srhg-ai-7cef3f93
    identity: ben-tracker
    oidc_token_path: /var/run/secrets/octo/oidc-token
  publish:
    kind: octo_sts
    url: https://octo.srhg-nonprod.cloud
    scope: srhg-ai-7cef3f93
    identity: ben-publish
    oidc_token_path: /var/run/secrets/octo/oidc-token
```

The tracker block then names its source instead of a token, and so does `publish`:

```yaml
tracker:
  provider:
    repo: srhg-ai-7cef3f93/ben
    credential_source: tracker
    claim_assignee: ben-bot          # REQUIRED here — see below
publish:
  kind: source
  source: publish
  env: GH_TOKEN
```

Five things to get right, each of which BEN either refuses or cannot see:

- **Mount the OIDC projection once.** Both sources read the same projected service-account token
  (`audience: octo`, `expirationSeconds: 3600`). Sharing it is the *intended* deployment, not a
  collision: the two identities select distinct trust policies, each limited by
  `repositories: [ben]`. BEN's split check reads URL, scope and identity — never the token path —
  precisely so this loads.
- **Set no `OCTO_*` environment variables.** There is no environment fallback and no default. A
  pod-level `OCTO_IDENTITY` cannot represent two identities and would collapse the §10.2 split.
- **`claim_assignee` is required** with a workload-identity source. Such a credential is statically
  known not to authenticate as a machine user, so there is no login for §8.4's claim to fall back
  to; omitting it is a load refusal rather than a runtime surprise.
- **`limits.attempt_timeout_ms` must fit the deadline.** `octo_sts` declares 50 minutes usable, so
  the attempt maximum is **45 minutes** — the remaining five absorb issuer clock skew and the
  publish step itself. The one-hour default does *not* fit, and the load refusal shows you the
  arithmetic. This gate is skipped entirely for `static` and for every legacy spelling, which is
  why existing configs are unaffected.
- **Sidekick's values are not these values.** It uses a different Octo STS instance, audience and
  region. Do not copy them.

`kind: static` (a `value` naming exactly one `$VAR`) stays available for development, and every
legacy spelling — `tracker.provider.token`, `publish.kind: token`, an omitted token falling back to
`GITHUB_TOKEN` — keeps working unchanged. Those are explicitly *unbounded*: they state no deadline,
and BEN records that rather than pretending to one. Naming both a legacy token and a
`credential_source` on one block is a load refusal, because one of the two is silently doing
nothing.

Two properties worth knowing when something goes wrong. A credential failure is classified
**transient**, **permanent**, or **unknown**, and only an explicit transient retries the attempt —
a wrong trust policy parks for a human rather than spending three attempts reaching the same place.
And whatever the class, a non-transient failure logs at **error** naming the source's *authority*
(`octo:<url>#<scope>#<identity>`) and never the token, so a misconfigured policy is readable off the
journal instead of inferred from a silent stall.

`ben config effective` renders `credential_sources` in full: it holds no secret by construction.

## Risk-accepted mode

A deployment that cannot put a boundary between the run and the daemon may still run unattended,
but §10.1 requires that be an explicit, recorded choice — never a default. In that mode **the
agent is trusted with the daemon's tracker authority**: the dispatch label becomes routing
rather than a security boundary, because a subverted run can apply it itself.

Every workflow records this choice in the required `deployment.mode` field: `protected`,
`risk-accepted`, or the human-present `attended` exemption. `risk-accepted` also requires a
non-blank `accepted_because`. Omission is a load refusal for `ben run` and `ben config effective`,
regardless of the supervisor. The sample unit's `ExecStartPre` gate is an additional deliberate
deployment acknowledgement, and stays until a deployment records the mode decision the README's
**Status** section describes.

Risk-accepted mode relaxes that one requirement and no others. Branch protection in particular
becomes *more* load-bearing, not less: with the label demoted to routing, PR review is the only
gate left.

BEN's dogfood workflow is intended for risk-accepted mode, on the reasoning that its sole labeler
is also the operator holding the credentials and reviewing every PR. That stops being true the
moment a second principal can label — which is why SPEC §13 names it as a trigger for the
container workspace strategy. It is not an approved unattended deployment:
[issue #76](https://github.com/srhg-ai-7cef3f93/ben/issues/76) closed on 2026-08-20, but that
closure records readiness, not the deployment-mode decision — see the README's **Status** section.

The sample unit is shipped but blocked by default; this document describes the posture required
before enabling it.
