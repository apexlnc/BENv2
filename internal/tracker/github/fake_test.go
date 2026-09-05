package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v90/github"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
	"github.com/srhg-ai-7cef3f93/ben/internal/gitremote"
)

const (
	testOwner = "acme"
	testRepo  = "widgets"
	testLogin = "ben-bot"
)

// fakeGitHub is a request recorder that models the two server behaviors this
// adapter is built around: ETag revalidation, and GitHub's rule that a 304
// costs nothing against the rate limit (SPEC §8.5). Tests assert on `billed`,
// which is exactly what the core budget would have been charged.
type fakeGitHub struct {
	t   *testing.T
	mux *http.ServeMux
	srv *httptest.Server

	mu        sync.Mutex
	requests  []recordedRequest
	billed    int
	remaining int
	limit     *limitResponse
}

// limitResponse makes every subsequent request a rate-limit refusal, shaped
// the way GitHub shapes one.
type limitResponse struct {
	// retryAfterSeconds, when > 0, sets the Retry-After header.
	retryAfterSeconds int
	reset             time.Time
	// secondary swaps the primary signal (X-RateLimit-Remaining: 0) for the
	// documentation_url GitHub uses for abuse limits.
	secondary bool
}

type recordedRequest struct {
	Method      string
	Path        string
	Query       string
	IfNoneMatch string
	Body        string
	Status      int
	Billed      bool
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, mux: http.NewServeMux(), remaining: 5000}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)

	// The authenticated identity every claim rides on (SPEC §8.4). A token
	// named "daemon-*" gets its own login, so tests can race two principals.
	f.handle("GET /api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		login := testLogin
		if tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); strings.HasPrefix(tok, "daemon-") {
			login = tok
		}
		writeJSON(w, r, &gh.User{Login: gh.Ptr(login)})
	})
	return f
}

// fakeIssue is a mutable issue: assignees, labels, and the assignment event
// log that decides contested claims. Enough state to run a real claim race.
type fakeIssue struct {
	mu          sync.Mutex
	number      int
	state       string
	stateReason string
	assignees   []string
	labels      []string
	events      []*gh.IssueEvent
	comments    []*gh.IssueComment
	nextEventID int64
	// onAssign runs after an assignment lands, outside the lock — the seam
	// tests use to synchronize two claimants.
	onAssign func()
	// failReadBack makes GET on the issue fail, simulating a claim we wrote
	// but cannot verify.
	failReadBack bool
	// swallowAssign accepts an assignment with 201 and discards it — the
	// silent failure SPEC §8.4 says read-back exists to catch.
	swallowAssign bool
	// failAssign applies the assignment and *then* answers 500: the write
	// landed, the caller cannot know it, and the claim must be unwound rather
	// than left standing.
	failAssign bool
	// redirectAssign sends the assignment through a 307 before applying it.
	// net/http preserves the method and body, exercising one logical GitHub
	// call that reaches the transport more than once.
	redirectAssign bool
}

func newFakeIssue(number int, labels ...string) *fakeIssue {
	return &fakeIssue{number: number, state: "open", labels: labels, nextEventID: 100}
}

// eventTime is deliberately second-granularity and identical for every event:
// GitHub's timestamps are, and a race inside one second is the case the
// ordering rule has to survive.
var eventTime = gh.Timestamp{Time: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}

func (fi *fakeIssue) snapshotIssue() *gh.Issue {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	iss := issueFixture(fi.number, fi.labels...)
	iss.State = gh.Ptr(fi.state)
	if fi.stateReason != "" {
		iss.StateReason = gh.Ptr(fi.stateReason)
	}
	return withAssignees(iss, fi.assignees...)
}

// closeIssue and reopenIssue move tracker state and leave the matching
// change-log entry behind. The pair is what a close-and-reopen fixture needs:
// state ends up back at open, and only the event still says it closed
// (SPEC §9.8).
//
// `state_reason` moves the way GitHub moves it, because every timestamp in this
// fake is the same second (see eventTime): a fixture that only flipped `state`
// back and forth would leave the sweep's change token unmoved and quietly
// misrepresent the case §9.8 is built around.
func (fi *fakeIssue) closeIssue() {
	fi.setState("closed", "completed")
	fi.recordEvent("closed", nil, nil)
}

func (fi *fakeIssue) reopenIssue() {
	fi.setState("open", "reopened")
	fi.recordEvent("reopened", nil, nil)
}

func (fi *fakeIssue) setState(state, reason string) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.state, fi.stateReason = state, reason
}

func (fi *fakeIssue) assign(login string) {
	fi.mu.Lock()
	if !containsFold(fi.assignees, login) {
		fi.assignees = append(fi.assignees, login)
		fi.events = append(fi.events, &gh.IssueEvent{
			ID:        gh.Ptr(fi.nextEventID),
			Event:     gh.Ptr("assigned"),
			Assignee:  &gh.User{Login: gh.Ptr(login)},
			CreatedAt: &eventTime,
		})
		fi.nextEventID++
	}
	hook := fi.onAssign
	fi.mu.Unlock()

	if hook != nil {
		hook()
	}
}

func (fi *fakeIssue) unassign(login string) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	var kept []string
	for _, a := range fi.assignees {
		if !strings.EqualFold(a, login) {
			kept = append(kept, a)
		}
	}
	fi.assignees = kept
	fi.events = append(fi.events, &gh.IssueEvent{
		ID:        gh.Ptr(fi.nextEventID),
		Event:     gh.Ptr("unassigned"),
		Assignee:  &gh.User{Login: gh.Ptr(login)},
		CreatedAt: &eventTime,
	})
	fi.nextEventID++
}

func (fi *fakeIssue) label(name string) {
	fi.addLabel(name)
	fi.recordEvent("labeled", nil, &gh.Label{Name: name})
}

func (fi *fakeIssue) unlabel(name string) {
	fi.removeLabel(name)
	fi.recordEvent("unlabeled", nil, &gh.Label{Name: name})
}

// arbitraryEvent appends a change-log entry outside the six claim kinds.
func (fi *fakeIssue) arbitraryEvent(kind string) {
	fi.recordEvent(kind, nil, nil)
}

func (fi *fakeIssue) recordEvent(kind string, assignee *gh.User, label *gh.Label) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.events = append(fi.events, &gh.IssueEvent{
		ID:        gh.Ptr(fi.nextEventID),
		Event:     gh.Ptr(kind),
		Actor:     &gh.User{Login: gh.Ptr("actor")},
		Assignee:  assignee,
		Label:     label,
		CreatedAt: &eventTime,
	})
	fi.nextEventID++
}

// dropAssignmentEvents simulates a log that has lost the assignment while the
// assignee stands — event retention, or a transfer (SPEC §9.10 gate 3).
func (fi *fakeIssue) dropAssignmentEvents() {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.events = slices.DeleteFunc(fi.events, func(ev *gh.IssueEvent) bool {
		return ev.GetEvent() == "assigned" || ev.GetEvent() == "unassigned"
	})
}

// reverseEvents serves the change log newest-first, so a test can tell
// "sorted by the adapter" from "happened to arrive in order".
func (fi *fakeIssue) reverseEvents() {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	slices.Reverse(fi.events)
}

// commentsSince models the comments endpoint's `since` filter.
//
// Deliberately the *strict* reading of it: GitHub documents "only show results
// that were last updated after the given time", and an inclusive boundary is not
// something the API promises. A fake that granted one would let the adapter come
// to depend on it, and the dependency would be invisible until a real milestone
// comment went missing and got posted twice (SPEC §8.4).
func (fi *fakeIssue) commentsSince(since string) []*gh.IssueComment {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if since == "" {
		return append([]*gh.IssueComment(nil), fi.comments...)
	}
	cutoff, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return nil
	}
	var out []*gh.IssueComment
	for _, c := range fi.comments {
		if c.GetUpdatedAt().After(cutoff) {
			out = append(out, c)
		}
	}
	return out
}

// addComment appends a comment nobody in this package wrote — the issue's own
// discussion, which is what the marker lookup must not have to walk.
func (fi *fakeIssue) addComment(body string, at time.Time) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	stamp := gh.Timestamp{Time: at}
	fi.comments = append(fi.comments, &gh.IssueComment{
		ID:        gh.Ptr(int64(len(fi.comments) + 1)),
		Body:      gh.Ptr(body),
		CreatedAt: &stamp,
		UpdatedAt: &stamp,
	})
}

func (fi *fakeIssue) currentComments() []string {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	out := make([]string, 0, len(fi.comments))
	for _, c := range fi.comments {
		out = append(out, c.GetBody())
	}
	return out
}

func (fi *fakeIssue) currentAssignees() []string {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return append([]string(nil), fi.assignees...)
}

func (fi *fakeIssue) currentLabels() []string {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return append([]string(nil), fi.labels...)
}

func (fi *fakeIssue) addLabel(name string) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if !containsFold(fi.labels, name) {
		fi.labels = append(fi.labels, name)
	}
}

func (fi *fakeIssue) removeLabel(name string) bool {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	var kept []string
	found := false
	for _, l := range fi.labels {
		if strings.EqualFold(l, name) {
			found = true
			continue
		}
		kept = append(kept, l)
	}
	fi.labels = kept
	return found
}

// testCloneURL is the clone URL the fake repository reports. Its host is
// deliberately unrelated to the fake's API host: GitHub Enterprise admins
// choose the two independently, so a test whose clone host merely echoed the
// API host could not tell a cached server answer from a derived one.
const testCloneURL = "https://git.example.com/" + testOwner + "/" + testRepo + ".git"

var testRepositoryIdentity = mustRepositoryIdentity(testCloneURL)

func mustRepositoryIdentity(remote string) string {
	identity, err := gitremote.RepositoryIdentity(remote)
	if err != nil {
		panic(err)
	}
	return identity
}

// serveRepo registers the repository read Ready uses as its reachability
// probe. Registered per test rather than globally so a test can model a repo
// the credential cannot see by simply not calling it.
func (f *fakeGitHub) serveRepo() { f.serveRepoWithCloneURL(testCloneURL) }

// serveRepoWithCloneURL is serveRepo with the repository's `clone_url` under
// the test's control — the field Ready caches for the base clone (SPEC §6.2).
// An empty string omits it, modelling a server that names no remote.
func (f *fakeGitHub) serveRepoWithCloneURL(clone string) {
	f.handle("GET /api/v3/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("owner") != testOwner || r.PathValue("repo") != testRepo {
			http.NotFound(w, r)
			return
		}
		repo := &gh.Repository{Name: gh.Ptr(testRepo), FullName: gh.Ptr(testOwner + "/" + testRepo)}
		if clone != "" {
			repo.CloneURL = gh.Ptr(clone)
		}
		writeJSON(w, r, repo)
	})
}

// serve registers the endpoints Claim, Release, and SetStateLabels touch.
func (fi *fakeIssue) serve(f *fakeGitHub) {
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		fi.mu.Lock()
		fail := fi.failReadBack
		fi.mu.Unlock()
		if fail {
			http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, fi.snapshotIssue())
	})
	f.handle("POST /api/v3/repos/{owner}/{repo}/issues/{number}/assignees", func(w http.ResponseWriter, r *http.Request) {
		fi.mu.Lock()
		redirect := fi.redirectAssign
		fi.mu.Unlock()
		if redirect && r.URL.Query().Get("redirected") == "" {
			location := *r.URL
			query := location.Query()
			query.Set("redirected", "1")
			location.RawQuery = query.Encode()
			w.Header().Set("Location", location.RequestURI())
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		var body struct {
			Assignees []string `json:"assignees"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
		fi.mu.Lock()
		swallow, fail := fi.swallowAssign, fi.failAssign
		fi.mu.Unlock()
		if !swallow {
			for _, login := range body.Assignees {
				fi.assign(login)
			}
		}
		if fail {
			http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, r, fi.snapshotIssue())
	})
	f.handle("DELETE /api/v3/repos/{owner}/{repo}/issues/{number}/assignees", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Assignees []string `json:"assignees"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
		for _, login := range body.Assignees {
			fi.unassign(login)
		}
		writeJSON(w, r, fi.snapshotIssue())
	})
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues/{number}/events", func(w http.ResponseWriter, r *http.Request) {
		fi.mu.Lock()
		events := append([]*gh.IssueEvent(nil), fi.events...)
		fi.mu.Unlock()
		writePaged(f, w, r, events)
	})
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		writePaged(f, w, r, fi.commentsSince(r.URL.Query().Get("since")))
	})
	f.handle("POST /api/v3/repos/{owner}/{repo}/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		var body gh.IssueComment
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
		fi.mu.Lock()
		// Stamped in the same second as every event in this fake (see
		// eventTime): a milestone comment is posted immediately after the label
		// transition that anchors it, and second-granularity timestamps put both
		// in one second routinely. That is the boundary the `since` filter below
		// has to survive.
		fi.comments = append(fi.comments, &gh.IssueComment{
			ID:        gh.Ptr(int64(len(fi.comments) + 1)),
			Body:      body.Body,
			CreatedAt: &eventTime,
			UpdatedAt: &eventTime,
		})
		fi.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, r, &body)
	})
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues/{number}/labels", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, labelObjects(fi.currentLabels()))
	})
	// Both label writes append a timeline entry, because GitHub does — and
	// §9.10 step 3 classifies from those entries rather than from the label set,
	// so a server that moved labels silently could not show what the adapter's
	// own projection leaves in the log. Only a write that *changes* something
	// records one: adding a label the issue already carries is idempotent on
	// GitHub and writes no entry.
	f.handle("POST /api/v3/repos/{owner}/{repo}/issues/{number}/labels", func(w http.ResponseWriter, r *http.Request) {
		var body []string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
		for _, name := range body {
			if !containsFold(fi.currentLabels(), name) {
				fi.label(name)
			}
		}
		// GitHub answers a label add with the issue's complete label set.
		writeJSON(w, r, labelObjects(fi.currentLabels()))
	})
	f.handle("DELETE /api/v3/repos/{owner}/{repo}/issues/{number}/labels/{label}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("label")
		if !containsFold(fi.currentLabels(), name) {
			http.Error(w, `{"message":"Label does not exist"}`, http.StatusNotFound)
			return
		}
		fi.unlabel(name)
		writeJSON(w, r, labelObjects(fi.currentLabels()))
	})
}

func labelObjects(names []string) []*gh.Label {
	out := make([]*gh.Label, 0, len(names))
	for _, n := range names {
		out = append(out, &gh.Label{Name: n})
	}
	return out
}

func (f *fakeGitHub) handle(pattern string, h http.HandlerFunc) {
	f.mux.HandleFunc(pattern, h)
}

// exhaustBudget reports X-RateLimit-Remaining: 0 on every response while
// still serving them — the state GitHub is in when only conditional requests
// are still free.
func (f *fakeGitHub) exhaustBudget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remaining = 0
}

// rateLimitAll makes the server refuse everything from now on.
func (f *fakeGitHub) rateLimitAll(l limitResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limit = &l
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))

	f.mu.Lock()
	limit := f.limit
	f.mu.Unlock()

	rec := httptest.NewRecorder()
	if limit != nil {
		writeLimit(rec, *limit)
	} else {
		f.mux.ServeHTTP(rec, r)
	}

	billed := rec.Code != http.StatusNotModified

	f.mu.Lock()
	if billed {
		f.billed++
		if f.remaining > 0 {
			f.remaining--
		}
	}
	remaining := f.remaining
	f.requests = append(f.requests, recordedRequest{
		Method:      r.Method,
		Path:        r.URL.Path,
		Query:       r.URL.RawQuery,
		IfNoneMatch: r.Header.Get("If-None-Match"),
		Body:        string(body),
		Status:      rec.Code,
		Billed:      billed,
	})
	f.mu.Unlock()

	for k, vs := range rec.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set(gh.HeaderRateLimit, "5000")
	if w.Header().Get(gh.HeaderRateRemaining) == "" {
		w.Header().Set(gh.HeaderRateRemaining, strconv.Itoa(remaining))
	}
	if w.Header().Get(gh.HeaderRateReset) == "" {
		w.Header().Set(gh.HeaderRateReset, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	}
	w.WriteHeader(rec.Code)
	w.Write(rec.Body.Bytes()) //nolint:errcheck // test server
}

func writeLimit(w http.ResponseWriter, l limitResponse) {
	body := `{"message":"API rate limit exceeded"}`
	if l.secondary {
		body = `{"message":"You have exceeded a secondary rate limit","documentation_url":"https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api#secondary-rate-limits"}`
	} else {
		w.Header().Set(gh.HeaderRateRemaining, "0")
	}
	if l.retryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(l.retryAfterSeconds))
	}
	if !l.reset.IsZero() {
		w.Header().Set(gh.HeaderRateReset, strconv.FormatInt(l.reset.Unix(), 10))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(body)) //nolint:errcheck // test server
}

// writeForbidden answers the way GitHub answers a request the credential's
// permissions do not cover: a 403 naming the resource, and nothing else. The
// ordinary rate headers ServeHTTP adds on the way out are the whole difficulty
// (#198) — a permanent refusal is indistinguishable from a limit if a future
// X-RateLimit-Reset is taken as evidence.
func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com/rest/pulls/pulls#list-pull-requests","status":"403"}`)) //nolint:errcheck // test server
}

// writePaged answers one page of a list and names the next one in the Link
// header, the way GitHub does.
//
// The per-issue lists paginate here rather than answering whole because the
// adapter's walks are bounded a page at a time (budget.go): a fake that served
// every change log in one response could not see a walk stop, and a bound nothing
// exercises is a bound nobody knows holds.
func writePaged[T any](f *fakeGitHub, w http.ResponseWriter, r *http.Request, items []T) {
	q := r.URL.Query()
	per := 30 // GitHub's default when the caller does not ask
	if v, err := strconv.Atoi(q.Get("per_page")); err == nil && v > 0 {
		per = min(v, 100) // and its maximum
	}
	page := 1
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		page = v
	}

	start := min((page-1)*per, len(items))
	end := min(start+per, len(items))
	if end < len(items) {
		next := *r.URL
		q.Set("page", strconv.Itoa(page+1))
		next.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, f.srv.URL, next.RequestURI()))
	}
	writeJSON(w, r, items[start:end])
}

// writeJSON answers conditionally: a matching If-None-Match earns a 304, which
// the recorder does not bill.
func writeJSON(w http.ResponseWriter, r *http.Request, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := fnv.New64a()
	h.Write(b)
	etag := fmt.Sprintf(`"%x"`, h.Sum64())

	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(b) //nolint:errcheck // test server
}

func (f *fakeGitHub) adapter(t *testing.T, mutate ...func(*core.TrackerConfig)) *Adapter {
	t.Helper()
	cfg := core.TrackerConfig{
		Provider: map[string]any{
			"repo":    testOwner + "/" + testRepo,
			"token":   "t0ken",
			"api_url": f.srv.URL,
		},
		RequiredLabels: []string{"ben-queue"},
		WorkflowKey:    "ben-1a2b3c4d",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := New(compileOptions(cfg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// compileOptions is what the loader does to a written tracker block, applied
// here so this package's tests keep stating configurations the way an operator
// writes them (SPEC §8, amendment 9).
//
// The three promoted keys leave the block: `token` becomes an implicit `static`
// source — over the literal when one is written, over the documented
// $GITHUB_TOKEN fallback when it is not — and `claim_assignee` becomes a field.
// A test that wants a *different* credential source replaces Credential after
// the fact.
func compileOptions(cfg core.TrackerConfig) core.TrackerOptions {
	provider := map[string]any{}
	for k, v := range cfg.Provider {
		switch k {
		case "token", "claim_assignee", "credential_source":
		default:
			provider[k] = v
		}
	}
	assignee, _ := cfg.Provider["claim_assignee"].(string)
	token, _ := cfg.Provider["token"].(string)
	cred := credential.NewEnv(FallbackTokenEnv)
	if strings.TrimSpace(token) != "" {
		cred = credential.NewLiteral("site:tracker.provider.token", strings.TrimSpace(token))
	}
	return core.TrackerOptions{
		Provider:       provider,
		RequiredLabels: cfg.RequiredLabels,
		ActiveStates:   cfg.ActiveStates,
		TerminalStates: cfg.TerminalStates,
		WorkflowKey:    cfg.WorkflowKey,
		ClaimAssignee:  strings.TrimSpace(assignee),
		Credential:     cred,
	}
}

func (f *fakeGitHub) snapshot() (requests []recordedRequest, billed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...), f.billed
}

// calls returns the recorded requests matching method and a path substring.
func (f *fakeGitHub) calls(method, pathContains string) []recordedRequest {
	reqs, _ := f.snapshot()
	var out []recordedRequest
	for _, r := range reqs {
		if r.Method == method && strings.Contains(r.Path, pathContains) {
			out = append(out, r)
		}
	}
	return out
}

// callsExact matches the full path — needed for the list endpoint, which is a
// prefix of every per-issue one.
func (f *fakeGitHub) callsExact(method, path string) []recordedRequest {
	reqs, _ := f.snapshot()
	var out []recordedRequest
	for _, r := range reqs {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGitHub) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests, f.billed = nil, 0
}

// issueFixture builds a plausible open issue carrying the required label.
func issueFixture(number int, labels ...string) *gh.Issue {
	created := gh.Timestamp{Time: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(number) * time.Hour)}
	iss := &gh.Issue{
		Number:                   gh.Ptr(number),
		Title:                    gh.Ptr(fmt.Sprintf("Ticket %d", number)),
		Body:                     gh.Ptr("do the thing"),
		State:                    gh.Ptr("open"),
		HTMLURL:                  gh.Ptr(fmt.Sprintf("https://github.com/%s/%s/issues/%d", testOwner, testRepo, number)),
		CreatedAt:                &created,
		UpdatedAt:                &created,
		IssueDependenciesSummary: &gh.IssueDependenciesSummary{BlockedBy: gh.Ptr(0), TotalBlockedBy: gh.Ptr(0)},
	}
	for _, l := range labels {
		iss.Labels = append(iss.Labels, &gh.Label{Name: l})
	}
	return iss
}

func withAssignees(iss *gh.Issue, logins ...string) *gh.Issue {
	for _, l := range logins {
		iss.Assignees = append(iss.Assignees, &gh.User{Login: gh.Ptr(l)})
	}
	return iss
}

func withBlockedBy(iss *gh.Issue, open, total int) *gh.Issue {
	iss.IssueDependenciesSummary = &gh.IssueDependenciesSummary{
		BlockedBy:      gh.Ptr(open),
		TotalBlockedBy: gh.Ptr(total),
	}
	return iss
}
