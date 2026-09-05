# Ticket preflight (`ticketprep`, #222)

`ticketprep` is a bounded ticket-readiness linter and decision facilitator. It
produces a read-only packet before implementation; it is not a planner, a spec
store, an approval gate, or a BEN daemon feature.

The GitHub issue stays canonical. No operation contacts a forge or model, edits
an issue or document, applies a label, authorizes `ben-queue`, dispatches work,
or claims that a ticket is ready. The repository-local `prep-ticket` skill is
the human workflow over this deterministic kernel: invoke it as `$prep-ticket`
in Codex or `/prep-ticket` in Claude Code. It is explicit-only, one shot, and
does not invoke another workflow.

The canonical harness-neutral procedure lives at
`docs/runbooks/agent-skills/prep-ticket.md`. The files below
`.agents/skills/prep-ticket` and `.claude/skills/prep-ticket` only register that
procedure and carry each harness's explicit-invocation control; their bodies do
not fork the workflow.

## The four operations

```sh
# issue.json is a strict snapshot supplied by the operator or a read-only forge client
go run ./cmd/ticketprep capture \
  -repo . -issue issue.json -out capture.json

# advice.json is model-authored except for visibly declared provenance
go run ./cmd/ticketprep validate \
  -capture capture.json -advice advice.json -out packet.json

# recapture first; this comparison never hides stale advice
go run ./cmd/ticketprep freshness \
  -packet packet.json -current current-capture.json -out freshness.json

go run ./cmd/ticketprep render \
  -packet packet.json -current current-capture.json -out report.md
```

Every input flag accepts `-` for stdin, and every `-out` defaults to stdout.
Only one input in an operation may use stdin. Named outputs are replaced only
after the complete output has been validated and rendered.

A read-only GitHub capture can avoid an intermediate issue file:

```sh
gh issue view 152 --repo srhg-ai-7cef3f93/ben \
  --json number,url,title,body \
  --jq '{schema_version:1,number:.number,url:.url,title:.title,body:.body}' |
  go run ./cmd/ticketprep capture -repo . -issue - -out capture.json
```

`gh` performs the network read in that example; `ticketprep` remains offline.
The capture artifact itself retains the exact decoded title and body.

## Trust classes

The packet does not offer the model a field named `trusted`, `observed`, or
`verified`. Its sections have disjoint ownership:

| Class | Fields | What the claim means |
|---|---|---|
| Wrapper-established | `schema_version`, `kernel_version`, content and packet digests, suggestion IDs, freshness status and bindings | Computed by this kernel under the documented byte/schema rules. |
| Declared subject | issue number, URL, title and body under `subject`; source is `declared_issue_snapshot` | Exact values supplied to `capture`, structurally checked and bound together. The offline kernel does not independently ask the forge whether they are current. |
| Repository fact | normalized origin identity, origin fingerprint, full commit/tree IDs, path and Go declaration results, instruction blobs, validation commands and locations | Read from Git configuration or the recorded commit's object tree. Working-tree files are never a fact source. |
| Declared invocation provenance | `provider`, `model`, `command`, `prompt` | Copied by an operator or skill. V0 did not launch or observe the model, so these are explicitly not verified invocation facts. |
| Agent advisory | every field under `advice` | Bounded supporting analysis plus action-bearing `DEC-*`, `SPLIT-*`, and `REC-01` suggestions for human disposition; never scope or authority. |
| Unknown | `facts.unknown`, or a path/symbol fact whose status is `unknown` | V0 cannot interpret the literal safely or uniquely. It does not guess. |

`repository.identity` is normalized as lowercase host plus `owner/repository`
for GitHub. `remote_fingerprint` is SHA-256 of the credential-free configured
origin spelling, so provenance retains a transport-sensitive pin without
publishing a fetch URL. Capture refuses an origin with embedded credentials or
a Git transport helper and refuses an issue URL naming a different repository.

## Exact issue-content identity

The content digest is:

```text
SHA-256(
  "ticketprep.issue-content.v1\x00"
  || uint64-big-endian(len(titleUTF8)) || titleUTF8
  || uint64-big-endian(len(bodyUTF8))  || bodyUTF8
)
```

Title and body must be valid UTF-8. Their decoded bytes are hashed exactly: no
Unicode or newline normalization, trimming, or final-newline insertion. CRLF
and LF, NFC and NFD, and a present or absent final newline differ. JSON member
order and escape spelling do not enter the digest.

Capture resolves `HEAD` once, derives its tree from that immutable commit, and
uses `git ls-tree`, `git grep <commit>`, and `git cat-file` for observations.
Dirty tracked files and untracked files are therefore unable to enter facts
under the recorded identity. There is no working-tree fallback in v0.
Every capture Git process sets `GIT_NO_LAZY_FETCH=1` and
`GIT_NO_REPLACE_OBJECTS=1`: a missing promisor object refuses instead of
contacting its remote, and replacement refs cannot make a recorded object ID
describe different inspected bytes.

## Input schemas

All objects require exact, case-sensitive keys. The decoder rejects duplicate
keys at every depth, unknown or missing fields, `null`, trailing JSON values,
invalid UTF-8, unsupported versions/enums, and the bounds below.

The issue snapshot is:

```json
{
  "schema_version": 1,
  "number": 152,
  "url": "https://github.com/owner/repository/issues/152",
  "title": "exact title",
  "body": "exact body"
}
```

The advisory input is:

```json
{
  "schema_version": 1,
  "declared_provenance": {
    "provider": "openai",
    "model": "unknown",
    "command": "$prep-ticket",
    "prompt": "repository-local one-shot ticket review"
  },
  "advice": {
    "restated_outcome": "one observable outcome",
    "candidate_non_goals": [],
    "assumptions_to_confirm": [],
    "decision_queue": [
      {
        "question": "Which contract owns the choice?",
        "kind": "human_decision",
        "material_effect": "decision",
        "changes": "the public configuration shape",
        "options": [
          "Put the setting under workspace.",
          "Put the setting at the top level."
        ],
        "recommended_option": 1
      }
    ],
    "applicable_constraints": [],
    "acceptance_gaps": [],
    "proposed_acceptance_tests": [],
    "affected_area_hypotheses": [],
    "candidate_delivery_splits": [],
    "recommendation": "clarify",
    "reasons": ["A human must choose the public configuration shape."]
  }
}
```

`declared_provenance.command` records the invocation actually used, including
`/prep-ticket` when Claude Code produced the advice. Provider and model remain
declared—not wrapper-verified—in either harness.

`decision_queue` is only the current dependency frontier. `kind` is one of
`research_question`, `human_decision`, or `prototype_question`.
`material_effect` is one of `decision`, `acceptance_gap`, or `split_boundary`,
and `changes` must say what the answer materially changes. Every human decision
has two or three concrete, unique `options` and a one-based
`recommended_option`; both enter the packet digest. Research and prototype
questions may instead use an empty option list and zero recommendation. V0
displays the frontier; it does not ask interactively.

The recommendation vocabulary is closed:

- `no_material_gap_identified`
- `clarify`
- `decompose_decisions`
- `decompose_delivery`
- `requires_contract_decision`
- `insufficient_context`

`decompose_decisions` requires `decision_decomposition` with a destination,
`not_yet_specifiable`, and `out_of_scope`, plus a nonempty current decision
frontier; it forbids delivery splits. `decompose_delivery` requires two to five
`candidate_delivery_splits` and forbids decision decomposition or an unresolved
decision queue. Each split has
`outcome`, `independently_verifiable_by`, and `blocked_by`. `blocked_by` contains
unique one-based positions of earlier splits; the wrapper renders those as its
own `SPLIT-NN` IDs. This ordering makes cycles and model-authored IDs
unrepresentable. Every other recommendation forbids both decomposition shapes.

## Bounds

| Value | V0 maximum |
|---|---:|
| General JSON artifact | 1 MiB |
| Advisory input or disposition record | 64 KiB |
| Issue title / body / URL | 1 KiB / 256 KiB / 4 KiB |
| Repository identity | 4 KiB |
| Declared provider / model | 256 bytes each |
| Declared command | 1 KiB |
| Restated outcome / declared prompt | 2 KiB each |
| Repository fact string / other advisory string | 1 KiB |
| Non-goals / assumptions / reasons | 5 each |
| Current-frontier decisions | 5 |
| Concrete options per human decision | 2–3 |
| Constraints / gaps / tests / affected areas | 8 each |
| Not-yet-specifiable / out-of-scope items | 5 each |
| Delivery splits / blocking edges per split | 5 / 4 |
| Each repository fact collection | 128 |
| Tree entries / inspected blob | 100,000 / 2 MiB |

These are validator rules, not prompt requests. A response outside them is
rejected before rendering.

## Facts and uncertainty

Backtick-delimited literals drive the intentionally small v0 fact pass:

- an exact safe repository file path is checked in the recorded tree; bare
  directory-or-repository slugs stay unknown in v0;
- a basename is `unknown` when more than one committed path has it;
- an uppercase or camel-case Go identifier is `exists` only when exactly one
  top-level committed Go declaration has that name, `unknown` when declarations
  are ambiguous, and `absent` when none exists; all-lowercase issue vocabulary
  is not presumed to name Go code;
- applicable `AGENTS.md` files are collected from the root and verified path
  ancestors, with their blob IDs;
- validation-shaped commands in their `## Canonical commands` fenced block are
  retained with source line/blob, supplemented by matching Makefile targets;
- path-like or dotted literals outside this grammar are reported under
  `unknown` rather than reinterpreted.

This is literal syntax evidence, not semantic reachability. V0 has no import or
reverse-import graph and makes no build-tag, generated-code, workspace,
replacement, or nested-module completeness claim.

## Freshness and rendering

The schema owns freshness—not the model:

| Binding | Sections |
|---|---|
| Subject | restated outcome, candidate non-goals, assumptions, decision queue |
| Subject and repository | constraints, gaps, tests, affected areas, both decomposition shapes, reasons, recommendation |

Issue identity or exact content movement marks every section `STALE`. Repository
identity, commit, or tree movement marks only subject-and-repository sections
`STALE`. Stale content remains in the report beside the badge and reason.
Rendering therefore requires a supplied comparison capture. A matching section
is labeled `MATCHES SUPPLIED CAPTURE`, never `CURRENT`: the offline kernel has
not independently established forge currency.

The renderer alone assigns `OUT-01`, `NGO-01`, `ASM-01`, `DEC-01`, `CON-01`,
`GAP-01`, `TEST-01`, `AREA-01`, `DEST-01`, `FOG-01`, `OOS-01`, `SPLIT-01`,
`WHY-01`, and `REC-01` by validated schema order. Decision options receive
derived IDs such as `DEC-01-OPT-01`; advisory JSON cannot choose any ID. The
captured issue body is
shown with its line boundaries inside a wrapper-owned preformatted container;
every other dynamic string is flattened to one physical line. Backslashes,
newlines, controls, and bidirectional formatting are visibly escaped, and
HTML/Markdown punctuation is encoded inside wrapper-owned code containers.
Agent prose cannot create report headings, links, HTML, tables, fences, BEN
markers, or fact rows. Validation commands render one per line.

A complete optional disposition document contains only the packet digest and
one entry per action-bearing wrapper ID: `DEC-*` or `SPLIT-*`, plus `REC-01`.
Those two families are mutually exclusive, so a v0 packet asks for at most six
dispositions. The remaining IDs are stable supporting references and cannot be
submitted as disposition items:

```json
{
  "schema_version": 1,
  "packet_digest": "sha256:...",
  "items": [
    {
      "suggestion_id": "DEC-01",
      "disposition": "accepted",
      "selected_option_id": "DEC-01-OPT-01"
    }
  ]
}
```

The four values are `accepted`, `rejected`, `already-present`, and `unclear`.
An accepted or already-present human decision with options must select exactly
one option belonging to that decision. Rejected or unclear items and
non-decision suggestions cannot select one. Unknown, cross-decision,
non-disposition, duplicate, missing action-item, or wrong-packet IDs refuse;
prose is never copied into the record.

## Human-selected next routes

The packet can advise one route, but never starts it:

| Condition | Possible workflow after explicit human choice |
|---|---|
| Bounded but underspecified | One later bounded `grill-me`-style frontier round |
| Missing shared terminology or architecture contract | Separate docs/signoff workflow |
| Destination visible but delivery remains foggy | `wayfinder`-style decision map |
| Understood but holds several shippable outcomes | `to-tickets`-style delivery slices |
| Already implementation-sized | Finish human packet review |

Interactive clarification, issue deltas, tracker writes, docs writes, plugin
packaging, hooks, and #214's five-ticket outcome study are later work.

## Predeclared pilot

V0's single ergonomics pilot is issue #152. Retain its original capture,
validated packet, rendered report, operator/triage time, complete human
action-item dispositions, any separately human-authored issue edit, and notes
about missing context or awkward schema. It tests usability, not delivery lift.

Issue #152 is excluded from #214's future five-ticket cohort and cannot count
toward that study.
