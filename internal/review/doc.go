// Package review is the pure kernel of the forge-side review controller
// (#11): the per-issue state reducer, the durable marker vocabulary it reads
// and writes, and the validation deciding whether a published milestone may
// drive anything at all.
//
// It is not part of the daemon. The orchestrator never imports it and it never
// imports the orchestrator; the two communicate only through tracker and forge
// artifacts — SPEC §8.4's published milestone comment in, a `COMMENT` review
// plus either an unassignment or a label revocation out. That is the whole
// coupling, and keeping it there is what buys a bounded code → review → revise
// loop without a DAG engine.
//
// Three rules shape everything below.
//
//   - **Nothing here is an approval.** The controller may stop automation
//     (remove the human's required label) and it may hand a claim back
//     (unassign BEN). It never applies a required label, never emits APPROVE,
//     never merges or closes, and never writes a `ben:*` state label. Applying
//     a required label is SPEC §9.5's approval act and belongs to a human;
//     removing one is revocation and asserts nothing.
//
//   - **The forge is the state.** The controller keeps no counter and no
//     database. Rounds are counted from the reviews it published, delivery is
//     deduplicated from the markers it posted, and a no-review terminal route
//     records its intent and occurrence approval epoch before mutation. A
//     crash is repaired by reading those artifacts back without revoking a
//     newer human approval. Every decision below is a function of artifacts
//     anybody can read off the issue and the pull request.
//
//   - **Two keys, two jobs.** The published milestone's *occurrence* is the
//     delivery and idempotency key; the pull request's *head SHA* is the
//     progress key. The review also records the *base SHA*, because head and
//     base together name the exact diff, but only distinct heads spend rounds.
//     Conflating these jobs either reviews one head forever or drops a
//     redelivery on the floor.
//
// [Reduce] is the whole state machine and is a pure function: one observation
// in, one [Step] out. The driver executes that step, re-observes, and asks
// again until it is told there is nothing to do — which is why an interrupted
// route resumes rather than repeats.
package review
