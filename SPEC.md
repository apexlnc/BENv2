# BEN — Specification

Status: **v1, locked** (2026-08-06)

**Amendments to the locked spec** (each requires explicit human sign-off before editing):

| Date | Sections | Change | Authority |
|---|---|---|---|
| 2026-09-04 | §7.3 | **`output_overflow` joins the closed taxonomy, non-retryable.** A single stdout line past §7.5's scanner ceiling becomes a runner-owned verdict of its own, claimed by the reader that hit it through the same funnel as `stalled` and `timeout`, so the signal ladder runs and publication waits for it. The transcript records the cut as a BEN-namespaced line and deliberately retains **no fragment** of the oversized line: a fragment ending wherever the buffer ran out is the one shape a credential can straddle the stateless redacting writer unmatched. Before this the condition was indistinguishable from EOF — the child sat blocked on a full pipe, produced no activity, and `stall_timeout_ms` read it as `stalled`, retryable, so the retry reproduced it and burned `max_attempts` on a condition that is not transient. The same change bounds what the harness *holds* of the child's output without amending the sections that govern it: `progress(text)` is cut at 64 KiB where the adapter mints it, the notice stating the cut inside the bound (§7.2 is unchanged — the transcript keeps the whole line), and a readiness probe's captured output is retained to 64 KiB and refused past it (§7.1's `Ready` classifies from a bounded body) | [#235](https://github.com/srhg-ai-7cef3f93/ben/issues/235) — sign-off in review of [#287](https://github.com/srhg-ai-7cef3f93/ben/pull/287) |
| 2026-09-03 | §7.1, §7.5, §9.8, §9.10 | **Linux local attempts use a non-escapable execution domain, not a POSIX process group.** BEN atomically places a trusted PID-namespace supervisor in a delegated cgroup-v2 leaf, then creates the cgroup namespace so it roots at that leaf. The supervisor covers every inherited proc/cgroup alias without detaching locked mounts, permits provider-created nested proc/cgroup2 mounts only where the kernel keeps them rooted at the attempt or descendants, and remains alive until the entire PID namespace is empty. `Probe` is read-only; `Stop` uses pidfd TERM then bounded `cgroup.kill`; both decide from supervisor exit with a populated-cgroup veto. Canonical boot, cgroup, PID/start and namespace evidence makes recovery reuse-safe. A provider-owned janitor removes quiet attempt trees, startup sweeps empty residue, and persistent cleanup failure refuses new local starts. The matrix is Linux-only and fail-closed with no process-group fallback. The mechanism landed dormant in #274; this #234 change atomically activates SPEC, production, fake, readiness and the shipped unit. | [#274](https://github.com/srhg-ai-7cef3f93/ben/issues/274) — [superseding v4 design](https://github.com/srhg-ai-7cef3f93/ben/issues/274#issuecomment-5529006448), [exact maintainer sign-off](https://github.com/srhg-ai-7cef3f93/ben/issues/274#issuecomment-5529097478) before implementation |
| 2026-08-31 | §5.2.10 | **An `octo_sts` issuer is HTTPS-only.** Its exchange presents the projected workload-identity JWT as a bearer credential, so `http` is a load-time refusal rather than a supported transport or a warning; URL canonicalization therefore drops only the default HTTPS port | [#245](https://github.com/srhg-ai-7cef3f93/ben/issues/245) — explicit maintainer sign-off to fix the reviewed change in [#250](https://github.com/srhg-ai-7cef3f93/ben/pull/250) |
| 2026-08-31 | §5.2, §5.4, §5.6, §5.8, §6.1, §6.2, §8.2, §8.4, §9.7, §9.10, §10.1, §11 | **One claim-scoped base/target branch, on local and Airlock substrates.** Optional `workspace.base_branch` selects an unqualified branch; omission selects the repository default when a new assignment epoch is first prepared. The selected target is persisted atomically with that epoch's verification base and is retained across retry, rollback, reload, restart and default movement. `target_branch` is trusted prompt data, while exact PR-base equality is authoritative publish evidence. `FindPR` refuses more than one exact-head open PR without choosing by order or target. Airlock's daemon-owned mirror selects and stores the same tuple; remote verification reads it from the mirror claim, never from workflow state or the sandbox. Targetless pre-amendment local, mirror and remote-cycle records are named, non-authorizing legacy state for their existing epoch; only a later assignment epoch may retain valid outgoing base/cycle facts and replace them with a complete tuple before hooks or launch. Deploy only after the existing empty-principal drain. | [#152](https://github.com/srhg-ai-7cef3f93/ben/issues/152) — accepted ticketprep packet `sha256:772099818aff9bd25682abe88cbc6852c8cb828b650c91fc4019b7dd7b6974be`: `DEC-01`–`DEC-05` selected `OPT-01`, `REC-01` accepted; implementation and this exact locked-spec edit explicitly authorized by the user |
| 2026-08-20 | §3.1, §6.1, §6.2, §8.2, §8.4, §9.2, §9.5, §9.6, §9.7, §9.8, §9.10, §12.3 | **Claim-scoped verification bases.** The tracker-native ID of the current claim-establishing `assigned` event is the claim epoch. Before BEN projects `ben:claimed`, the workspace provider durably records a pending intent for that epoch. At its first claim-aware prepare, after remote-first reattachment and before any current-attempt hook, it derives #94's prior-work fact against the outgoing epoch's pin and atomically replaces the pending intent with `{epoch, current branch head}`. Every later prepare and every §9.7 read validates that same pair; retries, continuations, human unparks and restarts inside one assignment retain it, while an unassignment followed by reassignment creates a new epoch and base. Recovery never asks §9.7 against a missing, pending, unreadable or mismatched epoch. A matching pending epoch with no run marker resumes the pre-hook prepare; a pending epoch with a run marker, or an absent/unreadable/mismatched record, parks and cannot be unparked into a launch under that claim. The epoch and base are one atomic safety fact, so no crash can expose a new epoch with an old base or an old epoch with a new base. This narrows `done`; it changes no tracker write capability and preserves #94's evidence-derived attempt floor. | [#11](https://github.com/srhg-ai-7cef3f93/ben/issues/11) — exact contract explicitly approved before implementation and this locked-spec edit |
| 2026-08-19 | §9.8, §11 | **A held release that cannot land is resolved by a read, not by the write.** A settled held-claim release remains owed and retried after failure, because a write's refusal cannot classify whether the issue or assignment still exists. Each failed attempt makes the claim eligible for a confirming `Get`: `ErrIssueNotFound`, or a successful read showing the configured principal no longer assigned, permits forgetting it; a failed read or continued assignment retains the claim and re-drives the same release. The confirmation shares §9.8's existing one-per-tick held-claim budget and cursor with sweep absences, so the two disjoint candidate sets form one fair rotation rather than two request allowances. This supersedes #148's K-absence/K-tick latency sentence: K is now the size of the stable union offered to that rotation. The confirmation neither re-derives the settled reason nor re-baselines `revision`, and it reads no `ClaimHistory`; `HeldClaims` failure does not gate it. During graceful shutdown, confirming a release ordered before the drain completes that already-ordered effect and may run under the same bound without initiating a new release or terminal projection | [#135](https://github.com/srhg-ai-7cef3f93/ben/issues/135) — [sign-off #5346274704](https://github.com/srhg-ai-7cef3f93/ben/issues/135#issuecomment-5346274704) before implementation |
| 2026-08-19 | §5.2, §5.4, §5.5, §5.7, §5.8, §6.2, §7.1, §7.3, §7.7, §8, §8.4, §8.5, §9.2, §9.7, §9.8, §10.2, §11 | **Every tracker, base-fetch and publish credential is obtained from a credential source at the moment it is needed**, and a bounded source states a deadline BEN does not use a token past. Sixteen amendments, one invariant: **no source failure falls through to a different credential.** A top-level `credential_sources` map declares named sources under a closed registered kind set — `octo_sts` and `static` — each with a **strict per-kind schema** enforced by a `Describe` that is **pure**: no network, no filesystem, no instance, so a workload-identity config load-validates on a host holding no credential and `make workflow-check` stays credential-free. A partially configured source refuses; it never degrades to another kind. **Every legacy spelling compiles into an implicit source**, so there is exactly one runtime treatment and no nil-means-legacy branch. Credential errors carry a class — transient, permanent, or the **inert zero**, which parks — and that class **routes at exactly two sites**: §9.8's attempt retry at `Prepare`/`Start`, and §9.7's verification. It routes nothing on the tracker's own paths, where a read retries next tick and an owed write **stays owed across every class**; there it is read only for log severity. A transient §9.7 credential failure retries **in `verifying`**, once per poll tick, recording no attempt and routing no verdict — fail-closed covers evidence that contradicts or cannot be established, and a credential that could not be obtained establishes neither. `credential` joins §7.3's closed taxonomy as the retryable reason, and `preparing → needs-review` joins §9.2's table for the classes that park. An **empty token value** and **TTL insufficiency** are classified at the credential boundary, before any downstream GitHub request, git invocation or agent launch. Reload gains **pure, name-free bindings** replacing the raw `Config.Tracker` comparison: a rename with an identical definition is not a rebuild and an edit beneath an unchanged name is, with a loader-resolved literal's **full SHA-256** in the binding key so a rotated literal still rebuilds. Source identity (`Authority`) and definition (`BindingKey`) are **distinct keys**: Octo scope is in both, OIDC token path in the binding key only — so one projected service-account token federating two trust-policy identities is the intended deployment and loads. The §10.2 split becomes one rule over identities read from **loader provenance**: two credentials with equal authority are one credential however spelled, and tracker↔publisher equality is a load refusal. `Repository` and `RunnerOptions` carry **no credential scope**; each source instance owns its own, and §8.5's backoff gate is keyed by **(endpoint, authority)** rather than by the token. Out of scope, named: `github_app`, whose private-key custody the interfaces admit without anticipating | [#156](https://github.com/srhg-ai-7cef3f93/ben/issues/156) — contract reviewed over ten revisions before filing; all sixteen amendments signed off before code, per the ticket's acceptance list |
| 2026-08-19 | §9.8 | **Held-claim absence confirmations are paced.** §9.8's evidence rule is unchanged: absence from the assignee-filtered `HeldClaims` read is never evidence, and a held claim is never acted on without a confirming `Get`. The confirmations are now capped at **one held claim per tick**, offered in an explicit rotation that advances on the offer rather than its outcome. A confirmation that keeps failing therefore cannot retake the only slot and starve the rest; K simultaneous absences resolve over K ticks. This changes latency, not verdicts, and bounds the absence path by a constant rather than by the review backlog. A deferral MUST be reported, since a silent cap reads as having covered the set | [#148](https://github.com/srhg-ai-7cef3f93/ben/issues/148) — [sign-off #5344889690](https://github.com/srhg-ai-7cef3f93/ben/pull/171#issuecomment-5344889690) in review of [#171](https://github.com/srhg-ai-7cef3f93/ben/pull/171) |
| 2026-08-18 | §8.4 | **The claim assignee is configured independently of the tracker credential.** `claim_assignee` names the machine-user account BEN assigns, while omission preserves the credential-authenticated login fallback. Post-write verification and assigned-to-other exclusion apply identically to both sources. Because §9.8 and §9.10 read claims by assignee, deployments running more than one daemon MUST give each a distinct assignee | [#155](https://github.com/srhg-ai-7cef3f93/ben/issues/155) — [sign-off #5334871852](https://github.com/srhg-ai-7cef3f93/ben/issues/155#issuecomment-5334871852) before implementation |
| 2026-08-14 | §1, §8.1, §10.1 | **The agent may complete the merge.** Requirement 3's second load-bearing property — *the agent MUST NOT be able to complete the merge even once every requirement is satisfied*, which made the merge itself restricted to the reviewing principals — is **withdrawn**. §3.4 gates whether a human approved, not who presses the button afterwards, and finishing the loop after review is the intended shape of the tool rather than a concession. **Three** properties replace the two, because the relaxation is conditional in ways the old text did not have to state. *The approval binds to the commit that merges* survives and now carries alone what two carried before. *The required approval MUST be one the publish identity cannot supply* is new, and closes a hole the first draft of this amendment left open: forbidding an author from approving their own pull request constrains the agent only on pull requests the agent opened, while the pull-request write it needs to open one at all lets it approve a **human's or a second agent's** — an approval that counts, and that satisfies last-push approval too since the agent was not the pusher. The requirement is therefore over the approval, not the author: code-owner review over an owners file covering every path, with no agent among the owners. *The publish identity MUST NOT hold a capability that exempts it* is the promoted identity note, stated as a capability rather than a role because branch protection is waived for administrators **and** for custom roles carrying bypass-branch-protections, so "not an administrator" is insufficient. Requirement 3's opening sentence gains "to **satisfy** that requirement itself, or to **bypass** it". §1 invariant 5 and §8.1's write boundary said the issue closes when *a human merges*; both now say the merge is gated by a human's approving review whoever performs it. Withdrawing the property also removes the only reason BEN's guidance needed a mechanism that restricts merging while leaving reviewers able to merge — the finding that ruled out ruleset `update` rules and forced classic protection's push list | [#83](https://github.com/srhg-ai-7cef3f93/ben/issues/83) — sign-off in review of [#147](https://github.com/srhg-ai-7cef3f93/ben/pull/147), whose first round found the cross-author approval hole and the §1/§8.1 contradiction |
| 2026-08-14 | §5.6, §9.6 | **The prior attempt's account.** `run` gains a second leaf, `run.previous_attempt`: a bounded, preformatted account of the attempt before this one — its outcome, the commits and changed files it left on the branch, and the tail of what the agent said — derived only from facts BEN already retains, and composed when that attempt's outcome is **routed** rather than at render, which would read a workspace the next attempt may already be preparing. It joins the untrusted span and is **doubly untrusted**: an agent that had already read the fenced issue body wrote it, so it can restate anything that body carried, and carrying it into the next prompt outside a fence would launder it in one hop. **Every string in it is agent-authored, whatever proves it exists** — git is authoritative about a commit existing and a path being in a tree, never about the words the agent chose for either, so a commit subject and a filename are fenced exactly as the output tail is. It is therefore **one string fenced whole** rather than an object with a field per fact, whose safety would otherwise rest on one field still being present. The fence's note names its author, since telling an agent that the issue reporter wrote its own previous output is worse guidance than none. Absent on attempt 1, on an evidence-derived attempt floor, on a fresh host after a restart, and on the §9.6 **continuation track**, which carries the resume token and so already holds its own history. **Absent binds the empty string, not null** — the one nullable member of the closed set that does, because an untrusted variable may not appear in a condition, so no prompt can guard it, and a null emitted unguarded fails the strict backstop: null would make the variable unrenderable on exactly the attempts where it is absent. Bounded by an implementation constant well under `limits.max_prompt_bytes`, with every truncation **stated in the rendered text** and every cut on a UTF-8 boundary; the bound is not a guarantee against the §5.6 ceiling, which still refuses an oversized prompt. A branch that could not be read is reported as unread rather than as empty, and no failure in composing the account may fail an attempt. The read is **bounded in memory and in how long it will wait**: a commit subject and a filename are agent-authored and unbounded, and the locks a git read takes are held across fetches, so an unbounded read would let a repository an agent controls exhaust the daemon, and an unbounded wait would stall a finished attempt behind an unrelated fetch — holding a §9.5 slot and blocking the §11 drain | [#61](https://github.com/srhg-ai-7cef3f93/ben/issues/61) — contract [#5286876603](https://github.com/srhg-ai-7cef3f93/ben/issues/61#issuecomment-5286876603), amended per sign-off [#5286956659](https://github.com/srhg-ai-7cef3f93/ben/issues/61#issuecomment-5286956659). The empty-string binding and the two sentences on where the account is composed are consequences found during implementation, outside that sign-off's scope, and carry their own: requested at [#5296409992](https://github.com/srhg-ai-7cef3f93/ben/issues/61#issuecomment-5296409992) — the empty-string binding and where the account is composed, joined by the bounded-read requirement that the review of [#144](https://github.com/srhg-ai-7cef3f93/ben/pull/144) produced — with sign-off in review of that pull request |
| 2026-08-14 | §8.2, §9.8 | **Parked records are swept, not polled.** §9.8's per-record `Get` covered `needs-review` too, and that set grows with human latency exactly as the held set does — the cost shape #27 rejected for `done` claims. The one `HeldClaims` read the sweep already makes answers both parked rules: it is unfiltered in state and labels, and a parked issue is assigned, so it is in the response with the `state` the terminal rule reads and the labels the unpark rule reads. §9.8 therefore refreshes the **running** records per-issue and sweeps parked records with held claims off one conditional read. The bound is on **round trips**, not only billed requests: the per-issue reads are issued serially in one pass, so a review backlog was delaying the reconciliation of every running record behind it, and an authenticated 304 refunds the core budget but not GitHub's request-point allowance (§8.5). Three consequences settled here rather than assumed. **Absence** from the read is confirmed with one `Get` and never acted on (unchanged rule, new verdicts): deleted → dispose and drop; open but not assigned to the principal → the assignment **is** the claim (§8.3), so keep the workspace, leave the state label standing and drop the record, which is where §9.10 arrives too since `ClaimedByPrincipal` no longer returns it; still ours → the read lagged. **The unpark rule reads the response's labels only if the record's own state-label projection had landed before the read was issued** — otherwise BEN reads its own unlanded write as a human's gesture, which a §9.5 drift park (from `queued`, no `ben:*` ever projected) made reachable on `main`; that condition gates the label rule alone, since neither state nor assignment is a fact BEN's writes can move. **Running records keep their per-issue `Get`**: they are bounded by `max_concurrent_agents`, and their sharpest verdict — a human took the assignment — reads as an unexplained absence from an assignee-filtered list, so folding them in would add a confirmation to the one path that must stop a live process promptly | [#36](https://github.com/srhg-ai-7cef3f93/ben/issues/36) — the ticket names the two decisions it left open; contract, the decisions taken on them, the measured cost shape and the projection condition [#5295617656](https://github.com/srhg-ai-7cef3f93/ben/issues/36#issuecomment-5295617656). sign-off in review of [#143](https://github.com/srhg-ai-7cef3f93/ben/pull/143), over two rounds that changed what is signed here: the first found three defects in this row's own consequences — batched absences still O(parked), assignment checked ahead of terminal state, and a projection condition wide enough for a wedged comment to suppress a human unpark — and the second found that the confirmation budget left fairness to Go's unspecified map order, and that this section still contradicted the row about what a conditional read costs |
| 2026-08-13 | §5.2, §5.6, §10.1 | Every workflow declares a process-lifetime deployment mode: `protected`, `risk-accepted`, or the human-present `attended` exemption. There is no default; omission is a load refusal. Risk acceptance carries a non-blank recorded reason. The declaration asserts externally established facts, changes no run behavior, and requires restart to change | [#128](https://github.com/srhg-ai-7cef3f93/ben/issues/128) — contract [#5286960668](https://github.com/srhg-ai-7cef3f93/ben/issues/128#issuecomment-5286960668), wording review [#5287011212](https://github.com/srhg-ai-7cef3f93/ben/issues/128#issuecomment-5287011212), [sign-off](https://github.com/srhg-ai-7cef3f93/ben/issues/128#issuecomment-5289376286) |
| 2026-08-13 | §9.2, §9.5, §10.3 | **Approval binds to content.** `ben-queue` gated the issue and not its bytes: an author may edit title or body after a labeler approves, `revision` deliberately excludes both (§8.3), and the prompt was re-rendered from *current* content on every attempt — so the edit did not even have to precede dispatch. §9.5 gains the content-bound rule: the **approving instant** is the standing `labeled` event of the last required label applied; a claim **pins** the approved title and body and every attempt renders from the pin; an issue whose content changed after that instant, or whose edit the tracker cannot order against it, is not dispatched but parks `ben:needs-review`. **Reapproval is the same act and the only one** (§6.7) — a labeler re-applying a required label moves the instant and re-takes the pin, while the §9.2 re-queue approves nothing, so a re-queue over unreapproved drift parks again. The check runs at each dispatch decision (claim read-back, the §9.6 re-fetch on both tracks) and at no reconciliation tick, which refreshes routing facts only; §9.2 therefore gains `queued → needs-review` and `backoff → needs-review`, the two parks its own new rule requires. A tracker that cannot date an edit has stated nothing rather than "unedited", and an edit sharing a second with the approving label is unorderable and refused (§8.4). The canonical rendered prompt is retained per attempt beside its transcript, at the same permissions and subject to the same redaction (§10.3). §9.10's **unprojected claim** row becomes an **unapproved claim**: the window between an assignment landing and its first projection is the window this check occupies, so recovery adopts it at attempt 1 projecting nothing and runs the check as a fresh claim does, rather than announcing `ben:claimed` and dispatching content no principal is known to have approved | [#49](https://github.com/srhg-ai-7cef3f93/ben/issues/49) — contract [#5259464136](https://github.com/srhg-ai-7cef3f93/ben/issues/49#issuecomment-5259464136), decisions and revised wording [#5262172029](https://github.com/srhg-ai-7cef3f93/ben/issues/49#issuecomment-5262172029), sign-off with the title finding and the two-write race entry [#5262245815](https://github.com/srhg-ai-7cef3f93/ben/issues/49#issuecomment-5262245815) — which covers §9.5 and §10.3. The two §9.2 rows and the §9.10 row are consequences settled during implementation and review, outside that sign-off's scope, and carry their own: request [#5287228521](https://github.com/srhg-ai-7cef3f93/ben/issues/49#issuecomment-5287228521), sign-off [#5289325601](https://github.com/srhg-ai-7cef3f93/ben/issues/49#issuecomment-5289325601) in review of [#129](https://github.com/srhg-ai-7cef3f93/ben/pull/129) |
| 2026-08-13 | §6.1, §6.2, §6.3, §6.4, §7.1 | `Workspace` reports every path its provider owns — worktree, shared git dir, and a BEN-placed per-workspace private dir that shares the worktree's lifetime — and `RunSpec` carries them, so a sandbox posture binds a trusted shared git dir rather than one read from the agent's writable tree, and an adapter-owned harness config dir is placed by the provider rather than derived; §6.3's containment check extends to every reported path, and §7.1 gains the invocation-input/adapter-configuration distinction that keeps the bind-at-`New` rule intact | [#81](https://github.com/srhg-ai-7cef3f93/ben/issues/81), [#114](https://github.com/srhg-ai-7cef3f93/ben/issues/114) — sign-off [#5286463683](https://github.com/srhg-ai-7cef3f93/ben/issues/81#issuecomment-5286463683) |
| 2026-08-13 | §5.2, §5.5, §5.8, §6.7, §7.1, §7.6, §10.2 | The publish credential is a first-class closed `publish` block (§5.2.8; `kind: token` in v1) rather than an unmarked `agent.provider.env_passthrough` entry, because the identity BEN publishes as is the operator's choice and a forge rule names a principal rather than a token (§10.1). `env` names a child variable **owned exclusively by the publish credential** — no adapter environment surface may respell it, and it may itself name neither an adapter-owned variable nor a §7.6 allowlist variable, since two config sites writing one child variable means one of them is silently doing nothing. `value` is exactly one `$VAR` reference resolved **per attempt**: a file therefore stays loadable on a host holding no secret, an absent value is a `Ready` refusal at startup and a contained per-attempt failure after it, and the field — holding a name and never a secret — is printed as written. The variable it names joins §10.2's split check as a third site. `RunnerKind.Structural` receives the whole agent configuration, as `TrackerKind.Structural` already does and for the reason §5.7 already gives | [#117](https://github.com/srhg-ai-7cef3f93/ben/issues/117) — contract [#5283226794](https://github.com/srhg-ai-7cef3f93/ben/issues/117#issuecomment-5283226794), sign-off with four corrections [#5283383040](https://github.com/srhg-ai-7cef3f93/ben/issues/117#issuecomment-5283383040) |
| 2026-08-12 | §5.6, §9.6, §9.10 | A newly established claim derives an attempt floor of 2 from positive §9.7 branch-advanced evidence after its first `Prepare`; the floor means work may already exist, carries no invented prior outcome, and moves the fresh failure-budget baseline with it. A later gate-4 reclaim therefore agrees with the neighbouring human re-queue path without persisting or inferring an exact discarded counter | [#94](https://github.com/srhg-ai-7cef3f93/ben/issues/94) — [sign-off](https://github.com/srhg-ai-7cef3f93/ben/issues/94#issuecomment-5272789449) |
| 2026-08-12 | §7.1, §7.5 | `RunHandle` gains `Probe`: the group question split into an operation that observes and one that acts, because the caller needing it soonest — a finished run whose process may still be flushing its transcript — must not signal. `Done` becomes the phase edge selecting which is permissible, and authorizes neither reuse nor release; only a confirmed termination does | [#79](https://github.com/srhg-ai-7cef3f93/ben/issues/79) — sign-off in the [`Probe`/`Stop` addendum](https://github.com/srhg-ai-7cef3f93/ben/issues/79#issuecomment-5262298136), which supersedes the earlier contract comments on that issue |
| 2026-08-07 | §5.7, §5.8, §7.1, §7.3, §7.6, §8.2, §11 | Adapter contract: per-kind registration exposing a pure `Structural` plus `New`, with provider config bound at construction and readiness moved to `Ready(ctx)` on the constructed adapter (`RunSpec` carries no provider passthrough, and `BEN_` is reserved to the orchestrator exclusively — `RunSpec.Env` may carry nothing else, and no provider environment surface may define it); `config effective` calls `Structural` only, and an omitted provider field with a documented env fallback is a readiness failure rather than a load error | [#17](https://github.com/srhg-ai-7cef3f93/ben/issues/17) — sign-off [#5220402224](https://github.com/srhg-ai-7cef3f93/ben/issues/17#issuecomment-5220402224), superseding the partial grants [#5220258943](https://github.com/srhg-ai-7cef3f93/ben/issues/17#issuecomment-5220258943) and [#5220330066](https://github.com/srhg-ai-7cef3f93/ben/issues/17#issuecomment-5220330066) |
| 2026-08-07 | §8.2, §8.4, §9.2, §9.10, §12.3 | Crash-recoverable claim projection: recovery classifies from positive evidence rather than the absence of a state label; contested claims resolve from the tracker's ordered assignment log, releasing self-only and yielding when unorderable | [#15](https://github.com/srhg-ai-7cef3f93/ben/issues/15) — sign-off [#5219659335](https://github.com/srhg-ai-7cef3f93/ben/issues/15#issuecomment-5219659335), which **supersedes** the earlier [#5219437276](https://github.com/srhg-ai-7cef3f93/ben/issues/15#issuecomment-5219437276) on the contested-claim half |
| 2026-08-08 | §9.2, §9.8 | Human unpark is a legal edge: the §9.2 transition table gains the `needs-review → backoff` row that its own prose, §9.8, and two §9.10 recovery passages already require, and the re-queue restores the run budgets — continuation turns and accumulated cost reset, `max_attempts` measured from the re-queue — so a park for `budget_exceeded` or exhausted `max_turns` can make progress instead of immediately re-parking. `attempt` keeps counting (§9.10's `attempt ≥ 2` and the template's `{% if attempt %}` both read it as *work may already exist*), and §9.10 is unchanged | [#38](https://github.com/srhg-ai-7cef3f93/ben/issues/38) — sign-off [#5227344073](https://github.com/srhg-ai-7cef3f93/ben/issues/38#issuecomment-5227344073) |
| 2026-08-11 | §10.2 | The credential split enforced: §10.2 states the minimum scope of each of the two credentials — tracker gets issues write and repository read, publish gets contents and pull-request write, neither gets the other — makes sharing one credential an operator obligation that is explicitly *not* machine-verifiable, and makes a workflow in which the tracker credential's **variable** can reach an agent process a **load refusal**. The comparison is over the *variables* a file references rather than the secrets they resolve to, over the whole `agent.provider` block rather than an adapter's nominated credential keys, and over the variable a value came *from* rather than the name the child receives it under; §7.6's allowlist counts as reaching the child, provenance records every variable an interpolated value names, and an adapter MUST declare the fallback it reads when a provider field is omitted | [#47](https://github.com/srhg-ai-7cef3f93/ben/issues/47) — sign-off in review of [#85](https://github.com/srhg-ai-7cef3f93/ben/pull/85) |
| 2026-08-10 | §5.6 | The untrusted span: `issue.title` and `issue.body` render fenced, in delimiters carrying a nonce derived from the value so the content cannot close its own fence, and are refused anywhere but a whole `{{ … }}` emission — a restriction that extends to any value carrying fenced content (a container holding an untrusted member, a string captured from a body that emitted one), while selecting a trusted descendant from a tainted container stays permitted and is governed by its own taint. A rendered prompt over `limits.max_prompt_bytes` is refused rather than truncated | [#50](https://github.com/srhg-ai-7cef3f93/ben/issues/50) — sign-off in review of [#72](https://github.com/srhg-ai-7cef3f93/ben/pull/72), which contributed the "selecting a trusted descendant" sentence |
| 2026-08-07 | §3.1, §6.2, §9.7 | Workspace durability contract: the workspace root is a disposable cache — `Prepare` reattaches an existing origin `ben/*` branch remote-first (fast-forward when strictly behind, keep unpushed work when strictly ahead, fail closed on true divergence); the claim-time base is git-derived as the issue branch's head at this daemon's first prepare; commits never pushed before a workspace is lost are a documented degradation | [#16](https://github.com/srhg-ai-7cef3f93/ben/issues/16) — sign-off [#5220326642](https://github.com/srhg-ai-7cef3f93/ben/issues/16#issuecomment-5220326642) |
| 2026-08-07 | §8.2, §8.3, §9.2, §9.8, §9.10, §12.3, §13 | Retained `done` claim release: `done` produces a **held-claim record** that §9.8 refreshes every tick with one ETag-conditional principal-assignee list read, releasing on a `closed` event in the claim cycle or on loss of the required labels — so release is bounded by a running daemon rather than by process restart. `ClaimEvent` gains `closed`/`reopened` and the normalized issue gains an opaque `revision` change token over a named **revision projection** — state, the tracker's state-change reason, `updated_at` — since second-granularity timestamps alone cannot express a close-and-reopen inside one second, making that case discoverable in steady state wherever the projection expresses it; the two remaining restart-coupled cases — a PR closed unmerged, and a reopen the projection leaves unmoved — are named in §9.2 and released by §9.10 gate 1 | [#27](https://github.com/srhg-ai-7cef3f93/ben/issues/27) — sign-off [#5221231708](https://github.com/srhg-ai-7cef3f93/ben/issues/27#issuecomment-5221231708) |
| 2026-08-11 | §2.2, §6.7, §10.1, §13 | Threat model stated: **untrusted issue authors, trusted labelers** — applying a `required_label` is the sole human approval act, author-controlled text is data never instruction, and absent the §10.1 boundary a subverted run's ceiling is the daemon's OS account. The agent and the daemon are **distinct principals**: at a shared process identity BEN cannot guarantee its tracker credential is unreachable, and a run that obtains it can perform the approval act. §10.1 therefore names two modes for unattended operation. **Protected mode** requires (1) a dedicated non-root account, (2) the outcome-based property that the run cannot read or use the daemon's tracker credential or inspect the daemon process — a property, not a mechanism, with verification owed by the deployment, and not established by per-command sandboxing on its own — and (3) that the forge enforce review, since the credential that pushes a branch ordinarily suffices to merge it, which made §3.4's PR gate a convention rather than a control. **Risk-accepted mode** relaxes (2) and only (2), as an explicit recorded choice in which the agent is trusted with tracker authority and the label is routing; (3) becomes load-bearing there. Egress restriction and unreachability of the *harness* credential were considered and deliberately not required — see the §10.1 design note | [#51](https://github.com/srhg-ai-7cef3f93/ben/issues/51) — decision [#5248997365](https://github.com/srhg-ai-7cef3f93/ben/issues/51#issuecomment-5248997365), sign-off [#5256542447](https://github.com/srhg-ai-7cef3f93/ben/issues/51#issuecomment-5256542447) amending the issue's own acceptance criteria, and three review rounds on [#80](https://github.com/srhg-ai-7cef3f93/ben/pull/80), which established the distinct-principals finding, replaced a RECOMMENDED substrate with the outcome-based MUST, found the PR-review gate unenforced, and bound the approval to the commit that merges |
| 2026-08-11 | §6.4, §9.10, §12.3 | Reattachment requires positive quiet evidence. A per-workspace **run marker** is written before launch and removed only on confirmed absence, and no recovery verdict may reuse, dispose, or release a workspace whose run is not confirmed gone — verdicts touching the tracker alone are unaffected. A possibly-live workspace is retained and re-probed rather than re-dispatched; a marker whose **launch outcome is unknown** — a crash before the launch, a crash after launch and before the upgrade, or an interrupted cleanup of a failed launch, which are indistinguishable — parks. §12.3-1's convergence becomes conditional on the previous run ending, and a run that never ends stays classified as waiting rather than being resolved by a timeout | [#8](https://github.com/srhg-ai-7cef3f93/ben/issues/8) — decision [#5259009734](https://github.com/srhg-ai-7cef3f93/ben/issues/8#issuecomment-5259009734), wording [#5262062882](https://github.com/srhg-ai-7cef3f93/ben/issues/8#issuecomment-5262062882), sign-off [#5262163984](https://github.com/srhg-ai-7cef3f93/ben/issues/8#issuecomment-5262163984) |
| 2026-08-12 | §9.8, §11 | Shutdown semantics stated: a graceful shutdown **initiates no new release and no new terminal projection**. It stops dispatch, interrupts every in-flight run, waits for confirmed termination **wherever a handle exists**, and completes only the effects already ordered; what has durably landed stays standing for §9.10. §11's earlier "release what's confirmed stopped" is unreachable, since decision 8 excludes any `ben:*`-labelled issue from dispatch — so releasing the claim while the label stands strands the issue, and clearing the label too discards the recovery and attempt continuity §9.10 classifies from, up to and including commits never pushed | [#9](https://github.com/srhg-ai-7cef3f93/ben/issues/9) — sign-off in review of [#95](https://github.com/srhg-ai-7cef3f93/ben/pull/95), with the rationale corrected in [#5270206524](https://github.com/srhg-ai-7cef3f93/ben/issues/9#issuecomment-5270206524): the harm is continuity, not concurrent live agents |

BEN (Branch, Execute, Notify) is a single-binary Go daemon that works on GitHub Issues
autonomously: it claims labeled
issues, runs a coding agent against each one in an isolated git worktree, verifies the agent's
work from git facts, and hands the result to humans as a pull request. This document is the
complete, self-contained contract for building it.

**Lineage (non-normative).** This is an inspired redesign of
[OpenAI's Symphony](https://github.com/openai/symphony), informed by a survey of
[contrabass](https://github.com/junhoyeo/contrabass) (an existing Go reimplementation whose
production failure modes are adopted here as normative requirements), Warp Oz, FleetQ, and
PilotDeck. It is **not** a conforming implementation of OpenAI's spec; every component was
re-decided. No familiarity with any of those systems is needed to build from this document.

## 0. Normative language

`MUST`, `MUST NOT`, `REQUIRED`, `SHOULD`, `SHOULD NOT`, `RECOMMENDED`, `MAY`, and `OPTIONAL`
are per RFC 2119. Non-normative rationale appears as *Design note* paragraphs.

## 1. What BEN is

One daemon serves one **workflow**: a repo-owned `WORKFLOW.md` file that pairs runtime config
(YAML front matter) with a per-issue prompt template (markdown body). The daemon polls GitHub
Issues for open issues carrying the workflow's required labels, and for each dispatchable
issue:

1. **Claims** it (assigns a machine user, verified by read-back) so no other daemon or human
   duplicates the work.
2. **Prepares** an isolated workspace: a git worktree on a per-issue branch, cut from a
   daemon-owned bare clone.
3. **Runs** a coding agent (Claude Code or Codex CLI) headlessly inside the worktree with the
   rendered prompt.
4. **Verifies** the outcome from git facts — commits on the branch, branch on origin, an open
   PR — never from the agent's own claims.
5. **Publishes nothing itself.** The agent pushes the branch and opens the PR; the human gate
   is PR review. The issue closes when the PR merges (via `Fixes #N`), and no PR reaches that
   without a human's approving review — though the merge itself may be performed by the agent
   (§10.1).

State is projected onto GitHub as `ben:*` labels and structured comments — GitHub is the
team's dashboard. Locally the daemon keeps only a rebuildable cache: kill it at any moment and
a fresh start reconstructs its world from the tracker and git.

## 2. Goals and non-goals

### 2.1 Goals

- Turn issue execution into a repeatable daemon workflow with bounded concurrency.
- Isolate each issue's work in a disposable git worktree; contain the blast radius of edits.
- Keep workflow policy in-repo (`WORKFLOW.md`), versioned with the code it operates on.
- Support exactly two agent harnesses — **Claude Code** and **Codex CLI** — behind one
  interface, without either leaking into the core.
- Make GitHub the shared status surface; make the daemon restart-safe with no database.
- Serve a team from one shared daemon; support per-developer daemons as an on-ramp.

### 2.2 Non-goals

- **Not a security sandbox.** A worktree contains *edits*; the agent can read the filesystem
  and reach the network. Process-level sandboxing is the harness's job, configured through
  the adapter-owned provider block (§5.2.5). The spec does not require adapters to enforce
  one — it requires the *deployment* to bound what an unsandboxed run can reach (§6.7, §10.1).
- No web UI, HTTP listener, or multi-tenant control plane in v1 (deferred, §13).
- No DAG workflows, agent crews, per-tool-call interception, agent memory or model-routing
  subsystems, proactive task discovery, or billing ledgers.
- No harnesses beyond Claude Code and Codex CLI; no wrapper-CLI integrations (§7.7).
- No conformance with OpenAI's Symphony spec, and no third-party conformance profile for this
  one (§12.4).

## 3. Design invariants

These cross-cutting rules bind every section that follows. Tests target them directly (§12.3).

1. **Statelessness.** The tracker and git are the source of truth; all local state is a
   rebuildable cache. A fresh daemon pointed at the same `WORKFLOW.md` MUST reconstruct its
   world from tracker claims/labels and git branches. (Documented degradations: agent
   session continuation tokens are local-only — a rebuilt daemon starts sessions fresh; and
   commits never pushed before a workspace is lost are unrecoverable; the claim-base exception
   follows, §6.2.) The claim-base record is provider-owned local
   safety evidence. A fresh process with the same workspace tree reads it; loss of that tree
   while an assignment already stands cannot reconstruct the historical pinning instant from
   the branch that now exists, so recovery parks that claim rather than manufacturing a base
   after the fact. A later, newly verified assignment may establish a new epoch from the branch
   head it actually finds. Thus loss may require human re-claiming, but can never turn earlier
   work into current success.
2. **Single mutator.** One authority loop owns every orchestrator state transition. Watchers,
   timers, and runner events feed it signals; nothing else mutates scheduling state.
3. **Coordination marks vs. work.** The orchestrator writes queue mechanics only — claims,
   state labels, structured milestone comments. The agent writes all content — substantive
   issue comments, the branch, the PR. The orchestrator MUST NOT close issues and MUST NOT
   write prose beyond structured milestones.
4. **Isolation substitutes for approval.** The agent runs permissively inside its worktree;
   the human gate is PR review. A pushed branch and an open PR are inert until reviewed.
5. **Evidence over claims.** Success is verified from git facts (branch advanced, branch on
   origin, open PR), never from agent output. Unverifiable success parks as `needs-review`.
6. **Normalized boundaries.** The core never sees a raw agent event or a raw provider payload.
   Adapters translate to closed enums and normalized models at the boundary.
7. **One `WorkflowDefinition`.** The orchestrator core is parameterized by exactly one
   `WorkflowDefinition` value; nothing in the core reads process-global config. (Multi-workflow
   or hosted multi-tenant is "loop over N definitions" — an additive change, not a rework.)

## 4. System overview

Components, and the section that specifies each:

| Component | Role | Spec |
|---|---|---|
| Workflow loader / config | Parse + validate `WORKFLOW.md`; hot-reload; secrets | §5 |
| Template engine | Strict Liquid rendering of the prompt body | §5.6 |
| Workspace provider | Worktree lifecycle, safety invariants, hooks | §6 |
| Agent runner | Harness adapters behind one interface + event enum | §7 |
| Tracker adapter | Normalized reads, closed write set, GitHub mapping | §8 |
| Orchestrator | State machine, dispatch, retry, reconciliation, recovery | §9 |
| CLI & observability | `ben run/status/config effective`, logging, state files | §10–11 |

Data flow: the tracker adapter normalizes GitHub issues → the orchestrator dispatches through
the workspace provider and agent runner → runner events drive the state machine → the
orchestrator projects state back to GitHub through the adapter's write set.

## 5. The workflow contract (`WORKFLOW.md`)

### 5.1 File shape and discovery

One repo-owned, version-controlled markdown file: YAML front matter (config) delimited by
`---` lines, followed by the prompt template body. Discovery precedence: explicit path given
to the CLI, else `WORKFLOW.md` in the current working directory. The file MUST be
self-contained: point the daemon at it and you have a workflow.

**One workflow per daemon.** N workflows = N daemons (each with its own label partition of
the queue). Cross-workflow composition needs no machinery: the tracker is the bus — an agent
spawns sibling-workflow work by filing an issue with that workflow's labels (`gh issue
create`).

**Workflow key.** Each daemon derives a stable `workflow_key`: the sanitized basename
(§6.3, invariant 3) of the directory containing `WORKFLOW.md`, a hyphen, and the first 8 hex
characters of the FNV-1a hash of the file's absolute path (e.g. `myrepo-a1b2c3d4`). It names
the data/state directories (§6.2, §10.3) and appears in daemon identity strings (§8.4).

### 5.2 Front-matter schema

Top-level keys — this set is **closed**: `version`, `tracker`, `polling`, `workspace`,
`hooks`, `agent`, `publish`, `limits`, `deployment`, `credential_sources`.

#### 5.2.1 `version` (integer, optional, default `1`)

Config-format version. A daemon that does not support the stated version MUST refuse to load
with an error naming the required newer BEN.

#### 5.2.2 `tracker` (object)

- `kind` (string, REQUIRED) — selects the tracker adapter. v1 ships exactly `github`.
- `provider` (object, default `{}`) — **adapter-owned opaque block**, exempt from core
  strict validation. Each adapter documents and validates its own keys (§8.4 for GitHub).
  `credential_source` names an entry in `credential_sources` (§5.2.10); naming it alongside a
  legacy in-block credential is a load refusal.
- `required_labels` (list of strings, default `[]`) — an issue must carry every one to be
  BEN's. Compared case-insensitively, trimmed. The GitHub adapter REJECTS an empty list
  at validation (dispatching an entire backlog is a misconfiguration, not a default).
- `active_states` / `terminal_states` (lists of provider-native state names, optional) —
  adapter defaults apply (GitHub: `open` / `closed`). Compared case-insensitively.

#### 5.2.3 `polling` (object)

- `interval_ms` (integer, default `30000`).

#### 5.2.4 `workspace` (object)

- `root` (path or `$VAR`, default `$XDG_DATA_HOME/ben`, i.e. `~/.local/share/ben`) —
  holds `<workflow_key>/base.git` and `<workflow_key>/issues/<workspace_key>/` (§6.2).
  `~` expanded; relative paths resolve against the directory containing `WORKFLOW.md`;
  normalized to absolute before use.
- `base_branch` (string, OPTIONAL) — the unqualified branch a fresh issue workspace is cut from
  and its pull request MUST target. It is preserved unchanged and untrimmed, MUST be valid UTF-8
  of 1–255 bytes, MAY contain slashes, MUST NOT begin with `-`, `refs/`, or `origin/`, and
  `refs/heads/<value>` MUST pass Git's `check-ref-format`. Omission selects the repository's
  then-current default branch when each new assignment epoch first prepares; it does not create a
  process-lifetime default. A written value whose remote branch is absent, or an omitted value
  whose advertised default branch is absent, is a readiness refusal before dispatch (§11).

#### 5.2.5 `agent` (object)

- `kind` (string, REQUIRED) — selects the runner adapter: `claude-code` or `codex-exec`.
- `provider` (object, default `{}`) — adapter-owned opaque block: auth, permission/sandbox
  mode, model, binary path. There is deliberately **no shared permission vocabulary** in the
  core; harness permission models differ in kind. Each adapter documents its keys (§7.7).

#### 5.2.6 `hooks` (object)

Four OPTIONAL multiline shell scripts plus a timeout. All run with cwd = workspace path via
`sh -lc` (or stricter), bounded by `timeout_ms` (integer, default `60000`). All four are
implemented in v1 — a hook key that appears in the schema is a hook that fires.

| Hook | Fires | Failure |
|---|---|---|
| `after_create` | Once, after worktree creation (dependency bootstrap lives here) | Aborts workspace creation |
| `before_run` | Before **each** attempt | Aborts the attempt |
| `after_run` | After each attempt, any outcome | Logged, ignored |
| `before_remove` | Before any dispose, including the startup sweep | Logged, ignored |

#### 5.2.7 `limits` (object) — the closed orchestrator knob set

| Key | Default | Meaning |
|---|---|---|
| `max_concurrent_agents` | `3` | Global cap on live agent processes (the only concurrency knob) |
| `max_turns` | `4` | Max continuation sessions per issue per clean-exit chain (§9.6) |
| `max_attempts` | `3` | Max failure-driven attempts per issue (§9.6) |
| `max_retry_backoff_ms` | `300000` | Backoff ceiling |
| `max_cost_usd` | unset (disabled) | Per-issue cumulative cost cap (§9.9) |
| `stall_timeout_ms` | `300000` | Runner-enforced: no events for this long → `failed(stalled)` |
| `attempt_timeout_ms` | `3600000` | Runner-enforced hard cap per attempt |
| `max_prompt_bytes` | `262144` | Ceiling on the rendered prompt (§5.6). MUST be positive — no value disables it |

The orchestrator alone reads `limits`; per-run values reach adapters via the `RunSpec` (§7.1).

#### 5.2.8 `publish` (object, OPTIONAL) — the publish credential

The credential the **agent** authenticates its push and its pull request with (§6.7, §10.2). A
first-class discriminated block rather than a key in `agent.provider`, because the identity BEN
publishes as is the operator's choice and a forge rule names a principal rather than a token
(§10.1); an unmarked entry in an adapter-owned block is nowhere to state one.

- `kind` (string, REQUIRED) — selects how the credential is obtained. This set is **closed**:
  `token`, and `source`.
- `source` (string, REQUIRED for `kind: source`) — names an entry in `credential_sources`
  (§5.2.10). The credential is then minted per attempt from that source rather than read from a
  variable, and `value` MUST NOT also be given: two config sites feeding one credential means one
  of them is silently doing nothing.
- `env` (string, REQUIRED) — the **child** environment variable the resolved credential is
  injected as, e.g. `GH_TOKEN`. Whatever it names is **owned exclusively by the publish
  credential** (§7.6): an `agent.provider` environment surface MUST NOT respell it, it MUST NOT
  carry the `BEN_` prefix, and it MUST NOT itself name a variable another site owns — an
  adapter-owned variable, or one on §7.6's daemon-environment allowlist.
- `value` (`$VAR` reference, REQUIRED for `kind: token`) — the variable holding the credential.
  It MUST be **exactly one `$VAR` reference and nothing else**: no literal, no surrounding text,
  no second reference. A literal would put a credential in a repo-owned file and in every
  `config effective` rendering of it (§5.8, §10.3); an interpolation would make one token the
  concatenation of several secrets, none of which is the credential.

Because the field holds a **name** and never a secret, its value resolves per attempt and not at
load (§5.5), and `config effective` prints it as written.

Omitting the whole block is permitted, and means BEN injects no publish credential: the agent
authenticates from what §7.6's allowlist already carries — `HOME`, and whatever the forge CLI
stores under it. That is the pre-existing arrangement, not a recommendation; an unattended
deployment owes §10.1's requirement 3 an identity distinct from its reviewers, and this block is
where that is said.

#### 5.2.9 `deployment` (object)

REQUIRED, with no default. This block declares facts the deployment has arranged; it changes no
run behavior.

- `mode` (string, REQUIRED) — closed set: `protected`, `risk-accepted`, `attended`.
- `accepted_because` (string) — REQUIRED and non-blank after trimming for `risk-accepted`;
  optional otherwise.

The declaration is process-lifetime configuration. A reload that changes it is invalid: the daemon
keeps the last-known-good declaration, blocks new dispatch under §5.4, and requires a restart to
adopt the change.

#### 5.2.10 `credential_sources` (map, OPTIONAL) — named credential sources

A top-level OPTIONAL `credential_sources` map declares named credential sources. Each entry states
a `kind` from the closed registered set and that kind's REQUIRED fields, and **contains no secret**.
`octo_sts` requires literal `url`, `scope`, `identity` and `oidc_token_path` fields; none admits
`$VAR` indirection. `tracker.provider.credential_source` and `publish.source` name an entry;
`publish.kind` admits `source` alongside `token`.

Every **tracker, base-fetch and publish** credential is obtained from a credential source at the
moment it is needed. A **bounded** source states a deadline and BEN MUST NOT use a token past it;
a `static` source is explicitly **unbounded** and states none. **No source failure falls through to
a different credential.** The harness's own API keys are out of scope: they stay in `agent.provider`
(§5.2.5, §7.6) as the adapter's parameters, not BEN's credentials.

Each entry's schema is **strict per kind**, and a partially configured source is a refusal — never
a downgrade to another kind (§5.7):

| kind | REQUIRED, non-blank after trimming | canonicalization |
|---|---|---|
| `octo_sts` | `url`, `scope`, `identity`, `oidc_token_path`; all four MUST be literal scalars — no `$VAR` reference or interpolation | `url` parsed; scheme MUST be `https`; scheme and host lowercased, the default HTTPS port dropped, trailing slash stripped, path preserved; userinfo, query and fragment REFUSED. The other three are trimmed and otherwise preserved |
| `static` | `value`, and it MUST be **exactly one `$VAR` reference** — not a literal, not an interpolation | the variable name |

An unknown key in a source block is a refusal, per §5.3: a typo in `oidc_token_path` that silently
left it unset is how a partial configuration degrades to a static token.

This is a rule of its own, not `tracker.provider.api_url`'s reused. That key refuses non-endpoint
*components* at validation (§8.4) and canonicalizes separately, for a different purpose; it strips
no trailing slash and constrains no scheme.

**Legacy spellings compile into implicit sources**, so a source is always present and there is
exactly one runtime treatment (§8). `publish.kind: token` and `tracker.provider.token` remain valid.
Naming both a legacy token and a `credential_source` on one block is a load refusal.

### 5.3 Strictness and versioning

- Unknown top-level keys, and unknown keys inside any non-opaque object, MUST fail validation.
  (A silent `poling:` typo is worse than a version-skew error.) The two carve-outs are
  `tracker.provider` and `agent.provider`.
- YAML front matter MUST decode to a map. An **empty prompt body is a validation error** —
  there is no fallback prompt.

### 5.4 Hot reload

Dynamic reload is REQUIRED, with these normative mitigations:

- Watch the **parent directory** of `WORKFLOW.md` (editor atomic saves via rename silently
  kill single-file watches), debounce ~200 ms.
- Valid reload: applies to future dispatch, retry scheduling, reconciliation, hooks, and
  launches. In-flight runs are never restarted by a reload.
- Invalid reload: keep last-known-good config for in-flight runs, **block new dispatches**,
  emit a loud operator-visible error until fixed. Never crash; never kill in-flight runs.
- Defensively revalidate before each dispatch cycle (missed-watch-event backstop, §9.4).

Reload compares **pure, name-free bindings**, not raw configuration sections. An adapter's binding
carries the **kind and canonical binding key** of every credential source it uses — the complete
definition, excluding the source's name — so a rename with an identical definition is not a rebuild
and an edit beneath an unchanged name is. A binding key contains no secret and changes with every
behaviour-affecting field, **including the value of a credential the loader resolved to a literal**;
for such a literal the key carries the full SHA-256 of the resolved value, never a prefix, because a
truncated digest lets a collision suppress a required rebuild. For `octo_sts`, configured scope is
part of both authority and binding key; OIDC token path is part of the binding key only.
`limits.attempt_timeout_ms` is part of the agent binding, because the publisher's readiness gate is
computed against it (§7.7).

`tracker.kind` and `agent.kind` keep comparisons of their own beside the bindings: the registry
resolves a kind *to* an adapter, so a kind selects which adapter is built rather than what it is
built from, and a binding that carried it would conflate the two.

### 5.5 Secrets

Explicit `$VAR_NAME` indirection only: a config value opts into environment resolution by
containing it. Empty resolution = missing secret = validation error. Environment variables
never globally override YAML. Adapters MAY document fallback env names for omitted provider
fields (e.g. the GitHub adapter honoring `GITHUB_TOKEN`); such fallbacks are adapter-local.
Repo config references names; the host supplies values.

**Not every credential site references a variable.** A `static` source (§5.2.10) and
`publish.value` reference a variable, resolved per fetch. `tracker.provider.token` is resolved by
the loader and thereafter held as a literal, though its **provenance still names the variable** and
the split check (§10.2) reads it from there. A workload-identity source references no variable at
all: it names an issuer URL, a policy scope, an identity and a token path. All four are non-secret
literals and are printed in full (§5.8).

**One deferred resolution, and its reason.** `publish.value` (§5.2.8) is a variable *name* at load
and a value only per attempt. Resolving it at load would make a workflow file unloadable on every
machine that does not hold the publish secret — including CI, which is what load-validates the
repo's own `WORKFLOW.md` (§5.8) — so the file is refused for naming no variable, never for the host
not holding one. An unresolvable publish variable is a **readiness** refusal at startup (§5.7's
`Ready`) and a credential-source failure after that: the `static` source classifies the missing
value permanent, so §9.8 parks the dispatched attempt without a §7.3 reason or spending the
remaining automatic retry budget. This is the same load/readiness split §5.8 already draws for an
omitted provider field with a documented environment fallback; §9.8 governs the runtime route.

### 5.6 Prompt template contract

Engine: **Liquid**. Strictness is normative regardless of engine: **unknown variables and
unknown filters MUST fail rendering**. Stock Liquid is lax, so the implementation MUST enforce
strictness itself — primary enforcement is a **load-time AST walk** checking every referenced
variable against the closed set below; a render-time strict check is the backstop.

The closed variable set:

| Variable | Contents |
|---|---|
| `issue` | The full normalized issue object (§8.3): `identifier`, `title`, `body`, `labels`, `state`, `assignees`, `blockers`, `url`, `created_at`, `updated_at` |
| `attempt` | Integer attempt number; `null`/absent only when the numbered attempt is 1. A newly established claim can begin at 2 under §9.6 when git evidence says work may already exist |
| `workspace` | Absolute workspace path (string) |
| `target_branch` | Trusted string: the target branch retained with this claim epoch's verification base (§6.2) |
| `run` | `run.id` (unique per attempt), `run.previous_outcome` (`null` when this record has no previous run outcome, including an evidence-derived attempt floor; otherwise `"succeeded"` after a clean-exit continuation or the failure reason string from §7.3), and `run.previous_attempt` (the prior attempt's account — see below) |

The daemon is always headless; there is no shared behavioral posture knob.
`deployment.mode` (§5.2.9, §10.1) declares external deployment facts and does not change how a run
behaves. The prompt body is the behavioral posture surface — behavioral instructions (including
"no human is present") belong in the template.

**`run.previous_attempt` — the prior attempt's account.** A single preformatted string holding
what BEN retained about the attempt before this one: its `run.previous_outcome`, the commits it
left on the branch past the claim-time base, the files those commits changed, and a tail of the
agent's own prose. It exists because a retry that knows nothing about its predecessor's failure
is how three attempts fail identically and bill three budgets (§9.6).

It MUST be **derived only from facts BEN already retains** — no model call, so the same attempt
composes the same bytes — and MUST be composed when that attempt's outcome is **routed**, not at
render time, which would read a workspace the next attempt may already be preparing.

It is **absent** — and absent MUST bind the empty string rather than `null` (see below) — on:
attempt 1; an evidence-derived attempt floor; a fresh host after a restart, which §9.10 step 6's
degradation language already covers; and the §9.6 **continuation track**, which carries the
resume token, so the session already holds its own history and duplicating it would spend the
context window the resume exists to save. A human re-queue restores budgets, not memory: the
account of the parked attempt stands.

It MUST be **bounded**, with every truncation **stated in the rendered text** rather than left
silent — a summary that quietly ends mid-list reads to an agent exactly like a complete one — and
every cut MUST land on a UTF-8 boundary, since a split rune inside a fence is a fence that may
not survive rendering. The bound is an implementation constant well under
`limits.max_prompt_bytes`, and it is **not** a guarantee against that ceiling: the addition can
still push a large prompt over it, and the refusal below then applies as it otherwise would. A
branch that could not be read MUST be reported as unread rather than as empty — "committed
nothing" about a branch nobody looked at is a fabrication the agent would act on — and no failure
in composing the account may fail the attempt.

**The read that produces it MUST be bounded in memory and in how long it will wait.** Both halves
are load-bearing, and both are about the same thing: this read serves a prompt, so a repository an
agent controls must not be able to make it expensive. A commit subject and a filename are
agent-authored and unbounded in length, and there is no limit on how many of either a branch may
carry, so a provider that buffered the whole of what git offers before capping it could be made to
exhaust the daemon. And the locks a git read takes are held across fetches, so a provider that
waited indefinitely for one would stall a *finished* attempt for as long as an unrelated fetch
lasts — holding a §9.5 concurrency slot and blocking the §11 drain. Giving up is a legitimate
result and reports an unread branch.

**The untrusted span.** `issue.title` and `issue.body` are written by whoever filed the issue
and MAY be edited after the queue label is applied. `run.previous_attempt` joins them, and is
worse: it is agent output produced by an agent that had **already read the fenced issue body**,
so anything an attacker puts in that body the agent can be induced to restate — and carrying it
into the next prompt outside a fence would launder it in one hop. **Every string in it is
agent-authored, whatever proves it exists**: git is authoritative about a commit existing and a
path being in a tree, never about the words the agent chose for either, so `git commit -m
"<injected>"` and a filename spelled the same way are fenced exactly as the prose tail is. That
is why the variable is one string fenced **whole** rather than an object with a field per fact,
whose safety would rest on one field still being present.

All three MUST render **fenced**: wrapped in delimiters that name the variable and carry a nonce
derived from the value, so the content cannot close its own fence. The delimiter MUST also name
**who wrote** the content it holds — telling an agent that the issue reporter wrote its own
previous output is worse guidance than none. Every other member of the closed set is the
tracker's own answer, or requires the triage rights that granting the queue label already
implies.

An absent untrusted value MUST bind the **empty string**, not `null`, and `run.previous_attempt`
is the only member this applies to. The two nullable members above bind `null` so that
`{% if %}` sees a falsy value; an untrusted variable may not appear in a condition at all (below),
so no prompt can guard this one, and a `null` emitted unguarded fails the strict check — `null`
would therefore make the variable unrenderable on exactly the attempts where it is absent. An
absent value MUST NOT render an empty fence.

Because a filter could truncate the closing delimiter away and a property access would measure
the fence rather than the content, an untrusted variable MUST be refused anywhere but a whole
`{{ … }}` emission. That restriction MUST extend to any value **carrying** fenced content: an
object or array containing an untrusted member, and a string captured from a body that emitted
one. Selecting a trusted descendant from a tainted container is permitted; the selected value's
own taint governs its use.

Fencing is behavioural guidance, not a security control. It does not substitute for credential
separation (§10.2) or content-bound approval (§9.5).

**Prompt size.** A rendered prompt over `limits.max_prompt_bytes` MUST be refused, not
truncated: truncation would cut the closing delimiter off the untrusted span, and an issue body
is otherwise attacker-controlled token spend billed against `limits.max_cost_usd`. The refusal
is a contained per-attempt failure (§5.7), not a load error.

**Canonical publish snippet.** Because the agent owns publishing (§6.7), every workflow
prompt MUST instruct it. The RECOMMENDED canonical block:

```liquid
## Publishing

When — and only when — the task is complete:

1. Commit all changes. Work only on the branch already checked out in this
   workspace; never create, switch, or force-update branches.
2. Push it: `git push origin HEAD`.
3. Open a pull request against `{{ target_branch }}` with
   `gh pr create --base {{ target_branch | shellescape }}`, and put
   `Fixes #{{ issue.identifier }}` in the PR body so the issue closes on merge.
4. Do not merge the pull request. Do not close the issue.

{% if attempt %}
This is attempt {{ attempt }}.
{% if run.previous_outcome == "succeeded" %}Your previous session ended cleanly
but without a published pull request — inspect the workspace, finish the
remaining work, and publish.{% elsif run.previous_outcome %}Your previous session failed
({{ run.previous_outcome }}) — inspect the workspace, recover, and continue.
{% else %}This branch already carries work, but the previous run outcome did
not survive the claim boundary — inspect the workspace and continue.
{% endif %}
{% endif %}
{{ run.previous_attempt }}
```

`run.previous_attempt` is emitted **unguarded**, and outside the `{% if attempt %}` block that
brackets everything else about a retry. That is not an oversight: it cannot be guarded — a
condition on an untrusted variable is refused — and it does not need to be, because an absent
account binds the empty string and emits nothing.

### 5.7 Validation and failure semantics

Strict at load, contained at run. Load/reload rejects: YAML errors, unknown keys, invalid
values, template parse failures, template variables outside the closed set, empty prompt
body. Startup with invalid config MUST refuse to start. Residual render-time failure fails
only that run attempt.

**Structural validation is separate from readiness, for every adapter family.** Each adapter
*kind* is registered at package level and exposes two entry points before any instance
exists (§7.1, §8.2):

- **`Structural(config)`** — is this configuration well-formed? **Pure**: it MUST NOT touch
  the network, the filesystem, or a subprocess, and MUST be answerable with no credentials
  present and no harness installed. It receives the whole adapter config — the opaque
  provider block *and* the core-owned fields alongside it — because a rule like "required
  labels must be non-empty" spans both, and validating a new provider block against a
  previous reload's core fields would be a silent hot-reload bug (§5.4).
- **`New(config)`** — construct, **binding that config to the instance**. Structural failures
  surface here too; nothing else does.

Readiness is **`Ready(ctx)`** on the constructed adapter: credentials resolving, a tracker
answering, a harness binary present and identifying itself. It owns everything that can fail
because the world is not set up rather than because the config is wrong. It reports on the
**bound** config — an adapter whose readiness could be checked against one configuration and
then operate under another would make the check meaningless, so a reload constructs a fresh
adapter and re-checks before it is used (§5.4, §7.1).

*Design note.* Validation cannot be a method on the constructed adapter, which is the shape
that fails: a malformed config fails during construction, leaving no instance to ask. Making
the structural check a property of the *kind* is what lets `ben config effective` report a
bad block for an adapter it could never have built.

**Credential source kinds are a closed registered set**, on the same terms. A kind reduces a source
block to a descriptor **purely** — no network, no instance, no filesystem — so a workload-identity
configuration load-validates on a host holding no credential. A partially configured source is a
refusal, never a downgrade to another kind. Purity is what the descriptor's other consumer needs
too: reload compares descriptors (§5.4), and a comparison that had to reach the network could not
run on every debounce.

### 5.8 Introspection

`ben config effective [--json]` is REQUIRED: the fully-resolved configuration (defaults
applied, `$VAR`s resolved) with per-field provenance (default / file / environment) and
**mandatory secret redaction**. Adapter `Structural` results (§5.7, §7.1, §8.2) surface here.

**It calls `Structural` only, never `New` or `Ready`.** Inspecting a configuration MUST NOT
require credentials, network access, or an installed harness: the command has to work on a
laptop without secrets and in CI, where it is what load-validates the repo's own
`WORKFLOW.md` against the real adapters. `ben run` is the caller that additionally invokes
`New` and `Ready`, and refuses to start on either (§11).

`credential_sources` (§5.2.10) is rendered **in full**: it holds no secret. A `static` source's
`value` is a variable reference and is printed as written.

`workspace.base_branch` renders unchanged with file provenance when written. Omission renders
`<repository-default>` in text and `null` in JSON, with default provenance; introspection does not
contact the repository to replace that selector with a branch name.

**Credential policy.** An explicit `$VAR` that resolves empty is rejected by the **loader**,
per §5.5, before any adapter is consulted — the workflow named a secret and the host did not
supply it, which is a fact about the file and needs no adapter to see it. An **omitted**
provider field that an adapter documents an environment fallback for is a different failure:
structurally valid, unresolved until `Ready`. Neither is an adapter `Structural` concern; the
first never reaches it and the second passes it.

`publish.value` (§5.2.8) is a third case and behaves like the second: the loader validates that it
names a variable and deliberately does not read it (§5.5), so a host that does not hold the value
fails at `Ready` rather than at load. It is printed as the reference it is — a variable name is not
a secret, and hiding it would conceal exactly what an operator needs to read when the publish
credential is misconfigured, which is the call §10.2 already makes for `env_passthrough`.

## 6. Workspace model

### 6.1 Provider interface

Workspace strategies sit behind a pluggable interface so container/remote strategies (hosted
tier, §13) slot in without touching the orchestrator:

```
WorkspaceProvider:
  IsApplicable(workflow) bool          // priority-ordered strategy selection
  Prepare(issue, attempt) → Workspace  // create or reattach under an already established claim base. The
                                       // Workspace reports every path the provider
                                       // owns — the worktree, the shared git dir,
                                       // and a private directory outside the
                                       // worktree — since the provider chooses the
                                       // layout (§6.2) and is the only party that
                                       // knows them without inspecting the tree
  Dispose(workspace, keep bool)        // keep=true preserves for forensics
```

The three-method `WorkspaceProvider` strategy interface remains closed. The orchestrator's
narrower workspace consumer additionally supplies the expected claim epoch on every prepare,
can durably begin an epoch before label projection, and can read the epoch/base pair without
preparing during recovery. Those are consumer-specific safety/evidence operations, for the same
reason `PublishFacts` and the pre-hook #94 observation do not widen the strategy interface.
`Workspace` carries the positive claim epoch beside `BaseSHA` and `TargetBranch`; once pinned, the
triple is inseparable input to prompt rendering and verification. A zero epoch, empty base, or
empty target authorizes no hook, launch, reuse or success verdict.

v1 ships exactly **one** strategy: git worktree + per-issue branch. (GitHub-first means the
target is always a git repo; snapshot-copy has no v1 user.)

### 6.2 Worktree strategy

- **Base repo.** The daemon owns its source: a **bare clone** (`base.git`) created on first
  run and fetched before each attempt. The branch retained for the claim is fetched, and fresh
  worktrees are cut from it. Nothing lives inside any
  repo working tree; there is no human-managed checkout to inherit state from. Any unexpected
  base-clone state is **fail-closed**: loud error, no auto-repair. Base-clone fetch
  authenticates with the tracker credential (§10.2). A repository carries a credential
  **source** selected by assembly (§11), not a resolved credential. The source owns its complete
  exchange binding, including any scope; the repository carries and derives no credential scope.
  The daemon resolves the credential **immediately before each remote git invocation**, and
  redaction covers the credential *that invocation used* — a value captured once at construction
  scrubs a stale token while the live one flows through git's stderr into error text and logs.
- **Layout.** `<workspace.root>/<workflow_key>/base.git` plus
  `<workspace.root>/<workflow_key>/issues/<workspace_key>/`. The XDG-data default survives
  reboots and tmp-sweeps, so kept-on-failure forensics persist. A third sibling,
  `<workspace.root>/<workflow_key>/private/<workspace_key>/`, holds per-workspace state a
  harness needs to write but the *repository* must not carry — an agent harness's own config
  and scratch directories. "Private" means **placed and disposed by BEN, outside the worktree**,
  not unwritable: the harness writes it freely. Keeping it out of the worktree is what stops
  that state from reaching a commit, and pointing the harness there instead of at its
  operator-scoped config directory under `$HOME` stops a workspace from using or mutating that
  directory.
- **Branch.** Per-issue branch `ben/<workspace_key>`, created at first prepare and
  **reattached on every retry — never `-B`** (force-recreating the branch has discarded agent
  commits in production; treat this as load-bearing). Reattach is **remote-first** (#16):
  every prepare probes origin for `ben/<workspace_key>`, fetching it into a non-branch ref
  namespace (fetch cannot move a checked-out branch). Branch on origin but not local → the
  local branch is created at the remote head, never derived from the selected base branch (a
  daemon that has never seen the issue attaches the existing work — the #11 handoff). Both
  exist → a local branch strictly behind fast-forwards; strictly ahead (unpushed work)
  attaches as-is; true divergence **fails closed** — fast-forwarding either way would
  discard someone's commits.
- **Durability.** The workspace root is a **disposable cache** (§3.1). Branch identity and
  pushed commits reconstruct from origin's `ben/*` branch and the claim from the tracker.
  Bootstrap completion (§6.5's `after_create`) is deliberately per-host: a fresh host re-runs
  it. Commits never pushed before the root is lost are unrecoverable (§3.1).

  **Claim base.** The workspace provider owns one durable claim-base record per workspace key.
  Its authorizing state is `pinned(epoch, base_sha, target_branch)`; the only transitional state is
  `pending(epoch, outgoing_pin?)`. The outgoing pin includes its target when it was recorded by
  this version and remains a reachable git fact
  until the transition finishes. Creating the pending intent is durable and idempotent for one
  epoch. Repeating it for an already pinned matching epoch is a no-op; a conflicting pending
  intent is not overwritten.

  After remote-first reattachment has settled the local issue branch, and before `after_create`,
  `before_run`, a run marker or an agent can mutate it, the provider observes the current branch
  head against the outgoing pin for §9.6 and atomically replaces the pending record with
  `pinned(epoch, current_head, selected_target)`. Recovery can therefore observe the old pinned record, the new
  pending intent retaining that old pin, or the new pinned record — never a torn epoch/base/target
  tuple. The current head is the fetched selected-target head for a genuinely fresh issue and the reattached issue
  head otherwise. Retries, continuations, human unparks and restarts in the same epoch read the
  existing base and target and MUST NOT remint either, even after configuration or repository-default
  movement. A later claim-establishing assignment ID creates a new pending epoch and, at its first
  prepare, a new base and target.

  Branch identity and pushed commits still reconstruct from origin. The claim-base record does not:
  its missing, unreadable or contradictory shape is a positive failure to establish the safety
  precondition and is handled by §9.10, never inferred from a worktree directory or from the
  current branch head.

### 6.3 Safety invariants

1. The agent subprocess cwd MUST equal the workspace path.
2. **Every path the `Workspace` reports** — the workspace path, the shared git dir, and the
   private dir — MUST stay under the workspace root (normalize both to absolute;
   **symlink-normalize before prefix checks** — macOS `/private` aliasing breaks naive
   prefix comparison). The private dir MUST additionally lie outside the workspace path: it
   exists to hold what the worktree must not, and one inside it is the same directory with a
   longer name. Checking only the worktree would leave the two paths a consumer binds into a
   sandbox posture (§10.1) unchecked, which is exactly backwards.
3. `workspace_key` is the issue `identifier` sanitized to `[A-Za-z0-9._-]`; if sanitization
   changed it, append a stable hash suffix of the original identifier with ≥64 bits of
   entropy (collision resistance).

### 6.4 Lifecycle: reuse, dispose, startup sweep

One workspace per issue, persistent across attempts and continuation sessions. On **verified,
published success** the worktree is disposed — the pushed branch is the archive. **Failures
keep the workspace** (`Dispose(keep=true)`) for debugging; disk is bounded by failure count,
not throughput. The private dir shares the worktree's lifetime exactly — created with it, kept
when it is kept, removed when it is removed — because it holds mutable harness state associated
with the attempts that produced the tree, including continuation state §7.1 may require;
retaining it preserves forensic context, not trusted evidence of the original configuration.
Attempt-scoped children within it (a per-attempt temp dir) may be removed independently. The
shared git dir outlives both: it is per-workflow, not per-workspace. At startup, sweep
workspaces of terminal-state issues (tracker fetch failure
at startup = warn and continue). Per-issue locks MUST guarantee one issue never has two live
workspaces. Per-issue locks do not survive the process; across restarts, §9.10's run marker
carries the same guarantee.

### 6.5 Hooks

Firing points and failure semantics as §5.2.6. All hooks run with cwd = workspace path under
`hooks.timeout_ms`.

### 6.6 Worktree failure taxonomy (normative)

Adopted from contrabass's production fix history; these are requirements, not advice:

- Verify worktree registration via `git worktree list --porcelain`; on mismatch, prune stale
  registrations and retry **once**.
- Crashed-run debris blocking `git worktree add` → prune-and-retry, never
  manual-delete-and-hope.
- Ambiguous git errors **fail closed** — no plain-directory fallback guessing.
- Symlink-normalize paths before any prefix check (§6.3).
- Per-issue locks serialize all workspace operations for an issue.

### 6.7 Trust model and publishing

**Isolation substitutes for approval** (§3.4). The agent runs permissively inside the
worktree; the spec's non-goals (§2.2) state plainly that this is not a security sandbox.

**Threat model (normative).** BEN assumes **untrusted issue authors and trusted labelers**.
Anyone who can file may put arbitrary text in front of the agent; only a principal the tracker
authorizes to label may dispatch it. Applying a `required_label` (§5.2.2) is the human approval
act, and it is the only one — nothing gates the run between the label and the agent. A
deployment that lets untrusted principals label MUST NOT run unattended.

Three consequences bind the rest of this document:

- Author-controlled text is **data, never instruction**. `issue.title` and `issue.body` render
  fenced (§5.6), and the content a labeler approved MUST be bound to the dispatch that runs it
  (§9.5) — otherwise "trusted labeler" describes an approval of content the agent never saw.
- **Absent the §10.1 boundary**, a subverted run reaches everything the daemon's OS account
  reaches: its filesystem and `$HOME`, including any harness credential stored there (§7.6);
  its network; and every credential it holds (§10.2). The worktree bounds edits to the
  repository and nothing else. §10.1 states what an unattended deployment MUST do about it.
- **The agent and the daemon are distinct principals, and equal permissions do not make them
  one.** At a shared process identity BEN **cannot guarantee** the daemon's tracker credential
  is unreachable from the run it launched — whether one process may inspect another's memory or
  environment is an OS and policy question this spec does not settle. A run that obtains that
  credential can perform the approval act itself: label, claim, and clear `ben:*`. The two-
  credential split (§10.2) therefore bounds *accidental* forwarding (§7.6), not a determined
  run, and the gate it protects is the dispatch label, not PR review (§3.4) — which is itself a
  control only where the forge enforces it (§10.1). §10.1 states the two modes an unattended
  deployment MUST choose between, and which of these properties each one requires.

**The agent publishes everything**: it pushes the issue branch and opens the PR via its
provider-native tooling (e.g. `gh`), carrying its own credentials (§7.6, §10.2). The
orchestrator holds no git write credential **and no BEN component authenticates a git write**.
Naming the publish credential in the workflow file (§5.2.8) does not change that: the daemon
resolves the variable from its own environment and hands the value to the adapter to inject, which
is what it already did for an `env_passthrough` entry. It holds the credential in the sense any
process holds its environment, and uses it for nothing. What the section adds is not a capability
but a place to state *which identity* the agent pushes as (§10.1, §10.2).
A pushed branch and an open PR are inert until
reviewed — pre-verification work reaching a branch is the gate working, not a leak — for as
long as the forge enforces that, which §10.1 makes a requirement rather than an assumption.

## 7. Agent runner

### 7.1 Interface

```
RunnerKind (one per agent.kind, registered at package level):
  Structural(agentConfig) error       // PURE: the agent configuration well-formed — the opaque
                                      // provider block and the core-owned fields beside it
                                      // (§5.2.8's publish credential), because the reservation
                                      // between the two spans both (§5.7). No filesystem, no
                                      // subprocess, no network. Surfaces in
                                      // `ben config effective`
  New(options) → (AgentRunner, error) // binds that configuration

AgentRunner (the constructed runner):
  Ready(ctx) error                    // readiness of the BOUND config: binary present and
                                      // identifying itself, auth plausible. Everything that
                                      // can fail because the world is not set up
  Capabilities() → {resume bool, usage bool, ...}
  Start(ctx, RunSpec) → (RunHandle, error)

RunSpec:  { workspace {path, shared git dir, private dir} (§6.1 — reported by the provider
            that owns them), prompt, continuation token?, env map (§7.6 — `BEN_`-prefixed
            keys only), limits {stall_timeout, attempt_timeout, max_turns, cost_cap} }
```

**Where the publish credential is source-derived, readiness performs one exchange**, rejects an
empty credential, applies the attempt-lifetime gate to the deadline, and **discards the token**.
Readiness failure refuses startup; the same gate failing at launch fails only that attempt. The
runner's options carry the publish *binding* — the child variable and a fresh-only source — and the
attempt timeout the gate is arithmetic over; they carry no credential scope, which belongs to the
source instance (§11).

The three gates, `<=` at load matching `>=` at runtime:

```
load:    attempt_timeout_ms + 5m <= descriptor(publish source).MinFreshTTL
Ready:   probeToken.UsableUntil - now >= attempt_timeout + 5m
Start:   token.UsableUntil - now      >= attempt_timeout + 5m
```

All three are **skipped when the source is explicitly unbounded** (`MinFreshTTL` zero), which is
what keeps `static` and every legacy spelling valid. The margin is a fixed five-minute constant and
not an operator key: it absorbs issuer clock skew plus the publish step's own duration, and a
workflow author is positioned to tune neither. `octo_sts` declares 50 minutes — GitHub documents a
one-hour expiry, which is not a guarantee of one hour *remaining* after an exchange — so the attempt
maximum under it is 45 minutes. `attempt_timeout_ms` keeps its one-hour default: a bounded source
must set a valid timeout explicitly, and the refusal shows the arithmetic.

**The provider config binds at `New`, and `RunSpec` does not carry it.** A runner that took
its provider block per-run could pass `Ready` against one configuration and `Start` against
another — and a reload changing the block would leave a runner whose readiness was
established against the previous one. Binding at construction removes the divergence rather
than documenting it: `Ready` checks exactly what `Start` will use. The `RunSpec` env namespace
(§7.6) is what keeps it closed.

**`RunSpec` carries per-attempt invocation inputs, never adapter configuration.** That is the
line the rule above draws, and it is narrower than "facts": `prompt` is rendered from a
template and `limits` come from the workflow file, so most of a `RunSpec` is
configuration-*derived*. What binds at `New` is the **adapter's own** configuration — the
provider block, which `Ready` verifies — and what a `RunSpec` may add is what this invocation
needs and could not have existed at construction. The workspace paths are that: which
workspace, not what the adapter does with it.

They arrive from the provider (§6.1) rather than being derived by the adapter, for two
different reasons that should not be collapsed. The **shared git dir** is a trust question: it
is discoverable from inside the worktree — `git rev-parse --git-common-dir` reads it out of
`<workspace>/.git` — and the worktree is the agent's own writable tree, so an adapter that
discovered it could be pointed at a repository the agent selected, and §6.2 reattaches, so a
rewritten pointer survives into the next attempt. The **private dir** is an ownership
question: §6.2's layout is the provider's, and an adapter reconstructing it by string
arithmetic on the workspace path would be a second definition of it, wrong the first time the
provider's layout changes. So an adapter MUST NOT derive either — not from the workspace path,
not from the workspace's contents — and an adapter that needs neither ignores both.

A reload therefore **constructs a fresh adapter and re-checks readiness before it is used**;
an invalid one blocks new dispatch under §5.4's existing rule. In-flight runs keep the
instance they started with, since §5.4 already forbids a reload from restarting them.

```
RunHandle: { Events <-chan Event, Done, Probe, Stop(mode) }
```

`Probe` and `Stop` answer the same substrate-neutral question — is this run's **execution domain
positively quiet** — and are not interchangeable. The substrate owns its domain and evidence: a
local run uses §7.5's kernel domain, while a remote run uses backend domain-quiet evidence. Neither
core nor the shared contract interprets a PID, process group, signal, cgroup, namespace, or remote
session.

`Probe` MUST only observe: one fresh read-only evaluation of the domain's quiet evidence, no
teardown, no verdict claimed, no cleanup enqueued, and no effect on stream, transcript, or
lifecycle. `Stop` performs the substrate's bounded teardown and then evaluates the same predicate.
A caller that merely needs the answer MUST use `Probe`, including while a finished direct process
may still be flushing its transcript.

`Done` is a phase edge, not permission for reuse or release: before it, `Probe` is the only
permissible question; after it, `Stop` may tear down domain members that outlived direct execution.
A clean natural exit MUST confirm quiet without prior signal delivery. Only confirmed domain quiet
authorizes workspace reuse, disposal, verification, or claim release (§9.8).

There is no separate Resume method: **continuation is an adapter-opaque token** carried in
the `RunSpec`, minted by the adapter in a prior `started` event, stored but never interpreted
by the orchestrator. (Today it maps to `claude --resume` / `codex` resume; a future
app-server adapter makes it a live handle without interface change.) An adapter without
resume declares so in `Capabilities()` and MUST fail loudly if handed a token; the
orchestrator warns at load — not mid-run — when a workflow needs what an adapter lacks.

### 7.2 Event model (closed enum)

Adapters translate raw harness streams to exactly these events at the boundary; **the
orchestrator never sees a raw agent event**:

`started(session_id, continuation_token?)` · `progress(text)` ·
`usage({input_tokens, output_tokens, cost_usd?})` · `heartbeat` · `succeeded` ·
`failed(reason)`

Any raw stdout line counts as activity (adapters synthesize `heartbeat`). The raw stream is
retained verbatim in the per-run transcript (§10.3). `usage` is best-effort normalized.

### 7.3 Failure taxonomy and retryable verdicts (closed set)

| Reason | Retryable | Notes |
|---|---|---|
| `crashed` | yes | Process exit without a terminal event |
| `stalled` | yes | No events past `stall_timeout_ms` |
| `timeout` | yes | `attempt_timeout_ms` exceeded |
| `rate_limited` | yes | Backoff applies naturally |
| `auth` | no | Credentials won't fix themselves |
| `launch_error` | no | Config-shaped causes should have been caught by `Structural` or `Ready` (§5.7); the rest are knowable only at `Start`, e.g. a `RunSpec` violating the env namespace (§7.6) |
| `killed` | no | Deliberate stop |
| `budget_exceeded` | no | Orchestrator-initiated on cost-cap breach; parks as `needs-review` (§9.9) |
| `credential` | yes | A **transient** failure to obtain a credential. An unknown or permanent credential failure is not a run failure: it parks (§9.2) |
| `output_overflow` | no | The runner's own verdict on a single stdout line past the scanner ceiling (§7.5): the reader that hit it claims it, the run is killed, and the transcript marks the cut. Not transient — the same agent on the same input reproduces it — so no attempt is spent retrying it |

Verdicts are **static** — the orchestrator applies retry policy (§9.6) without inspecting
agent internals.

### 7.4 Liveness and outcome

Liveness is **runner-owned**: the adapter enforces the stall timeout and hard attempt
timeout. The **terminal event is ground truth**: process exit without one = `failed(crashed)`;
silence past the stall window = `failed(stalled)`. Exit codes are advisory only. `succeeded`
means "the agent claims done" — real success is the orchestrator's git-fact verification
(§9.7).

### 7.5 Process discipline

**Local execution domain.** Each supported local attempt owns a fresh Linux cgroup-v2 leaf and a
trusted BEN supervisor which is PID 1 of fresh user, PID, cgroup, and mount namespaces. The trusted
child MUST first be created atomically in the leaf with `clone3(CLONE_INTO_CGROUP|CLONE_PIDFD|
CLONE_NEWUSER|CLONE_NEWPID|CLONE_NEWNS)`. `CLONE_NEWCGROUP` MUST NOT be a clone flag: after
placement and UID/GID mapping, the trusted child MUST call `unshare(CLONE_NEWCGROUP)` before
supervisor exec or any untrusted code, so the cgroup namespace roots at the attempt leaf.

The less-privileged mount namespace's inherited mounts MAY be locked and MUST NOT be assumed
individually detachable. Before untrusted exec the supervisor MUST make propagation private;
snapshot every inherited proc, cgroup, and cgroup2 alias; stack inert read-only covers over
noncanonical aliases; stack fresh private procfs and leaf-rooted read-only cgroup2 mounts at their
canonical paths; and verify by mount identity and filesystem facts that every old alias is
unreachable. It MUST close ancestor-control and namespace descriptors and place the provider in a
nested user namespace with no capability over the supervisor-owned containment namespaces. A
process group MAY exist for job control but is not termination evidence.

Untrusted code MAY create further user, cgroup, mount, and PID namespaces and MAY mount fresh
procfs/cgroup2 filesystems when host policy permits. This is not a readiness failure: a new cgroup
namespace MUST root at the caller's current attempt leaf or a descendant, and a new procfs MUST be
governed by the attempt PID namespace or a descendant. Such a cgroup2 view MAY create and organize
descendant cgroups but MUST NOT expose or permit migration to the delegated parent, daemon, or
sibling attempt. `no_new_privs` MUST NOT be treated as a namespace or mount prohibition. The
provider adds no syscall filter solely to forbid these nested mounts.

The supervisor reports direct-provider exit separately and remains PID 1 until every process in
the attempt PID namespace exits naturally. Thus `setsid`, double-fork, exec, and nested namespaces
neither escape accounting nor turn direct-process exit into quiet. A provider MUST NOT see or
migrate to the delegated parent or a sibling attempt. The daemon retains the clone pidfd and
attempt-cgroup descriptor; untrusted code receives neither.

**Quiet predicate.** Local positive quiet is a host-boot mismatch, or positive proof that the
recorded PID-namespace init exited. A still-live exact supervisor is unconfirmed even if its cgroup
is empty, removed, swept, or replaced. Matching `cgroup.events` `populated 1` is unconfirmed and
vetoes an exit observation as a containment invariant violation. After supervisor exit, quiet is
confirmed unless the matching cgroup is positively populated: Linux kills all remaining
PID-namespace members when its init exits. Malformed/reused identity, a torn or failed observation,
cancellation, and every unknown result are unconfirmed. Leader exit, direct `wait`, `Done`, pipe
EOF, elapsed time, signal history, process-group `ESRCH`, cgroup emptiness, and cgroup removal alone
are not quiet evidence.

Local `Probe` performs one read-only pidfd/process-identity and cgroup evaluation; it sends no
signal and writes, removes, or enqueues nothing. Local `Stop` observes first, sends TERM only
through the recorded supervisor pidfd, waits one grace, applies recursive `cgroup.kill`, and waits
at most one further grace for the same predicate. A failed pidfd TERM skips to `cgroup.kill`; it
never falls back to a numeric PID or process group. It reports confirmed only on the predicate.
Stream-drain and scanner bounds are unchanged.

**Resource cleanup.** The local provider MUST register every created attempt with a provider-owned
janitor which outlives its handle. After positive supervisor exit and recursive cgroup emptiness,
the janitor removes nested cgroups and the leaf bottom-up with descriptor-relative identity checks
and bounded retries. Startup MUST sweep canonical empty attempt trees left by a crash and retain
populated trees without killing them. Cleanup is not a quiet verdict, does not clear a marker, and
is never caused by `Probe`. Persistent cleanup failure MUST fail closed for future local starts
rather than permit unbounded accumulation.

**Support.** Local execution is supported only where the runtime can exercise unified cgroup-v2
delegation with `nsdelegate`, `cgroup.kill`, descriptor-relative cleanup, the ordered
clone-then-cgroup-unshare protocol, the required user/PID/cgroup/mount namespaces,
locked-mount-safe alias cover, contained provider-created namespace/mount behavior,
`openat2`/`statx`, `pidfd_open`, and `pidfd_send_signal`. The nested-mount canary MAY observe a
host-policy denial or successful leaf/descendant-rooted mounts; it MUST refuse only a broader view
or successful parent/sibling migration. `Ready` and `Start` MUST otherwise refuse when a required
primitive is absent, blocked, or fails the containment/cleanup canary. There is no process-group
fallback. Unsupported platforms remain build/test/inspection and remote-execution platforms but
MUST refuse a real local run.

### 7.6 Secrets and I/O

Secrets MUST NOT appear in argv (visible in `ps`) and MUST NOT be inherited wholesale from
the daemon's environment: the adapter injects a per-run env built from `$VAR`-resolved
provider config, a minimal allowlisted passthrough (PATH, HOME, locale), and the **publish
credential** (§5.2.8) resolved for this attempt. Harnesses that authenticate via env (e.g.
`ANTHROPIC_API_KEY`) document that as their adapter-owned surface; the publish credential is the
one child variable the **core** names, and the adapter injects it under the name `publish.env`
gives. That is not an exception to "the adapter owns the child environment" but a consequence of
§10.1: the publish identity is the operator's to choose, and a forge rule names principals, so it
has to be statable without editing an adapter's opaque block. Injection stays the adapter's — the
core supplies a variable and a value and writes nothing. The prompt passes via **stdin** where the
CLI supports it, else a temp file inside the workspace — never argv.

**The adapter owns the child environment, and `BEN_` is reserved to the orchestrator.** The
reservation binds from both sides:

- `RunSpec.Env` MUST contain **only** `BEN_`-prefixed keys. Any other key is a refusal —
  `failed(launch_error)`, non-retryable — whether or not the adapter happens to set it.
- An adapter's provider block MUST NOT define a `BEN_`-prefixed key in any environment
  surface it exposes (e.g. `env`, `env_passthrough`). This is a property of the configuration
  file, so it is a **`Structural` refusal at load** (§5.7), not a dispatch-time one.

Enforcing only the first half would leave the collision expressible from the config side,
which is the worse direction: it would be authored once and hit every run.

**One child variable, one owning config site.** The same argument generalizes past `BEN_`: every
child variable has exactly one site that may set it, which is the site where its value is
validated. So `publish.env` (§5.2.8) names a variable belonging to the publish credential
**exclusively** — an adapter's provider block MUST NOT define it in `env` or `env_passthrough` —
and, symmetrically, `publish.env` MUST NOT name an adapter-owned variable or one on the
daemon-environment allowlist below. Both directions are one defect: two sites writing one variable
means that, whichever wins, one of them is silently doing nothing, and the loser is whichever the
composition order happens to place first. Both are properties of the file, so both refuse at
**load** rather than at dispatch, and the refusal MUST name the **owning** site — "set it through
its named `agent.provider` key" is false advice for a variable the `publish` block owns.

Which stage refuses follows from who can know the rule, not from which direction it points. The
reserved sets the **core** owns — the `BEN_` prefix, and the daemon-environment allowlist below —
are checked by the loader, which needs no adapter to see them and therefore refuses in `Load`
itself. The collision with an adapter's own variables needs that adapter's table, which is opaque
to the loader (§5.2.5), so it is a `Structural` refusal (§5.7) — reached by `ben config effective`
with no credentials and no installed harness, which is what load-validates a repo's `WORKFLOW.md`
in CI. One rule, split by what each stage can answer.

The second direction is the sharper one, and not symmetric with what the generic surfaces may do.
`agent.provider.env: {HOME: /some/path}` stays legal — an operator setting `HOME` to a path is
choosing where the harness looks — while `publish.env: HOME` sets `HOME` to a *credential*, which
is never a meaningful configuration and points both harnesses' stored-credential lookup at a
token. `publish.env: ANTHROPIC_API_KEY` is the same defect aimed at the harness's own auth.

*Design note.* A rule phrased as "may not override what the adapter derived from its provider
config" is too narrow in two ways, both reachable. If a provider field is **omitted**, the
adapter derives nothing, so the per-run map may freely *inject* the credential — `Ready`
verifies auth by one route and `Start` runs with another. And variables like `HOME` reach the
child through the allowlisted passthrough rather than the provider block, so they are not
"provider-derived" at all, yet a harness resolving credentials from a keychain or config file
under `HOME` authenticates as whoever that override points at. A blacklist of adapter-owned
keys would have to enumerate every variable any harness might ever read, and rots the moment
one reads a new one.

The namespace inverts it: the adapter owns the entire environment, and the orchestrator
contributes only its own correlation variables, which have no reason to be anything but
`BEN_*`. Collision then cannot be expressed rather than merely being forbidden — but only if
the reservation is exclusive, which is why the provider block is barred from the prefix too.
A one-sided namespace is not a namespace.

Refusal rather than precedence, throughout. Binding the provider config at construction (§5.7)
is pointless if the environment can be rewritten per run: `Ready` would verify one credential
and `Start` would execute with another, which is the divergence binding exists to remove.
Layering the per-run map last, on the reasoning that the orchestrator's word is final, is the
natural implementation and the wrong one. If the two ever disagree about a variable, one of
them is confused about who owns it, and no ordering makes that safe.

### 7.7 v1 adapters

Exactly two, both process-per-attempt:

- **`claude-code`** — `claude -p --output-format stream-json`, resume via `--resume`.
- **`codex-exec`** — `codex exec --json`, resume via the Codex CLI's session mechanism.

Each adapter documents its `agent.provider` keys (auth, permission/sandbox mode, model,
binary override) and its publish-credential surface. Codex **app-server** (persistent
JSON-RPC) is a named future adapter — the opaque token and handle-based lifecycle were shaped
so it fits without interface change. Wrapper-CLI integrations are **ruled out** as a pattern:
lossy, poll-based, hostage to the wrapper's toolchain.

*Design note.* "Agent-agnostic" means exactly these two harnesses fit without contortion.
The closed enums and provider blocks are justified by observed semantic leakage between even
two transports — not by readiness for a harness zoo. No design concession is owed to other
harnesses.

## 8. Tracker adapter

### 8.1 Write boundary

Invariant 3 (§3) applied: the orchestrator writes **queue mechanics only** — claim/release,
state labels, milestone comments. The agent writes all content. The orchestrator MUST NOT
close issues: the agent's PR carries `Fixes #N` and GitHub closes the issue when that PR
merges, which a human's approving review gates regardless of who performs the merge (§10.1).
Issue state tracks *human acceptance*, not agent belief. A rejected PR simply leaves the issue
open for re-dispatch or triage.

### 8.2 Interface

A normalized **read kernel** plus a small closed write set — explicitly *not* a CRUD state
enum (adapters forced to fake a generic state model end up no-op'ing half of it and eroding
into capability checks inside the orchestrator):

```
TrackerKind (one per tracker.kind, registered at package level):
  Structural(trackerConfig) error      // PURE: no network, filesystem, or subprocess
  New(options) → (TrackerAdapter, error)  // explicit options, not the raw config: the
                                          // provider block, workflow key, claim assignee,
                                          // and a credential source

TrackerAdapter:
  Ready(ctx) error                     // readiness: credentials resolve, tracker reachable
  Fetch(ctx) → []Issue                 // normalized candidates (ETag-aware)
  FindPR(issue|branch) → PR?           // publish evidence for §9.7
  Claim(issue) → verified bool         // write + read-back verification
  Release(issue)
  SetStateLabels(issue, state)
  Comment(issue, milestoneText)        // structured milestones only; idempotent (§8.4)

  Get(identifier) → Issue              // one tracked issue as it stands (§9.8);
                                       // unfiltered, no `dispatchable` verdict
  ClaimedByPrincipal(ctx) → []Issue    // recovery candidates (§9.10); unfiltered,
                                       // cache-bypassing
  HeldClaims(ctx) → []Issue            // the §9.8 sweep, serving held claims and
                                       // parked records: the same query, ETag-conditional
  ClaimHistory(issue) → []ClaimEvent   // ordered claim/label/state evidence (§9.10)
```

The orchestrator decides *when* per its policy (§9); the adapter decides *how* the calls
render on the tracker.

`FindPR` enumerates open pull requests for the exact issue-branch head across every returned page,
stopping only when enumeration is exhausted or a second candidate proves ambiguity. Zero returns
no PR; exactly one returns its complete facts, including its base branch; more than one returns the
shared `ErrPRAmbiguous`, independent of update order or target. It MUST NOT select the newest or
filter candidates by the expected target.

**A tracker adapter is constructed from explicit options**: the provider block, the workflow key,
the claim assignee, and a credential source. Legacy in-provider credentials are compiled into an
implicit source (§5.2.10), so the source is **always present** and there is exactly one runtime
treatment — no nil-means-legacy branch anywhere. The provider block handed to the adapter is
**reduced**: `token`, `credential_source` and `claim_assignee` are excluded, because each has been
promoted to a named option and a key surviving in the map is a second path to the credential.

The two recovery reads exist because startup has no local record to consult and MUST NOT
infer ownership from the absence of a fact (§9.10). Both bypass the conditional cache: an
answer served from our own ETag cache would attest to nothing.

`ClaimedByPrincipal` is deliberately **unfiltered** — every issue the principal holds, in any
state, with any labels. `Fetch`'s label and state filters structurally cannot serve recovery:
the claims most in need of cleanup are exactly the ones that have left the queue partition
(§9.10 step 1).

`HeldClaims` asks that same question on the steady-state path (§9.8) and differs in exactly
one respect: it is **ETag-conditional**, so a review backlog costs one request and — 304s
being unbilled (§8.5) — no rate-limit budget. The cache posture is a separate method rather
than a flag because it is a property of the contract: recovery MUST read origin, and a
signature that lets the caller ask for either could serve recovery a cached answer.

Being unfiltered in state and labels is what lets **one** such read serve every set §9.8 sweeps —
held claims and parked records alike — rather than one read per set: a parked issue is assigned and
labelled, so it is in the response carrying the state and labels its rules read. It stays filtered
by **assignee**, though, and that is the fact `Get` remains for: an issue this list omits has not
been described, and only a read that names it can say why (§9.10).

`ClaimEvent` is the normalized projection of the tracker's own ordered change log —
`{kind, actor, subject, at, id}` over `assigned` / `unassigned` / `labeled` / `unlabeled` /
`closed` / `reopened`. Per invariant 6 the core never sees the provider's raw event payload.
`subject` carries the assignee login or the label name (`closed`/`reopened` have none), and it
is load-bearing: recovery distinguishes a `done` projection from a human re-queue by *which*
`ben:*` label a transition removed (§9.10). Ordering is by `(at, id)`, since tracker
timestamps may be coarser than the events they order (§8.4).

The positive ID of the current claim-establishing `assigned` event is also the **claim epoch**.
It scopes §6.2's base and §9.7's success evidence to one assignment. This does not make every
event ID an epoch: label-transition IDs remain §8.4 milestone occurrences, and a caller MUST NOT
substitute one role for the other. Zero or an absent current assignment event establishes no
epoch.

The two state kinds carry what current tracker state cannot: a `closed` event **stands in the
log after a reopen**, so a retained `done` claim whose issue was closed and reopened between
two observations is still classified from evidence rather than silently retained (§9.2, §9.8).
Reading the same fact off the issue's current state would make the close a moment that a
poll interval can straddle and lose.

Signatures here are illustrative; the binding Go contract is settled with the rest of the
interface.

### 8.3 Normalized issue model

`{ identifier, title, body, labels, state, assignees, blockers, url, created_at,
updated_at, revision }` plus a computed **`dispatchable`** verdict:

- all `required_labels` present (case-insensitive), and
- `state` in `active_states`, and
- **unclaimed — no assignee at all** (§8.4), and
- zero open blockers, and
- no `ben:*` state label present (§9.3 — a state label means a daemon, past or present,
  owns a verdict on this issue).

The claim exclusion is **any** party, not merely another one. Assigned-to-other is a human
calling dibs; assigned-to-self is this daemon's own retained claim on a published issue awaiting
review (§9.2), and dispatching over that would redo finished work. The claim is what blocks
re-dispatch in both directions, which is why its lifetime is specified rather than open-ended
(§9.8).

`identifier` MUST be stable and unique within tracker scope — it names workspaces (§6.3).
`created_at` feeds FIFO dispatch (§9.5).

**`revision` is an opaque change token**: the core compares it for equality and MUST NOT
interpret it. It exists for exactly one question — *might this issue have gone terminal since
we last looked?* — and it is therefore defined over a named subset of the polled issue, not
over all of it.

**The revision projection** (normative) is:

1. the issue's `state`;
2. whatever the tracker exposes about the most recent state change — on GitHub `state_reason`,
   which a reopen sets to `reopened`; and
3. `updated_at`.

**The projection is exhaustive.** Adapters MUST derive the token from **exactly** these three
elements — no fewer and no more — and it MUST change whenever any of them changes. Both bounds
are load-bearing, in opposite directions:

- **No fewer.** `updated_at` alone reduces to the bug this token was introduced to fix: tracker
  timestamps are second-granularity (§8.4), so it holds still across a close-and-reopen sharing
  one second — exactly the case the held-claim sweep must catch (§9.8) — while `state_reason`
  moves. Each element covers a case the others cannot: `state` a close that stands, the reason a
  close a reopen has undone, `updated_at` a *repeated* reopen, which narrows the blind spot from
  any second cycle to one landing inside a single second.
- **No more.** A title, a body, a label, an assignee, a comment count: none can mean the issue
  went terminal, so folding any of them in buys nothing and spends a change-log read on every
  edit — in the one place per-issue reads were ruled out (§9.8). "Mostly these three" is not a
  contract; an adapter that also hashed the title would move the token on every rename.

Equality means only "the tracker attests nothing in the projection changed": a trigger to look
closer, never a verdict, since absence of a fact is never evidence (§9.10).

The projection is *sufficient* precisely because it gates one rule — §9.8's history read. Every
other sweep rule reads its own fact straight from the list response: terminal state from
`state`, partition membership from `labels`, ownership from presence in the read. So a label a
human pulls in the same second as our last observation is still caught on the next sweep, by the
rule that reads labels, not by this token. That premise is what licenses the exclusion; if it
ever stops holding, the projection has to widen with it.

*Residual, accepted and bounded.* On GitHub the case the projection cannot express takes an
already-`reopened` issue closed and reopened **again** inside the same second as the last
observation — state, reason, and timestamp all land where they already were. §9.10 gate 1 is
the backstop: it reads the change log unconditionally, so the next start classifies it and the
claim is released then rather than never (§9.2 records this as one of the two restart-coupled
cases). Webhook ingestion (§13) is the seam if that latency ever matters; reconciliation stays
poll-based (§8.5).

### 8.4 GitHub mapping

- **What is BEN's:** issues carrying the workflow's `required_labels` — opt-in by
  labeling. Provider block keys (adapter-documented): `repo` (owner/name, REQUIRED), `token`
  (`$VAR`; `GITHUB_TOKEN` fallback), `api_url` (optional, for GHE), `claim_assignee`
  (optional machine-user account; MUST be distinct per daemon), `credential_source` (optional;
  names a §5.2.10 entry). A workload-identity credential source **REQUIRES** `claim_assignee`:
  such a credential is statically known not to yield a machine-user principal, so the
  combination is a load refusal. An omitted `token` resolves from `GITHUB_TOKEN` and its
  credential identity is `env:GITHUB_TOKEN` (§10.2).
- **Supported surface:** github.com, and GitHub Enterprise Server releases that are **currently
  supported by GitHub** (via `api_url`). Fields this spec relies on are read against that set —
  `state_reason` for §8.3's revision, native blocked-by for §8.3's blockers, issue events for
  §8.4 and §9.10 — so field availability is scoped once here rather than caveated per rule. An
  out-of-support GHES is not a v1 target.
- **`identifier`:** the issue number as a string.
- **Dependencies:** native blocked-by relations (GA Aug 2025) surface directly into
  `blockers`; no body-text conventions.
- **Claim = machine-user assignee.** The assignee is the account named by `claim_assignee`, or,
  where that key is absent, the account the tracker credential authenticates as. A deployment
  running more than one daemon MUST give each a distinct assignee — §9.8's and §9.10's reads are
  assignee-filtered, so daemons sharing one account read each other's claims as their own
  (§10.1's operator obligations). Human-visible in every GitHub view; one seat; 5,000 req/hr is
  ample for one daemon. Two production-proven
  hardenings are **normative**: (1) **post-write verification** — GitHub returns 201 even when
  assignment silently fails, so read back and confirm the claim landed before dispatching; (2)
  **assigned-to-other exclusion** — an issue assigned to any other party is not dispatchable
  (a human called dibs) — both apply identically to a configured and a credential-derived
  principal. After post-write verification, BEN reads `ClaimHistory` and takes the epoch from the
  current claim-establishing `assigned` event, ordered exactly as contested-claim arbitration is.
  The adapter does not synthesize an epoch and the orchestrator does not derive one from a
  timestamp. If the current assignment cannot be given a positive event ID, no agent may launch.
  The **GitHub App + claim-label profile** (12,500 req/hr per installation; App bots
  can't be assignees) is the documented team/hosted upgrade path, not built in v1.
- **Contested claims.** GitHub has no conditional writes, so a read-back showing company
  cannot be resolved by the write itself. It is resolved from the tracker's own **ordered
  assignment log**: replay the issue's `assigned`/`unassigned` events, and the still-assigned
  party whose standing assignment began first wins. Ordering is by
  **`(created_at, event id)`** — event timestamps are second-granularity and racing daemons
  land inside the same second routinely, so ids break the tie truthfully rather than
  arbitrarily. Every claimant reads the same log and reaches the same verdict, which is what
  makes the winner agreed rather than raced.

  Three rules make the replay sound. A login the log has **seen and since released** has
  withdrawn — skip it, or a departing loser would leave the race with no winner at all. A
  login the log has **never seen** yet is somehow assigned (event retention, a transfer) is
  unorderable: **refuse to guess and yield.** A daemon that loses, or cannot establish an
  order, releases **only its own** assignment — never another party's (§9.10).

  *Design note.* This yields exactly one winner whenever the log can order the contenders,
  which is the ordinary case, and safely yields everyone when it cannot. Ambiguity therefore
  costs a wasted round, never a double dispatch. No cooldown, backoff, or deferral state is
  introduced: that would be scheduling policy, and the adapter is not where scheduling policy
  lives. The residual risk — every contender yielding on an unorderable log and re-colliding
  next tick — is accepted rather than engineered around. §10.1 already scopes same-queue
  multi-daemon as tolerated-but-wasteful with no queue SLO, and §13 defers real leases; making
  progress against a degraded event-history endpoint is explicitly not a v1 requirement.

  *Known window.* Winning the order means no other party was assigned first — not that we are
  alone. A human who assigns themselves between our write and our read-back loses the order
  and BEN dispatches alongside them. BEN never removes their assignment, and §9.8 reconciles
  the run as unroutable, stopping it and releasing. Only **detection** is bounded, at one poll
  interval: the issue is tracked as running, so the next tick sees the changed assignee set.
  **Termination and release are not bounded** — §9.8 stop semantics retain the claim whenever
  termination is unconfirmed and retry on later ticks, precisely so a possibly-alive process
  never shares a workspace with a replacement. A human who takes an issue mid-run may
  therefore wait several ticks for BEN to let go, and MUST NOT be told otherwise. Closing the
  window entirely would require a won-but-not-sole claim outcome, which the closed state
  machine (§9.2) has no state for.
- **Daemon identity** in claim comments: `<hostname>/<workflow_key>` plus the claim
  principal — multi-daemon-ready.
- **Milestone comments** at exactly: claimed (with daemon identity), published (PR link),
  failed (reason from §7.3), needs-review. No per-tick spam.
- **Milestone comments are idempotent per projection occurrence.** Each carries a
  machine-readable marker naming its kind and the id of the label transition that defines its
  occurrence. `Comment` MUST NOT post a second comment bearing a marker already present on the
  issue; re-issuing one is a no-op, not a duplicate. The occurrence is **per kind**, because
  the four milestones recur differently:

  | Milestone | Occurrence |
  |---|---|
  | `claimed` | The **first** `labeled ben:claimed` of the claim cycle |
  | `needs-review` | **Each** `labeled ben:needs-review` |
  | `failed` | The `labeled ben:failed` |
  | `published` | The `unlabeled` that clears the projection at `done` |

  Neither a coarser nor a finer key works for all four. Keying everything on the *claim cycle*
  suppresses a legitimate second `needs-review`: re-queueing retains the assignment (§9.2), so
  a second failure lands in the same cycle. Keying everything on *each* transition spams
  `claimed`: §9.3 maps preparing, verifying, and backoff onto `ben:claimed` too, so the label
  is added and removed repeatedly within one cycle and the claim milestone would repost on
  every re-entry — exactly the per-tick noise this section forbids.

  Idempotency is what lets recovery *complete* an interrupted terminal projection rather than
  choose between skipping the comment and double-posting it: a kill between the label write
  and the comment would otherwise leave a milestone that can never be written, because the
  projection it belonged to is already finished and no later transition will re-attempt it.
  The cost is one comment read per milestone write, at most four per issue per occurrence.

### 8.5 Polling discipline

ETag-conditional list polling on the core budget (304s are free). The Search API (30/min)
MUST stay out of the poll loop. Rate-limit handling honors both `Retry-After` and
`X-RateLimit-Reset`. Webhooks are a future latency add-on feeding the same normalized
create-task path — reconciliation stays poll-based (§13).

Server-directed backoff is keyed by **(API endpoint, credential source authority)** — preserving
the endpoint scope, and adding an identity that is stable, non-secret and unchanged across
rotation. For `octo_sts`, that authority includes configured scope, because scope selects the
trust-policy namespace. A token-keyed gate abandons its backoff on every refresh and accumulates
one entry per rotation for the life of the process.

## 9. Orchestrator

### 9.1 Authority model

One goroutine — the **authority loop** — owns all state transitions, fed by watcher/timer/
runner signals under an errgroup supervisor. The run record carries `{stage, attempt,
failure_reason}` as **fields**; the closed failure taxonomy (§7.3) preserves what a wider
state enum would otherwise exist to distinguish.

### 9.2 States and transition map

Nine states. The transition map is **normative and closed** — an illegal transition is a bug
(loud error), not a no-op.

`queued` → `claimed` → `preparing` → `running` → `verifying` → { `done` | `needs-review` }
with `backoff` and `failed` on the failure track.

| From | To | Trigger |
|---|---|---|
| queued | claimed | Selected by dispatch; `Claim()` verified by read-back, the content-approval check passed (§9.5), and a pending intent for the resulting claim epoch is durable (§6.2) |
| queued | needs-review | Claim verified, but the content is not one a labeler approved (§9.5) |
| claimed | preparing | Immediately follows verified claim |
| preparing | running | Worktree ready, `before_run` passed, agent started |
| preparing | backoff | Prep/hook failure, retryable, attempts remain |
| preparing | failed | Non-retryable prep failure (fail-closed git errors) or attempts exhausted |
| preparing | needs-review | An **unknown or permanent** credential failure in workspace preparation, or in minting the publish credential (§9.8) |
| running | verifying | Runner event `succeeded` |
| running | backoff | `failed(reason)` with retryable verdict, attempts remain |
| running | failed | Non-retryable verdict, attempts exhausted, or kill |
| running | needs-review | `budget_exceeded` (§9.9) |
| verifying | done | Publish evidence complete (§9.7) |
| verifying | preparing | Clean exit without complete publish evidence; sessions < `max_turns`; issue still active + routable — the **continuation** re-dispatch (~1 s) |
| verifying | needs-review | Evidence contradicts the claim, or `max_turns` exhausted without publish |
| backoff | preparing | Timer fired; issue re-fetched, active + routable; content still approved (§9.5); slot free |
| backoff | backoff | Timer fired; no free slots (requeue with fresh timer) |
| backoff | needs-review | Timer fired; the content is no longer one a labeler approved (§9.5) |
| needs-review | backoff | Human removed the state label; reconciliation re-queues (§9.8) |
| *any non-terminal* | failed | Kill (`failed(killed)`) — legal from every non-terminal state |

**Release** (claim removed, local record dropped) is an exit from the machine, not a state:
it fires when a claim fails verification, when reconciliation or a backoff re-fetch finds the
issue absent/terminal/unroutable (§9.6, §9.8), and when `failed` is reached (see below).

Terminal and parked states:

- **`done`** — published and verified. State labels removed; PR-link comment stands; the
  **claim is retained** (assignee stays) so the issue is not re-dispatched while the PR
  awaits review. *Assigned-to-self + no state label* is the tracker-visible shape of
  "published, awaiting review" — but it is also the shape of a claim killed before its label
  was projected, so it is **not** self-classifying: recovery separates the two from label
  history (§9.10). Merge closes the issue via `Fixes #N`.

  **The retained claim's lifetime is bounded by a running daemon rather than by a restart,
  except in the two cases named below.** `done` produces a **held-claim record** — no
  workspace, no runner, nothing to stop — and §9.8's held-claim sweep releases it on the close,
  within one poll interval. Detection survives a close-and-reopen inside that interval wherever
  the reopen moves the issue's `revision` (§8.3): the sweep then reads the `closed` **event**
  (§8.2), which stands in the log after a reopen, so the claim is released and the reopened
  issue returns to the queue as new work rather than staying assigned and undispatchable. A
  record whose issue leaves the workflow's label partition is released the same way (§9.8).

  **Two cases stay restart-coupled, both by design:**

  1. **A PR closed unmerged.** Rejecting a PR neither closes the issue nor leaves any event on
     it, so the sweep has nothing to observe and the claim stands. §9.10 reclassifies it at the
     next start — publish evidence is then incomplete, which is a contradiction, not a pass, so
     it parks `needs-review`. A human unassigns the principal or closes the issue; §13 names the
     PR-state sweep that would close the gap and the trigger for building it.
  2. **A close-and-reopen the revision projection cannot express** (§8.3) — on GitHub, a
     second such cycle inside one timestamp second. The sweep never learns to look; §9.10
     gate 1 reads the change log unconditionally and releases at the next start.

  In both the issue is open and undispatchable by anyone until then, and in neither is the claim
  released by any tick. Everything else about `done` release is bounded by a poll interval.
- **`needs-review`** — parked for a human; claim and workspace retained; `ben:needs-review`
  label set. A human re-queues by removing the state label (reconciliation notices and
  re-enters `backoff`), or closes the issue. The re-queue **restores the run budgets**: the
  continuation turn count and accumulated cost reset to zero, and `limits.max_attempts` is
  measured from the re-queue. Two of the three ways into `needs-review` are exhausted bounds
  (`budget_exceeded` §9.9, `max_turns` §9.6); carrying them forward would re-park on the next
  run and make the documented gesture a no-op. Removing the label is the human authorizing a
  fresh budget — the same authority as clearing `ben:failed`. `attempt` itself is **not**
  reset: §9.10 reads `attempt ≥ 2` as *work may already exist on the branch* and the template
  branches on `{% if attempt %}` (§9.6), so a re-queue — which always follows a run that left
  a workspace, often a branch and a stale PR — MUST NOT present itself as a first attempt. The
  counter is an identity; only the budgets reset.

  A claim parked because its epoch/base state is missing, unreadable, contradictory or inconsistent
  with a run marker is **epoch-faulted**. Removing `ben:needs-review` alone does not satisfy the
  missing safety fact: every later dispatch decision rechecks it and parks again before hooks or
  launch. Ordinary recovery is to remove the principal's assignment and deliberately create a new
  claim-establishing assignment after the workspace is quiet; BEN never edits or guesses an epoch
  in place.
- **`failed`** — attempts exhausted or non-retryable. `ben:failed` label set with a
  reason comment; the **claim is released** so a human can take it; the label blocks
  re-dispatch (§8.3) until a human clears it.

### 9.3 Tracker label projection

| Orchestrator state | GitHub label |
|---|---|
| queued | *(required labels present, no state label)* |
| claimed / preparing / verifying / backoff | `ben:claimed` |
| running | `ben:running` |
| needs-review | `ben:needs-review` |
| failed | `ben:failed` |
| done | *(state labels removed; PR comment stands)* |

Transient detail (preparing vs verifying vs backoff) lives in `ben status`, not label
churn.

### 9.4 Tick sequence

Every `polling.interval_ms` (first tick immediately at startup, after recovery §9.10):

1. **Reconcile** (§9.8) — always runs, even when validation fails.
2. **Dispatch preflight validation** — including defensive config revalidation (§5.4).
   Failure skips dispatch this tick, never reconciliation.
3. **Fetch** candidates (ETag).
4. **Sort** (§9.5).
5. **Dispatch** while slots remain.

### 9.5 Dispatch policy

**FIFO, age-only**: oldest `created_at` first, `identifier` as tiebreak. No priority in v1
(GitHub has no native field; a `ben:urgent` jump-the-queue label is a named cheap
extension, §13). Eligibility: `dispatchable` (§8.3) ∧ not already tracked locally ∧ free
slots. **Concurrency is global-only**: `limits.max_concurrent_agents` caps live agent
processes — the one scarce resource.

**Approval binds to content.** Applying a `required_label` approves the issue *as it read at
that moment* (§6.7). Where `required_labels` names more than one, the **approving instant** is
the standing `labeled` event of the last of them to be applied — approval is not complete until
the set is.

A claim **pins** the approved title and body, and every attempt renders from the pin: an edit
made after the approving instant never reaches an agent. The pin is the content read at claim,
which is admissible only because the same check establishes that nothing edited it since that
instant — BEN never reconstructs a historical body. An issue whose content changed after the
approving instant, or whose edit the tracker cannot order against it, is **not dispatched**: it
parks `ben:needs-review` for reapproval.

The same ordered `ClaimHistory` read that dates approval also identifies the current claim epoch.
After approval passes, but before `queued → claimed` projects any state label, BEN durably begins
that epoch in the workspace provider. Until that write succeeds, it retains the verified claim,
projects nothing and launches nothing; a later tick retries the operation. An absent or
unorderable current assignment is a stated refusal, not epoch zero. Content approval and claim-
epoch initialization are separate predicates: passing either cannot stand in for the other.

**Reapproval is that same act and the only one** (§6.7): a labeler re-applies a required
label — removing and re-applying it where it already stands — which moves the approving
instant, and the pin is taken afresh against it. The §9.2 human re-queue resumes a parked run
and approves nothing, so a re-queue over drift that has not been reapproved parks again.

Absence of edit evidence is not evidence of no edit (§9.10).

*Design note.* The check runs at every **dispatch decision** — the claim read-back, and the
§9.6 re-fetch on both retry tracks — and at none of the reconciliation ticks in between.
Reconciliation exists to notice that a run must *stop*, so it refreshes the routing facts
(§9.8's state, labels, assignees, blockers) and leaves the pin alone; that is what makes the
per-tick cost zero and keeps content out of §8.3's revision projection. A tracker that cannot
answer when content was last edited has stated nothing, not "unedited", and the issue parks.
Second-granularity timestamps (§8.4) make an edit sharing a second with the approving label
unorderable against it, and unorderable is a refusal — the change-log id does not rescue it,
since that id orders events against *each other* and a content edit is not in the log.

**Retention.** The canonical rendered prompt for each attempt is retained alongside that
attempt's transcript (§10.3), at the same permissions, so "what was this agent told" is
answerable after the fact. It is the bytes the agent was given, not a later re-render.

### 9.6 Retry policy (dual-track)

Gated entirely by the runner's static verdicts (§7.3):

- **Continuation track (clean exit).** `succeeded` but verification finds no complete publish
  evidence: after ~1 s, re-check the issue; if still active and routable, re-dispatch **with
  the continuation token** through `preparing` (hooks fire per attempt), up to
  `limits.max_turns` sessions; exhausted → `needs-review`. The template needs no new surface:
  it branches on `{% if attempt %}` and `run.previous_outcome`.
- **Failure track (retryable verdict).** → `backoff` with delay
  `min(10s · 2^(attempt−1), limits.max_retry_backoff_ms)` plus **deterministic FNV-based
  jitter** (reproducible in tests), up to `limits.max_attempts`; non-retryable → `failed`
  directly, no wasted attempts.
- **Backoff firing** re-fetches the issue by ID first: absent → release; terminal → dispose
  workspace + release; active + routable → dispatch (or requeue if no slots); otherwise
  release.

`attempt` increments on both tracks. A new record ordinarily begins at 1. When the current claim
epoch is pending, its first claim-aware `Prepare` observes the reattached branch **against the
outgoing epoch's pin before atomically installing the current epoch's pin**. Positive evidence
that the head moved beyond and descends from that outgoing base gives the record an attempt floor
of 2. The floor means only *work may already exist*: it does not invent
`run.previous_outcome`, consume a continuation turn, or reduce the failure-attempt budget. No
outgoing pin on a genuinely fresh branch gives no floor; an expected pin that cannot be read is
an error and launches nothing. Deriving the fact after installing the new pin is forbidden — it
would compare the head to itself and erase #94's evidence.

`max_turns` bounds the continuation chain. `max_attempts` bounds failure-track dispatches measured
from the current claim or human re-queue baseline, independently of an evidence-derived attempt
floor. A human re-queue that retains the assignment retains the epoch and base too.

### 9.7 Verification (git facts)

For the first `Prepare` of a claim epoch, after remote-first reattachment and before any
current-attempt hook, atomically record `{claim_epoch, branch HEAD, target_branch}` as that claim's
verification base (§6.2). On every attempt and during recovery, the epoch MUST equal §9.10 step 2's current
claim-establishing assignment ID before publish evidence is read. Thus “branch advanced” means
that **this claim** added commits. A controller unassignment followed by BEN reassignment remints
the base at the prior PR head; a no-op reviser cannot satisfy leg 1 from commits an earlier claim
produced. On `succeeded`:

1. **Branch advanced** — the local issue branch HEAD differs from and descends from the
   recorded base SHA.
2. **Branch on origin** — the pushed branch exists remotely with those commits.
3. **PR exists and joins the target** — `FindPR` returns exactly one open PR for the branch, and
   its base branch exactly equals the claim's retained `target_branch`.

All three → `done` (publish milestone comment with PR link, dispose workspace, retain claim).
No PR after a clean exit → continuation track (§9.6). One PR against another target is contradictory
evidence → `needs-review`; multiple exact-head open PRs are an ambiguity error and fail closed.
Other evidence contradicting the
claim (e.g. no commits at `max_turns`, or verification itself fails ambiguously) →
`needs-review`, workspace kept. Fail closed: verification errors never count as success.

**One exception, and it is not a relaxation of fail-closed.** A **transient** credential failure
while reading publish evidence is retried **in `verifying`, once per poll tick**; the attempt is
neither ended nor recorded, and no verdict is routed, until one is final. This section's
fail-closed rule covers evidence that contradicts or cannot be established; a credential that
could not be obtained establishes **neither**, and the evidence itself is unchanged on git and the
tracker. An unknown or permanent credential failure parks, as this section already does. The record
keeps its claim and its `ben:claimed` label throughout, which is what makes the retry free: §9.10
reads exactly that at the next start if the daemon dies mid-wait.

### 9.8 Reconciliation and stop semantics

Stall detection lives in the runner (§7.4), not here. Each tick, refresh the **running**
records — `claimed`, `preparing`, `running`, `verifying` — from the tracker with a `Get` per
record (§8.2). That set is the one `max_concurrent_agents` bounds (§9.5), so its cost is a policy
number rather than a human's — and its sharpest verdict is one a filtered list can only report as
an absence it cannot explain: an issue whose assignment a human took is simply missing from the
sweep read, so folding these in would put a confirming `Get` (§9.10) on the one path that must
tear down a live process promptly, in place of the direct read it already makes:

- Running issue turned terminal → stop the run, dispose workspace, release.
- Running issue still active but unroutable (labels/assignee changed) → stop, **keep**
  workspace, release.
- Refresh failure → keep everything running; retry next tick.

**Credential failures.** A credential source failure is classified **transient**, **permanent**, or
**unknown**, and unknown is the inert zero: a class that defaulted to transient would make every
unclassified error retryable by omission, and an omission is exactly what a new kind or a new error
path produces.

The class governs **this section's automatic attempt retry at `Prepare` and `Start`, and only
that**: only an explicit transient classification retries there, and a permanent or unknown failure
parks `ben:needs-review` (§9.2) **without spending the remaining automatic retry budget** — a
misconfigured trust policy fails identically every time. A dispatched preparation **is** an attempt
in the §9.6 accounting, so the park does not un-count it; what it does not do is spend the attempts
that remain.

It does **not** govern the tracker's own routes. A read retries on the next poll tick, and an
**owed write remains owed across every class** — for the process lifetime, which the drain waits
for; an abrupt restart reconstructs from tracker-visible facts alone (§9.10), which is precisely why
the write is retried until accepted rather than logged and abandoned. Discarding it on a permanent
error would leave assigned-with-no-state-label, which §9.10 step 3 never revisits. Verification
retries ride the poll tick within `verifying` (§9.7).

Three routes, named separately, because folding them into one sentence is what would let a reader
apply attempt backoff to a poll. On the routes the class does not govern it is still **read**, for
log severity: a non-transient failure logs at **error** naming the source's authority — never the
token — so an operator reads a wrong trust policy off the log instead of inferring it from a silent
stall. Reporting on a class is not routing by it.

Two classifications belong to the credential boundary rather than to a source, because a source
reports what the exchange did and cannot know what the caller needs: **TTL insufficiency** (§7.7),
which is arithmetic and therefore permanent, and an **empty credential value**, which is a permanent
refusal *before any downstream GitHub request, git invocation or agent launch*. A source that
returns success with no credential is a source defect, and no consumer may discover it by making an
unauthenticated call.

**The sweep.** Parked (`needs-review`) records and held claims are refreshed **together**, from
**one** ETag-conditional `HeldClaims` read per tick (§8.2) — the principal's assignments, any
state, any labels. Neither set is bounded by anything the daemon decides: both grow with how long a
human takes, and a `Get` per record is O(that) *per tick* — in round trips and in GitHub's
request points, which an authenticated 304 spends even where it refunds the core budget (§8.5).
One read serves both because an issue in either set is assigned, and therefore in the response,
carrying the state and the labels every rule below reads.

**Parked records.** A parked record is a run the machine still owns — claim, workspace, attempt
counters — waiting for a human:

- **Terminal state** → stop (there is nothing running), dispose workspace, release. First, ahead
  of the label rule: a human resolving an issue by hand closes it *and* clears BEN's label, and
  the label rule alone would re-dispatch it.
- **State label gone** → restore the run budgets and re-enter `backoff` (§9.2). The response's
  label set is usable for this only when the record owed no `ben:*` **projection** when the read
  was issued. While one is owed the tracker is still reporting the labels from before the
  transition, and this rule would read BEN's own unlanded write as a human's gesture — a §9.5
  drift park being the case that bites, since it comes from `queued` where no `ben:*` label has
  been projected at all. Two bounds on that condition, and both are load-bearing:

  - **The projection, not every owed write.** A record also owes milestone comments (§8.4) and
    local effects, none of which touch a label, and a comment can fail indefinitely on its own
    §8.5 allowance. Gating on the whole queue lets one wedged comment suppress every human
    re-queue for the life of the record, with the state label standing on the issue throughout.
  - **This rule, not the classification.** Neither an issue's state nor its assignment is a fact
    BEN's writes can move, so a record whose projection is stuck retrying is still classified by
    the rules around it. Otherwise a failing write hides a closed or deleted issue for exactly as
    long as it keeps failing.
- **Required labels gone** → nothing. A parked record MUST NOT be unparked, released or stopped
  for leaving the label partition: a labeler removing a required label and re-applying it is what
  reapproval looks like from here, and it is two writes rather than one (§9.5). This is where the
  parked rules deliberately differ from both the running rule above and the held rule below.
- **Absent from the read** → confirm with one `Get`, and never act on the absence itself (§9.10);
  the confirmation carries the same projection condition, asked as of the moment *it* was issued.
  The confirmation answers two independent questions, and both must be read: **is the claim
  ours**, which decides whether a release is owed, and **is the issue terminal**, which decides
  the workspace exactly as it does for a record still assigned to us. So:

  - *deleted or transferred* (`ErrIssueNotFound`) → dispose, drop the record, nothing to release;
  - *the principal is not assigned* → the assignment **is** the claim (§8.3), so there is nothing
    to release and nothing left to track: drop the record, leaving the state label standing, and
    dispose the workspace **keeping** it only if the issue is still active. A closed issue whose
    assignment a human also dropped is one gesture, and it is terminal: it disposes. Dropping the
    record is where a restart arrives too, `ClaimedByPrincipal` no longer returning the issue for
    §9.10 to classify — and clearing the label instead of leaving it is what discards the recovery
    and attempt continuity §9.10 reads back (see **Shutdown semantics**);
  - *still ours* → the read lagged, and the rules above apply to what the `Get` returned.

  **At most one such confirmation per tick.** It is the only other request the parked rules can
  spend, so leaving it per-record would restore the O(parked) cost in the one case that produces
  every absence at once — a human unassigning the principal from a backlog. K absences resolve
  over K ticks, which is free: an absence is an issue that is no longer ours or no longer exists,
  and neither is urgent. A deferral MUST be reported (§8.5's accounting), since a silent cap reads
  as having covered everything.
- **Refresh failure** → keep everything; retry next tick.

Epoch validity is rechecked before every unparked record can prepare. An epoch-faulted park whose
label is removed therefore re-parks without running; the human gesture restores budgets but does
not create missing verification evidence. Releasing and later reassigning the principal creates
the new tracker event from which a new epoch may be initialized. Disposal and ordinary claim
release do not erase a valid outgoing pin: the next claim needs it to derive §9.6's prior-work
fact before replacing it.

**Held claims.** `done` retains the claim (§9.2) and leaves a **held-claim record**:
identifier, claim-cycle anchor, PR link, and the `revision` last observed for the issue. It is
not a run — no workspace, no runner, nothing to stop — and it is not local state either, but a
cache of a fact the tracker can enumerate, so recovery rebuilds the whole held set from
`ClaimedByPrincipal` and the §9.10 table. A record lives from `done` until its claim is
released, or until the process exits. Its rules read the same response:

- **Terminal state** → dispose (a no-op: §9.7 already disposed at `done`), release, drop the
  record. The merge path and every manual close end here, on the list response alone.
- **Open, but the `revision` differs** from the record's last observation → spend one
  `ClaimHistory` and look for a `closed` event inside the current claim cycle (anchored per
  §9.10 step 2). Found → release and drop the record, exactly as a still-closed issue would:
  the issue was closed and reopened, and the log still says so after the state has moved back.
  Not found → keep the record and re-baseline the revision.

  The trigger is load-bearing, because the two facts live in different places. A close that a
  reopen has undone survives **only** in the log, and reading the log for every held claim on
  every tick is the O(held) cost this sweep exists to avoid — so a rule that consulted history
  only for issues *currently* reading closed could never discover a reopen at all. The revision
  triggers and never decides: the event decides, and a bump that turns out to be a comment costs
  one read and changes nothing. It MUST be the §8.3 token rather than `updated_at`, whose
  second granularity cannot express a close and a reopen inside one second — the residual that
  survives even so is named in §8.3 and backstopped by §9.10 gate 1.
- **Required labels gone** → release, drop the record. Mirrors the unroutable rule above and
  §9.10 gate 4: the issue has left the workflow, so the claim has no standing.
- **A settled release fails to land** → keep the release owed and retry it independently of the
  sweep. The write's error decides nothing (#134); once the failed attempt is no longer in flight,
  it earns one confirming `Get`. `ErrIssueNotFound`, or a successful read showing the principal no
  longer assigned, drops the record because there is no claim of ours left to release. A failed
  read or continued assignment retains the record and immediately re-drives the same settled
  release; that attempt's failure earns another confirmation. The confirmation MUST NOT re-derive
  the release reason, re-baseline `revision`, or read `ClaimHistory`.

  The release write and its confirmation share one per-record operation slot. A confirmation that
  may drop the record MUST NOT overlap a queued or executing release: rejecting the stale result
  cannot retract the write, which could otherwise cross into a later claim cycle and remove its
  assignment.
- **Absent from the read** → confirm with one `Get` before acting. An assignee-filtered list
  cannot separate "the principal was unassigned" from consistency lag, and absence of a fact is
  never evidence (§9.10). Confirmed not ours → drop the record; there is nothing to release.
- **Confirmation budget** → at most one held-claim `Get` per tick across sweep absences and failed
  settled releases. The two candidate sets are disjoint — a releasing record is classified under
  no sweep rule — and are offered in one explicit rotation that advances on the offer rather than
  its outcome. Every member of a stable K-candidate set is offered within K ticks, so an
  unconfirmable claim from either set cannot starve the other. A deferral MUST be reported (§8.5's
  accounting), since a silent cap reads as having covered everything. The release writes
  themselves are owed effects and are not part of this read bound.
- **Refresh failure** → keep every sweep-derived record and retry next tick. It gates neither a
  settled release retry nor the confirmation a failed release earned.

**Poll cost is bounded by the read shape, not by policy.** One conditional list read per tick,
O(pages) and unbilled while nothing changes — independent of how many claims are held and how
many records are parked, the two quantities the daemon does not control: both grow with human
latency. A `Get` per record would be O(held + parked) *per tick*, and it is not the mechanism for
either. That is a bound on **round trips** and not only on billed requests, and the distinction
matters twice: the per-issue refreshes are issued serially in one pass, so a review backlog
otherwise delays the reconciliation of every *running* record behind it; and an authenticated 304
refunds the core budget but not GitHub's request-point allowance (§8.5), so "the reads are free"
was never the whole account of what O(parked) costs. Held-claim absences and failed releases may
add at most one confirming `Get` between them per tick; parked absences may add one of their own.
Those budgets are separate because the held and parked sets, and their verdicts, are separate.
`ClaimHistory` costs one read per **observed change** to a held issue:
an idle tick over any number of records reads no history at all, and the ordinary close needs none
either, the list response having settled it. A parked record's rules read no history at all, and
its `Get` is spent only on the absence they may not act on.

Every terminal outcome is held until its execution domain is positively quiet. Before `Done`,
reconciliation may only `Probe`; after `Done`, it may `Stop`. Events-close and Done may arrive in
either order, but both use one predicate: a naturally completed empty domain progresses without a
signal, while a surviving member remains unconfirmed until bounded teardown or later natural quiet.
Scheduler order changes neither safety nor liveness.

An unconfirmed domain retains the handle, claim, workspace, run marker, and concurrency slot, reads
no publish evidence, and retries on later ticks. Graceful shutdown likewise completes only after
every held local or remote domain is confirmed quiet; the supervisor's existing outer deadline
remains the final bound. Removal of an already-quiet substrate resource may continue in its provider
janitor and does not weaken that domain decision.

**Shutdown semantics.** A graceful shutdown (§11) **initiates no new release and no new terminal
projection**. It stops dispatch, interrupts every in-flight run, waits for confirmed termination
wherever a handle exists, and completes the effects already ordered. Whatever claim or projection
has durably landed stays standing, and §9.10 recovery is what resumes the work at the next start.
One read is part of completing such an effect: a confirming `Get` earned by a failed held-claim
release ordered before the drain may still be offered under the same one-per-tick held bound. The
draining sweep classifies nothing and offers no absence, so this initiates neither a new release nor
a new terminal projection.

The alternative — releasing each confirmed stop, as a literal reading of §11 once suggested — is
unreachable rather than merely undesirable: an issue bearing a `ben:*` state label is excluded
from dispatch, so releasing the claim while the label stands strands the issue until a human
clears it by hand. Clearing the label as well is worse, and the reason is *not* concurrency —
shutdown waits for confirmed termination, so no process survives to share the workspace. It is
that the label events and the claim-establishing assignment are what §9.10 classifies from, so
clearing them discards the issue's **recovery and attempt continuity**: `attempt` restarts at 1,
which §9.10 and the prompt template both read as *no work exists on this branch yet*; the claim
cycle no longer matches the §8.4 milestone markers; and a re-dispatch that reattaches at the
pinned base can walk past commits that were never pushed. §3.1 accepts losing unpushed work when
the workspace root is lost — not when the root is intact and the daemon discarded the means of
finding it.


### 9.9 Budget enforcement

Accumulated `usage` events breaching `limits.max_cost_usd` → stop the run,
`failed(budget_exceeded, retryable=false)` → park as `needs-review`, workspace kept.

### 9.10 Recovery — the statelessness invariant realized

Startup reconstructs the world from tracker + git. Claim and state projection span multiple
GitHub writes and GitHub offers no multi-write atomicity, so a kill can land between any two
of them. The window cannot be closed; it is made **self-describing** instead. The governing
rule: **recovery classifies from positive evidence, and the absence of a fact is never
evidence.** No candidate is left in an unclassified state, and no classification rests on an
assumption the tracker could not confirm.

1. **Fetch candidates: every issue assigned to the claim principal**, in any tracker state,
   carrying any labels or none (`ClaimedByPrincipal`, §8.2). Cache-bypassing; the Search API
   stays out of this path as everywhere else (§8.5). The candidate set is deliberately **not**
   narrowed to the workflow's label partition or to active states: a claim that must be
   released can outlive both. An issue closed, or stripped of its queue labels, while the
   daemon was down still carries our assignment, and filtering on the queue would hide exactly
   the claims that need cleanup. The claim principal is unique per daemon (§10.1, normative),
   so everything it holds in repo scope is ours to account for.
2. **Establish the current claim.** For each candidate, the **claim-establishing event** is
   the most recent `assigned` event naming the principal with no later `unassigned` for it
   (`ClaimHistory`, §8.2). It defines the current **claim cycle**: evidence means evidence
   dated after that event; anything earlier belongs to a previous cycle and MUST NOT be read
   as current. The current claim epoch is the positive ID of this claim-establishing assignment
   event.
3. **Reconstruct one effective projection.** Classification reads the *ordered label events*,
   never the current label set. Label projection **adds before removing** (§9.3), so an
   interrupted projection leaves two `ben:*` labels standing rather than none — a set-based
   reading would match two rows at once and return contradictory verdicts. The **effective
   projection** is the label of the most recent standing `labeled ben:*` event (standing = no
   later `unlabeled` for that label). Any other standing `ben:*` label is the residue of an
   interrupted projection, and recovery completes the projection by removing it.
4. **Classify.**

   **The workspace precondition.** Before launch, the daemon writes the run marker. Once a run
   domain exists it upgrades the marker with substrate-owned evidence identifying that domain, not
   merely its direct process. The identified marker is evaluated by the same read-only positive
   quiet predicate used at run time. Only confirmed domain quiet removes the marker; live,
   unreadable, unsupported, malformed, reused, or otherwise unconfirmed evidence retains the claim
   and workspace and is re-evaluated. A marker not upgraded across the launch window remains
   `unknown_launch` and parks as before.

   Linux local evidence is a versioned canonical payload naming the host boot,
   delegated/root/attempt cgroup device and inode identities, unpredictable attempt name,
   supervisor host PID and start ticks, its immutable PID-namespace device/inode identity, and its
   launch-fixed cgroup-namespace identity. Live observation uses the clone pidfd; restart
   observation first opens a pidfd and compares immutable procfs identities before interpreting
   liveness. Cgroup discovery and cleanup are descriptor-relative after initial validated procfs
   discovery; a missing, swept, or replaced cgroup is not disappearance. A reboot proves an old
   local domain quiet.

   The local provider owns cgroup-resource cleanup independently of the marker: a janitor removes a
   positively exited and recursively empty attempt tree, and startup sweeps canonical empty crash
   residue while preserving populated trees. Neither action clears the marker or proves quiet. The
   shipped systemd profile delegates the subtree but retains `KillMode=mixed`; after abrupt
   main-process failure, recovery confirms only by proving the recorded PID-namespace init exited
   and applying the populated-cgroup veto, not by assuming systemd teardown or an absent path. An
   exact supervisor surviving manual replacement remains possibly live. Legacy same-boot `pgid`
   evidence remains possibly live even on group `ESRCH`; remote evidence keeps its backend-defined
   identity and domain-quiet proof. Core and the universal contract interpret no scheme.

   A candidate whose workspace is **possibly live** is retained: the claim and the workspace
   are kept, nothing is disposed or released, no agent is dispatched, and the question is asked
   again on later ticks. An orphan that ends therefore converges with no human — its stream did
   not survive the restart, but §9.7's evidence is git facts and never needed it.

   A candidate whose **launch outcome is unknown** parks `ben:needs-review` with the claim and
   workspace retained. Waiting cannot end a question that has no answer coming, and guessing in
   either direction risks a second agent in a live worktree or a claim abandoned to one.

   The precondition governs the workspace only. Verdicts that touch the tracker alone —
   completing an interrupted projection, re-issuing a milestone comment (§8.4) — are unaffected
   and proceed, so a possibly-live workspace never suppresses the repair that §9.10 owes.

   Gates first, in order; each either resolves the candidate or falls through to the next. The
   projection table below is then exhaustive over what reaches it.

   **Every recovery verdict — the gates included — re-issues the milestone comment for the
   state it lands in**, and states with no milestone get none. Because `Comment` is idempotent
   per occurrence (§8.4), that is a no-op when the comment survived and a repair when it did
   not — and it is the **only** repair available, since the projection that owed the comment
   is already finished and no later transition re-attempts it. All four milestones converge
   this way, not just the terminal two: gate 3 and gate 2's unorderable fallback both park
   `ben:needs-review` and both owe its comment.

   1. Tracker state **terminal**, or **open with a `closed` event inside this claim cycle** →
      dispose workspace (§6.4), release — our own assignment only, as everywhere. Merged,
      closed, or abandoned while we were down; and closed-then-reopened while we were down,
      which is the same fact with the state moved back. Step 2 has already read the history, so
      the second reading costs nothing — and because this gate reads the log unconditionally, it
      is also the backstop for the one close-and-reopen a conditional poll cannot see (§8.3).

      The reopened case MUST resolve **here, on the event**, and MUST NOT fall through to the
      projection table. `FindPR` returns open PRs only (§9.7), so a *merged* PR leaves publish
      evidence incomplete, and the table would read that as a contradiction and park
      `ben:needs-review` with the claim retained — on an issue whose reopen is the evidence
      against it. This is §9.8's sweep rule stated at startup, so which of the two observes the
      close cannot change the verdict.
   2. Assignees ≠ `{principal}` → contested. Run **§8.4 arbitration** over the standing
      assignments. Another party assigned first → release **only our own** and stop. Ours
      first → retain it and continue classifying; the co-assignee is either a crashed loser
      that will release itself or a human. Unorderable → fail closed as gate 3 does.

      Where the candidate then classifies as active work, §9.8 reconciles the human away on a
      later tick. Where it classifies as `done`, recovery adopts a **held-claim record**, and
      §9.8's held-claim sweep releases our assignment on the close — within one poll interval,
      and without a restart. The co-assignee's own assignment is never ours to remove, at
      `done` or anywhere else. Until the close the issue is not dispatchable by anyone while
      anyone holds it (§8.3), the PR is already up, and merge closes the issue via `Fixes #N`.
      It is the same path by which every `done` claim is released (§9.2), not a special case.

      Releasing unconditionally here would be a **mutual-release bug**: two daemons recovering
      the same published issue — reachable when one crashed after assigning but before its
      read-back released it — would both let go, leaving the issue unassigned and unlabelled,
      which is to say dispatchable, and the published work would be redone. Arbitration is
      what makes exactly one of them release.
   3. **No standing `assigned` event** naming the principal, though the issue is assigned to
      it (event retention, a transfer) → the log cannot account for our own claim. Fail
      closed: retain the claim, park `ben:needs-review`, raise a loud operator error. Refuse
      to guess, exactly as §8.4 does with an unorderable race.
   4. Active, but **outside the workflow's label partition** (required labels removed while
      down) → release, **keep** the workspace. Mirrors §9.8's unroutable rule.

      If the labels later return and the issue is claimed anew, that claim is fresh for the run
      budgets, but §9.6 derives its attempt floor after the kept workspace and branch are
      reattached. Recovery neither persists nor infers an exact prior counter.

   After the tracker-only gates have resolved terminal, contested, unaccountable and
   out-of-partition claims, recovery reads the provider's claim-base state and the run marker. It
   MUST NOT ask §9.7 until the state is `pinned` for exactly the current claim epoch.

| Claim-base state | Run marker / history | Recovery |
|---|---|---|
| `pinned` for the current epoch, with a target | ordinary marker states | Use that base and target; apply the existing workspace precondition and ordinary projection classification. |
| `pending` for the current epoch | marker absent and no current-cycle `ben:running` evidence | Ask no publish-evidence question. Resume the claim-aware first-`Prepare` path; re-run §9.5 first where the approved content pin did not survive. The pending→pinned transition precedes hooks and launch. |
| `pending` for the current epoch | any marker entry, or current-cycle `ben:running` evidence | Park epoch-faulted. Under the specified ordering a run cannot have begun while the epoch was pending, so this is torn or legacy state, not a state from which to infer success. |
| absent, unreadable, targetless legacy, multiply represented, or for another epoch | any | Park epoch-faulted. Do not verify against an older base and do not mint a historical base or target from the branch or repository default now present. |

   Run-marker absence says only “the workspace is free now”; it is not evidence that no earlier run
   occurred and cannot repair a missing or mismatched epoch. Conversely, the epoch check does not
   weaken the run-marker precondition: a matching pinned epoch whose run may still be live still
   waits, and an unknown launch still parks under the existing rule.

   Deploy the epoch-and-target-aware version only while `ClaimedByPrincipal` is empty — no running,
   parked or held claim for that principal. A pre-epoch assignment has no trustworthy epoch/base
   pair, and a targetless pre-amendment record has no trustworthy historical target; either is
   deliberately parked. A targetless local claim-base, daemon-side mirror claim, or remote workspace
   cycle is readable only as named non-authorizing legacy state for the same epoch. Historical bases
   and remote cycle identity may be carried only as outgoing facts of a **new assignment created
   after the upgrade**, which resolves and atomically writes a complete target-bearing record before
   hooks or launch. Today's configuration or repository default is never retrofitted onto a claim
   already standing.

   Then, on the effective projection:

| Effective projection | Evidence | Verdict |
|---|---|---|
| `ben:failed` | `failed` releases the claim (§9.2), so a standing assignment means the release never landed | Complete the projection, post the `failed` milestone comment with the reason resolved per step 6, release. The label stands — it blocks re-dispatch until a human clears it |
| `ben:needs-review` | — | Complete the projection, re-issue the `needs-review` comment, adopt as parked; §9.8 handles unparking |
| `ben:claimed` / `ben:running` | §9.7 publish evidence complete | Finish the interrupted `done`: clear every `ben:*`, post the publish comment, dispose, retain claim, adopt a held-claim record (§9.8) |
| `ben:claimed` / `ben:running` | §9.7 evidence incomplete | **Orphan**: complete the projection, re-issue the `claimed` comment, re-enter `backoff` at `attempt ≥ 2`, workspace and branch reattached (never `-B`) |
| none standing | No `ben:*` `labeled` event in this claim cycle | **Unapproved claim** — killed between assignment and label projection, which is the window §9.5's content check occupies. Adopt at `attempt 1` as a claim that has not been approved: project **nothing** and post no comment, and run the §9.5 check as a fresh claim does — it either transitions to `claimed`, which owes the label and the milestone by the ordinary path, or parks `ben:needs-review` for reapproval. Label projection precedes `preparing` (§9.2, §9.3), so no attempt can have run |
| none standing | The cycle's last `ben:*` transition removed **`ben:needs-review`** | **Human re-queue**, not `done` (§9.2): re-enter `backoff` at `attempt ≥ 2` |
| none standing | The cycle's last `ben:*` transition removed **`ben:failed`** | A human cleared the failure but the release never landed: release. The issue returns to the queue unclaimed |
| none standing | The cycle's last transition removed **`ben:claimed`/`ben:running`**, and §9.7 evidence is **complete** — all three legs | **Published-awaiting-review** (`done`): re-issue the publish milestone comment (idempotent, §8.4), retain the claim, adopt a held-claim record (§9.8) |
| none standing | The cycle's last transition removed **`ben:claimed`/`ben:running`**, and §9.7 evidence is **anything less** — no evidence at all, or partial: a remote branch with no open PR, a PR closed unmerged, a branch that does not descend from base | Contradiction → `needs-review`, workspace kept. Fail closed; silence is not a verdict |

   Nothing reaching this table has a `closed` event in its claim cycle: gate 1 has already
   released those, on the event rather than on publish evidence. That is what keeps the release
   rule in one place — startup and the §9.8 sweep read the same fact and cannot drift apart.

5. Sweep terminal-state workspaces (§6.4), subject to the workspace precondition: one whose
   run may still be live is left in place and swept once that run is confirmed gone.
6. **Two facts are local-only and may not survive**; both are documented degradations, never
   inferred:
   - **Continuation tokens** are gone on a fresh host: recovered sessions start fresh.
   - **The §7.3 failure reason** lives in the run record. Neither the `ben:failed` label nor
     `ClaimHistory` carries it, so a `failed` comment reconstructed by recovery cannot always
     name it. Take it from the local transition log (§9.11) when present — the ordinary
     same-host restart — and when it is absent, the comment MUST say the reason did not
     survive the restart. It MUST NOT be invented, and the comment MUST NOT be skipped: a
     `ben:failed` label with no explanation is worse than an honest one.

*Design note — why the removed label's identity is load-bearing.* Publish evidence alone
cannot separate a published issue from a freshly-killed claim: a PR left by an earlier
`needs-review` cycle would make the second look like the first. But "a `ben:*` label was added
and later removed" is no better, and fails in the same direction. **Re-queueing a
`needs-review` issue retains the assignment and removes only the label** (§9.2), so it opens
no new claim cycle and produces exactly that shape — with a stale PR standing by to
corroborate it. Reading such an issue as `done` would silently discard the human's re-queue.
*Which* label was removed is what actually distinguishes the cases: `done` clears
`ben:claimed`/`ben:running`, a re-queue clears `ben:needs-review`, and only the daemon ever
removes the former. Git facts then corroborate or contradict the reading; they never
substitute for it.

No durable orchestrator database exists, by design.

### 9.11 Transition log and state files

Every transition appends `{ts, issue, from, to, actor, reason}` to an append-only white-box
state file under the state dir (§10.3). `ben status` renders it; milestone comments
(§8.4) are its tracker-visible projection at load-bearing transitions.

## 10. Deployment and team use

### 10.1 Topology

The canonical team shape is **one shared daemon per repo × workflow queue**: teammates
enqueue by labeling, watch state labels and the assignee, and gate at PR review; nobody
touches the daemon. **Per-developer daemons are a documented on-ramp**: each dev runs one
with their own PAT and agent keys; claims ride dev identity, and assigned-to-other exclusion
makes everyone's daemons mutually back off — pure composition. (Documented caveats: sleeping
laptops orphan claims; no queue SLO.)

**Unique claim principal per daemon instance is normative.** Same-queue multi-daemon
(throughput scale-out) is tolerated — claim mechanics make races safe-but-wasteful — but
lease-based failover/autoscale is deferred to the hosted tier (§13).

**Isolation substrate (normative for unattended operation).** When no human is present, a daemon
MUST run in exactly one of the two unattended modes below — `protected` or `risk-accepted` — chosen
deliberately. The per-developer on-ramp above is exempt only while a human is present, declared as
`attended` (§5.2.9); that exemption ends the moment they are not.

Every workflow MUST declare `deployment.mode`. `protected` and `risk-accepted` assert the
corresponding unattended mode below. `attended` asserts that a human is present for the entire
lifetime of this process; BEN cannot detect the human leaving, and the assertion ceases to be true
the moment they do. Changing the declaration requires a restart.

BEN verifies none of these deployment properties. It refuses omission, records the declared mode at
startup, logs `risk-accepted` with its `accepted_because` value, and logs `attended` at Warn on
every startup.

**Protected mode.** All three MUST hold:

1. **A dedicated OS account.** Its `$HOME` holds no credentials beyond that daemon's own, and
   the daemon MUST NOT run as root — Claude Code refuses its headless permission mode under
   root outside a recognized sandbox.
2. **The run cannot reach the daemon.** An unattended run MUST be unable to read or use the
   daemon's tracker credential, or to inspect or control the daemon process. The account alone
   does not achieve this, because by default the agent shares the daemon's process identity
   (§6.7). This is a property, not a mechanism: a separate run identity, a whole-process
   sandbox, and a container can each establish it, this spec endorses none of them, and a
   deployment MUST verify that its chosen mechanism achieves it on its own platform rather than
   assume it. The v1 adapters' per-command sandboxing does not establish it on its own — a
   boundary around the agent's subprocesses is not a boundary around the agent.
3. **The forge enforces review.** Every branch selectable by `workspace.base_branch` — including
   the repository default selected by omission — MUST require review before merge, and the
   agent's publish identity MUST NOT be able to push to it directly, to **satisfy** that
   requirement itself, or to **bypass** it. Without this, §3.4's "the human gate is PR review" is
   a convention rather than a control: on a forge where the credential that pushes a branch also
   suffices to merge it, an agent that publishes can approve its own work by merging it.

   The agent **MAY complete the merge** once every requirement is satisfied, and completing it
   is the intended shape: §3.4 gates whether a human approved, not who pressed the button
   afterwards. Three properties hold that distinction open, and none follows from requiring a
   review:

   - **The approval MUST bind to the commit that merges.** A review that survives a subsequent
     push lets the agent take an approval of one tree and merge a different one — the same
     approve-then-substitute defect §9.5 closes for issue content, one layer down. Where the
     agent may perform the merge, this is the entire distance between *a human approved this
     tree* and *this tree landed*.
   - **The required approval MUST be one the publish identity cannot supply.** That a forge
     forbids an author approving their own pull request does **not** achieve this: it constrains
     the agent only on the pull requests the agent opened. The publish identity holds
     pull-request write — it needs that to open a pull request at all — so it can submit an
     approving review on one opened by a human or by a second agent, where an approval from a
     principal with write access counts and the first property is satisfied too, the agent not
     having been the pusher. Nothing human-reviewed anything, and the merge is legitimate on
     every rule stated so far. The requirement is therefore over the **approval** and not over
     the author: a rule whose approvers are enumerated humans — code-owner review over an owners
     file covering every path, and no agent among the owners — demands an approval no agent
     credential can produce.
   - **The publish identity MUST NOT hold a capability that exempts it.** Administrators, and
     any role carrying a bypass-branch-protections permission, sit outside these rules by
     default, and an agent wearing one merges whatever it likes. This is a **capability** to
     check rather than a role name to compare: "not an administrator" is the common shorthand
     and it is insufficient, because a custom role can carry the bypass permission without
     carrying the administrator role.

   All three are enforceable only once the agent publishes as an identity distinct from the
   humans who review (§10.2), because forge rules name *principals*, not tokens.

**Risk-accepted mode.** A deployment MAY relax requirement 2, *the run cannot reach the daemon*,
and nothing else — running unattended without that boundary, as an explicit, recorded choice, and
only understood as one in which **the agent is trusted with the daemon's tracker authority**: the
dispatch label becomes routing rather than a security boundary, since a subverted run can apply
it itself. The dedicated account (1) and the enforced review (3) continue to hold, and the
enforced review becomes load-bearing — with the label demoted, PR review is the only remaining
gate (§3.4). Deployments MUST NOT arrive in this mode by default or by omission.

*Design note.* Two further controls were considered for this section and deliberately left out.
**Egress restriction** is not required: a domain allowlist enforced from the client-supplied
hostname, without terminating TLS, is defence in depth rather than a boundary — a broad allow
such as `github.com` remains an exfiltration path — and a MUST the spec must immediately
disclaim is worse than a RECOMMENDED one. **"The agent cannot reach a stored harness
credential"** is not achievable while the harness authenticates from `$HOME` (§7.6): a run that
can authenticate can read what it authenticates with. The requirement is therefore scoped to
the *daemon's* credentials, which the agent has no need of at all.

### 10.2 Credentials

Exactly **two**, operator-owned, no vault integration:

1. **Tracker credential** — a machine-user PAT, or a credential obtained by exchanging workload
   identity through a configured issuer (§5.2.10), authorizing writes that name the machine-user
   assignee of §8.4. It also authenticates the base-clone git fetch. Minimum scope: read/write the
   workflow repository's **issues** — assignment, labels, comments — plus repository read for the
   base clone. It MUST NOT carry contents write or pull-request write.

   **Where a workload-identity source is configured, BEN stores no long-lived GitHub credential
   and implements no vault integration**: a token exists only in the daemon's memory and is not
   used past the deadline its source stated. A legacy literal credential is resolved at load as
   before and is explicitly unbounded. This applies to the tracker, base-fetch and publish
   credentials; the harness's own API keys remain `agent.provider` parameters (§5.2.5).
2. **Agent credentials** — the harness's own API keys, which live in `agent.provider` (§5.2.5),
   plus the **publish credential**, which has a top-level block of its own (§5.2.8) rather than a
   key in that block: it is the operator's identity choice, not an adapter's parameter. The
   publish credential's minimum scope: **contents write and pull-request write**, and it MUST
   NOT carry issues write.

**The two MUST NOT be the same credential.** That is an operator obligation, and it is not
machine-verifiable: BEN compares *credential identities*, so the same PAT exported under two names
satisfies every check it can make.

What BEN does enforce is narrower and stated as such: **a workflow in which the tracker
credential's identity can reach an agent process is a load refusal** (§5.3, §5.7), not a warning.
The comparison is over the credential **identities** a configuration references — a variable name,
a source authority, or, **only where no variable was ever referenced**, a config site — never the
credentials they resolve to. It is over the identity a value came *from* rather than the name the
child receives it under. So the refusal catches the configuration that shares an identity and
cannot catch the deployment that duplicates a secret across two — the residual is named here rather
than left for an operator to infer from a passing load.

Stated as one rule: **two credentials with equal authority are one credential however they are
spelled, and the tracker's and the publisher's authorities being equal is a load refusal.** That
subsumes every combination — the same source named twice, two `octo_sts` sources sharing
`(url, scope, identity)`, two `static` sources naming one `$VAR`, and legacy↔named where a `static`
source and the tracker's `$GITHUB_TOKEN` fallback both reduce to `env:GITHUB_TOKEN`.

Authorities are namespaced so a variable cannot collide with a site. An `octo_sts` authority
includes canonical URL, configured scope and identity. A shared OIDC **token path** is **not** part
of source identity: one projected service-account token federating two trust-policy identities is
the intended deployment, and refusing it would refuse the topology this exists to enable.

**Authority is read from loader provenance, not from the config site.** `tracker.provider.token` is
resolved by the loader to a literal, but `$FOO` survives in provenance, so a `$FOO`-referenced
tracker token has authority `env:FOO` and still collides with a `static` source over `$FOO`.
Attributing it to its config site instead would silently regress this check. A token written as a
true file literal has authority `site:tracker.provider.token`; an omitted token has authority
`env:GITHUB_TOKEN`, stated explicitly, because an undeclared fallback is what made #47's collision
invisible.

The scopes are complements, not nested, which is the argument for two credentials rather than one
narrowed to fit: §9.3's label projection and §8.4's assignment need issue write, while §6.7's
publish needs contents and pull-request write. A single credential spanning both is exactly the
capability a subverted run needs to rewrite the queue that dispatched it — strip `ben:*`, take the
assignment, close the issue, claim more work. Scope is the operator's to grant and BEN cannot
verify it; what BEN enforces is that one *credential identity* is not doing both jobs.

Obtained from a credential source at the moment each is needed (§5.2.10) and injected per-run
(§7.6); the legacy `$VAR` spellings (§5.5) compile into implicit unbounded sources and behave
exactly as before. Team sharing is an ops pattern (org-owned machine user, secret manager or
workload-identity issuer feeding the daemon), not spec machinery.

*Design note — what "can reach an agent process" covers.* Every value in an `agent.provider`
block reaches the child, because turning that block into a child process is the adapter's whole
job: values become argv (`--model`), child environment entries, or files the child reads. So the
rule is over the **whole block**, not over the keys an adapter calls credentials — `model:
$TRACKER_PAT` is the same leak as `env: { GH_TOKEN: $TRACKER_PAT }`, and any enumeration of
"credential keys" is one key behind the next one somebody adds. Two routes are additionally
invisible to the block: §7.6's daemon-environment allowlist is copied into every child
unconditionally, so a tracker credential drawn from `PATH`, `HOME` or `TERM` is in the child
whatever the block says; and an `env_passthrough` entry is a variable *name* rather than a value,
which is the one thing the loader cannot see for itself and an adapter therefore MUST declare.

A third site joins the block, the allowlist and `env_passthrough`: `publish.value` (§5.2.8) names a
variable whose secret is injected into the child *by construction*, so the tracker credential's
variable appearing there is the same refusal as anywhere else. It is compared as a reference like
the rest — the loader never resolves it and does not need to, because the comparison was always
over variable identities rather than over secrets (§5.5).

*Design note — why the check is over names.* §5.5 makes a `$VAR` reference the thing the file
actually says, so the variable is the credential's identity and the resolved bytes are not.
Comparing values would mean holding two secrets side by side to compare them, would risk echoing
one in the refusal, and would false-positive two genuinely distinct credentials that happen to be
equal during a rotation. Comparing the *child's* variable name instead of the source would miss
the rename — `env: { GH_TOKEN: $TRACKER_PAT }` is the same leak as forwarding `TRACKER_PAT` by
name, and it is the spelling an operator reaches first when the harness wants one name and their
secret manager exports another. A value interpolating several variables carries a secret from each
of them, so provenance MUST record every one; and a documented fallback an adapter reads when its
provider field is **omitted** MUST be declared by that adapter, or a configuration naming no
variable at all cannot be checked — which is how BEN's own dogfood workflow held one PAT for both
jobs from B04 until #47.

### 10.3 Status surface and observability

- **GitHub is the dashboard**: labels, milestone comments, PR links — durable, team-visible.
- **`ben status`** renders local white-box state files: current runs, attempts, recent
  failures, next backoff timers, the transition log tail.
- **No HTTP listener in v1** (deferred with its trigger, §13).
- Logging: `slog` JSON to stdout with per-run correlation attrs (issue identifier, run id,
  session id); the supervisor (systemd/journald) owns sinks and rotation.
- State dir: `$XDG_STATE_HOME/ben/<workflow_key>/` (`~/.local/state/...`) — run records,
  transition log, per-run raw agent transcripts, the canonical rendered prompt retained beside
  each transcript (§9.5), continuation tokens. The retained prompt carries the untrusted issue
  body verbatim (§5.6), so it is written at the transcript's own permissions and is subject to
  the same redaction.

## 11. CLI

| Command | Behavior |
|---|---|
| `ben run [path]` | Run the daemon for the workflow at `path` (default `./WORKFLOW.md`). Selects adapters by `tracker.kind`/`agent.kind` from the kind registry, then `Structural` → `New` → `Ready`; after the offline base-cache identity check, workspace readiness resolves the configured branch or current repository default with the tracker credential and refuses an absent branch before bundle publication or launch. The same applies on every workspace rebuild. Refuses to start on any readiness failure (§5.7). `signal.NotifyContext` graceful shutdown, per §9.8: stop dispatch, interrupt in-flight runs, wait for confirmed termination wherever a handle exists, and complete the effects already ordered, including bounded confirmation of a failed held release ordered before the drain. |
| `ben status [--json]` | Render the white-box state files (§10.3). Read-only; works while the daemon runs. |
| `ben config effective [--json]` | §5.8. |

**Assembly constructs one instance per named credential source** (§5.2.10), hands the tracker and
workspace the **same** tracker-source instance, and gives the publisher a **distinct** instance
narrowed to the fresh-only surface — the publisher has no cached path, because a token handed to an
agent must cover the whole attempt (§7.7). Each instance owns its configured exchange scope;
assembly passes **no repository-derived credential scope** to a consumer or runner. A source kind's
`New` returns the full surface and consumers receive narrowed views, so narrowing is assembly's
decision and not a kind's to get wrong.

Single static binary; systemd (or equivalent) is the supervisor.

## 12. Testing and verification strategy

### 12.1 Unit kernel

Table-driven tests over the pure core: config strictness matrix (§5.3, §5.7), template AST
validation (§5.6), the **transition map** (every listed transition legal; everything else
errors), policy functions (dispatch eligibility, backoff formula — deterministic jitter makes
it exactly assertable), workspace-key sanitization/hashing, retryable verdicts.

### 12.2 Contract tests with fakes

The closed interfaces make fakes cheap — a deliberate payoff. A **FakeRunner** (scripted
event sequences: happy path, crash-without-terminal-event, stall, budget breach), a
**FakeTracker** (in-memory issues, claim-race simulation, 304 responses), and a
**FakeWorkspaceProvider** drive the orchestrator through every scenario without network or
subprocesses. Each real adapter additionally gets adapter-level tests against recorded
fixtures of its harness/API.

### 12.3 Invariant scenarios (integration)

The design invariants (§3) as executable end-to-end scenarios against the fakes:

1. Kill −9 the daemon at any point; restart; no duplicate dispatch, no lost claim (§9.10).
   "Any point" includes every boundary between the writes of a multi-write projection —
   specifically the windows after assignment and before label projection, mid-`done`
   (labels cleared, publish comment not yet posted), and mid-`failed` (label set, claim not
   yet released). Every candidate is classified from positive evidence. **Convergence is
   conditional on the previous tenure's run ending.** Where a run may still be live the restart
   classifies the candidate as *waiting* — claim and workspace retained, no second agent
   dispatched (§12.3-5) — and it converges once positive quiet evidence arrives. A run that
   never ends is never converged: it stays visibly waiting, counted as neither outcome, rather
   than being resolved by a timeout that would be a guess. The one window admitting no answer
   at all — a marker whose launch outcome is unknown — parks for a human instead.
2. Race two daemons for one issue; **exactly one wins** by assignment order from the
   tracker's event log; the losers release, and each releases only its own assignment, never
   another party's (§8.4). Where the log cannot order the contenders, **every claimant
   yields** — no winner, no double dispatch — and the race re-runs. At most one daemon ever
   dispatches, in both branches. No human assignment is ever removed.
3. Agent claims success with no commits → `needs-review`, never `done` (§9.7).
4. Retry after failure preserves agent commits (never `-B`) (§6.2).
5. Unconfirmed stop retains the claim; no workspace is ever shared (§9.8).
6. Invalid hot-reload blocks dispatch, spares in-flight runs (§5.4).
7. Kill from every non-terminal state lands in `failed(killed)` (§9.2).
8. Secrets never appear in argv or the child env beyond the allowlist (§7.6).
9. Orchestrator never closes an issue, never writes unstructured prose (§8.1).
10. Budget breach stops the run and parks it (§9.9).
11. A published issue's retained claim is released **without a restart** once the issue closes,
    and so is a close-and-reopen inside one poll interval **where the reopen moves the issue's
    `revision`** — the `closed` event is the evidence, not current state (§9.2, §9.8). The two
    restart-coupled cases §9.2 names are asserted as such, not as releases: a PR closed
    unmerged, and a reopen the revision projection cannot express, each released by §9.10
    gate 1 at the next start. The sweep spends one conditional read per tick however many
    claims are held.
12. Reclaim one issue after an earlier claim published its branch and PR. The new assignment epoch
    pins that prior head before hooks; a no-op attempt cannot reach `done`, while a new descendant
    commit can. Kill at every boundary around pending creation and the atomic pending→pinned
    transition: recovery either resumes before launch, waits on the existing run-marker precondition,
    or parks epoch-faulted; no case verifies against the prior epoch's base. Retries and a human
    unpark within one assignment retain the base, and an unassignment/reassignment remints it.

### 12.4 Real-integration smoke profile (RECOMMENDED, not CI-required)

A canary repo + a real harness (`claude-code`) running one scripted issue end-to-end through
claim → worktree → publish → verify. Manual or nightly; validates the two real adapters
against harness drift.

**No third-party conformance profile.** This spec governs one implementation; RFC-2119
language disciplines *it* and its tests, not external conformers. If the hosted tier ever
demands multiple implementations, a conformance profile is a fresh effort.

## 13. Deferred extensions (named, with triggers)

Each is deliberately out of v1 but has a reserved seam and an activation trigger:

| Extension | Seam already in place | Trigger |
|---|---|---|
| GitHub App claim profile (claim-label convention, 12,500 req/hr/installation) | §8.4 documented path | Team scale: PAT seat cost or rate limits bite |
| Claim leases (TTL/renewal/contested takeover) | Unique-principal claims (§10.1) | Hosted multi-instance failover/autoscale |
| HTTP API + web dashboard | White-box state files (§10.3) | Operating a fleet (hosted) |
| Webhook ingestion | Same normalized create-task path (§8.5) | Poll latency matters |
| Container workspace strategy, curated images | `WorkspaceProvider` (§6.1) | Hosted tier; untrusted repos; a second labeling principal, whose approval a risk-accepted deployment would let a subverted run replay (§10.1) |
| Multi-workflow daemon | Single-`WorkflowDefinition` core (§3.7) | Ops burden of N daemons |
| Codex app-server adapter | Opaque continuation token (§7.1) | Live-thread continuation / latency |
| Generic `command` runner adapter (payload-on-stdin contract) | Runner interface (§7.1) | A harness beyond the two (consciously not soon) |
| `ben:urgent` priority label | FIFO dispatch (§9.5) | Demand for queue-jumping |
| PR-state sweep over held claims (release/park on a PR closed unmerged) | Held-claim records + `FindPR` (§9.8, §8.2) | Rejected-PR claims stalling a queue between restarts (§9.2) |
| Plan-review gate (agent posts plan comment, waits for label) | Prompt + labels only — no core machinery | Teams wanting approval before code |
| Team-vs-personal secrets; org budget/quota; log queryability | — | Hosted tier |

## Appendix A. Go implementation notes (non-normative defaults, normative where marked)

- Stdlib plus four small deps: `fsnotify` (v1.10+), `gopkg.in/yaml.v3`
  (`KnownFields(true)` for §5.3), `google/go-github` (REST, pinned major),
  `osteele/liquid` (or equivalent — **risk flag**: verify the AST walk for §5.6 strictness is
  achievable via configuration or wrapping; a render-time strict check is the fallback
  backstop).
- Front matter: hand-split on `---`, then YAML decode.
- Subprocesses: `os/exec` + one `bufio.Scanner` per stream; the §7.5 process-group and
  ordering rules are **normative**.
- Lifecycle: `signal.NotifyContext` + `errgroup.WithContext`; single static binary; systemd
  as supervisor.
- GraphQL (`shurcooL/githubv4`) only if poll cost ever demands it.
