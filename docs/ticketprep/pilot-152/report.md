# Ticket preflight — wrapper-owned report

> **ADVISORY ONLY:** this packet grants no approval, does not authorize `ben-queue`, and does not establish implementation readiness.

> **FRESHNESS:** badges compare this packet only with the supplied comparison capture; the offline kernel did not query the forge.

## Wrapper-established facts

- packet digest: <code>sha256:772099818aff9bd25682abe88cbc6852c8cb828b650c91fc4019b7dd7b6974be</code>
- repository: <code>github.com/srhg-ai-7cef3f93/ben</code>
- commit: <code>87d01e949fcab4053df1ed34c74562bc22d340e7</code>
- tree: <code>3a1ecd8ba562a41a386ee1eede284010fc9072fd</code>
- issue: <code>&#35;152 https://github.com/srhg-ai-7cef3f93/ben/issues/152</code>
- title: <code>Configurable per-workflow base/target branch &#40;workspace cut from and PR opened against something other than the repo default branch&#41;</code>
- content digest: <code>sha256:573840a53d765816267c611bb78f50595f4498fe22387d58699748c8718e55ca</code>

<details>
<summary>Captured issue body — declared snapshot, safely escaped</summary>

<pre><code>&#42;&#42;Spec:&#42;&#42; §5.2.4/§6.2 &#40;workspace base derivation&#41;, §5.6 &#40;canonical publish snippet&#41; · &#42;&#42;Depends on:&#42;&#42; —

&#35;&#35; Problem

BEN&#39;s workspace provider has no concept of a base branch other than the repository&#39;s actual
GitHub default branch. &#96;internal/workspace/workspace.go&#96;&#39;s &#96;remoteDefaultBranch&#96; reads the
remote HEAD symref, and a fresh issue&#39;s workspace branch is always cut from that fetched head
&#40;&#96;workspace.go:761,843,868&#96;&#41;. The canonical publish snippet &#40;SPEC §5.6&#41; also hardcodes &quot;open a
pull request against the default branch.&quot;

This surfaced on a real deployment: &#96;NFTYDoor/nftydoor-admin&#96; mirrors a Bitbucket repository —
GitHub &#96;main&#96; must stay a clean, GitHub-authored-commit-free mirror, and a whole stabilization
epic requires every BEN-authored branch to be cut from, and every PR opened against, a
dedicated &#96;switchboard-stabilization&#96; branch instead. Changing the repo&#39;s actual GitHub default
branch is explicitly a non-goal of that epic &#40;it would violate the mirror invariant a different
way&#41;. There is no other way today to make BEN base and publish against a non-default branch —
not a config key, not a documented workaround, and it isn&#39;t named in §13&#39;s deferred extensions
either.

&#35;&#35; Proposal &#40;for discussion, not a locked design&#41;

Add an optional &#96;workspace.base&#95;branch&#96; &#40;or similar&#41; config key: when set, &#96;Prepare&#96; fetches and
bases fresh workspaces on that ref instead of the repository&#39;s default branch, and the canonical
publish snippet&#39;s PR-target instruction uses it instead of &quot;the default branch.&quot; Needs a decision
on:

- whether the base ref is validated to exist at &#96;Ready&#96;-time &#40;readiness failure&#41; or load time;
- interaction with the remote-first reattach logic &#40;§6.2&#41; — reattaching to an existing
  &#96;ben/&lt;workspace&#95;key&gt;&#96; branch should presumably still take precedence over the configured base
  for a fresh prepare;
- whether this belongs in &#96;workspace&#96; or is closer to a &#96;publish&#96;-block concern, since it affects
  both the git base and the PR target.

&#35;&#35; Acceptance

- &#91; &#93; A workflow can configure a non-default base/target branch and have BEN&#39;s workspace
      provider and canonical publish snippet both honor it.
- &#91; &#93; &#96;ben config effective&#96; surfaces the configured value &#40;or its default-branch fallback&#41;
      with provenance, per §5.8.
- &#91; &#93; Structural validation rejects an obviously malformed ref name at load; existence is a
      readiness concern, not a load concern &#40;mirrors how &#96;publish.value&#96; is handled, §5.5&#41;.
- &#91; &#93; &#96;make check&#96; green &#40;evidence in the PR&#41;</code></pre>

</details>

### Literal repository observations

- path: <code>internal/workspace/workspace.go =&gt; exists at internal/workspace/workspace.go &#40;blob 745fca9e1580e08a0ff5d6204a52bc2c101e9e5a&#41;</code>
- path: <code>workspace.go:761,843,868 =&gt; unknown: basename is ambiguous across 4 committed paths</code>
- Go symbol: <code>Prepare =&gt; unknown: identifier is ambiguous across 3 committed declarations</code>
- Go symbol: <code>Ready =&gt; unknown: identifier is ambiguous across 8 committed declarations</code>
- Go symbol: <code>remoteDefaultBranch =&gt; exists at internal/workspace/workspace.go:791 &#40;blob 745fca9e1580e08a0ff5d6204a52bc2c101e9e5a&#41;</code>
- unknown: <code>NFTYDoor/nftydoor-admin: literal is not an unambiguous v0 path or Go symbol reference</code>
- unknown: <code>ben/&lt;workspace&#95;key&gt;: literal is not an unambiguous v0 path or Go symbol reference</code>
- unknown: <code>publish.value: literal is not an unambiguous v0 path or Go symbol reference</code>
- unknown: <code>workspace.base&#95;branch: literal is not an unambiguous v0 path or Go symbol reference</code>

### Applicable instructions and declared validation

- instructions: <code>AGENTS.md &#40;blob 2f64b8cdca8653fce3a6abb5bb87936d92746d4a&#41;</code>
- validation command: <code>make check &#91;AGENTS.md:55; blob 2f64b8cdca8653fce3a6abb5bb87936d92746d4a&#93;</code>
- validation command: <code>make test &#91;AGENTS.md:57; blob 2f64b8cdca8653fce3a6abb5bb87936d92746d4a&#93;</code>
- validation command: <code>make lint &#91;AGENTS.md:58; blob 2f64b8cdca8653fce3a6abb5bb87936d92746d4a&#93;</code>
- validation command: <code>go run ./cmd/ben config effective WORKFLOW.md &#91;AGENTS.md:63; blob 2f64b8cdca8653fce3a6abb5bb87936d92746d4a&#93;</code>
- validation command: <code>make race &#91;Makefile:53; blob 519cb037b749c9eaac7a70be6c00405c22b014ef&#93;</code>
- validation command: <code>make fmt-check &#91;Makefile:70; blob 519cb037b749c9eaac7a70be6c00405c22b014ef&#93;</code>
- validation command: <code>make vet &#91;Makefile:76; blob 519cb037b749c9eaac7a70be6c00405c22b014ef&#93;</code>
- validation command: <code>make workflow-check &#91;Makefile:87; blob 519cb037b749c9eaac7a70be6c00405c22b014ef&#93;</code>
- validation command: <code>make worktree-check &#91;Makefile:111; blob 519cb037b749c9eaac7a70be6c00405c22b014ef&#93;</code>

## Declared invocation provenance — not verified

- provider: <code>OpenAI</code>
- model: <code>unknown</code>
- command: <code>$prep-ticket &#40;Codex; revised after four human pilot reviews&#41;</code>
- prompt: <code>repository-local one-shot review of issue &#35;152 plus four rounds of human pilot feedback</code>

## Agent-authored advisory

> **REVIEW ITEMS:** only `DEC-*`, `SPLIT-*`, and `REC-01` require a disposition. An accepted or already-present human decision must select one wrapper-owned `DEC-*-OPT-*` ID. Other IDs are stable supporting references, not separate approval chores.

### Restated outcome — MATCHES SUPPLIED CAPTURE (subject)

- **OUT-01** supporting agent text: <code>Let one workflow&#39;s single configured branch select both the source for a fresh workspace and the required pull-request target, with an explicit default fallback and preserved target semantics across retries and restarts.</code>

### Candidate non goals — MATCHES SUPPLIED CAPTURE (subject)

- **NGO-01** supporting agent text: <code>Do not change the forge repository&#39;s configured default branch.</code>
- **NGO-02** supporting agent text: <code>Do not rebase or recreate an existing ben/&lt;workspace&#95;key&gt; branch solely because the configured or default target moves.</code>
- **NGO-03** supporting agent text: <code>Do not add a per-issue branch override or let issue-authored content select the base.</code>
- **NGO-04** supporting agent text: <code>Do not move publishing from the agent to the daemon.</code>

### Assumptions to confirm — MATCHES SUPPLIED CAPTURE (subject)

- **ASM-01** supporting agent text: <code>The configured value names a branch under origin rather than an arbitrary ref, tag, or commit.</code>

### Decision queue — MATCHES SUPPLIED CAPTURE (subject)

- **DEC-01** [accepted; selected DEC-01-OPT-01] agent text: <code>Which public config path, exact branch grammar, effective representation, and readiness seam define the issue&#39;s single base/target branch? &#91;kind: human&#95;decision; changes decision: the public configuration, load-time grammar, effective output, and startup-readiness contracts&#93;</code>
  - **DEC-01-OPT-01** [agent-recommended, selected] agent option: <code>Use optional workspace.base&#95;branch containing a 1-255-byte valid UTF-8 branch name, unchanged and untrimmed. It may contain slashes, must not start with -, refs/, or origin/, and refs/heads/&lt;value&gt; must pass git check-ref-format. A written value renders unchanged in effective text/JSON with file provenance; omission renders &lt;repository-default&gt;/null with default provenance. Add &#40;&#42;workspace.Provider&#41;.Ready&#40;context.Context&#41; error after CheckBaseCache on initial build and every workspace rebuild; its credentialed lookup returns workspace.ErrBaseBranchNotFound when the explicit branch, or current HEAD branch used for omission, is absent. buildWorkspace wraps cmd/ben.ErrNotReady before bundle publication or agent launch.</code>
  - **DEC-01-OPT-02** agent option: <code>Use workspace.base&#95;branch but require a fully qualified refs/heads/&lt;name&gt; that passes git check-ref-format. Preserve it verbatim in effective output, strip the prefix for forge comparisons, and use the same Ready, ErrBaseBranchNotFound, and ErrNotReady routing.</code>
  - **DEC-01-OPT-03** agent option: <code>Use optional top-level target&#95;branch with option 1&#39;s unqualified grammar, effective representation, readiness seam, and refusal behavior, even though the value also controls workspace ancestry.</code>
- **DEC-02** [accepted; selected DEC-02-OPT-01] agent text: <code>Is pull-request target compliance prompt guidance only, or a publication-evidence invariant; which trusted template binding and local PR fact carry it? &#91;kind: human&#95;decision; changes decision: the locked SPEC §5.6 guidance contract and the local publication-evidence boundary&#93;</code>
  - **DEC-02-OPT-01** [agent-recommended, selected] agent option: <code>Add a trusted root Liquid string target&#95;branch from core.Workspace.TargetBranch. Add BaseBranch to core.PR, populate it at the tracker boundary, and require exact equality in the local verifier. Prompt text remains guidance; verifier evidence is authoritative. A single wrong-target PR is VerdictContradicted and §9.7 routes it to needs-review; no PR is VerdictIncomplete.</code>
  - **DEC-02-OPT-02** agent option: <code>Add trusted run.target&#95;branch from core.Workspace.TargetBranch with the same BaseBranch evidence, exact comparison, and verdict routing, placing claim-scoped configuration under the existing run object.</code>
  - **DEC-02-OPT-03** agent option: <code>Keep target selection as prompt guidance only and do not extend local pull-request evidence.</code>
- **DEC-03** [accepted; selected DEC-03-OPT-01] agent text: <code>When the local tracker finds multiple open pull requests with the exact issue-branch head, including a correct- and wrong-target pair, what cardinality rule determines verification? &#91;kind: human&#95;decision; changes acceptance&#95;gap: local tracker enumeration, deterministic verification, and parity with the remote substrate&#93;</code>
  - **DEC-03-OPT-01** [agent-recommended, selected] agent option: <code>Align both substrates on ambiguity refusal. Introduce shared core.ErrPRAmbiguous, retaining ErrRemotePRAmbiguous as an alias if needed, and make local FindPR examine every returned page until exhaustion or a second exact-branch open candidate. Zero returns nil, exactly one returns its full facts, and more than one returns ErrPRAmbiguous regardless of update order or target; it never chooses the newest or filters by target. Verification propagates the error and locked §9.7 parks needs-review.</code>
  - **DEC-03-OPT-02** agent option: <code>Give correct-target precedence. Enumerate all exact-head open candidates in the verifier; exactly one matching the retained target wins even when wrong-target candidates exist, more than one correct-target candidate is ErrPRAmbiguous, and only wrong-target candidates produce VerdictContradicted.</code>
  - **DEC-03-OPT-03** agent option: <code>Treat any exact-head wrong-target candidate as VerdictContradicted even when a correct-target PR also exists; multiple all-correct candidates remain ErrPRAmbiguous.</code>
- **DEC-04** [accepted; selected DEC-04-OPT-01] agent text: <code>Must the same single base/target contract apply to Airlock workflows; if so, which daemon-owned per-claim record stores the selected target and which trusted read seam supplies it to remote verification? &#91;kind: human&#95;decision; changes split&#95;boundary: the substrate scope and the remote target-selection boundary&#93;</code>
  - **DEC-04-OPT-01** [agent-recommended, selected] agent option: <code>Define one local-and-Airlock contract. Configure the selector on mirror.Mirror, but on the first RecordClaim for an epoch resolve the explicit branch or then-current repository default and atomically persist TargetBranch with BaseSHA in mirror.claimRecord/core.RemoteClaim before any remote run; same-epoch retries return it unchanged. Add verify.RemoteClaimSource.Claim&#40;context.Context, core.RemoteClaimRef&#41;, implemented by the mirror; RemoteChecker reads that trusted record for run.Claim and RemoteExpectation retains only workflow-scoped Repository. remotews persists the returned target in its cycle/workspace for restore and prompt use, but not as verifier authority. mirror.Ready returns mirror.ErrBaseBranchNotFound and buildRemoteWorkspace wraps ErrNotReady. Airlock wiring may ship as a dependent child; until then a configured non-default branch refuses rather than being ignored.</code>
  - **DEC-04-OPT-02** agent option: <code>Scope the setting to local execution and make every Airlock workflow that writes workspace.base&#95;branch refuse as unsupported.</code>
- **DEC-05** [accepted; selected DEC-05-OPT-01] agent text: <code>At what lifetime is the selected target fixed, and how does the upgrade handle existing local claim-base, mirror claim, and remote cycle records that contain no historical target? &#91;kind: human&#95;decision; changes acceptance&#95;gap: durable target retention, retry/restart behavior, and safe rollout over legacy records&#93;</code>
  - **DEC-05-OPT-01** [agent-recommended, selected] agent option: <code>Resolve once per new assignment epoch before its first attempt and atomically persist TargetBranch with the base pin; retain it across retry, rollback, reload, restart, and default movement. Deploy only when ClaimedByPrincipal is empty for every affected workflow, matching §9.10&#39;s drain. Existing targetless records remain readable only as non-authorizing legacy state: workspace.ErrClaimTargetUnrecorded, mirror.ErrClaimTargetUnrecorded, or remotews.ErrClaimTargetUnrecorded prevents same-epoch prepare, restore, prompt, and verification; recovery parks epoch-faulted and never infers today&#39;s default. Only a later assignment with a new epoch may carry its trusted old base/cycle identity as outgoing state, resolve the then-current target, and atomically replace the record before hooks or a run.</code>
  - **DEC-05-OPT-02** agent option: <code>Require the same empty-principal drain, then refuse startup while any legacy record exists. The operator must archive the old state and retire its retained local or remote workspaces before starting with empty versioned stores; no record is migrated or target inferred.</code>

### Applicable constraints — MATCHES SUPPLIED CAPTURE (subject_and_repository)

- **CON-01** supporting agent text: <code>SPEC.md is locked; adding a config field or trusted template variable requires explicit human sign-off before implementation.</code>
- **CON-02** supporting agent text: <code>SPEC §6.2 already settles remote-first branch preservation and claim-pin immutability; neither is an open decision for this ticket.</code>
- **CON-03** supporting agent text: <code>The workflow loader and Liquid variable set are closed and strict; omission must preserve the current default while malformed written values refuse at load.</code>
- **CON-04** supporting agent text: <code>Issue title/body are untrusted template data, so a base or target branch cannot be derived from issue-authored text.</code>
- **CON-05** supporting agent text: <code>ben config effective must show resolved values and provenance consistently in text and JSON.</code>
- **CON-06** supporting agent text: <code>Base fetches continue to use the workspace repository&#39;s credential source at the moment of the remote Git invocation.</code>
- **CON-07** supporting agent text: <code>No new dependency is permitted without human sign-off.</code>
- **CON-08** supporting agent text: <code>SPEC §9.10 already requires an empty-principal upgrade drain and forbids minting historical claim state from today&#39;s branch; target rollout must preserve that rule.</code>

### Acceptance gaps — MATCHES SUPPLIED CAPTURE (subject_and_repository)

- **GAP-01** supporting agent text: <code>The issue does not yet name the exact config path, branch grammar, normalized effective value, or callable readiness boundary.</code>
- **GAP-02** supporting agent text: <code>The closed template contract exposes no trusted target branch, and core.PR carries no base branch, so prompt guidance cannot prove local target compliance.</code>
- **GAP-03** supporting agent text: <code>Local FindPR returns the first exact-head open pull request by update order; correct- and wrong-target PRs together therefore produce an ordering-dependent verdict while remote verification refuses multiplicity.</code>
- **GAP-04** supporting agent text: <code>Neither core.Workspace nor the local claim-base record retains the selected target, including the outgoing value a pending-epoch rollback must restore.</code>
- **GAP-05** supporting agent text: <code>The local provider has no credentialed Ready seam or named missing-branch refusal; assembly currently calls only the intentionally offline CheckBaseCache.</code>
- **GAP-06** supporting agent text: <code>Airlock&#39;s workflow-scoped RemoteExpectation cannot represent concurrent claims that retained different targets across a default-branch movement.</code>
- **GAP-07** supporting agent text: <code>Mirror claim records retain BaseSHA but not the selected target, and mirror pinning independently falls back to the repository default instead of one claim-scoped selection.</code>
- **GAP-08** supporting agent text: <code>Existing local claim-base, mirror claim, and remote cycle records contain no target; filling them from current configuration or the current repository default would fabricate historical claim state.</code>

### Proposed acceptance tests — MATCHES SUPPLIED CAPTURE (subject_and_repository)

- **TEST-01** supporting agent text: <code>Table-drive omitted, valid explicit, malformed, case-sensitive unknown, duplicate, and null configuration; assert effective text/JSON value and provenance without weakening the strict loader.</code>
- **TEST-02** supporting agent text: <code>Prove local and mirror Ready use the remote credential, return their ErrBaseBranchNotFound for a valid absent branch, and assembly wraps ErrNotReady before bundle publication or agent launch while preserving authentication and transport failures.</code>
- **TEST-03** supporting agent text: <code>In a real Git fixture with different default/configured branches, prove a fresh issue branch starts at the configured commit while an existing remote ben/&lt;workspace&#95;key&gt; branch reattaches unchanged and retains its claim target.</code>
- **TEST-04** supporting agent text: <code>Prove trusted guidance names the target; one correct-target PR publishes, one wrong-target PR is VerdictContradicted and reaches needs-review, and no PR remains VerdictIncomplete.</code>
- **TEST-05** supporting agent text: <code>Return correct- plus wrong-target local PRs in both update orders and prove FindPR returns ErrPRAmbiguous, the verifier never selects either, and the real route reaches needs-review; retain the equivalent remote multiplicity case.</code>
- **TEST-06** supporting agent text: <code>Exercise local retry, pending rollback, reload, restart, and default movement, then feed a targetless legacy record: same-epoch use parks without inference while a new epoch retains only valid outgoing base state and writes a complete target before launch.</code>
- **TEST-07** supporting agent text: <code>For Airlock, start concurrent claims across default movement and prove each mirror record retains its own target, RemoteChecker reads it through RemoteClaimSource, and wrong-target evidence is VerdictContradicted and reaches needs-review.</code>
- **TEST-08** supporting agent text: <code>Feed targetless legacy local, mirror, and remote-cycle records through reload/restart: same-epoch paths return the respective package&#39;s ErrClaimTargetUnrecorded and park without prepare, restore, prompt, verification, or default inference; a post-drain new epoch preserves only valid cycle/base state and atomically writes its newly selected target before dispatch.</code>

### Affected area hypotheses — MATCHES SUPPLIED CAPTURE (subject_and_repository)

- **AREA-01** supporting agent text: <code>internal/config schema, strict load/defaulting, provenance, effective text/JSON, and reload bindings</code>
- **AREA-02** supporting agent text: <code>internal/workspace target selection, credentialed Ready check, named absence refusal, fresh prepare, remote-first reattachment, durable claim target/rollback, and retry/restart tests</code>
- **AREA-03** supporting agent text: <code>internal/template closed variable descriptors and strict rendering</code>
- **AREA-04** supporting agent text: <code>internal/core PR facts and shared ambiguity error, internal/tracker/github complete local PR enumeration, internal/fake fidelity, and internal/verify target/cardinality evidence</code>
- **AREA-05** supporting agent text: <code>internal/orchestrator, cmd/ben local assembly, and internal/integration coverage for the cross-component invariant</code>
- **AREA-06** supporting agent text: <code>cmd/ben/remote and internal/remotews cycle/workspace projection for claim-scoped Airlock targets</code>
- **AREA-07** supporting agent text: <code>internal/mirror claim-time target/base records, legacy refusal and readiness; internal/verify RemoteClaimSource verification; and their conformance/substrate tests</code>
- **AREA-08** supporting agent text: <code>SPEC §5.2.4, §5.6, and §6.2 plus operator workflow guidance after explicit sign-off</code>

### Reasons — MATCHES SUPPLIED CAPTURE (subject_and_repository)

- **WHY-01** supporting agent text: <code>The requested outcome is bounded, but its public field/grammar, evidence binding, multi-PR cardinality, remote seam, and legacy-state rollout are consequential contract choices.</code>
- **WHY-02** supporting agent text: <code>Prompt guidance cannot establish target compliance while the local verifier lacks a pull-request base fact, and choosing the first of multiple PRs makes the verdict depend on update order.</code>
- **WHY-03** supporting agent text: <code>Airlock&#39;s workflow-scoped RemoteExpectation cannot safely stand in for a claim target; the target must live beside the immutable mirror pin and be read back by claim identity.</code>
- **WHY-04** supporting agent text: <code>Locked §9.7 already routes a single wrong-target PR as contradiction and verification ambiguity as needs-review; aligning both substrates preserves that distinction.</code>
- **WHY-05** supporting agent text: <code>Existing durable records cannot acquire a historical target from current state; rollout needs an explicit drain and non-authorizing legacy path before implementation can be safe.</code>

### Recommendation — MATCHES SUPPLIED CAPTURE (subject_and_repository)

- **REC-01** [accepted] agent text: <code>requires&#95;contract&#95;decision</code>

---
This is a read-only decision aid. Every downstream clarification, documentation, split, tracker edit, or queue action requires an explicit human choice.
