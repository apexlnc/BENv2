# Ticketprep v0 ergonomics pilot — issue #152

Status: fourth review approved; the packet has a complete digest-bound human
disposition. Prior selections remain recorded below only as history.

Issue #152 was predeclared in #222 before implementation because it is concrete,
cross-cutting, names repository surfaces, and still contains contract decisions.
It is explicitly excluded from #214's later five-ticket cohort and must not be
counted as evidence of delivery lift.

## Bound subject

- Issue: `https://github.com/srhg-ai-7cef3f93/ben/issues/152`
- Content digest: `sha256:573840a53d765816267c611bb78f50595f4498fe22387d58699748c8718e55ca`
- Commit: `87d01e949fcab4053df1ed34c74562bc22d340e7`
- Tree: `3a1ecd8ba562a41a386ee1eede284010fc9072fd`
- Packet digest: `sha256:772099818aff9bd25682abe88cbc6852c8cb828b650c91fc4019b7dd7b6974be`

The initial and supplied pre-render comparison captures are byte-identical.
Every advisory section therefore `MATCHES SUPPLIED CAPTURE`; the kernel does
not call that independently forge-current. The human reviewer separately
reported that the live issue matched and had no comments. That report is not
elevated to a wrapper-established fact.

## Artifacts

- `capture.json`: original exact subject and committed repository observations
- `advice.json`: bounded model-authored value plus declared provenance
- `packet.json`: validated binding of those two inputs
- `current-capture.json`: supplied pre-render comparison capture
- `freshness.json`: section-level comparison
- `dispositions.json`: complete human approval of the current recommended options
- `report.md`: wrapper-rendered packet with the digest-bound dispositions

Issue #152, its labels, and its queue state remain unchanged. The human
separately authorized an issue #222 contract edit recording the six-action-item
disposition and digest-bound option rules.

## Time

The first agent triage pass took 50 seconds from the finalized capture to the
first validated packet, measured from artifact write times. Kernel compilation,
forge-read latency, later fact-filter corrections, and human review are excluded
from that figure. The first review findings are retained below, but no elapsed
human-review time was supplied and none is inferred.

## What the first pass changed

The dry run exposed three mechanical false-positive/noise classes before human
review:

1. all-lowercase branch and config vocabulary was being searched as Go symbols;
2. every canonical command was shown as validation, including daemon execution;
3. an external `owner/repository` slug looked like an absent local file.

The kernel now requires an uppercase/camel-case signal for an unqualified Go
symbol, filters to validation-shaped commands, and leaves a bare slash-delimited
slug unknown. The retained artifacts were regenerated after those changes.

## What the first human review changed

The review correctly found that the first packet was not yet dispositionable:

1. it treated publish-target prompt guidance as if it were evidence even though
   the local verifier could accept a pull request against the wrong base;
2. it reopened settled branch/claim preservation instead of asking how long the
   selected target lives across retry, reload, restart, and default movement;
3. it mentioned Airlock without mapping mirror, remote verification, assembly,
   and substrate tests; and
4. it reopened one-versus-two branch settings when the issue already asks for
   one value.

The revised packet makes those the actual decision and test frontier. The
renderer now includes the safely escaped issue body, emits validation commands
one per line, uses `MATCHES SUPPLIED CAPTURE`, and limits disposition work to
the decision/split frontier plus the recommendation. This pilot therefore asks
for six responses rather than 44 repetitive ones.

## Why the first disposition was withdrawn

The human response was `accept all defaults`, but the packet held open questions
rather than concrete recommended answers. Marking those questions `accepted`
did not record what was chosen, and the later prose below was outside the packet
digest. That response therefore established direction, not final dispositions.

The revised advice now carries two or three exact options and one recommended
option for each human decision. The packet digest binds that text, the renderer
assigns each choice a `DEC-*-OPT-*` ID, and an accepted decision is invalid
without a selected option ID.

## Superseded second human selection

For packet
`sha256:b52075d8af34996e5af89769932649a628ad1ac2346b7f8749b3d4d949726e2e`,
the human explicitly approved:

- `DEC-01-OPT-01`
- `DEC-02-OPT-01`
- `DEC-03-OPT-01`
- `DEC-04-OPT-01`
- `DEC-05-OPT-01`
- `REC-01` as `accepted`

Issue #222 was amended with the same authorization: every action-bearing
suggestion requires a disposition and accepted human decisions select a
digest-bound option, while supporting reference IDs create no separate review
chore. Issue #152 remains unchanged.

## What the third human review changed

The next review found three contract defects in those selected defaults:

1. a workflow-scoped `RemoteExpectation` cannot represent concurrent claims
   that retained different targets across repository-default movement;
2. the selected wrong-target rule contradicted the locked §9.7 verdict and
   route; and
3. the missing-branch rule named readiness without a callable seam or named
   refusal.

The new packet makes the mirror claim record authoritative for the remote
target, names a consumer-owned `RemoteClaimSource` read seam, preserves
`VerdictContradicted` → `needs-review`, and specifies concrete local and remote
`Ready` methods plus package-owned `ErrBaseBranchNotFound` refusals. It also
adds concurrency, rollback, restart, and real routing tests. Because
`DEC-02-OPT-01`, `DEC-04-OPT-01`, and `DEC-05-OPT-01` changed materially, the
old disposition artifact was removed instead of being presented against the
new packet. All six action items are unreviewed again.

## What the fourth human review changed

The next review found two remaining places where implementation could still
invent safety-critical behavior:

1. local `FindPR` selected the first updated pull request while the remote read
   refused multiple candidates, so a correct-target and wrong-target pair made
   the local verdict depend on response order; and
2. existing local claim-base, mirror-claim, and remote-cycle records predate
   `TargetBranch`, so filling the field from today's workflow or repository
   default would fabricate historical claim state.

The new packet gives pull-request cardinality its own decision. Its recommended
option aligns both substrates on a shared ambiguity refusal: local enumeration
must inspect all pages or stop once the second exact-branch open candidate makes
the result ambiguous, never select by update order, and route the verification
error to `needs-review`. The acceptance test presents the correct/wrong pair in
both orders.

The lifetime decision now includes rollout. Its recommended option requires
the locked §9.10 empty-principal drain, treats every targetless legacy record as
non-authorizing under a named package error, and permits only a later assignment
epoch to write a complete target-bearing record. Same-epoch recovery parks and
never derives a historical target from current state. To keep the frontier at
five decisions, the already coupled config path, grammar, effective output, and
readiness seam are one decision.

These changes materially replaced `DEC-01`, `DEC-03`, and `DEC-05`, so no prior
selection carried forward into the fresh approval below.

## Final human disposition

For packet
`sha256:772099818aff9bd25682abe88cbc6852c8cb828b650c91fc4019b7dd7b6974be`,
the human approved every current recommended option:

- `DEC-01-OPT-01`
- `DEC-02-OPT-01`
- `DEC-03-OPT-01`
- `DEC-04-OPT-01`
- `DEC-05-OPT-01`
- `REC-01` as `accepted`

The disposition is complete and digest-bound. It records contract choices for
later issue #152 implementation; it does not mutate issue #152, apply
`ben-queue`, or authorize any other tracker action.

## Remaining ergonomics observations

- A basename with line numbers (`workspace.go:761,843,868`) correctly remains
  unknown because four committed paths share that basename; v0 does not infer
  the nearby full path.
- Exact provider/model identity was unavailable to the copying workflow, so the
  model is honestly declared `unknown` rather than presented as verified.
- The full evidence and supporting analysis remain dense by design, but only
  six lines request action.
- The shared skill procedure now also audits singular reads against multiple
  real candidates and requires an explicit non-fabricating migration policy for
  every newly required durable fact.
