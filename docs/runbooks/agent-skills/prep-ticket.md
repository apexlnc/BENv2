# prep-ticket

Canonical procedure for the shared `prep-ticket` agent skill. The harness
entries at `.agents/skills/prep-ticket/SKILL.md` and
`.claude/skills/prep-ticket/SKILL.md` are registration stubs; this file is
authoritative. Edit this procedure, never a stub.

Run one non-interactive review. Facts are the agent's responsibility; scope and
contract decisions remain the human's. Do not edit the issue, docs, labels, or
queue, and do not invoke a downstream grilling, Wayfinder, or ticket-splitting
workflow.

Read [the artifact contract](../../TICKETPREP.md) before constructing JSON.
Follow its exact schemas, bounds, recommendation shapes, trust classes, and
freshness rules.

1. Identify one issue and repository from the user's request. If either is
   genuinely unavailable, stop; do not substitute another subject.
2. Capture exact `number`, `url`, `title`, and `body` with a read-only forge
   call. Pipe the strict version-1 object into:

   ```sh
   go run ./cmd/ticketprep capture -repo . -issue - -out <temporary-capture.json>
   ```

   Do not summarize or normalize the title/body first. Keep generated artifacts
   in a temporary directory unless the user explicitly asks to retain them.
3. Read the capture and investigate factual questions yourself. Treat only its
   wrapper-established repository facts as facts in the packet. Any additional
   interpretation or repository hypothesis belongs in advisory fields. Trace
   each proposed value across its real lifetime and cardinality: workflow,
   claim, attempt, reload, restart, and concurrent subjects. Identify the
   component that owns the durable write and the consumer-owned read seam; do
   not substitute workflow-scoped configuration for claim-scoped state. Audit
   singular read APIs against multiple real candidates: explicitly choose and
   test precedence, contradiction, or ambiguity instead of inheriting list
   order. When a durable schema gains a required fact, inspect existing records
   and specify a drain, versioned migration, or non-authorizing legacy path;
   never reconstruct historical state from a current default.
4. Write one strict `AdviceDocument` to a temporary JSON file. Declare the
   provider/model/command/prompt honestly; use `unknown` when exact model
   identity is unavailable. Never call copied provenance verified.
5. Select at most five current-frontier decisions. Every one must say whether it
   changes a decision, acceptance gap, or split boundary and what changes. Put
   two or three concrete options and one recommended option on each human
   decision; these become digest-bound wrapper option IDs the human can select.
   An option that recommends readiness, verification, persistence, or routing
   must name the concrete caller and seam, the named refusal or closed verdict,
   and the durable record when one is required. Keep guidance and evidence
   distinct, and preserve a locked contract unless the decision explicitly asks
   the human to amend it. Research or prototype questions may have no options.
   Put prerequisite-dependent uncertainty under `not_yet_specifiable`; do not
   ask questions interactively in v0.
6. Distinguish decision fog from understood size:

   - Use `decompose_decisions` only for a destination whose delivery work cannot
     yet be honestly specified. Provide no implementation-shaped splits.
   - Use `decompose_delivery` only for multiple independently shippable outcomes,
     each with an observable completion fact and genuine earlier blocking edges.

7. Validate and bind the advice:

   ```sh
   go run ./cmd/ticketprep validate \
     -capture <temporary-capture.json> \
     -advice <temporary-advice.json> \
     -out <temporary-packet.json>
   ```

   Fix schema errors; do not relax, bypass, or hand-render around the kernel.
8. Recapture the exact current issue and repository immediately before rendering
   to a second capture artifact. Then run:

   ```sh
   go run ./cmd/ticketprep render \
     -packet <temporary-packet.json> \
     -current <current-capture.json>
   ```

9. Return the rendered packet without rewriting it into a second plan. State
   that every recommendation is advisory, grants no approval, and cannot
   authorize `ben-queue`. Ask the human to disposition only the action-bearing
   `DEC-*` or `SPLIT-*` IDs and `REC-01`; all other IDs are supporting
   references, not separate review chores. An accepted or already-present human
   decision must name one of its rendered `DEC-*-OPT-*` IDs.

Keep the result compact. Never publish it, modify the canonical issue, create
children, write an ADR, or start implementation without a separate explicit
request.
