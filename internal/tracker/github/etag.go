package github

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
)

// noCacheKey marks a request context as "must hit the origin". Claim's
// read-back verification uses it: a conditional GET that a stale entry could
// answer would defeat the whole point of reading back (SPEC §8.4).
type noCacheKey struct{}

func withoutCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, noCacheKey{}, true)
}

// The cache bounds, and the overflow policy they govern.
//
// Sizing is read off one tick's working set. Keys are full URLs, so a tick holds
// one entry per list page — the queue read and the §9.8 sweep, at 100 issues a
// page — plus one per per-issue conditional read: a blocked-by summary per
// candidate whose cheap checks passed (applyVerdict), a Get per *running* record
// (parked records and held claims share the sweep's pages), a pull request list
// per verification. The per-candidate half is what
// scales, and it scales with the queue, so 4,096 entries leaves the steady state
// of a queue in the thousands entirely resident.
//
// Both bounds, because entries are not the only thing that grows: a page of 100
// issues with bodies is hundreds of kilobytes, and an entry count alone would
// let the cache hold gigabytes of them.
//
// Overflow evicts least-recently-used rather than flushing. The flush this
// replaces was defended as costing "one uncached round of polling", which is
// true only while overflow is rare — a working set above the bound made it
// *every* round, and the degradation arrived all at once instead of gradually.
const (
	maxCacheEntries = 4096
	maxCacheBytes   = 64 << 20
	// maxEntryBytes keeps one outsized response from evicting a whole working set
	// to make room for itself. A larger body passes through uncached, which costs
	// its own revalidation and nobody else's.
	//
	// 8 MiB because the biggest thing here is a list page, and a list page is the
	// one entry that must never be too large to cache: it is the poll path. A page
	// of 100 issues each at GitHub's 65,536-character body limit is under 7 MiB,
	// so the theoretical maximum fits, and several of them fit inside the byte
	// bound together with the per-candidate reads alongside them.
	maxEntryBytes = 8 << 20
)

// condCache makes GETs conditional (SPEC §8.5). GitHub answers a matching
// If-None-Match with 304 and does not bill it against the core rate limit, so
// the steady state of an idle queue costs no budget at all.
//
// It lives in the transport because go-github has no 304 handling of its own:
// CheckResponse treats any non-2xx as an error. Replaying the cached body as a
// 200 keeps that ignorance harmless.
type condCache struct {
	base http.RoundTripper

	// The bounds this instance runs under, from the constants above. Fields so
	// that a test can shrink them and drive the eviction policy without storing
	// thousands of entries; production has exactly one setting, and what that
	// setting has to hold is asserted from outside the constants that state it.
	maxEntries int
	maxBytes   int
	maxEntry   int

	mu      sync.Mutex
	entries map[string]*cacheEntry
	// bytes is the total body size held, kept incrementally: recomputing it per
	// insert would walk the whole map for a number every mutation already knows.
	bytes int
	// use is a monotonic use counter, not a time. Only the relative order
	// matters, and eviction wants the smallest.
	use uint64
	// hits counts 304s served from cache; misses counts everything else;
	// evictions counts entries dropped to stay inside the bounds. Tests assert
	// on these; nothing else reads them.
	hits, misses, evictions int
}

type cacheEntry struct {
	etag   string
	header http.Header
	body   []byte
	// used orders eviction. Bumped on every lookup that finds this entry, so
	// "recently used" means revalidated, not merely stored.
	used uint64
}

// refreshedHeaders are re-taken from the 304 rather than replayed from cache:
// they describe this exchange, not the cached representation.
var refreshedHeaders = []string{
	"Date",
	"X-Ratelimit-Limit",
	"X-Ratelimit-Remaining",
	"X-Ratelimit-Used",
	"X-Ratelimit-Reset",
	"X-Ratelimit-Resource",
	"Retry-After",
}

func newCondCache(base http.RoundTripper) *condCache {
	if base == nil {
		base = http.DefaultTransport
	}
	return &condCache{
		base:       base,
		maxEntries: maxCacheEntries,
		maxBytes:   maxCacheBytes,
		maxEntry:   maxEntryBytes,
		entries:    map[string]*cacheEntry{},
	}
}

func (c *condCache) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || req.Context().Value(noCacheKey{}) != nil {
		return c.base.RoundTrip(req)
	}
	key := req.URL.String()

	if entry := c.lookup(key); entry != nil {
		// Clone: RoundTrippers must not mutate the request they are given.
		req = req.Clone(req.Context())
		req.Header.Set("If-None-Match", entry.etag)

		resp, err := c.base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotModified {
			return c.replay(entry, resp), nil
		}
		return c.store(key, resp), nil
	}
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	return c.store(key, resp), nil
}

func (c *condCache) lookup(key string) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry != nil {
		c.use++
		entry.used = c.use
	}
	return entry
}

// store buffers a cacheable 200 so the next request can revalidate it. A
// response without an ETag, or any non-200, passes through untouched and
// invalidates whatever was cached for that key.
func (c *condCache) store(key string, resp *http.Response) *http.Response {
	c.mu.Lock()
	c.misses++
	c.mu.Unlock()

	etag := resp.Header.Get("ETag")
	if resp.StatusCode != http.StatusOK || etag == "" || resp.Body == nil {
		c.mu.Lock()
		c.drop(key)
		c.mu.Unlock()
		return resp
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		// A truncated body is the caller's problem to report, not ours to
		// cache. Hand back the error through a body that re-raises it.
		resp.Body = io.NopCloser(&errReader{err})
		return resp
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))

	c.mu.Lock()
	defer c.mu.Unlock()
	// Whatever this key held is gone either way: it is the stale representation
	// of the resource we just re-read.
	c.drop(key)
	if len(body) > c.maxEntry {
		return resp
	}
	c.evictFor(len(body))
	c.use++
	c.entries[key] = &cacheEntry{etag: etag, header: resp.Header.Clone(), body: body, used: c.use}
	c.bytes += len(body)
	return resp
}

// evictFor makes room for an n-byte body by dropping least-recently-used
// entries until both bounds hold again.
//
// The scan is O(entries) per eviction, deliberately: it accompanies an HTTP
// round trip that costs orders of magnitude more, and an intrusive list or heap
// would have to be kept correct on every cache hit to buy nothing measurable at
// this size. Called on the mutex.
func (c *condCache) evictFor(n int) {
	for len(c.entries) >= c.maxEntries || c.bytes+n > c.maxBytes {
		var oldestKey string
		var oldest uint64
		for k, e := range c.entries {
			if oldestKey == "" || e.used < oldest {
				oldestKey, oldest = k, e.used
			}
		}
		if oldestKey == "" {
			// Nothing left to evict: a body larger than the whole byte bound.
			// maxEntryBytes keeps this unreachable in production; the guard is
			// here so a misconfigured bound cannot spin.
			return
		}
		c.drop(oldestKey)
		c.evictions++
	}
}

// drop removes one entry and the bytes it accounted for. Called on the mutex.
func (c *condCache) drop(key string) {
	if entry, ok := c.entries[key]; ok {
		c.bytes -= len(entry.body)
		delete(c.entries, key)
	}
}

func (c *condCache) replay(entry *cacheEntry, resp *http.Response) *http.Response {
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining for connection reuse
	resp.Body.Close()

	c.mu.Lock()
	c.hits++
	c.mu.Unlock()

	header := entry.header.Clone()
	for _, name := range refreshedHeaders {
		header.Del(name)
		for _, v := range resp.Header.Values(name) {
			header.Add(name, v)
		}
	}
	// The httpcache convention, which go-github honors: a replayed body must
	// not be mistaken for a fresh observation of the resource.
	header.Set("X-From-Cache", "1")
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         resp.Proto,
		ProtoMajor:    resp.ProtoMajor,
		ProtoMinor:    resp.ProtoMinor,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(entry.body)),
		ContentLength: int64(len(entry.body)),
		Request:       resp.Request,
	}
}

func (c *condCache) stats() (hits, misses int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// occupancy reports what the cache is holding and what it has had to drop.
func (c *condCache) occupancy() (entries, bytes, evictions int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.bytes, c.evictions
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }
