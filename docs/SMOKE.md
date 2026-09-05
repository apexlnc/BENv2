# The real-integration smoke profile (SPEC §12.4)

One scripted issue, end to end on a canary repository, through a real agent
harness and a real GitHub: claim → worktree → publish → verify.

```sh
BEN_SMOKE_REPO=<owner>/<canary> make smoke
```

That is the whole interface. Everything else is configured by environment. The
command runs on a Linux systemd host: the script checks its local tools,
credential API access, and smoke workflow, then starts BEN in a transient
per-user scope with `Delegate=yes`. It waits for BEN to publish running state —
which happens only after the local execution-domain readiness canary completes —
before it creates any label or issue. Runtime failures can
still occur later, but a local-domain readiness refusal leaves the forge
unchanged.
The workflow source is committed as `scripts/smoke-workflow.md`.
`required_labels` is literal configuration, so the script replaces its one
queue-label marker in a temporary copy and then asks `ben config effective` to
validate the exact rendered copy the daemon will run. `make workflow-check`
load-validates the committed source and its inert marker in credential-free CI.

## Why this is not in CI

`make check` is the definition of green, and this is deliberately outside it
(SPEC §12.4: RECOMMENDED, not CI-required). It needs forge and harness credentials, spends
agent tokens, and writes to a repository — none of which belongs in a check that
must run on every push.

What it buys in exchange is the one thing CI structurally cannot have. The
§12.3 invariant suite models the outside world through `internal/fake`, and a
fake is faithful to the adapter it stands in for only as of the last time
somebody checked. Harness drift — a renamed stream field, a changed exit
convention, a new required flag, a GitHub response shape that moved — is
invisible to every test in this repository until this profile runs. Run it
after touching either adapter, before a release, and nightly if you have
somewhere to run it.

For what CI *does* cover, read the coverage map in `internal/integration/doc.go`.
No row is owed; every test it names is resolved against the module by
`internal/arch/testnames_test.go`, so a row that has rotted fails `make check`.

## What you need

**A Linux host booted with systemd and a reachable user manager.** Run the
canonical command from a login session where `systemctl --user
show-environment` succeeds. macOS and other unsupported local platforms refuse
before a GitHub request, and a Linux host without writable delegated unified
cgroup v2 refuses before a GitHub write. Use the separately documented
Kubernetes canary below when the real run belongs on the remote substrate.

`smoke.sh` uses `systemd-run --user --scope --property=Delegate=yes` for the one
daemon it both probes and runs. A scope is intentional: the command inherits
the invoking shell's environment, including the three credentials, rather than
copying them into persistent unit configuration. BEN's own `Ready` path remains
the authority — systemd merely accepting the scope is not a pass. The canary
must still prove the unified cgroup-v2, `nsdelegate`, namespace, pidfd,
`cgroup.kill`, and cleanup facts described in
[DEPLOY.md](DEPLOY.md#provisioning-the-account).
There is no process-group fallback.

**A canary repository.** A throwaway with a default branch and nothing of value
in it. The script refuses to run against the repository named in BEN's own
`WORKFLOW.md`: the dogfood repository is a real queue, and smoke issues do not belong in it.
That refusal **fails closed** (#244) — it reads the repository through BEN's own
loader (`ben config effective`, which needs no credential and no network), and a
`WORKFLOW.md` it cannot load, or a rendering it cannot get exactly one
`tracker.provider.repo` out of, stops the run rather than allowing it. The
comparison is case-folded, because GitHub repository names are: a canary that
differs from the dogfood repository only in case *is* the dogfood repository.
Branch protection is *not* wanted here — the point is to watch a pull request be
opened, not to test the review gate.

**Two GitHub credentials, and they must differ** (SPEC §10.2):

| variable | who holds it | scope |
|---|---|---|
| `GITHUB_TOKEN` | the daemon | issues: read/write, assignment, labels, comments, plus contents read for the base clone. **Not** push, **not** pull-requests. |
| `GH_TOKEN` | the agent | contents write, pull-requests write. **Not** issues. |

The script checks they are not the same string and refuses if they are, and then
asks each one to do its own job — one issues read, one pull-requests read —
before it creates anything. It also generates a per-run queue label, proves by
a paginated read that the label does not exist, and starts the daemon against
that absent route. Only after readiness does it create the label and canary
issue, so a startup probe cannot dispatch a pre-existing item. That is not
pedantry: a run holding the tracker
credential can strip its own `ben:*` labels, take the assignment, close the
issue — rewrite the queue that dispatched it.

**`ANTHROPIC_API_KEY` is the harness credential.** The smoke workflow runs
Claude through `sandbox_mode: srt`: an unsandboxed same-UID harness could rewrite
the daemon's Git scratch configuration, invalidating the remote evidence this
profile exists to test. That posture denies the logged-in session under
`$HOME`, so the credential must be supplied through the environment.

**`gh` prefers `GH_TOKEN` over `GITHUB_TOKEN` when both are set**, and both are
set here. So the script routes every call through `gh_tracker` or `gh_agent`
rather than calling `gh` bare: an unqualified call authenticates as the *agent*,
which under the scopes above cannot so much as read the issue it is working. A
bare `gh` in `scripts/smoke.sh` is a bug. BEN's own `WORKFLOW.md` leans on the
same precedence from the other side — its `publish` block injects `GH_TOKEN` so
the agent's `gh pr create` publishes as the publisher and not as the daemon.

**`claude` and `srt` on `PATH`**, plus `gh`, `git` and `go`. The script also runs
AGENTS.md's `go env` audit for real, because a persisted `GO111MODULE=off` is
invisible to every interactive shell that exports over it and perfectly visible
to BEN's hooks — which is how BEN's first dogfood run died.

## Knobs

| variable | default | meaning |
|---|---|---|
| `BEN_SMOKE_REPO` | — | required; `owner/name` of the canary |
| `BEN_SMOKE_TIMEOUT` | `900` | seconds to wait for the published verdict |
| `BEN_SMOKE_READY_TIMEOUT` | `120` | seconds to wait for delegated local readiness before any forge write |
| `BEN_SMOKE_KEEP` | unset | `1` keeps the issue, branch and pull request for inspection |

## What a pass means

The script waits for two separate facts, in order, and the distinction between
them is the whole of SPEC §3.5 — evidence over claims:

1. **An open pull request on `ben/<issue>`.** That is the *agent's* work: it
   committed, pushed, and ran `gh pr create`.
2. **BEN's publish milestone comment carrying that URL, with the `ben:*` state
   labels cleared.** That is BEN's verdict: the three §9.7 legs held — the
   branch advanced past its claim-time base, the base is an ancestor of the
   head, origin carries the head — and leg 3 found the open pull request.

A run that reaches 1 and not 2 has an agent that published and a BEN that would
not verify it, which is a far more interesting failure than either half alone.
The script says which one it stopped at.

On exit it drains a live daemon with `SIGTERM`, so an ordinary pass also
exercises §11's graceful shutdown: dispatch stops, in-flight runs are
interrupted, and the drain waits for every execution domain to be confirmed
quiet. If BEN has already exited, cleanup still waits on the delegated scope:
its supervisor or a descendant can outlive the daemon process. Forge cleanup is
authorized only after that scope reports inactive. If it does not report
inactive within a minute, the script warns and leaves both the scope and forge
artifacts alone — killing or deleting them there is exactly the abandonment the
drain exists to prevent.

Unless `BEN_SMOKE_KEEP=1`, the pull request and issue are closed, the branch is
deleted, and the per-run queue label is removed afterwards. The temporary
directory holding the built binary, effective config output, worktrees, state
directory, and daemon JSON log is always left behind, and the script prints its
path.

## Kubernetes canary runtime preflight

A different profile from `make smoke`, with the same purpose. The supervised canary
([DEPLOY.md](DEPLOY.md#kubernetes-canary)) runs the real daemon in a pod against BEN's own
repository, dispatched by a queue label rather than by a script — so there is no script to check
its tools first, and this section is that check. Run it **before** you add the label. Everything
below is otherwise reported half an hour later as a burned agent attempt, or not reported at all.

Read-only throughout, deliberately: GitOps owns the pod contract, so `kubectl apply`, `edit`,
`scale` and `delete` are reverted by Argo self-heal. Staging or scaling the canary is a merge to
`argocd-srhg-ai-nonprod`.

The examples name the deployment as it stands — namespace `ben`, pod `ben-daemon-0`, workflow
mounted at `/etc/app/WORKFLOW.md`. Take that last path from the daemon's own argv (step 2) rather
than from here: it is GitOps' to choose, and it is not the image's `CMD` default.

### 1. Ready, and running the digest you published

```sh
kubectl -n ben get pod ben-daemon-0 \
  -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}Ready={.status}{"\n"}{end}'
kubectl -n ben get pod ben-daemon-0 \
  -o jsonpath='{range .status.containerStatuses[*]}{.name} ready={.ready} restarts={.restartCount}{"\n"}  spec    {.image}{"\n"}  running {.imageID}{"\n"}{end}'
```

Want `Ready=True`, every container `ready=true`, and a restart count you can account for. The two
image lines answer different questions and the second is the one that matters: `.image` is the tag
the pod spec asked for, `.imageID` is the digest the kubelet resolved and is running. Tags in
`844479804508.dkr.ecr.us-east-2.amazonaws.com/ben/daemon` are immutable full commit SHAs, so the
tag also names the revision whose `make check` you are about to trust — but a rollout that only
half happened leaves a pod running the previous digest under a spec naming the new tag, and the
tag alone cannot say so.

Confirm that digest is the one the publish workflow built for that commit:

```sh
aws ecr describe-images --region us-east-2 --repository-name ben/daemon \
  --image-ids imageTag=<full-commit-sha> --query 'imageDetails[0].imageDigest' --output text
```

### 2. tini is PID 1, BEN is its child, and `ps` is there to say so

```sh
kubectl -n ben exec ben-daemon-0 -- ps -o pid=,ppid=,stat=,args= -e
```

```
      1       0 Ss   /usr/bin/tini -- /usr/local/bin/ben run /etc/app/WORKFLOW.md
      6       1 Sl   /usr/local/bin/ben run /etc/app/WORKFLOW.md
```

PID 1 is `/usr/bin/tini`, and `/usr/local/bin/ben` is its direct child — the entrypoint contract
the runtime image is built on. tini reaps the agent descendants orphaned to the container init
(#184) and forwards Kubernetes' SIGTERM to BEN, whose §9.8 drain then owns shutdown; BEN as PID 1
gives you the second guarantee without the first, and a shell wrapper gives you neither. This is
also where you read the workflow path the daemon was actually started with.

`ps` is part of that contract rather than a convenience of yours, so prove it is installed:

```sh
kubectl -n ben exec ben-daemon-0 -- sh -c 'command -v ps && ps -o stat= -p 1'
```

Want `/usr/bin/ps` and a process state (`Ss`). BEN's own `make check` runs in this image, and its
process-discipline suite asks exactly that query because signal 0 cannot tell a live process from
a zombie; on an image without procps the missing executable reads as a false death verdict (#186).

### 3. The harness the *active workflow* names, not both adapters

The image ships one agent harness. Ask the running workflow which one it dispatches instead of
assuming, starting from the argv step 2 printed:

```sh
kubectl -n ben exec ben-daemon-0 -- ben config effective /etc/app/WORKFLOW.md | grep -A2 '^agent:'
```

```
agent:
  kind: claude-code                                  (file)
  provider:
```

Then check that kind's harness binary, and only it:

| `agent.kind` | default binary | check | on this image |
|---|---|---|---|
| `claude-code` | `claude` | `kubectl -n ben exec ben-daemon-0 -- claude --version` → `2.1.221 (Claude Code)` | installed |
| `codex-exec` | `codex` | `kubectl -n ben exec ben-daemon-0 -- codex --version` | **absent** |

`agent.provider.binary` overrides the default name for either kind, and `ben config effective`
prints it whenever the workflow sets one — which is why the command above comes before the table.
A harness that is missing or unusable is not a dispatch-time surprise: `ben run` calls the
adapter's `Ready` while it builds the runtime, so the daemon refuses at startup and the pod
crash-loops. The value of checking here is being told *which binary* in one line, rather than
inferring it from a restart count.

### 4. The Git author is the app-bot noreply identity

```sh
kubectl -n ben exec ben-daemon-0 -- printenv GIT_AUTHOR_EMAIL
kubectl -n ben exec ben-daemon-0 -- git config --global user.email
```

Both must print the App's canonical noreply address —
`275096130+srhg-ai-octo-sts-nonprod[bot]@users.noreply.github.com`. A commit authored as anything
else is a commit GitHub does not attribute to the publishing App, which is the identity half of
§10.1's forge control.

**The second command is the one that is easy to skip and the one that fails.** A run's environment
is composed from §7.6's allowlist rather than inherited, so a pod-level `GIT_AUTHOR_*` reaches the
daemon and not the agent. Measured on `ben-daemon-0` on 2026-08-20: the pod environment carried
both variables, `$HOME/.gitconfig` did not exist, and git in the agent's environment answered
`fatal: empty ident name` — an attempt that can do the whole ticket and then fail to commit it.
Check the config-file half, which the agent's inherited `HOME` does reach.

### 5. Heartbeat and startup recovery, then the label

```sh
kubectl -n ben exec ben-daemon-0 -- ben status /etc/app/WORKFLOW.md
```

```
workflow   app-3f4a491e
config     /etc/app/WORKFLOW.md
state dir  /var/lib/ben/state/ben/app-3f4a491e
daemon     ben-daemon-0/app-3f4a491e
status     running — pid 6, last heartbeat 2s ago
```

Want `running` with a heartbeat of seconds, and a pid equal to the `ben` process step 2 showed
under tini. `stale` is a daemon that is not writing its record — killed or wedged — and a Ready
pod does not distinguish the two: `ben status` reads the §10.3 state files and never the daemon.
Run it *inside* the pod so `XDG_STATE_HOME` resolves the same state directory the daemon writes.
Check `RUNS` too: this canary sets `limits.max_concurrent_agents: 1`, so an issue already in flight
queues yours behind it.

Then the §9.10 half, from the daemon's own JSON log:

```sh
kubectl -n ben logs ben-daemon-0 | jq -c 'select(.msg | test("deployment declares|recovering|recovery"))'
```

Want the declared mode — `deployment declares attended mode` — and `recovering`, carrying the
`candidates` count and the `principal` whose claims were reconstructed. `deployment declares
risk-accepted mode` on this canary is a stop: it asserts a boundary the pod does not have, since
daemon and agent share its process identity.

`recovery did not complete` is the line that must be absent. It means the daemon started without
reconstructing the claims this principal holds, and dispatch skips only issues a local record
already covers — so labelling into that state can put a second agent onto an issue already being
worked.

Only now add the queue label `ben config effective` printed under `tracker.required_labels`
(`ben-kube-canary` for this canary, deliberately not the dogfood queue's `ben-queue`):

```sh
gh issue edit <issue> --repo srhg-ai-7cef3f93/ben --add-label ben-kube-canary
```

## The review-controller cycle (#11)

A third profile, and the only end-to-end proof that the loop of
[REVIEW.md](REVIEW.md) closes. `make check` covers every decision the controller
can make and every crash it can resume from, against a fake forge; what it
structurally cannot cover is the part where GitHub, a real model and a real BEN
daemon each behave the way we modelled them.

**Not yet run.** This section is the procedure, written from the design rather
than from a recording, and it says so for the same reason the fixture gaps
below do: a profile that claims to have passed and has not is worse than one
that admits it is pending.

Preconditions: the canary of **Kubernetes canary runtime preflight** above,
green; the controller deployed and dry-run per REVIEW.md; and one canary issue
whose ticket is small enough to be worth two rounds and clearly wrong enough on
the first pass to earn `changes_requested` — a ticket whose acceptance criteria
name a test that the obvious first implementation omits works well.

Then, in order, reading each fact off the issue and the pull request rather than
off a log:

1. **A human applies the queue label.** The controller must do nothing yet:
   there is no published milestone.
2. **BEN claims, works, publishes.** An open pull request on `ben/<issue>`, and
   BEN's published milestone carrying its URL — the two facts `make smoke`
   already waits for.
3. **Round one.** The controller publishes exactly one `COMMENT` review by the
   controller identity, bound by `commit_id` to that head, carrying a
   `ben:review` marker whose `verdict` is `changes_requested`. Then: the queue
   label is **still standing**, BEN is no longer assigned, and one `ben:route`
   comment records `outcome=revise`.
4. **BEN reclaims and revises.** A new `assigned` event — and therefore a new
   claim epoch — a new head on the same branch and the same pull request, and a
   second published milestone with a *different* occurrence.
5. **Round two.** A second review at the new head. Given a clean verdict: the
   queue label is removed, the informational label is added if configured, BEN
   is **still assigned** (the controller never unassigns on a stop path), and a
   second route comment records `outcome=human-review`.
6. **BEN releases.** Within a poll tick or two, BEN observes the revocation and
   drops the claim itself (SPEC §9.8, "Required labels gone").
7. **The human gate is intact.** The pull request is unmergeable: no approving
   review exists, and neither of the controller's reviews is one.

What each step is really testing, and what to write down if it fails:

- Step 3 proves the trigger path — that BEN's milestone comment actually
  delivers an `issue_comment` event to the controller's identity. If it does
  not, the scheduled sweep picks it up within the cron period and the cycle
  still completes; that is a *pass with a note*, not a failure, and the note is
  which credential BEN posted with.
- Step 4 is the claim-epoch prerequisite doing its job. If BEN publishes
  `done` again with no new commits, §9.7's base was not reminted and the whole
  loop is unsound — stop and treat it as a regression of
  [#191](https://github.com/srhg-ai-7cef3f93/ben/pull/191), not of the controller.
- Step 5's "still assigned" is the one an eager reading of the ticket gets
  wrong. The controller unassigning on a stop path would race BEN's own release.

Then run the negative half, which is cheaper and catches more:

- **Redelivery.** Reconcile the same issue again — `benreview -repo o/r -issue
  <n>`, or simply let the daemon's next review sweep come round. Nothing
  changes: no second review, no second route comment, and no second Codex run.
- **No progress.** Re-apply the queue label, let BEN reclaim, and let it publish
  without adding a commit. Before revocation the controller records a
  `ben:route-intent` for that occurrence, unchanged head and occurrence's
  approval event; it then stops with `outcome=no-progress` and no new review.
- **Revocation.** Remove the queue label mid-cycle. The controller stops and BEN
  releases.

Leave the issue and pull request open afterwards and link them from the ticket:
this profile's output is the artifacts, not a script's exit code.

## Re-recording the adapter fixtures

Both agent adapters are tested against **recorded** harness streams, so that the
translators are checked against what the harness emits rather than against what
we remember it emitting (SPEC §12.2):

- `internal/agent/claudecode/testdata/stream-success.jsonl` — claude 2.1.221
- `internal/agent/codexexec/testdata/stream-success.jsonl` and
  `stream-auth-failure.jsonl` — codex-cli 0.147.0

Each `testdata/README.md` carries the exact command that produced its file and
the edits applied to it. Re-record with the same command when a harness changes
shape; this profile is what tells you it has.

Two fixture gaps are known and open, and both need a recording against a live
system rather than a change to this repository:

- **claude-code has no recorded failure stream.** codex-exec has one
  (`stream-auth-failure.jsonl`), and it is the fixture behind that adapter's
  `auth` verdict. The claude-code failure verdicts are covered by the
  conformance suite's synthesized streams only.
- **The GitHub adapter has no recorded API payloads.** `fake_test.go` models the
  server's *behaviour* — ETag revalidation, 304s costing no rate limit, primary
  and secondary limit shapes — faithfully and in detail, but the payloads it
  serves are built from `go-github` structs. So issue normalization is tested
  against our model of GitHub rather than against GitHub.

Neither is fabricated in the meantime. A fixture that says "recorded" and was
not is worse than no fixture: it pins the shape we already believed.
