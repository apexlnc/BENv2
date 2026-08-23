**Spec:** §8.5 · **Depends on:** — · Relates to #36

The poll path is better protected than it looks: `condCache` conditionally revalidates every
GET not marked `withoutCache` (`etag.go:67`), `listBlockedBy` included, and GitHub does not
bill a 304. Two residual mechanisms are unbounded and unmeasured.

`maxCacheEntries = 512` overflows via `clear(c.entries)` (`etag.go:127`) — a total flush, not
an eviction. The comment's "the only cost is one uncached round of polling" holds per flush;
if the steady-state working set exceeds 512 it becomes *every* round, and the degradation is
discontinuous rather than gradual. Keys are full URLs, so paginated list pages and per-issue
blocked-by responses all consume entries.

The genuinely billed cost is on the uncached paths, all correctly uncached: recovery is
O(candidates) via `ClaimHistory`, and every `Comment` walks the full event log *and* the full
comment list (`adapter.go:790-806`).

**Acceptance**

- [ ] A per-tick request budget the adapter enforces and reports; exceeding it degrades visibly rather than silently
- [ ] Cache sizing sufficient for a realistic queue, and an overflow policy that does not flush everything
- [ ] `Comment`'s two uncached walks bounded, or cached against a change token
- [ ] `make check` green (evidence in the PR)
