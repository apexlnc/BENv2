**Spec:** §8.2, §9.8 · **Depends on:** —

`core.ErrIssueNotFound` is how a read says *gone* rather than *could not ask* — the distinction §9.8 turns on. Today exactly two adapter methods produce it: `Get` and the §9.5 content read (`ContentApproval`). Every other call that names an issue answers a 404 by wrapping go-github's `ErrorResponse` raw:

- `ClaimHistory` → `issueEvents` → `ListIssueEvents`
- `SetStateLabels` → `projectLabel` → `AddLabelsToIssue` / `ListLabelsByIssue`
- `Comment` → `CreateComment`
- `Release` → `releaseAs` → `RemoveAssignees`
- `FindPR`, `listBlockedBy`

So a caller cannot ask "did this fail because the issue is gone?" of anything but those two, and one that tries gets a permanent retry instead of an answer.

**This already bit once.** #49's `onApproved` classified absence from whichever of its two reads failed; `ClaimHistory` is the one that 404s first for a deleted issue and it does not classify, so a vanished issue held its claim and its §9.5 concurrency slot forever. Fixed there by routing absence detection back through `Get` (reconciling a `queued` record once its claim verifies) rather than by widening the mapping — deliberately, because widening it is this ticket.

**Why it is not obviously just "map them all".** Two questions need deciding rather than assuming:

1. **Which methods should classify.** For reads the answer looks like yes: the sentinel exists so the loop can route on absence, and `ClaimHistory` is a read kernel method (§8.2) whose 404 means exactly what `Get`'s does. For **writes** it is less clear — a 404 on `RemoveAssignees` can mean the issue is gone, but also that the token lost access or the repository moved, and BEN's posture is fail-closed on ambiguity (§8.4's unorderable race, §9.10 gate 3). Concluding "gone" from a write's refusal and forgetting the record is the fail-*open* direction: it drops a claim that may still be standing.
2. **What `core.TrackerAdapter`'s contract says.** Today only `Get`'s doc mentions the sentinel. Whichever methods gain it need their doc updated, and the fakes in `internal/fake` need to match — a fake that classifies where the adapter does not is what let #49's half-fix pass review twice (see that thread).

**Acceptance**

- [ ] A decision recorded on (1): which methods classify absence, reads and writes considered separately, with the fail-closed reasoning for whichever are excluded
- [ ] `core.TrackerAdapter` documents the sentinel on exactly those methods
- [ ] The GitHub adapter maps 404 → `core.ErrIssueNotFound` there and nowhere else
- [ ] `internal/fake` matches, and a test asserts the classification *and its absence* on the rest (`TestEveryCallRefusesForAnIssueTheTrackerDoesNotHave` is the shape)
- [ ] `make check` green (evidence in the PR)
