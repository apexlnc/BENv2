package reviewctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// pager serves a fixed number of pages of a JSON array, with the Link header
// GitHub sends. The point of the fixture is that no single page carries the
// answer: everything the reducer does with these lists is a filter or a count,
// so a client that stopped at page one would return a different answer rather
// than a smaller one.
type pager struct {
	t     *testing.T
	pages map[string][]string // path -> one JSON array per page
	seen  map[string]int
	query map[string][]string
	base  string
}

// key selects the fixture list: by path, or by path?state=<s> when a fixture
// distinguishes states — Candidates reads /pulls twice with different state
// filters, and serving both from one list would blur exactly the distinction
// under test.
func (p *pager) key(r *http.Request) string {
	if s := r.URL.Query().Get("state"); s != "" {
		if _, ok := p.pages[r.URL.Path+"?state="+s]; ok {
			return r.URL.Path + "?state=" + s
		}
	}
	return r.URL.Path
}

func (p *pager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := p.key(r)
	pages, ok := p.pages[key]
	if !ok {
		http.Error(w, "no fixture for "+key, http.StatusNotFound)
		return
	}
	p.seen[key]++
	if p.query == nil {
		p.query = map[string][]string{}
	}
	p.query[key] = append(p.query[key], r.URL.RawQuery)

	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		fmt.Sscanf(v, "%d", &page)
	}
	if page < 1 || page > len(pages) {
		http.Error(w, "page out of range", http.StatusBadRequest)
		return
	}
	if got := r.URL.Query().Get("per_page"); got != "100" {
		p.t.Errorf("per_page = %q on %s, want 100", got, r.URL.Path)
	}
	if page < len(pages) {
		// The next link keeps the request's own query, the way GitHub's does:
		// dropping the state filter here would silently change which fixture
		// the next page reads.
		q := r.URL.Query()
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf("<%s%s?%s>; rel=\"next\"", p.base, r.URL.Path, q.Encode()))
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, pages[page-1])
}

func TestMultiPageListsAggregateToOneResult(t *testing.T) {
	p := &pager{t: t, seen: map[string]int{}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	p.base = srv.URL

	comment := func(id int, author string) string {
		return fmt.Sprintf(`{"id":%d,"user":{"login":%q},"body":"b%d","created_at":"2026-08-21T12:00:0%dZ"}`, id, author, id, id%10)
	}
	rev := func(id int, head string) string {
		return fmt.Sprintf(`{"id":%d,"user":{"login":"ben-review-bot"},"body":"x","commit_id":%q,"state":"COMMENTED","submitted_at":"2026-08-21T12:00:00Z"}`, id, head)
	}
	event := func(id int, kind string) string {
		return fmt.Sprintf(`{"id":%d,"event":%q,"actor":{"login":"a-human"},"created_at":"2026-08-21T12:00:00Z"}`, id, kind)
	}

	p.pages = map[string][]string{
		"/repos/acme/ben/issues/11/comments": {
			"[" + comment(1, "a") + "," + comment(2, "b") + "]",
			"[" + comment(3, "c") + "]",
			"[" + comment(4, "d") + "," + comment(5, "e") + "]",
		},
		"/repos/acme/ben/issues/11/events": {
			"[" + event(1, "labeled") + "]",
			"[" + event(2, "assigned") + "," + event(3, "unassigned") + "]",
		},
		"/repos/acme/ben/pulls/42/reviews": {
			"[" + rev(1, head1) + "]",
			"[" + rev(2, head2) + "," + rev(3, head3) + "]",
		},
	}

	c := NewClient(srv.URL, "t", "acme", "ben", 5*time.Second)
	ctx := context.Background()

	comments, err := c.Comments(ctx, 11)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 5 {
		t.Errorf("comments = %d, want 5 across 3 pages", len(comments))
	}
	events, err := c.Events(ctx, 11)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 3 {
		t.Errorf("events = %d, want 3 across 2 pages", len(events))
	}
	reviews, err := c.Reviews(ctx, 42)
	if err != nil {
		t.Fatalf("Reviews: %v", err)
	}
	if len(reviews) != 3 {
		t.Errorf("reviews = %d, want 3 across 2 pages", len(reviews))
	}
	if p.seen["/repos/acme/ben/pulls/42/reviews"] != 2 {
		t.Errorf("the reviews endpoint was read %d times, want one request per page", p.seen["/repos/acme/ben/pulls/42/reviews"])
	}
}

// A Link header is server-controlled, and following it verbatim is how a
// paginating client is walked onto another host with its Authorization header
// attached. The refusal is only half the contract: a refused link and an absent
// one must be told apart, because one is a failed read and the other is a
// finished list.
func TestNextLinkStaysOnTheApiRoot(t *testing.T) {
	const base = "https://api.github.com"
	for _, tc := range []struct {
		name    string
		link    string
		want    string
		wantErr bool
	}{
		{name: "empty", link: ""},
		{
			name: "the ordinary next",
			link: `<https://api.github.com/repos/a/b/issues/1/comments?page=2>; rel="next"`,
			want: "/repos/a/b/issues/1/comments?page=2",
		},
		{
			name: "last without next",
			link: `<https://api.github.com/repos/a/b/issues/1/comments?page=9>; rel="last"`,
		},
		{
			name:    "another host is not followed",
			link:    `<https://evil.example/steal>; rel="next"`,
			wantErr: true,
		},
		{
			// The one that bites: this shares every character of the API root
			// and is a different host. Returning the remainder verbatim would
			// have sent the next request there with the token attached.
			name:    "a prefix that only looks like the root",
			link:    `<https://api.github.com.evil.example/x>; rel="next"`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextLink(tc.link, base)
			if got != tc.want {
				t.Errorf("nextLink = %q, want %q", got, tc.want)
			}
			if errors.Is(err, ErrPageLinkOffRoot) != tc.wantErr {
				t.Fatalf("nextLink error = %v, want ErrPageLinkOffRoot: %v", err, tc.wantErr)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("nextLink error = %v, want nil", err)
			}
			if tc.wantErr && strings.Contains(err.Error(), "Bearer") {
				t.Error("the error carries the credential")
			}
		})
	}
}

// The realistic trigger is not a hostile server but a GHES or reverse-proxy
// deployment whose Link headers name its configured public hostname rather than
// the address the client was given. The refusal is right either way; ending the
// walk there is not, so this drives the mismatch end to end and asserts a short
// list never reaches a caller.
func TestARefusedPageLinkIsAnErrorNotAShortList(t *testing.T) {
	p := &pager{t: t, seen: map[string]int{}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	p.base = "https://ghes.public.example" // not srv.URL: the client is configured with the other one
	p.pages = map[string][]string{
		"/repos/acme/ben/issues/11/comments": {
			`[{"id":1,"user":{"login":"a"},"body":"b1","created_at":"2026-08-21T12:00:01Z"}]`,
			`[{"id":2,"user":{"login":"b"},"body":"<!-- ben:route -->","created_at":"2026-08-21T12:00:02Z"}]`,
		},
	}

	got, err := NewClient(srv.URL, "t", "acme", "ben", 5*time.Second).
		Comments(context.Background(), 11)
	if err == nil {
		t.Fatalf("Comments = %d comments and no error, want an error: page two carries the marker", len(got))
	}
	if !errors.Is(err, ErrPageLinkOffRoot) {
		t.Fatalf("Comments error = %v, want ErrPageLinkOffRoot", err)
	}
	if p.seen["/repos/acme/ben/issues/11/comments"] != 1 {
		t.Errorf("the refused link was requested anyway (%d reads)", p.seen["/repos/acme/ben/issues/11/comments"])
	}
}

// The write set, asserted at the wire. This is the independent anchor for the
// permission model: the reducer's own tests prove it never *decides* to
// approve, and this proves the client could not carry one out.
func TestWritesAreTheOnesTheDesignAllows(t *testing.T) {
	type call struct {
		method, path, body string
	}
	var got []call

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, call{r.Method, r.URL.Path, string(body)})
		if auth := r.Header.Get("Authorization"); auth != "Bearer secret" {
			t.Errorf("Authorization = %q", auth)
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "{}")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret", "acme", "ben", 5*time.Second)
	ctx := context.Background()

	if err := c.PublishReview(ctx, 42, head1, "body"); err != nil {
		t.Fatal(err)
	}
	if err := c.Unassign(ctx, 11, "ben-claim-bot"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveLabel(ctx, 11, "ben-queue"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLabel(ctx, 11, "human-review"); err != nil {
		t.Fatal(err)
	}
	if err := c.PostComment(ctx, 11, "route"); err != nil {
		t.Fatal(err)
	}

	want := []call{
		{"POST", "/repos/acme/ben/pulls/42/reviews", ""},
		{"DELETE", "/repos/acme/ben/issues/11/assignees", ""},
		{"DELETE", "/repos/acme/ben/issues/11/labels/ben-queue", ""},
		{"POST", "/repos/acme/ben/issues/11/labels", ""},
		{"POST", "/repos/acme/ben/issues/11/comments", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %+v, want %d", got, len(want))
	}
	for i := range want {
		if got[i].method != want[i].method || got[i].path != want[i].path {
			t.Errorf("call %d = %s %s, want %s %s", i, got[i].method, got[i].path, want[i].method, want[i].path)
		}
	}

	var reviewBody map[string]any
	if err := json.Unmarshal([]byte(got[0].body), &reviewBody); err != nil {
		t.Fatal(err)
	}
	if reviewBody["event"] != "COMMENT" {
		t.Errorf("review event = %v, want COMMENT and nothing else", reviewBody["event"])
	}
	if reviewBody["commit_id"] != head1 {
		t.Errorf("review commit_id = %v, want the reviewed head", reviewBody["commit_id"])
	}
	if !strings.Contains(got[1].body, "ben-claim-bot") {
		t.Errorf("unassign body = %s, want exactly the claim principal", got[1].body)
	}
}

func TestControllerCredentialIsResolvedForEveryRequest(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		io.WriteString(w, `{"number":11,"state":"open","labels":[],"assignees":[]}`)
	}))
	defer srv.Close()

	tokens := []string{"first-bounded-token", "rotated-bounded-token"}
	next := 0
	c := NewCredentialClient(srv.URL, func(context.Context) (string, error) {
		token := tokens[next]
		next++
		return token, nil
	}, "acme", "ben", 5*time.Second)
	if _, err := c.Issue(context.Background(), 11); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Issue(context.Background(), 11); err != nil {
		t.Fatal(err)
	}
	want := []string{"Bearer " + tokens[0], "Bearer " + tokens[1]}
	if len(auth) != len(want) || auth[0] != want[0] || auth[1] != want[1] {
		t.Fatalf("Authorization headers = %v, want %v", auth, want)
	}
}

func TestNotFoundIsAFactAndEverythingElseIsAFailure(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusNotFound, true},
		{http.StatusForbidden, false},
		{http.StatusInternalServerError, false},
		{http.StatusUnauthorized, false},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "nope", tc.status)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "t", "acme", "ben", 5*time.Second)
			_, err := c.PullRequest(context.Background(), 42)
			if err == nil {
				t.Fatal("no error")
			}
			if isNotFound(err) != tc.want {
				t.Errorf("isNotFound = %v, want %v (%v)", isNotFound(err), tc.want, err)
			}
			if strings.Contains(err.Error(), "Bearer") {
				t.Error("the error carries the credential")
			}
		})
	}
}

// candidateFixtureNow is the sweep instant every Candidates fixture measures
// its horizon from.
var candidateFixtureNow = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func candidateClient(t *testing.T, srv *httptest.Server) (*client, *[]string) {
	t.Helper()
	var logged []string
	c := NewClient(srv.URL, "t", "acme", "ben", 5*time.Second).
		WithLog(func(format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			logged = append(logged, line)
			t.Log(line)
		})
	c.now = func() time.Time { return candidateFixtureNow }
	return c, &logged
}

// Reconciliation must reach issues whose label the controller removed even if
// their PR merged before the route marker landed. The canonical-branch half of
// the union therefore includes recently closed PRs rather than only the live
// queue.
func TestCandidatesUnionsBothSources(t *testing.T) {
	p := &pager{t: t, seen: map[string]int{}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	p.base = srv.URL
	p.pages = map[string][]string{
		"/repos/acme/ben/issues": {`[
			{"number":11,"state":"open"},
			{"number":99,"state":"open","pull_request":{}}
		]`},
		"/repos/acme/ben/pulls?state=open": {`[
			{"number":42,"head":{"ref":"ben/11","repo":{"full_name":"acme/ben"}}},
			{"number":44,"head":{"ref":"feature/x","repo":{"full_name":"acme/ben"}}}
		]`},
		"/repos/acme/ben/pulls?state=closed": {`[
			{"number":43,"head":{"ref":"ben/12","repo":{"full_name":"acme/ben"}},"updated_at":"2026-08-30T12:00:00Z"},
			{"number":45,"head":{"ref":"ben/not-a-number","repo":{"full_name":"acme/ben"}},"updated_at":"2026-08-30T11:00:00Z"}
		]`},
	}

	c, _ := candidateClient(t, srv)
	got, err := c.Candidates(context.Background(), "ben-queue")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{11, 12}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
	closed := p.query["/repos/acme/ben/pulls?state=closed"]
	if len(closed) == 0 || !strings.Contains(closed[0], "sort=updated") || !strings.Contains(closed[0], "direction=desc") {
		t.Errorf("closed pull query = %v, want update-recency order so the horizon can end the walk", closed)
	}
}

// Discovery shares the sweep's hard request budget with reconciliation. A
// repository whose first list needs more than that budget must resume from the
// server's next link on the next tick, not throw its pages away and restart at
// page one forever.
func TestCandidatePaginationProgressSurvivesAcrossSweeps(t *testing.T) {
	p := &pager{t: t, seen: map[string]int{}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	p.base = srv.URL
	p.pages = map[string][]string{
		"/repos/acme/ben/issues": {
			`[{"number":11,"state":"open"}]`,
			`[{"number":12,"state":"open"}]`,
			`[{"number":13,"state":"open"}]`,
		},
		"/repos/acme/ben/pulls?state=open":   {`[]`},
		"/repos/acme/ben/pulls?state=closed": {`[]`},
	}

	c, _ := candidateClient(t, srv)
	// Half of each sweep remains available for reconciliation. With a budget
	// of four, discovery therefore yields after two ordinary page reads.
	c.gate.sweepBudget = 4
	first, err := c.Candidates(context.Background(), "ben-queue")
	if err != nil {
		t.Fatalf("first candidate slice: %v", err)
	}
	second, err := c.Candidates(context.Background(), "ben-queue")
	if err != nil {
		t.Fatalf("resumed candidate slice: %v", err)
	}
	if got, want := fmt.Sprint(first), "[11 12]"; got != want {
		t.Fatalf("first candidate slice = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(second), "[13]"; got != want {
		t.Fatalf("resumed candidate slice = %s, want %s", got, want)
	}
	queries := p.query["/repos/acme/ben/issues"]
	var pages []string
	for _, raw := range queries {
		query, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, query.Get("page"))
	}
	if fmt.Sprint(pages) != "[ 2 3]" {
		t.Fatalf("issue pages = %v, want pages 1, 2, then 3 without a restart", queries)
	}
}

func TestCandidateSliceBeforeRateRefusalIsReturned(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/ben/issues" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"message":"Too many requests"}`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf("<%s%s?labels=ben-queue&page=2&per_page=100&state=open>; rel=\"next\"", srv.URL, r.URL.Path))
		io.WriteString(w, `[{"number":11,"state":"open"}]`)
	}))
	defer srv.Close()

	c, _ := candidateClient(t, srv)
	got, err := c.Candidates(context.Background(), "ben-queue")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Candidates error = %v, want ErrRateLimited", err)
	}
	if fmt.Sprint(got) != "[11]" {
		t.Fatalf("candidates before refusal = %v, want [11] retained for the controller", got)
	}
}

// Any GitHub account that can fork can open a pull request whose head ref is
// `ben/<n>`; only a head in the target repository can be a BEN publication,
// and a nil head repo is a deleted fork (#239).
func TestCandidatesExcludeForkHeads(t *testing.T) {
	p := &pager{t: t, seen: map[string]int{}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	p.base = srv.URL
	p.pages = map[string][]string{
		"/repos/acme/ben/issues": {`[]`},
		"/repos/acme/ben/pulls?state=open": {`[
			{"number":42,"head":{"ref":"ben/11","repo":{"full_name":"acme/ben"}}},
			{"number":50,"head":{"ref":"ben/13","repo":{"full_name":"mallory/ben"}}},
			{"number":51,"head":{"ref":"ben/14","repo":null}}
		]`},
		"/repos/acme/ben/pulls?state=closed": {`[]`},
	}

	c, logged := candidateClient(t, srv)
	got, err := c.Candidates(context.Background(), "ben-queue")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 11 {
		t.Fatalf("candidates = %v, want only the local-head 11", got)
	}
	found := false
	for _, line := range *logged {
		if strings.Contains(line, "excluded 2") {
			found = true
		}
	}
	if !found {
		t.Errorf("log = %v, want the two excluded fork heads named — a silent exclusion reads as coverage", *logged)
	}
}

// The closed list is walked newest-update-first only back to the horizon, so
// the sweep's request count is a function of live work, not repository age
// (#239's acceptance): a repository with ten times the closed history answers
// the same candidates from the same number of requests.
func TestClosedCandidatesStopAtTheHorizonNotAtHistory(t *testing.T) {
	sweep := func(t *testing.T, stalePages int) ([]int, int, []string) {
		t.Helper()
		stale := func(n int) string {
			return fmt.Sprintf(`{"number":%d,"head":{"ref":"ben/%d","repo":{"full_name":"acme/ben"}},"updated_at":"2020-01-01T00:00:00Z"}`, n+800, n)
		}
		pages := []string{`[
			{"number":43,"head":{"ref":"ben/12","repo":{"full_name":"acme/ben"}},"updated_at":"2026-08-30T12:00:00Z"},
			` + stale(77) + `
		]`}
		for i := 0; i < stalePages; i++ {
			pages = append(pages, "["+stale(100+i)+"]")
		}

		p := &pager{t: t, seen: map[string]int{}}
		srv := httptest.NewServer(p)
		defer srv.Close()
		p.base = srv.URL
		p.pages = map[string][]string{
			"/repos/acme/ben/issues":             {`[]`},
			"/repos/acme/ben/pulls?state=open":   {`[]`},
			"/repos/acme/ben/pulls?state=closed": pages,
		}

		c, logged := candidateClient(t, srv)
		got, err := c.Candidates(context.Background(), "ben-queue")
		if err != nil {
			t.Fatal(err)
		}
		return got, p.seen["/repos/acme/ben/pulls?state=closed"], *logged
	}

	shallow, shallowReads, logged := sweep(t, 1)
	deep, deepReads, _ := sweep(t, 10)

	if len(shallow) != 1 || shallow[0] != 12 {
		t.Fatalf("candidates = %v, want only the inside-horizon 12", shallow)
	}
	if len(deep) != len(shallow) || deep[0] != shallow[0] {
		t.Fatalf("history changed the answer: %v vs %v", deep, shallow)
	}
	if shallowReads != 1 || deepReads != 1 {
		t.Fatalf("closed-list reads = %d and %d, want exactly 1 each regardless of history", shallowReads, deepReads)
	}
	found := false
	for _, line := range logged {
		if strings.Contains(line, "not scanned") {
			found = true
		}
	}
	if !found {
		t.Errorf("log = %v, want the horizon cut stated — a silently truncated sweep reads as coverage", logged)
	}
}

// fakeClock drives the gate deterministically. Sleep advances time itself,
// the way a real sleep does, so a pacing wait converges instead of spinning.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
	return nil
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func gatedClient(t *testing.T, srv *httptest.Server, start time.Time) (*client, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: start}
	c := NewClient(srv.URL, "t", "acme", "ben", 5*time.Second)
	c.gate.now = clock.Now
	c.gate.sleep = clock.Sleep
	return c, clock
}

// A declared backoff is honoured without the network: every request before it
// passes is refused locally, and the refusal carries the same error the
// declaring response did, so a sweep can stop at the first (#239).
func TestDeclaredBackoffIsHonouredWithoutTheNetwork(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		wait    time.Duration
	}{
		{
			name:    "secondary limit via Retry-After",
			status:  http.StatusForbidden,
			headers: map[string]string{"Retry-After": "120"},
			wait:    120 * time.Second,
		},
		{
			name:   "secondary limit named only by the response body",
			status: http.StatusForbidden,
			body:   `{"message":"You have exceeded a secondary rate limit. Please wait before retrying."}`,
			wait:   minimumBackoff,
		},
		{
			name:    "429 via Retry-After",
			status:  http.StatusTooManyRequests,
			headers: map[string]string{"Retry-After": "60"},
			wait:    60 * time.Second,
		},
		{
			name:   "429 with no timing headers",
			status: http.StatusTooManyRequests,
			body:   `{"message":"Too many requests"}`,
			wait:   minimumBackoff,
		},
		{
			name:   "primary window spent to zero",
			status: http.StatusForbidden,
			headers: map[string]string{
				"X-Ratelimit-Remaining": "0",
				"X-Ratelimit-Reset":     strconv.FormatInt(start.Add(300*time.Second).Unix(), 10),
			},
			wait: 300 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits++
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				if tc.body != "" {
					io.WriteString(w, tc.body)
				} else {
					io.WriteString(w, `{"message":"limited"}`)
				}
			}))
			defer srv.Close()

			c, clock := gatedClient(t, srv, start)
			ctx := context.Background()

			if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
				t.Fatalf("the declaring response = %v, want ErrRateLimited", err)
			}
			if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
				t.Fatalf("under the backoff = %v, want ErrRateLimited", err)
			}
			if hits != 1 {
				t.Fatalf("the forge was asked %d times, want 1 — a refusal under backoff must cost no request", hits)
			}

			clock.Advance(tc.wait + time.Second)
			c.Issue(ctx, 11)
			if hits != 2 {
				t.Fatalf("after the backoff passed the forge was asked %d times, want 2", hits)
			}
		})
	}
}

// A secondary-limit response may carry the ordinary primary-window headers
// even while that window has requests left. Its reset is not a secondary
// retry deadline: GitHub requires the fallback wait in that case, including
// its escalation when the same operation remains limited.
func TestSecondaryLimitIgnoresPrimaryResetWhileRemaining(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var clock *fakeClock
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits <= 2 {
			w.Header().Set("X-Ratelimit-Remaining", "4999")
			w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(clock.Now().Add(10*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"message":"Too many requests"}`)
			return
		}
		io.WriteString(w, `{"number":11,"state":"open","labels":[],"assignees":[]}`)
	}))
	defer srv.Close()

	c, gotClock := gatedClient(t, srv, start)
	clock = gotClock
	ctx := context.Background()
	if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first secondary limit = %v, want ErrRateLimited", err)
	}
	clock.Advance(11 * time.Second)
	if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after the irrelevant ten-second reset = %v, want a local refusal", err)
	}
	if hits != 1 {
		t.Fatalf("network requests after the primary reset = %d, want 1", hits)
	}

	clock.Advance(minimumBackoff - 10*time.Second)
	if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second secondary limit = %v, want ErrRateLimited", err)
	}
	clock.Advance(minimumBackoff + time.Second)
	if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("halfway through the escalated backoff = %v, want a local refusal", err)
	}
	if hits != 2 {
		t.Fatalf("network requests during the escalated backoff = %d, want 2", hits)
	}

	clock.Advance(minimumBackoff)
	if _, err := c.Issue(ctx, 11); err != nil {
		t.Fatalf("after the two-minute fallback: %v", err)
	}
}

// GitHub requires exponentially increasing waits when a secondary-limit
// request keeps failing without a server-supplied deadline. A successful
// response ends that failure streak; otherwise one old incident would make
// every later isolated limit inherit an ever-growing penalty.
func TestHeaderlessSecondaryBackoffEscalatesUntilSuccess(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits <= 3 || hits == 5 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"message":"Too many requests"}`)
			return
		}
		io.WriteString(w, `{"number":11,"state":"open","labels":[],"assignees":[]}`)
	}))
	defer srv.Close()

	c, clock := gatedClient(t, srv, start)
	ctx := context.Background()
	for attempt, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("secondary-limit attempt %d = %v, want ErrRateLimited", attempt+1, err)
		}
		clock.Advance(wait - time.Second)
		if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("before attempt %d's %v deadline = %v, want a local refusal", attempt+1, wait, err)
		}
		if hits != attempt+1 {
			t.Fatalf("network requests before attempt %d's deadline = %d, want %d", attempt+1, hits, attempt+1)
		}
		clock.Advance(2 * time.Second)
	}

	if _, err := c.Issue(ctx, 11); err != nil {
		t.Fatalf("successful response after exponential backoff: %v", err)
	}
	if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first limit after success = %v, want ErrRateLimited", err)
	}
	clock.Advance(minimumBackoff - time.Second)
	if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("before the reset one-minute deadline = %v, want a local refusal", err)
	}
	clock.Advance(2 * time.Second)
	if _, err := c.Issue(ctx, 11); err != nil {
		t.Fatalf("success did not reset the fallback to one minute: %v", err)
	}
	if hits != 6 {
		t.Fatalf("network requests = %d, want 6", hits)
	}
}

// Candidate discovery and observation are reads that happen before a
// rate-limited route write is retried. Their success says nothing about that
// operation's secondary limit and must not turn its next refusal back into
// the first one-minute fallback.
func TestSuccessfulReadDoesNotResetWriteSecondaryBackoff(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	postHits, getHits := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			postHits++
			if postHits <= 2 {
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, `{"message":"Too many requests"}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case http.MethodGet:
			getHits++
			io.WriteString(w, `{"number":11,"state":"open","labels":[],"assignees":[]}`)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	c, clock := gatedClient(t, srv, start)
	ctx := context.Background()
	if err := c.PostComment(ctx, 11, "route"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first POST limit = %v, want ErrRateLimited", err)
	}
	clock.Advance(minimumBackoff + time.Second)
	if _, err := c.Issue(ctx, 11); err != nil {
		t.Fatalf("intervening successful GET: %v", err)
	}
	if err := c.PostComment(ctx, 11, "route"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second POST limit = %v, want ErrRateLimited", err)
	}

	clock.Advance(minimumBackoff + time.Second)
	if err := c.PostComment(ctx, 11, "route"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("halfway through the doubled POST backoff = %v, want a local refusal", err)
	}
	if postHits != 2 || getHits != 1 {
		t.Fatalf("network requests POST/GET = %d/%d, want 2/1", postHits, getHits)
	}

	clock.Advance(minimumBackoff)
	if err := c.PostComment(ctx, 11, "route"); err != nil {
		t.Fatalf("POST after the doubled backoff: %v", err)
	}
}

// A response's headers are complete before its body is read. A truncated body
// must not erase a Retry-After that already told the controller to stop, or the
// sweep would classify the read failure as per-candidate and keep issuing
// requests under the declared backoff.
func TestBodyReadFailureDoesNotEraseDeclaredBackoff(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	readErr := errors.New("response body broke")
	hits := 0
	c := NewClient("https://api.example.invalid", "t", "acme", "ben", 5*time.Second)
	clock := &fakeClock{t: start}
	c.gate.now = clock.Now
	c.gate.sleep = clock.Sleep
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"120"}},
			Body:       io.NopCloser(iotest.ErrReader(readErr)),
			Request:    req,
		}, nil
	})

	if _, err := c.Issue(context.Background(), 11); !errors.Is(err, ErrRateLimited) || !errors.Is(err, readErr) {
		t.Fatalf("declaring response error = %v, want both ErrRateLimited and the body read failure", err)
	}
	if _, err := c.Issue(context.Background(), 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("request under retained backoff = %v, want ErrRateLimited", err)
	}
	if hits != 1 {
		t.Fatalf("transport exchanges = %d, want only the response that declared the backoff", hits)
	}
}

// GitHub says every API request may redirect. The follow-up is another
// network exchange and therefore needs another reservation; otherwise one
// logical call can silently spend past the hard per-sweep budget.
func TestRedirectSpendsAnotherRequestReservation(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/repos/acme/ben/issues/11" {
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		io.WriteString(w, `{"number":11,"state":"open","labels":[],"assignees":[]}`)
	}))
	defer srv.Close()

	c, _ := gatedClient(t, srv, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	c.gate.sweepBudget = 1
	if _, err := c.Issue(context.Background(), 11); !errors.Is(err, ErrSweepBudget) {
		t.Fatalf("redirect past the budget = %v, want ErrSweepBudget", err)
	}
	if hits != 1 {
		t.Fatalf("network exchanges = %d, want the redirect response only", hits)
	}
}

// Client.Do hides redirect responses from its caller. If that exchange spends
// the final primary slot, the gate must learn it before the redirected request
// is allowed onto the network.
func TestRedirectCannotSpendAfterExhaustingThePrimaryWindow(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(start.Add(5*time.Minute).Unix(), 10))
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c, _ := gatedClient(t, srv, start)
	if _, err := c.Issue(context.Background(), 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("redirect after the final primary slot = %v, want ErrRateLimited", err)
	}
	if hits != 1 {
		t.Fatalf("network exchanges = %d, want no request after the exhausted redirect response", hits)
	}
}

// Retry-After is the server's deadline, not a hint to clamp to the primary
// window. A two-hour secondary backoff must still stand after hour one.
func TestRetryAfterLongerThanAnHourIsNotShortened(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"message":"limited"}`)
	}))
	defer srv.Close()

	c, clock := gatedClient(t, srv, start)
	if _, err := c.Issue(context.Background(), 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("declaring response = %v, want ErrRateLimited", err)
	}
	clock.Advance(time.Hour + time.Second)
	if _, err := c.Issue(context.Background(), 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("request before the two-hour deadline = %v, want ErrRateLimited", err)
	}
	if hits != 1 {
		t.Fatalf("network exchanges after one hour = %d, want 1", hits)
	}
	clock.Advance(time.Hour)
	c.Issue(context.Background(), 11)
	if hits != 2 {
		t.Fatalf("network exchanges after the deadline = %d, want 2", hits)
	}
}

func TestRetryAfterHTTPDateIsHonoured(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	want := start.Add(2 * time.Hour)
	h := http.Header{"Retry-After": []string{want.Format(http.TimeFormat)}}
	got, ok := retryAfterUntil(h, start)
	if !ok || !got.Equal(want) {
		t.Fatalf("HTTP-date Retry-After = (%v, %v), want %v", got, ok, want)
	}
}

func TestOversizedBackoffHeadersStayFailClosed(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	oversized := strings.Repeat("9", 100)
	for _, tc := range []struct {
		name  string
		parse func() (time.Time, bool)
	}{
		{
			name: "Retry-After",
			parse: func() (time.Time, bool) {
				return retryAfterUntil(http.Header{"Retry-After": []string{oversized}}, start)
			},
		},
		{
			name: "X-Ratelimit-Reset",
			parse: func() (time.Time, bool) {
				return rateResetUntil(http.Header{"X-Ratelimit-Reset": []string{oversized}})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			until, ok := tc.parse()
			if !ok || !until.After(start.Add(100*365*24*time.Hour)) {
				t.Fatalf("oversized backoff = (%v, %v), want a representable far-future refusal", until, ok)
			}
		})
	}
}

// A permission 403 is an answer about authority, not about pacing. Blocking on
// it would turn a revoked credential into a controller that silently waits.
func TestPlainForbiddenDeclaresNoBackoff(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("X-Ratelimit-Remaining", "4999")
		w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(start.Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
	}))
	defer srv.Close()

	c, _ := gatedClient(t, srv, start)
	ctx := context.Background()

	if _, err := c.Issue(ctx, 11); errors.Is(err, ErrRateLimited) {
		t.Fatal("a plain 403 was read as a rate limit")
	}
	c.Issue(ctx, 11)
	if hits != 2 {
		t.Fatalf("the forge was asked %d times, want 2 — a permission answer must not block the next read", hits)
	}
}

// The response that consumes the final primary slot is still a successful
// response. Its remaining=0 header closes the gate for the next request; it
// must not take one extra exchange just to rediscover the same exhaustion as
// a 403 (#239).
func TestSuccessfulFinalPrimaryRequestClosesTheGate(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("X-Ratelimit-Remaining", "0")
			w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(start.Add(5*time.Minute).Unix(), 10))
		}
		io.WriteString(w, `{"number":11,"state":"open","labels":[],"assignees":[]}`)
	}))
	defer srv.Close()

	c, clock := gatedClient(t, srv, start)
	ctx := context.Background()
	if _, err := c.Issue(ctx, 11); err != nil {
		t.Fatalf("the final allowed request lost its successful response: %v", err)
	}
	if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("request after remaining=0 = %v, want ErrRateLimited", err)
	}
	if hits != 1 {
		t.Fatalf("the exhausted window reached the forge %d times, want only the successful final request", hits)
	}

	clock.Advance(5*time.Minute + time.Second)
	if _, err := c.Issue(ctx, 11); err != nil {
		t.Fatalf("request after reset: %v", err)
	}
	if hits != 2 {
		t.Fatalf("requests after reset = %d, want 2", hits)
	}
}

// The per-sweep budget is a hard local stop, and discovery re-opens it because
// discovery is the first request of every sweep by construction.
func TestSweepBudgetBoundsRequestsPerSweep(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		io.WriteString(w, `{"number":11,"state":"open","labels":[],"assignees":[]}`)
	}))
	defer srv.Close()

	c, _ := gatedClient(t, srv, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	c.gate.sweepBudget = 2
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := c.Issue(ctx, 11); err != nil {
			t.Fatalf("request %d inside the budget: %v", i+1, err)
		}
	}
	if _, err := c.Issue(ctx, 11); !errors.Is(err, ErrSweepBudget) {
		t.Fatalf("past the budget = %v, want ErrSweepBudget", err)
	}
	if hits != 2 {
		t.Fatalf("the forge was asked %d times, want 2 — the refusal must cost no request", hits)
	}

	c.gate.beginSweep()
	if _, err := c.Issue(ctx, 11); err != nil {
		t.Fatalf("after a new sweep began: %v", err)
	}
	if hits != 3 {
		t.Fatalf("the forge was asked %d times after the budget re-opened, want 3", hits)
	}
}

// The pace window defers a burst instead of spending it: request 41 in one
// instant waits out the window, which is what holds the hourly ceiling under
// GitHub's primary allowance.
func TestPacingDefersTheBurst(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		io.WriteString(w, `{"number":11,"state":"open","labels":[],"assignees":[]}`)
	}))
	defer srv.Close()

	c, clock := gatedClient(t, srv, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()

	for i := 0; i < paceRequests+1; i++ {
		if _, err := c.Issue(ctx, 11); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	if hits != paceRequests+1 {
		t.Fatalf("the forge was asked %d times, want %d — pacing defers, it does not drop", hits, paceRequests+1)
	}
	if len(clock.slept) != 1 || clock.slept[0] != paceWindow {
		t.Fatalf("sleeps = %v, want exactly one wait of %v for the window to pass", clock.slept, paceWindow)
	}
}

func TestIssueOfBranch(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		want int
	}{
		{"ben/11", 11},
		{"ben/0", 0},
		{"ben/011", 0}, // not the canonical spelling of 11
		{"ben/11x", 0},
		{"ben/", 0},
		{"main", 0},
		{"bens/11", 0},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			n, ok := issueOfBranch(tc.ref)
			if (tc.want == 0) == ok {
				t.Fatalf("issueOfBranch(%q) = %d, %v", tc.ref, n, ok)
			}
			if ok && n != tc.want {
				t.Errorf("= %d, want %d", n, tc.want)
			}
		})
	}
}
