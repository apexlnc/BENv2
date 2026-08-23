package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v90/github"
)

// Revalidation must notice change: a cache that keeps serving a stale queue
// would freeze the daemon on whatever it saw first.
func TestConditionalPollPicksUpChanges(t *testing.T) {
	f := newFakeGitHub(t)

	var mu sync.Mutex
	page := []*gh.Issue{issueFixture(1, "ben-queue")}
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, r, page)
	})
	adapter := f.adapter(t)

	if _, err := adapter.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}

	mu.Lock()
	page = append(page, issueFixture(2, "ben-queue"))
	mu.Unlock()

	issues, err := adapter.Fetch(context.Background())
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues after the queue grew, want 2", len(issues))
	}

	// And the new representation replaces the cached one.
	third, err := adapter.Fetch(context.Background())
	if err != nil {
		t.Fatalf("third Fetch: %v", err)
	}
	if len(third) != 2 {
		t.Fatalf("got %d issues from the revalidated cache, want 2", len(third))
	}
	last := f.calls("GET", "/repos/acme/widgets/issues")
	if got := last[len(last)-1]; got.Status != http.StatusNotModified {
		t.Errorf("third poll answered %d; the updated body should have been cached", got.Status)
	}
}

// cacheable is a 200 the cache can store: an ETag and a body of a stated size.
func cacheable(etag string, size int) *http.Response {
	header := http.Header{}
	header.Set("ETag", `"`+etag+`"`)
	body := strings.Repeat("x", size)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// Overflow evicts the least recently used entry and keeps the rest. The flush
// this replaces was defended as costing one uncached round of polling — true
// only while overflow is rare, and a working set above the bound made it every
// round, all at once instead of gradually.
func TestCacheEvictsTheLeastRecentlyUsedRatherThanFlushing(t *testing.T) {
	c := newCondCache(nil)
	c.maxEntries = 3

	for _, key := range []string{"a", "b", "c"} {
		c.store(key, cacheable(key, 10))
	}
	// "a" is revalidated, so "b" is now the coldest thing in the cache.
	if c.lookup("a") == nil {
		t.Fatal("a was not cached")
	}
	c.store("d", cacheable("d", 10))

	if entries, bytes, evictions := c.occupancy(); entries != 3 || bytes != 30 || evictions != 1 {
		t.Errorf("occupancy = (%d entries, %d bytes, %d evicted), want (3, 30, 1)", entries, bytes, evictions)
	}
	for _, key := range []string{"a", "c", "d"} {
		if c.lookup(key) == nil {
			t.Errorf("%q was dropped; only the least recently used one may be", key)
		}
	}
	if c.lookup("b") != nil {
		t.Error("b survived; it was the least recently used entry")
	}
}

// Entries are not the only thing that grows: a page of 100 issues is hundreds of
// kilobytes, so a count bound alone would let the cache hold gigabytes of them.
func TestCacheEvictsToStayInsideItsByteBound(t *testing.T) {
	c := newCondCache(nil)
	c.maxEntries = 100
	c.maxBytes = 100

	c.store("big", cacheable("big", 60))
	c.store("small", cacheable("small", 30))
	if entries, bytes, _ := c.occupancy(); entries != 2 || bytes != 90 {
		t.Fatalf("occupancy = (%d entries, %d bytes), want (2, 90) — both fit", entries, bytes)
	}

	c.store("next", cacheable("next", 30))
	entries, bytes, evictions := c.occupancy()
	if bytes > c.maxBytes {
		t.Errorf("holding %d bytes, over the %d bound", bytes, c.maxBytes)
	}
	if entries != 2 || evictions != 1 {
		t.Errorf("occupancy = (%d entries, %d evicted), want (2, 1) — only what the bound needed", entries, evictions)
	}
	if c.lookup("big") != nil {
		t.Error("the coldest and largest entry survived while a newer one was refused room")
	}
}

// One outsized response must not evict a whole working set to make room for
// itself. It passes through uncached, which costs its own revalidation and
// nobody else's.
func TestCacheRefusesAnOversizedBody(t *testing.T) {
	c := newCondCache(nil)
	c.maxEntry = 16

	c.store("small", cacheable("small", 8))
	resp := c.store("huge", cacheable("huge", 64))

	if entries, bytes, evictions := c.occupancy(); entries != 1 || bytes != 8 || evictions != 0 {
		t.Errorf("occupancy = (%d entries, %d bytes, %d evicted), want the small entry untouched", entries, bytes, evictions)
	}
	// Uncached is not undelivered: the caller still gets the whole body.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the passed-through body: %v", err)
	}
	if len(body) != 64 {
		t.Errorf("body is %d bytes, want the 64 the server sent", len(body))
	}
}

// A key being re-stored releases what it held, whether or not the new
// representation is cacheable. Without that the byte accounting would drift up
// on every revalidation that changed a resource's size.
func TestCacheAccountsForAReplacedEntry(t *testing.T) {
	c := newCondCache(nil)

	c.store("k", cacheable("v1", 40))
	c.store("k", cacheable("v2", 10))
	if entries, bytes, _ := c.occupancy(); entries != 1 || bytes != 10 {
		t.Errorf("occupancy = (%d entries, %d bytes), want (1, 10)", entries, bytes)
	}

	// An uncacheable answer invalidates the key and its bytes with it.
	c.store("k", &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}})
	if entries, bytes, _ := c.occupancy(); entries != 0 || bytes != 0 {
		t.Errorf("occupancy = (%d entries, %d bytes), want the entry and its bytes gone", entries, bytes)
	}
}

// The sizing, asserted from outside the constant that sets it: a queue in the
// thousands has to stay resident, because that working set is what the whole
// conditional-poll discipline rests on (SPEC §8.5). One entry per candidate's
// blocked-by read plus the list pages over them is the shape being modelled.
func TestCacheHoldsARealisticQueue(t *testing.T) {
	const queue = 1000

	c := newCondCache(nil)
	for i := range queue {
		c.store(fmt.Sprintf("https://api.github.test/repos/acme/widgets/issues/%d/dependencies/blocked_by", i), cacheable(fmt.Sprint(i), 512))
	}
	for i := range queue / perPage {
		c.store(fmt.Sprintf("https://api.github.test/repos/acme/widgets/issues?page=%d", i), cacheable(fmt.Sprint("page", i), 200<<10))
	}

	entries, _, evictions := c.occupancy()
	if evictions != 0 {
		t.Errorf("evicted %d entries of a %d-issue queue's working set; the steady state must be resident", evictions, queue)
	}
	if entries != queue+queue/perPage {
		t.Errorf("holding %d entries, want the whole working set", entries)
	}
	// And the coldest of them still revalidates rather than being re-read.
	if c.lookup("https://api.github.test/repos/acme/widgets/issues/0/dependencies/blocked_by") == nil {
		t.Error("the first candidate's entry is gone, so the next poll pays for it again")
	}
}

// The bounds have to be able to coexist. A maximal entry that barely fits inside
// the byte bound would make the cache thrash on the very responses the poll path
// depends on, and one that does not fit at all would leave evictFor's guard
// against spinning doing the work instead of the policy.
func TestCacheBoundsAreCoherent(t *testing.T) {
	// A list page of 100 issues at GitHub's body limit — the largest response
	// this cache is ever offered, and the one it can least afford to refuse.
	const largestListPage = 100 * 65536
	if maxEntryBytes < largestListPage {
		t.Errorf("maxEntryBytes = %d cannot hold a maximal list page of %d bytes, so the poll path would go uncached",
			maxEntryBytes, largestListPage)
	}
	if maxCacheBytes/maxEntryBytes < 8 {
		t.Errorf("maxCacheBytes = %d holds only %d maximal entries; a poll would evict its own working set",
			maxCacheBytes, maxCacheBytes/maxEntryBytes)
	}
	if maxCacheEntries*maxEntryBytes < maxCacheBytes {
		t.Errorf("maxCacheEntries × maxEntryBytes = %d cannot reach maxCacheBytes = %d, so the byte bound never binds",
			maxCacheEntries*maxEntryBytes, maxCacheBytes)
	}
}

// An error response must not be cached as if it were the resource.
func TestCacheDropsEntryOnErrorResponse(t *testing.T) {
	f := newFakeGitHub(t)

	var mu sync.Mutex
	fail := false
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		failing := fail
		mu.Unlock()
		if failing {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, []*gh.Issue{issueFixture(1, "ben-queue")})
	})
	adapter := f.adapter(t)

	if _, err := adapter.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	if _, err := adapter.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch should have surfaced the 500")
	}

	mu.Lock()
	fail = false
	mu.Unlock()
	if _, err := adapter.Fetch(context.Background()); err != nil {
		t.Fatalf("third Fetch: %v", err)
	}
	last := f.calls("GET", "/repos/acme/widgets/issues")
	if got := last[len(last)-1]; got.IfNoneMatch != "" {
		t.Errorf("poll after an error was conditional against a dropped entry (If-None-Match: %s)", got.IfNoneMatch)
	}
}
