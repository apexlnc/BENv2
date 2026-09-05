package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v90/github"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// serveIssues registers the list endpoint over a fixed page.
func (f *fakeGitHub) serveIssues(issues ...*gh.Issue) {
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, issues)
	})
}

// serveBlockedBy registers the dependency endpoint, keyed by issue number.
func (f *fakeGitHub) serveBlockedBy(blockers map[string][]*gh.Issue) {
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues/{number}/dependencies/blocked_by",
		func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, r, blockers[r.PathValue("number")])
		})
}

func TestFetchNormalizesAndComputesDispatchable(t *testing.T) {
	f := newFakeGitHub(t)

	pullRequest := issueFixture(2, "ben-queue")
	pullRequest.PullRequestLinks = &gh.PullRequestLinks{URL: gh.Ptr("https://api.github.com/pulls/2")}

	f.serveIssues(
		issueFixture(1, "ben-queue"),
		pullRequest,
		withAssignees(issueFixture(3, "ben-queue"), "a-human"),
		issueFixture(4, "ben-queue", "ben:running"),
		withBlockedBy(issueFixture(5, "ben-queue"), 1, 1),
		withBlockedBy(issueFixture(6, "ben-queue"), 0, 2),
		issueFixture(7, "chore"),                               // required label absent
		withAssignees(issueFixture(8, "ben-queue"), testLogin), // our own retained done claim
	)
	f.serveBlockedBy(map[string][]*gh.Issue{
		"5": {issueFixture(50)},
		"6": {func() *gh.Issue { i := issueFixture(60); i.State = gh.Ptr("closed"); return i }()},
	})

	issues, err := f.adapter(t).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	got := map[string]bool{}
	for _, iss := range issues {
		got[iss.Identifier] = iss.Dispatchable
	}
	want := map[string]bool{
		"1": true,  // clean
		"3": false, // a human called dibs
		"4": false, // carries a ben:* state label
		"5": false, // open blocker
		"6": true,  // blockers all closed
		"7": false, // required label missing
		// Any assignee blocks, ours included (SPEC §8.3): assigned-to-self with
		// no state label is a published issue awaiting review, and the retained
		// claim is what keeps us from redoing it (SPEC §9.2).
		"8": false,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("dispatchable verdicts = %v, want %v (pull request #2 must be filtered out)", got, want)
	}

	first := issues[0]
	if first.Title != "Ticket 1" || first.Body != "do the thing" || first.State != "open" {
		t.Errorf("normalization dropped fields: %+v", first)
	}
	if first.URL == "" || first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Errorf("normalized issue missing url/timestamps: %+v", first)
	}
	if len(first.Labels) != 1 || first.Labels[0] != "ben-queue" {
		t.Errorf("labels = %v", first.Labels)
	}

	// Blockers ride along on the normalized model where they exist (SPEC §8.3).
	var blocked core.Issue
	for _, iss := range issues {
		if iss.Identifier == "5" {
			blocked = iss
		}
	}
	if len(blocked.Blockers) != 1 || blocked.Blockers[0].Identifier != "50" || !blocked.Blockers[0].Open {
		t.Errorf("blockers of #5 = %+v", blocked.Blockers)
	}

	// Poll cost: the dependency endpoint is only worth a request where the
	// answer can change a verdict (SPEC §8.5).
	var asked []string
	for _, r := range f.calls("GET", "/dependencies/blocked_by") {
		asked = append(asked, strings.Split(r.Path, "/issues/")[1])
	}
	if fmt.Sprint(asked) != "[5/dependencies/blocked_by 6/dependencies/blocked_by]" {
		t.Errorf("blocked_by asked for %v; want only the issues with a non-zero dependency summary", asked)
	}

	// The Search API must stay out of the poll path (SPEC §8.5).
	if searches := f.calls("GET", "/search/"); len(searches) > 0 {
		t.Errorf("poll used the Search API: %v", searches)
	}

	list := f.calls("GET", "/repos/acme/widgets/issues")[0]
	for _, want := range []string{"labels=ben-queue", "state=open", "sort=created", "direction=asc"} {
		if !strings.Contains(list.Query, want) {
			t.Errorf("list query %q missing %q", list.Query, want)
		}
	}
}

// Unknown dependencies must never read as "no dependencies" (SPEC §9.4: a
// failed fetch skips dispatch, it does not guess).
func TestFetchFailsClosedWhenBlockersUnavailable(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveIssues(withBlockedBy(issueFixture(1, "ben-queue"), 1, 1))
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues/{number}/dependencies/blocked_by",
		func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })

	if _, err := f.adapter(t).Fetch(context.Background()); err == nil {
		t.Fatal("Fetch must fail when the dependency endpoint does, not assume zero blockers")
	}
}

func TestFetchPaginates(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, r, []*gh.Issue{issueFixture(2, "ben-queue")})
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/repos/%s/%s/issues?page=2>; rel="next"`, f.srv.URL, testOwner, testRepo))
		writeJSON(w, r, []*gh.Issue{issueFixture(1, "ben-queue")})
	})

	issues, err := f.adapter(t).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(issues) != 2 || issues[0].Identifier != "1" || issues[1].Identifier != "2" {
		t.Fatalf("got %d issues %v, want both pages", len(issues), issues)
	}
}

// The §8.5 discipline, measured: an unchanged queue re-polls for free.
func TestFetchConditionalRequestsCostNoBudget(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveIssues(withBlockedBy(issueFixture(1, "ben-queue"), 0, 1))
	f.serveBlockedBy(map[string][]*gh.Issue{"1": {func() *gh.Issue {
		i := issueFixture(10)
		i.State = gh.Ptr("closed")
		return i
	}()}})
	adapter := f.adapter(t)

	first, err := adapter.Fetch(context.Background())
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	_, billed := f.snapshot()
	if billed == 0 {
		t.Fatal("the cold poll should have cost budget")
	}
	f.reset()

	second, err := adapter.Fetch(context.Background())
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}

	requests, billed := f.snapshot()
	if billed != 0 {
		t.Errorf("second poll billed %d requests; 304s must cost no core budget", billed)
	}
	if len(requests) == 0 {
		t.Fatal("second poll made no requests at all")
	}
	for _, r := range requests {
		if r.IfNoneMatch == "" {
			t.Errorf("%s %s was not conditional", r.Method, r.Path)
		}
		if r.Status != http.StatusNotModified {
			t.Errorf("%s %s answered %d, want 304", r.Method, r.Path, r.Status)
		}
	}
	if hits, _ := adapter.cache.stats(); hits != len(requests) {
		t.Errorf("cache served %d of %d revalidations", hits, len(requests))
	}
	// A cache that costs correctness is not a saving.
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Errorf("replayed poll differs:\n first  = %v\n second = %v", first, second)
	}
}

var claimPrincipalSources = []struct {
	name   string
	login  string
	mutate func(*core.TrackerConfig)
}{
	{name: "credential login", login: testLogin, mutate: func(*core.TrackerConfig) {}},
	{name: "configured assignee", login: "Configured-Bot", mutate: func(c *core.TrackerConfig) {
		c.Provider["claim_assignee"] = "Configured-Bot"
	}},
}

func TestClaimVerifiesByReadBack(t *testing.T) {
	for _, source := range claimPrincipalSources {
		t.Run(source.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			newFakeIssue(1, "ben-queue").serve(f)

			ok, err := f.adapter(t, source.mutate).Claim(context.Background(), core.Issue{Identifier: "1"})
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if !ok {
				t.Fatal("Claim() = false, want a verified claim")
			}
			if got := len(f.calls("GET", "/repos/acme/widgets/issues/1")); got != 1 {
				t.Errorf("read-back happened %d times, want exactly 1 (SPEC §8.4)", got)
			}
			assignments := f.calls("POST", "/assignees")
			if len(assignments) != 1 || !strings.Contains(assignments[0].Body, source.login) {
				t.Errorf("assignment requests = %+v, want one naming %q", assignments, source.login)
			}
			if got := f.calls("DELETE", "/assignees"); len(got) != 0 {
				t.Errorf("a verified claim must not be released: %v", got)
			}
			// An uncontested claim asks nothing about assignment order.
			if got := f.calls("GET", "/events"); len(got) != 0 {
				t.Errorf("uncontested claim read the event log: %v", got)
			}
		})
	}
}

// The §8.4 hardening: a human already holds the issue, so the assignment that
// GitHub accepted still loses.
func TestClaimLostRaceReleasesTheClaim(t *testing.T) {
	for _, source := range claimPrincipalSources {
		t.Run(source.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			issue := newFakeIssue(1, "ben-queue")
			issue.assign("a-human") // there first
			issue.serve(f)

			ok, err := f.adapter(t, source.mutate).Claim(context.Background(), core.Issue{Identifier: "1"})
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if ok {
				t.Fatal("Claim() = true against an earlier claimant; the issue must not be dispatched")
			}
			released := f.calls("DELETE", "/assignees")
			if len(released) != 1 {
				t.Fatalf("released %d times, want exactly 1", len(released))
			}
			if !strings.Contains(released[0].Body, source.login) {
				t.Errorf("release body %q does not name the claim principal %q", released[0].Body, source.login)
			}
			if got := fmt.Sprint(issue.currentAssignees()); got != "[a-human]" {
				t.Errorf("assignees after release = %s, want the winner alone", got)
			}
		})
	}
}

// GitHub logins are case-insensitive and case-preserving. A configured spelling
// must still verify against the canonical casing returned by the issue read-back.
func TestConfiguredClaimAssigneeReadBackIsCaseInsensitive(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.assign("ben-bot")
	issue.serve(f)

	ok, err := f.adapter(t, withClaimAssignee("Ben-Bot")).Claim(context.Background(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !ok {
		t.Fatal("Claim() = false when the configured login differed only in case from GitHub's read-back")
	}
}

// SPEC §12.3 scenario 2: race two daemons for one issue; exactly one wins.
// Both assignments land before either read-back, so both claimants see
// company — the case where "release whenever you see company" leaves nobody
// holding the issue.
//
// Repeated, because the interesting interleavings are downstream of the
// barrier: whether each daemon reads the event log before or after the loser
// has already withdrawn decides whether a naive rule finds a winner.
func TestClaimRaceHasExactlyOneWinner(t *testing.T) {
	for round := range 25 {
		f := newFakeGitHub(t)
		issue := newFakeIssue(1, "ben-queue")

		// Barrier: neither POST returns until both have landed.
		var arrived sync.WaitGroup
		arrived.Add(2)
		issue.onAssign = func() {
			arrived.Done()
			arrived.Wait()
		}
		issue.serve(f)

		daemons := []string{"daemon-alpha", "daemon-zulu"}
		results := make([]bool, len(daemons))
		errs := make([]error, len(daemons))

		var wg sync.WaitGroup
		for i, token := range daemons {
			wg.Add(1)
			go func() {
				defer wg.Done()
				adapter := f.adapter(t, func(c *core.TrackerConfig) { c.Provider["token"] = token })
				results[i], errs[i] = adapter.Claim(context.Background(), core.Issue{Identifier: "1"})
			}()
		}
		wg.Wait()

		winners := 0
		for i, won := range results {
			if errs[i] != nil {
				t.Fatalf("round %d: %s Claim: %v", round, daemons[i], errs[i])
			}
			if won {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d daemons won the race, want exactly 1 (SPEC §12.3 scenario 2)", round, winners)
		}

		// The winner holds the issue alone, and it is the daemon that said
		// so.
		remaining := issue.currentAssignees()
		if len(remaining) != 1 {
			t.Fatalf("round %d: assignees after the race = %v, want exactly the winner", round, remaining)
		}
		for i, won := range results {
			if won != strings.EqualFold(remaining[0], daemons[i]) {
				t.Fatalf("round %d: %s reported won=%v but the issue is assigned to %q", round, daemons[i], won, remaining[0])
			}
		}
	}
}

// A release removes this daemon's principal and nothing else. BEN never
// takes an assignment off anyone, human or daemon (SPEC §8.4, §9.10).
func TestReleaseNeverRemovesAnotherParty(t *testing.T) {
	for _, tt := range []struct {
		name       string
		coAssignee string
		run        func(*Adapter) error
	}{
		{"losing a contested claim", "a-human", func(a *Adapter) error {
			_, err := a.Claim(context.Background(), core.Issue{Identifier: "1"})
			return err
		}},
		{"an unorderable claim", "a-human", func(a *Adapter) error {
			_, err := a.Claim(context.Background(), core.Issue{Identifier: "1"})
			return err
		}},
		{"an explicit release", "another-daemon", func(a *Adapter) error {
			return a.Release(context.Background(), core.Issue{Identifier: "1"})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			issue := newFakeIssue(1, "ben-queue")
			issue.assign(tt.coAssignee) // there first
			issue.serve(f)
			if tt.name == "an unorderable claim" {
				issue.mu.Lock()
				issue.events = nil
				issue.mu.Unlock()
			}

			if err := tt.run(f.adapter(t)); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if got := fmt.Sprint(issue.currentAssignees()); got != "["+tt.coAssignee+"]" {
				t.Errorf("assignees = %s, want the other party untouched", got)
			}
			for _, r := range f.calls("DELETE", "/assignees") {
				if !strings.Contains(r.Body, testLogin) || strings.Contains(r.Body, tt.coAssignee) {
					t.Errorf("release body %q must name only this daemon's principal", r.Body)
				}
			}
		})
	}
}

// Without an order to appeal to, the safe answer is to yield: a wasted round
// beats two daemons on one branch.
func TestClaimYieldsWhenTheRaceCannotBeOrdered(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.assign("a-human")
	issue.serve(f)
	// An event log that has lost the human's assignment (retention, a
	// transfer) leaves the order unknowable.
	issue.mu.Lock()
	issue.events = nil
	issue.mu.Unlock()

	ok, err := f.adapter(t).Claim(context.Background(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatal("Claim() = true with no way to establish who was first")
	}
	if got := fmt.Sprint(issue.currentAssignees()); got != "[a-human]" {
		t.Errorf("assignees = %s, want our unresolvable claim withdrawn", got)
	}
}

// A silently dropped assignment (201, but we are not on the issue) is also a
// failed claim — and leaves nothing to release.
func TestClaimSilentFailureDoesNotRelease(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.assign("a-human")
	issue.swallowAssign = true // 201, and nothing changes
	issue.serve(f)

	ok, err := f.adapter(t).Claim(context.Background(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatal("Claim() = true, but the read-back never showed the principal")
	}
	if got := f.calls("DELETE", "/assignees"); len(got) != 0 {
		t.Errorf("nothing was claimed, so nothing should be released: %v", got)
	}
}

// An assignment we wrote but cannot verify is the one outcome that strands an
// issue forever: recovery reads assigned-with-no-state-label as
// published-awaiting-review and leaves it alone (SPEC §9.10 step 3).
func TestClaimReleasesWhenVerificationFails(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.failReadBack = true
	issue.serve(f)

	ok, err := f.adapter(t).Claim(context.Background(), core.Issue{Identifier: "1"})
	if ok {
		t.Fatal("Claim() = true without a successful read-back")
	}
	if err == nil {
		t.Fatal("Claim() swallowed a verification failure")
	}
	if got := f.calls("DELETE", "/assignees"); len(got) != 1 {
		t.Fatalf("released %d times, want exactly 1 — an unverifiable claim must be unwound", len(got))
	}
	if got := issue.currentAssignees(); len(got) != 0 {
		t.Errorf("issue left assigned to %v after an unverifiable claim", got)
	}
}

// The caller's remedy for a claim error is a release, and a release is a write
// (SPEC §9.10 step 3). Every refusal this adapter reaches before the assignment
// can have left the process therefore says so, so the orchestrator can leave the
// issue alone instead of spending write capacity undoing nothing —
// capacity a spent budget or a standing Retry-After has just said is gone.
//
// The last case is the anchor at the other boundary: a claim that did write must
// *not* carry the promise, whatever else is true of it. Without it this table
// would pass just as well against an adapter that marked every error.
func TestClaimRefusalsSayWhetherAnythingWasWritten(t *testing.T) {
	cases := []struct {
		name string
		// prepare returns the identifier to claim, having put the adapter in the
		// state this case is about.
		prepare      func(t *testing.T, f *fakeGitHub, a *Adapter) string
		notAttempted bool
	}{
		{
			name: "an identifier that is not an issue number",
			prepare: func(*testing.T, *fakeGitHub, *Adapter) string {
				return "not-a-number"
			},
			notAttempted: true,
		},
		{
			name: "a request budget with no room for the sequence",
			prepare: func(t *testing.T, f *fakeGitHub, a *Adapter) string {
				if _, err := a.claimPrincipal(context.Background()); err != nil {
					t.Fatal(err)
				}
				leaveOrdinaryBudget(t, a, 2)
				return "1"
			},
			notAttempted: true,
		},
		{
			name: "a rate limit the server already stated",
			prepare: func(t *testing.T, f *fakeGitHub, a *Adapter) string {
				f.rateLimitAll(limitResponse{retryAfterSeconds: 60, secondary: true})
				if _, err := a.claimPrincipal(context.Background()); err == nil {
					t.Fatal("the identity probe ignored the server's refusal")
				}
				return "1"
			},
			notAttempted: true,
		},
		{
			name: "an assignment that landed and could not be verified",
			prepare: func(t *testing.T, f *fakeGitHub, a *Adapter) string {
				return "1"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			issue := newFakeIssue(1, "ben-queue")
			// Read-back fails throughout: the write-side cases must be
			// not-attempted despite it, and it is what makes the last case an
			// error at all.
			issue.failReadBack = true
			issue.serve(f)
			adapter := f.adapter(t)

			identifier := tc.prepare(t, f, adapter)
			claimed, err := adapter.Claim(context.Background(), core.Issue{Identifier: identifier})
			if claimed || err == nil {
				t.Fatalf("Claim = (%v, %v), want a refusal", claimed, err)
			}
			if got := errors.Is(err, core.ErrClaimNotAttempted); got != tc.notAttempted {
				t.Fatalf("errors.Is(err, ErrClaimNotAttempted) = %v, want %v: %v", got, tc.notAttempted, err)
			}
			assignments := f.calls("POST", "/assignees")
			if tc.notAttempted && len(assignments) != 0 {
				t.Errorf("a claim that promised no write made %d assignment requests: %v", len(assignments), assignments)
			}
			if !tc.notAttempted && len(assignments) == 0 {
				t.Error("the anchor case never wrote, so it cannot show the promise being withheld")
			}
		})
	}
}

// A conditional read-back could be answered from our own cache, verifying
// nothing.
func TestClaimReadBackBypassesTheCache(t *testing.T) {
	f := newFakeGitHub(t)
	newFakeIssue(1, "ben-queue").serve(f)
	adapter := f.adapter(t)

	for range 2 {
		if _, err := adapter.Claim(context.Background(), core.Issue{Identifier: "1"}); err != nil {
			t.Fatalf("Claim: %v", err)
		}
	}
	readBacks := f.calls("GET", "/repos/acme/widgets/issues/1")
	if len(readBacks) != 2 {
		t.Fatalf("got %d read-backs, want 2", len(readBacks))
	}
	for i, r := range readBacks {
		if r.IfNoneMatch != "" {
			t.Errorf("read-back %d was conditional (If-None-Match: %s)", i, r.IfNoneMatch)
		}
		if !r.Billed {
			t.Errorf("read-back %d did not reach the origin", i)
		}
	}
}

func TestReleaseUnassignsThePrincipal(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.assign(testLogin)
	issue.serve(f)

	if err := f.adapter(t).Release(context.Background(), core.Issue{Identifier: "1"}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := issue.currentAssignees(); len(got) != 0 {
		t.Errorf("assignees after Release = %v, want empty", got)
	}
}

// The projection is a set-to, not an add-to: exactly the §9.3 label survives,
// whatever the issue carried before.
func TestSetStateLabelsWritesExactlyTheProjection(t *testing.T) {
	tests := []struct {
		name     string
		existing []string
		label    core.StateLabel
		want     string
	}{
		{"first projection", []string{"ben-queue"}, core.StateLabelClaimed, "[ben-queue ben:claimed]"},
		{"advance", []string{"ben-queue", "ben:claimed"}, core.StateLabelRunning, "[ben-queue ben:running]"},
		{"already projected", []string{"ben-queue", "ben:running"}, core.StateLabelRunning, "[ben-queue ben:running]"},
		{"park", []string{"ben-queue", "ben:running"}, core.StateLabelNeedsReview, "[ben-queue ben:needs-review]"},
		{"fail", []string{"ben-queue", "ben:running"}, core.StateLabelFailed, "[ben-queue ben:failed]"},
		// done: labels removed, PR comment stands (SPEC §9.3).
		{"done clears the projection", []string{"ben-queue", "ben:running"}, core.StateLabelNone, "[ben-queue]"},
		{"done with nothing to clear", []string{"ben-queue"}, core.StateLabelNone, "[ben-queue]"},
		{"duplicates all cleared", []string{"ben:claimed", "ben:failed"}, core.StateLabelRunning, "[ben:running]"},
		{"unrelated labels untouched", []string{"bug", "p1", "ben:claimed"}, core.StateLabelFailed, "[bug p1 ben:failed]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			issue := newFakeIssue(1, tt.existing...)
			issue.serve(f)

			// The caller's view is deliberately empty: the projection must
			// come from the tracker, not from whatever the orchestrator last
			// saw.
			err := f.adapter(t).SetStateLabels(context.Background(), core.Issue{Identifier: "1"}, tt.label)
			if err != nil {
				t.Fatalf("SetStateLabels: %v", err)
			}
			if got := fmt.Sprint(issue.currentLabels()); got != tt.want {
				t.Errorf("labels = %s, want %s", got, tt.want)
			}
		})
	}
}

// A caller holding an out-of-date view must not be able to leave a stale
// state label standing.
func TestSetStateLabelsIgnoresTheCallersStaleView(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue", "ben:claimed")
	issue.serve(f)

	// The orchestrator's copy predates the ben:claimed write.
	stale := core.Issue{Identifier: "1", Labels: []string{"ben-queue"}}
	if err := f.adapter(t).SetStateLabels(context.Background(), stale, core.StateLabelRunning); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	if got := fmt.Sprint(issue.currentLabels()); got != "[ben-queue ben:running]" {
		t.Errorf("labels = %s, want the stale ben:claimed cleared anyway", got)
	}
}

// Crash-safety: the new label goes on before the old one comes off, so a
// daemon killed mid-projection leaves an orphan (§9.10 step 2) rather than an
// issue that looks published and finished (§9.10 step 3).
func TestSetStateLabelsAddsBeforeItRemoves(t *testing.T) {
	f := newFakeGitHub(t)
	newFakeIssue(1, "ben-queue", "ben:claimed").serve(f)

	if err := f.adapter(t).SetStateLabels(context.Background(), core.Issue{Identifier: "1"}, core.StateLabelRunning); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}

	reqs, _ := f.snapshot()
	addAt, removeAt := -1, -1
	for i, r := range reqs {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.Path, "/labels") && addAt < 0:
			addAt = i
		case r.Method == "DELETE" && strings.Contains(r.Path, "/labels/") && removeAt < 0:
			removeAt = i
		}
	}
	if addAt < 0 || removeAt < 0 {
		t.Fatalf("expected both an add and a remove, got %v", reqs)
	}
	if addAt > removeAt {
		t.Errorf("removed the old label (request %d) before adding the new one (request %d)", removeAt, addAt)
	}
}

func TestSetStateLabelsRejectsLabelsOutsideTheProjection(t *testing.T) {
	f := newFakeGitHub(t)
	err := f.adapter(t).SetStateLabels(context.Background(), core.Issue{Identifier: "1"}, core.StateLabel("ben:urgent"))
	if !errors.Is(err, ErrUnknownStateLabel) {
		t.Fatalf("error = %v, want ErrUnknownStateLabel", err)
	}
	if reqs, _ := f.snapshot(); len(reqs) != 0 {
		t.Errorf("a refused projection must write nothing: %v", reqs)
	}
}

// Another actor removing the label first is the state we wanted anyway.
func TestSetStateLabelsToleratesAlreadyRemovedLabel(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben:claimed")
	issue.serve(f)
	f.handle("DELETE /api/v3/repos/{owner}/{repo}/issues/{number}/labels/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Label does not exist"}`, http.StatusNotFound)
	})

	if err := f.adapter(t).SetStateLabels(context.Background(), core.Issue{Identifier: "1"}, core.StateLabelRunning); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	if got := len(f.calls("POST", "/labels")); got != 1 {
		t.Errorf("the target label was written %d times, want 1", got)
	}
}

// SPEC §9.8 refreshes issues BEN is running, which Fetch's filters hide: a
// closed issue, or one whose queue label a human pulled mid-run.
func TestGetSeesWhatFetchFiltersOut(t *testing.T) {
	f := newFakeGitHub(t)
	closed := issueFixture(1) // no required label
	closed.State = gh.Ptr("closed")
	closed.Assignees = []*gh.User{{Login: gh.Ptr(testLogin)}}
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, closed)
	})
	f.serveIssues() // the queue read shows nothing

	adapter := f.adapter(t)
	queue, err := adapter.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(queue) != 0 {
		t.Fatalf("Fetch returned %v; the setup depends on it being empty", queue)
	}

	issue, err := adapter.Get(context.Background(), "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if issue.State != "closed" || issue.Identifier != "1" {
		t.Errorf("Get() = %+v, want the closed issue #1", issue)
	}
	if issue.Dispatchable {
		t.Error("Get must not present a dispatchability verdict")
	}
}

func TestGetReportsAMissingIssue(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})

	_, err := f.adapter(t).Get(context.Background(), "1")
	if !errors.Is(err, core.ErrIssueNotFound) {
		t.Fatalf("error = %v, want core.ErrIssueNotFound — reconciliation must tell gone from unreachable", err)
	}
}

// One deleted issue, every call that names it, and which of them turns the
// tracker's answer into a fact (#134, core.ErrIssueNotFound).
//
// **Anchored at the server, not at the adapter.** By default nothing is
// registered except the identity endpoint, so every call meets the same answer
// GitHub gives for an issue that is gone — a 404 over REST, and a null
// `repository.issue` in a 200 over GraphQL, which is how that API says it. A
// row's verdict is therefore the adapter's conclusion and nothing else, and the
// classification cannot be read off the same declaration that produces it.
//
// A row that would otherwise fail before reaching the request under test serves
// what it has to get past (`serve`). Every path that meets a 404 needs its own
// row: a call that fails at its first request proves nothing about a second one
// further in — which is why the queue read appears twice, once for the listing
// and once for the per-candidate dependency read behind it.
//
// The write rows are what make it a test rather than a restatement. Pushing the
// mapping one layer down — into issueEvents, which `Comment`'s occurrence lookup
// and `Claim`'s race arbitration also walk, or into isNotFound, which
// SetStateLabels asks about a label it *wants* to be missing — would satisfy
// every read row above and fail here. Fail-closed is the point: absence is what
// lets the loop forget a record, and a write's refusal cannot establish it
// (SPEC §9.10 step 3).
func TestOnlyTheIssueReadsClassifyAnAbsentIssue(t *testing.T) {
	// The issue number handleGraphQL's fixture asserts on.
	const id = "7"
	issue := core.Issue{Identifier: id}

	for _, tc := range []struct {
		name string
		call func(*Adapter) error
		// serve registers whatever this row needs *answered* before it can reach
		// the request whose 404 is under test. Empty means the default: only the
		// identity endpoint, so the row's first request is the one that fails.
		serve func(*fakeGitHub)
		// ready establishes the repository identity needed by RemotePR before
		// the absent-issue GraphQL observation under test.
		ready bool
		// classifies is whether this call answers with core.ErrIssueNotFound.
		classifies bool
	}{
		{name: "Get", classifies: true, call: func(a *Adapter) error {
			_, err := a.Get(context.Background(), id)
			return err
		}},
		{name: "ClaimHistory", classifies: true, call: func(a *Adapter) error {
			_, err := a.ClaimHistory(context.Background(), issue)
			return err
		}},
		{name: "ContentApproval", classifies: true, call: func(a *Adapter) error {
			_, err := a.ContentApproval(context.Background(), issue)
			return err
		}},
		{name: "RemotePR", classifies: true, ready: true, call: func(a *Adapter) error {
			_, err := a.RemotePR(context.Background(), core.RemotePRQuery{
				Issue: issue, Repository: remotePRRepositoryIdentity, Branch: "ben/7",
			})
			return err
		}},
		// The reads that do not name one issue. Their 404 is the repository's or
		// the credential's, so it cannot be a fact about an issue — and a list
		// read has no per-issue channel to report one on anyway.
		{name: "Fetch", call: func(a *Adapter) error {
			_, err := a.Fetch(context.Background())
			return err
		}},
		{
			// The queue read's *other* 404, and the one the row above cannot
			// reach: the per-candidate dependency read, which does name an issue.
			// The listing is served so the walk gets that far, and the blocked-by
			// endpoint is the request that fails.
			//
			// It stays unclassified for a reason the fan-out makes concrete. This
			// error surfaces as `Fetch`'s, so classifying it would say "the queue
			// is gone" over one candidate — and the queue read already fails
			// closed here on purpose, because an unknown dependency must never
			// read as "no dependencies" and release work early.
			name: "Fetch, on the blocker read that names a candidate",
			serve: func(f *fakeGitHub) {
				// TotalBlockedBy > 0, or the summary settles the verdict for free
				// and the dependency read never happens (mayHaveBlockers).
				f.serveIssues(withBlockedBy(issueFixture(7, "ben-queue"), 1, 1))
			},
			call: func(a *Adapter) error {
				_, err := a.Fetch(context.Background())
				return err
			},
		},
		{name: "ClaimedByPrincipal", call: func(a *Adapter) error {
			// The recovery read (§9.10 step 1). Same listing as HeldClaims and
			// the same answer, cache posture aside: both are asked about the
			// principal, not about an issue.
			_, err := a.ClaimedByPrincipal(context.Background())
			return err
		}},
		{name: "HeldClaims", call: func(a *Adapter) error {
			_, err := a.HeldClaims(context.Background())
			return err
		}},
		{name: "FindPR", call: func(a *Adapter) error {
			// Asks about a *branch*: the request names the repository and a head
			// ref, and never the issue at all.
			_, err := a.FindPR(context.Background(), issue, "ben/7")
			return err
		}},
		{name: "Claim", call: func(a *Adapter) error {
			_, err := a.Claim(context.Background(), issue)
			return err
		}},
		{name: "Release", call: func(a *Adapter) error {
			return a.Release(context.Background(), issue)
		}},
		{name: "SetStateLabels", call: func(a *Adapter) error {
			return a.SetStateLabels(context.Background(), issue, core.StateLabelClaimed)
		}},
		{name: "Comment", call: func(a *Adapter) error {
			return a.Comment(context.Background(), issue, core.MilestoneComment{Milestone: core.MilestoneClaimed})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handleGraphQL(func() graphQLIssue { return graphQLIssue{absent: true} })
			if tc.ready {
				f.serveRepoWithCloneURL(remotePRCloneURL)
			}
			if tc.serve != nil {
				tc.serve(f)
			}

			a := f.adapter(t)
			if tc.ready {
				if err := a.Ready(t.Context()); err != nil {
					t.Fatalf("Ready: %v", err)
				}
				f.reset()
			}
			err := tc.call(a)
			if err == nil {
				t.Fatalf("%s succeeded for an issue the tracker does not have", tc.name)
			}
			if got := errors.Is(err, core.ErrIssueNotFound); got != tc.classifies {
				if tc.classifies {
					t.Fatalf("%s error = %v, want core.ErrIssueNotFound: this read names one issue, so the caller must be able to route on absence", tc.name, err)
				}
				t.Fatalf("%s error = %v, which carries core.ErrIssueNotFound: concluding \"gone\" here drops a claim that may still be standing", tc.name, err)
			}
		})
	}
}

// SPEC §9.10 step 1: recovery candidates are everything the principal holds,
// unfiltered. The claims most in need of cleanup are exactly the ones that
// have left the queue partition, so a queue-shaped fetch would drop them.
func TestClaimedByPrincipalIsUnfiltered(t *testing.T) {
	f := newFakeGitHub(t)
	closed := issueFixture(1, "ben-queue") // merged PR closed it
	closed.State = gh.Ptr("closed")
	closed.Assignees = []*gh.User{{Login: gh.Ptr(testLogin)}}
	deLabeled := issueFixture(2) // a human pulled the queue label mid-run
	deLabeled.Assignees = []*gh.User{{Login: gh.Ptr(testLogin)}}

	f.handle("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, []*gh.Issue{closed, deLabeled})
	})

	issues, err := f.adapter(t).ClaimedByPrincipal(context.Background())
	if err != nil {
		t.Fatalf("ClaimedByPrincipal: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d candidates, want the closed one and the de-labeled one", len(issues))
	}
	for _, iss := range issues {
		if iss.Dispatchable {
			t.Errorf("issue %s carries a dispatchability verdict; recovery routes by the §9.10 table instead", iss.Identifier)
		}
	}

	list := f.callsExact("GET", "/api/v3/repos/acme/widgets/issues")[0]
	for _, want := range []string{"assignee=" + testLogin, "state=all"} {
		if !strings.Contains(list.Query, want) {
			t.Errorf("recovery query %q missing %q", list.Query, want)
		}
	}
	if strings.Contains(list.Query, "labels=") {
		t.Errorf("recovery query %q filters by label; a de-labeled claim still needs cleanup", list.Query)
	}
}

// Both recovery reads bypass the conditional cache: an answer served from our
// own ETag cache attests to nothing about the world we are reconstructing
// (SPEC §8.2).
func TestRecoveryReadsBypassTheCache(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.assign(testLogin)
	issue.serve(f)
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, []*gh.Issue{issue.snapshotIssue()})
	})
	adapter := f.adapter(t)

	for range 2 {
		if _, err := adapter.ClaimedByPrincipal(context.Background()); err != nil {
			t.Fatalf("ClaimedByPrincipal: %v", err)
		}
		if _, err := adapter.ClaimHistory(context.Background(), core.Issue{Identifier: "1"}); err != nil {
			t.Fatalf("ClaimHistory: %v", err)
		}
	}

	for _, path := range []string{"/api/v3/repos/acme/widgets/issues", "/api/v3/repos/acme/widgets/issues/1/events"} {
		calls := f.callsExact("GET", path)
		if len(calls) != 2 {
			t.Fatalf("%s called %d times, want 2", path, len(calls))
		}
		for i, c := range calls {
			if c.IfNoneMatch != "" {
				t.Errorf("%s call %d was conditional (If-None-Match: %s)", path, i, c.IfNoneMatch)
			}
			if !c.Billed {
				t.Errorf("%s call %d never reached the origin", path, i)
			}
		}
	}
}

// SPEC §9.10 step 2: the ordered evidence that separates a claim killed
// before label projection from one that ran to a label-clearing terminal.
func TestClaimHistoryNormalizesTheChangeLog(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.serve(f)
	issue.assign(testLogin)
	issue.label("ben:claimed")
	issue.unlabel("ben:claimed")
	issue.unassign("someone-else")
	// Noise the projection must drop.
	issue.arbitraryEvent("subscribed")
	issue.arbitraryEvent("renamed")

	history, err := f.adapter(t).ClaimHistory(context.Background(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("ClaimHistory: %v", err)
	}

	var got []string
	var lastID int64
	for i, ev := range history {
		if ev.At.IsZero() {
			t.Errorf("event %+v has no timestamp", ev)
		}
		// The id is what orders events the coarse timestamps cannot, and
		// what names the claim cycle in a milestone marker (SPEC §8.2, §8.4).
		if ev.ID == 0 {
			t.Errorf("event %+v carries no id", ev)
		}
		if i > 0 && ev.ID <= lastID {
			t.Errorf("event %d id %d does not follow %d; history must be ordered by (at, id)", i, ev.ID, lastID)
		}
		lastID = ev.ID
		got = append(got, string(ev.Kind)+":"+ev.Subject)
	}
	want := "[assigned:ben-bot labeled:ben:claimed unlabeled:ben:claimed unassigned:someone-else]"
	if fmt.Sprint(got) != want {
		t.Errorf("history = %v, want %s in tracker order with non-claim events dropped", got, want)
	}
}

// Every timestamp in the fixture is the same second, so ordering that holds
// here is ordering by id — the case §8.2 calls out.
func TestClaimHistoryOrdersBySameSecondEventID(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.serve(f)
	issue.assign(testLogin)
	issue.label("ben:claimed")
	issue.unlabel("ben:claimed")
	issue.label("ben:running")

	// Served newest-first: the adapter must not trust the endpoint's order.
	issue.reverseEvents()

	history, err := f.adapter(t).ClaimHistory(context.Background(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("ClaimHistory: %v", err)
	}
	var got []string
	for _, ev := range history {
		got = append(got, string(ev.Kind)+":"+ev.Subject)
	}
	want := "[assigned:ben-bot labeled:ben:claimed unlabeled:ben:claimed labeled:ben:running]"
	if fmt.Sprint(got) != want {
		t.Errorf("history = %v, want %s — same-second events ordered by id", got, want)
	}
}

// SPEC §9.10 step 3: recovery replays the *ordered label events*, and the order
// it assumes is the one SetStateLabels produces — the new label on before the
// old one comes off. This drives the adapter's own projection and reads the log
// back, rather than scripting the events, because scripting them would assert
// the fixture.
//
// The anchor for internal/fake's change log (AGENTS.md: a fake that restates a
// guarantee needs it pinned at an independent boundary). If the adapter ever
// removed first, recovery would read a `done` projection — a run abandoned as if
// it had succeeded — and no orchestrator test could see it, because the fake
// would have been changed to agree.
func TestSetStateLabelsLogsTheAddBeforeTheRemove(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.serve(f)
	issue.assign(testLogin)
	adapter := f.adapter(t)
	ctx := context.Background()

	for _, label := range []core.StateLabel{core.StateLabelClaimed, core.StateLabelRunning} {
		if err := adapter.SetStateLabels(ctx, core.Issue{Identifier: "1"}, label); err != nil {
			t.Fatalf("SetStateLabels(%s): %v", label, err)
		}
	}

	history, err := adapter.ClaimHistory(ctx, core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("ClaimHistory: %v", err)
	}
	var got []string
	for _, ev := range history {
		got = append(got, string(ev.Kind)+":"+ev.Subject)
	}
	want := "[assigned:ben-bot labeled:ben:claimed labeled:ben:running unlabeled:ben:claimed]"
	if fmt.Sprint(got) != want {
		t.Errorf("history = %v, want %s — the add precedes the remove, so an interrupted "+
			"projection leaves two ben:* labels standing rather than none", got, want)
	}

	// The re-projection of a label already standing writes no entry, which is
	// what makes recovery's own repair free (SPEC §9.10 step 4).
	before := len(history)
	if err := adapter.SetStateLabels(ctx, core.Issue{Identifier: "1"}, core.StateLabelRunning); err != nil {
		t.Fatalf("re-projecting: %v", err)
	}
	again, err := adapter.ClaimHistory(ctx, core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("ClaimHistory: %v", err)
	}
	if len(again) != before {
		t.Errorf("re-projecting ben:running added %d events; an idempotent add writes none", len(again)-before)
	}
}

// Invariant 6: the provider's raw payload stops at the adapter. ClaimEvent
// carries exactly the fields the core is allowed to see, over the six kinds
// §8.2 names — no more, and nothing else from the provider's open-ended
// vocabulary.
func TestClaimEventIsAClosedShape(t *testing.T) {
	kinds := map[string]core.ClaimEventKind{
		"assigned":   core.ClaimEventAssigned,
		"unassigned": core.ClaimEventUnassigned,
		"labeled":    core.ClaimEventLabeled,
		"unlabeled":  core.ClaimEventUnlabeled,
		"closed":     core.ClaimEventClosed,
		"reopened":   core.ClaimEventReopened,
	}
	for name, want := range kinds {
		ev := &gh.IssueEvent{Event: gh.Ptr(name), Assignee: &gh.User{Login: gh.Ptr("x")}, Label: &gh.Label{Name: "y"}}
		kind, subject, ok := claimEventKind(ev)
		if !ok || kind != want {
			t.Errorf("%q projected to %q (ok=%v), want %q", name, kind, ok, want)
		}
		// The state pair names neither an assignee nor a label, and must not
		// borrow one from a payload that happens to carry both.
		if (want == core.ClaimEventClosed || want == core.ClaimEventReopened) && subject != "" {
			t.Errorf("%q carries subject %q; state events name no subject", name, subject)
		}
	}
	for _, name := range []string{"referenced", "milestoned", "subscribed", "renamed", "locked", ""} {
		if _, _, ok := claimEventKind(&gh.IssueEvent{Event: gh.Ptr(name)}); ok {
			t.Errorf("%q leaked into the claim history", name)
		}
	}
}

// SPEC §9.8: the held-claim sweep releases a retained `done` claim on the
// `closed` **event**. A close-and-reopen between two sweeps leaves the issue
// reading open, so the event is the only surviving evidence that it closed —
// the case that made keying release on current state lose the window
// entirely (BUILD.md decision 14).
func TestClaimHistoryKeepsTheCloseAfterAReopen(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.serve(f)
	issue.assign(testLogin)
	issue.label("ben:claimed")
	issue.unlabel("ben:claimed") // done: labels cleared, claim retained
	issue.closeIssue()           // a human merged the PR
	issue.reopenIssue()          // and reopened before the next sweep looked

	adapter := f.adapter(t)
	current, err := adapter.Get(context.Background(), "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if current.State != "open" {
		t.Fatalf("state = %q; this fixture is only interesting while the reopen stands", current.State)
	}

	history, err := adapter.ClaimHistory(context.Background(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("ClaimHistory: %v", err)
	}
	var got []string
	for _, ev := range history {
		got = append(got, string(ev.Kind)+":"+ev.Subject)
	}
	want := "[assigned:ben-bot labeled:ben:claimed unlabeled:ben:claimed closed: reopened:]"
	if fmt.Sprint(got) != want {
		t.Errorf("history = %v, want %s — the close must outlive the reopen", got, want)
	}
}

// SPEC §8.3, §9.8: the sweep triggers a change-log read on the issue's opaque
// revision, and this is the case that rules `updated_at` out as that token.
// GitHub timestamps are second-granularity (§8.4), so a close and a reopen
// landing in the same second leave `updated_at` untouched — every timestamp in
// this fake is that same second, which is the fixture. The revision MUST still
// move, or the sweep never looks at the log and the claim is held forever.
func TestRevisionMovesOnASameSecondCloseAndReopen(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.assign(testLogin)
	issue.serve(f)
	adapter := f.adapter(t)

	before, err := adapter.Get(context.Background(), "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if before.Revision == "" {
		t.Fatal("normalized issue carries no revision; the sweep has no trigger")
	}

	issue.closeIssue()
	issue.reopenIssue()

	after, err := adapter.Get(context.Background(), "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != "open" || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("fixture invalid: state %q, updated_at moved %v→%v — this test is only "+
			"meaningful while the reopen leaves state open and the timestamp unmoved",
			after.State, before.UpdatedAt, after.UpdatedAt)
	}
	if after.Revision == before.Revision {
		t.Errorf("revision unchanged (%s) across a same-second close-and-reopen; a token that "+
			"cannot express this leaves the held claim undiscoverable (SPEC §9.8)", after.Revision)
	}

	// And it is stable when nothing happens, or every idle tick would buy a
	// history read and the O(held) cost would come back through the trigger.
	again, err := adapter.Get(context.Background(), "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.Revision != after.Revision {
		t.Errorf("revision moved without a change: %s → %s", after.Revision, again.Revision)
	}
}

// SPEC §8.3's exhaustiveness, enforced over *every* field rather than the ones a
// table remembered. revisionProjection is what token() can see, so this pins the
// type: any field added to it fails here, whether or not anyone wrote a row for
// it. The behavioral table below documents why each element is in or out; this is
// what makes the exclusion airtight, because no enumeration of unwanted fields
// can cover a provider payload that grows.
func TestRevisionProjectionTypeIsClosed(t *testing.T) {
	want := []string{"State", "StateReason", "UpdatedAt"}

	typ := reflect.TypeOf(revisionProjection{})
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("revisionProjection = %v, want exactly %v.\n"+
			"Adding a field widens the §8.3 projection: it buys a change-log read on every edit "+
			"of that field, in the one place per-issue reads were ruled out (§9.8). Removing one "+
			"reopens a detection hole — see the projection's doc comment for what each covers. "+
			"Either way this is a spec change, not an implementation detail.", got, want)
	}
}

// SPEC §8.3: the projection is **exhaustive** — the token derives from `state`,
// `state_reason`, and `updated_at`, and from nothing else. Both directions are
// asserted field by field, because each is a real failure: a subset reduces to
// the second-granularity bug the token was introduced to fix, and a superset
// buys a change-log read on every rename, in the one place per-issue reads were
// ruled out (§9.8). This is the semantic half; the structural half is
// TestRevisionProjectionTypeIsClosed, which does not depend on remembering to
// add a row.
func TestRevisionProjectionIsExact(t *testing.T) {
	base := func() *gh.Issue {
		iss := issueFixture(1, "ben-queue")
		iss.Assignees = []*gh.User{{Login: gh.Ptr(testLogin)}}
		iss.Comments = gh.Ptr(2)
		iss.Locked = gh.Ptr(false)
		return iss
	}
	// A different second, so an in-projection timestamp change is expressible.
	laterSecond := gh.Timestamp{Time: base().GetUpdatedAt().Add(time.Second)}

	tests := []struct {
		field    string
		mutate   func(*gh.Issue)
		wantMove bool
		why      string
	}{
		// Off-projection: none of these can mean the issue went terminal, and
		// §9.8's other rules read what they need from the list response.
		{"title", func(i *gh.Issue) { i.Title = gh.Ptr("renamed") }, false,
			"a rename would buy a change-log read per edit"},
		{"body", func(i *gh.Issue) { i.Body = gh.Ptr("edited") }, false,
			"an issue body is rewritten freely and says nothing about state"},
		{"labels", func(i *gh.Issue) { i.Labels = nil }, false,
			"the partition rule reads labels off the same response, not through this token"},
		{"assignees", func(i *gh.Issue) { i.Assignees = nil }, false,
			"ownership is read from presence in the sweep response"},
		{"comments", func(i *gh.Issue) { i.Comments = gh.Ptr(3) }, false,
			"a comment is the churn the narrow projection exists to ignore"},
		{"locked", func(i *gh.Issue) { i.Locked = gh.Ptr(true) }, false,
			"locking is not a lifecycle transition"},
		{"html url", func(i *gh.Issue) { i.HTMLURL = gh.Ptr("https://example.invalid/1") }, false,
			"off-projection, and a transfer is caught by the unfiltered recovery read"},

		// The projection itself, each element load-bearing for a different case.
		{"state", func(i *gh.Issue) { i.State = gh.Ptr("closed") }, true,
			"a close that stands"},
		{"state_reason", func(i *gh.Issue) { i.StateReason = gh.Ptr("reopened") }, true,
			"a close a reopen has undone — the case updated_at cannot express"},
		{"updated_at", func(i *gh.Issue) { i.UpdatedAt = &laterSecond }, true,
			"a repeated reopen across a second boundary"},
	}

	before := projectRevision(base()).token()
	for _, tc := range tests {
		t.Run(tc.field, func(t *testing.T) {
			mutated := base()
			tc.mutate(mutated)
			if moved := projectRevision(mutated).token() != before; moved != tc.wantMove {
				verb := map[bool]string{true: "moved", false: "held still"}
				t.Errorf("revision %s on a %s change, want it to %s: %s (SPEC §8.3)",
					verb[moved], tc.field, map[bool]string{true: "move", false: "hold still"}[tc.wantMove], tc.why)
			}
		})
	}
}

// The premise the exhaustive projection rests on, end to end: §9.8's partition
// rule reads labels off the sweep response, so a label pulled in the same second
// as the last observation is caught without the token moving. If this ever stops
// holding, the narrow projection stops being safe and the §8.3 MUST has to widen
// with it — so it is asserted, not assumed.
func TestSweepResponseShowsAPulledLabelWithoutMovingTheRevision(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.assign(testLogin)
	issue.serve(f)
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, []*gh.Issue{issue.snapshotIssue()})
	})
	adapter := f.adapter(t)

	before, err := adapter.HeldClaims(context.Background())
	if err != nil {
		t.Fatalf("HeldClaims: %v", err)
	}

	// A label pulled in the same second as the last observation: outside the
	// projection, and every timestamp in this fake is that one second.
	issue.removeLabel("ben-queue")

	after, err := adapter.HeldClaims(context.Background())
	if err != nil {
		t.Fatalf("HeldClaims: %v", err)
	}
	if after[0].Revision != before[0].Revision {
		t.Errorf("revision moved on a same-second label change (%s → %s); the projection is "+
			"state, state reason, and updated_at — widening it spends a change-log read per edit "+
			"(SPEC §8.3)", before[0].Revision, after[0].Revision)
	}
	// Sufficiency: §9.8's partition rule reads labels off this very response, so
	// the unmoved token costs nothing. If this ever fails, the narrow projection
	// stops being safe and the MUST has to widen with it.
	if containsFold(after[0].Labels, "ben-queue") {
		t.Errorf("labels = %v; the sweep response must show the pulled label, since the "+
			"partition rule reads it here rather than through the revision", after[0].Labels)
	}
}

// SPEC §9.8: the sweep's cost is set by the read shape, not by how many claims
// are held. One conditional list read per tick answers for all of them, and
// revalidation costs no core budget — which is what makes release affordable at
// a review backlog the daemon does not control.
func TestHeldClaimsSweepIsOneConditionalRead(t *testing.T) {
	f := newFakeGitHub(t)
	merged := issueFixture(1, "ben-queue") // closed under a merged PR
	merged.State = gh.Ptr("closed")
	deLabeled := issueFixture(2)             // required label pulled while published
	awaiting := issueFixture(3, "ben-queue") // still awaiting review
	held := []*gh.Issue{merged, deLabeled, awaiting}
	for _, iss := range held {
		iss.Assignees = []*gh.User{{Login: gh.Ptr(testLogin)}}
	}
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, held)
	})

	adapter := f.adapter(t)
	const sweeps = 3
	for i := range sweeps {
		issues, err := adapter.HeldClaims(context.Background())
		if err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
		if len(issues) != len(held) {
			t.Fatalf("sweep %d returned %d claims, want %d", i, len(issues), len(held))
		}
		for _, iss := range issues {
			if iss.Dispatchable {
				t.Errorf("issue %s carries a dispatchability verdict; the sweep asks a different question", iss.Identifier)
			}
			// The two fields §9.8's release rules read off this response: state
			// settles the ordinary close without touching the change log, and
			// the revision is the trigger that buys a ClaimHistory read when a
			// reopen has moved the state back.
			if iss.Revision == "" {
				t.Errorf("issue %s carries no revision; the sweep's change trigger has no input", iss.Identifier)
			}
		}
		if issues[0].State != "closed" {
			t.Errorf("held claim #1 state = %q, want closed — a merge must be visible in the sweep read itself", issues[0].State)
		}
	}

	calls := f.callsExact("GET", "/api/v3/repos/acme/widgets/issues")
	if len(calls) != sweeps {
		t.Fatalf("%d list requests for %d sweeps; the sweep must be one read, not one per claim", len(calls), sweeps)
	}
	billed := 0
	for _, c := range calls {
		if c.Billed {
			billed++
		}
	}
	if billed != 1 {
		t.Errorf("%d sweeps over %d held claims billed %d requests, want 1 — the rest revalidate for free (SPEC §8.5)", sweeps, len(held), billed)
	}
	for _, want := range []string{"assignee=" + testLogin, "state=all"} {
		if !strings.Contains(calls[0].Query, want) {
			t.Errorf("sweep query %q missing %q", calls[0].Query, want)
		}
	}
	// A queue-shaped filter would drop exactly the two claims that need
	// releasing: the closed one and the de-labeled one.
	if strings.Contains(calls[0].Query, "labels=") {
		t.Errorf("sweep query %q filters by label", calls[0].Query)
	}
}

// The sweep is conditional and recovery is not, and they ask the same question
// of the same URL. Two methods rather than one with a flag is what keeps the
// posture from leaking: a cached answer would attest to nothing about the world
// recovery is reconstructing (SPEC §8.2).
func TestSweepReadNeverServesRecoveryFromCache(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.assign(testLogin)
	issue.serve(f)
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, []*gh.Issue{issue.snapshotIssue()})
	})
	adapter := f.adapter(t)

	// Steady state: the second sweep revalidates against the cached entry.
	for range 2 {
		if _, err := adapter.HeldClaims(context.Background()); err != nil {
			t.Fatalf("HeldClaims: %v", err)
		}
	}
	if calls := f.callsExact("GET", "/api/v3/repos/acme/widgets/issues"); len(calls) != 2 || calls[1].IfNoneMatch == "" {
		t.Fatalf("second sweep was not conditional: %+v", calls)
	}

	f.reset()
	if _, err := adapter.ClaimedByPrincipal(context.Background()); err != nil {
		t.Fatalf("ClaimedByPrincipal: %v", err)
	}
	calls := f.callsExact("GET", "/api/v3/repos/acme/widgets/issues")
	if len(calls) != 1 {
		t.Fatalf("%d recovery reads, want 1", len(calls))
	}
	if calls[0].IfNoneMatch != "" {
		t.Errorf("recovery read was conditional (If-None-Match: %s) after the sweep warmed the cache", calls[0].IfNoneMatch)
	}
	if !calls[0].Billed {
		t.Error("recovery read never reached the origin")
	}
}

// claimedIssue sets up an issue mid-claim: assigned, ben:claimed projected —
// the state every milestone comment is posted from (SPEC §9.2 writes the
// label before the comment).
func claimedIssue(t *testing.T, f *fakeGitHub) *fakeIssue {
	t.Helper()
	issue := newFakeIssue(1, "ben-queue")
	issue.serve(f)
	issue.assign(testLogin)
	issue.label("ben:claimed")
	return issue
}

func TestCommentPostsTheMilestone(t *testing.T) {
	f := newFakeGitHub(t)
	claimedIssue(t, f)

	comment := core.MilestoneComment{Milestone: core.MilestoneClaimed}
	if err := f.adapter(t).Comment(context.Background(), core.Issue{Identifier: "1"}, comment); err != nil {
		t.Fatalf("Comment: %v", err)
	}

	posts := f.calls("POST", "/comments")
	if len(posts) != 1 {
		t.Fatalf("posted %d comments, want 1", len(posts))
	}
	// Daemon identity is adapter-side knowledge (SPEC §8.4).
	for _, want := range []string{"BEN claimed this issue", "@" + testLogin, "ben-1a2b3c4d"} {
		if !strings.Contains(posts[0].Body, want) {
			t.Errorf("comment body %q missing %q", posts[0].Body, want)
		}
	}
	if !strings.Contains(posts[0].Body, "<!-- ben:milestone kind=claimed occurrence=") {
		t.Errorf("comment body %q carries no milestone marker", posts[0].Body)
	}
}

// SPEC §8.4: re-issuing a milestone is a no-op, which is what lets recovery
// complete an interrupted terminal projection.
func TestCommentIsIdempotentPerOccurrence(t *testing.T) {
	f := newFakeGitHub(t)
	issue := claimedIssue(t, f)
	adapter := f.adapter(t)

	claimed := core.MilestoneComment{Milestone: core.MilestoneClaimed}
	for range 3 {
		if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, claimed); err != nil {
			t.Fatalf("Comment: %v", err)
		}
	}
	if got := issue.currentComments(); len(got) != 1 {
		t.Fatalf("three claim milestones left %d comments, want 1", len(got))
	}

	// A different kind is a different milestone, not a duplicate.
	issue.label("ben:failed")
	failed := core.MilestoneComment{Milestone: core.MilestoneFailed, Reason: core.FailureStalled}
	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, failed); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if got := issue.currentComments(); len(got) != 2 {
		t.Fatalf("a second milestone kind left %d comments, want 2", len(got))
	}
}

// The first direction the occurrence key must satisfy (SPEC §8.4): a human
// re-queues by removing the label, which retains the assignment — so a second
// failure lands in the *same claim cycle* and still owes its own comment.
// Keying on the claim cycle fails this.
func TestCommentPostsASecondNeedsReviewAfterRequeue(t *testing.T) {
	f := newFakeGitHub(t)
	issue := claimedIssue(t, f)
	adapter := f.adapter(t)
	parked := core.MilestoneComment{Milestone: core.MilestoneNeedsReview, Detail: "agent produced no commits"}

	issue.label("ben:needs-review")
	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, parked); err != nil {
		t.Fatalf("first park: %v", err)
	}

	// A human re-queues: label off, assignment untouched. BEN runs it again
	// and parks it again.
	issue.unlabel("ben:needs-review")
	issue.label("ben:needs-review")
	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, parked); err != nil {
		t.Fatalf("second park: %v", err)
	}

	got := issue.currentComments()
	if len(got) != 2 {
		t.Fatalf("got %d needs-review comments across a re-queue, want 2", len(got))
	}
	if got[0] == got[1] {
		t.Error("both parks share a marker; each labeling is its own occurrence")
	}
	if fmt.Sprint(issue.currentAssignees()) != "["+testLogin+"]" {
		t.Error("the re-queue was supposed to retain the assignment — the fixture no longer tests one claim cycle")
	}
}

// The opposite direction (SPEC §8.4): §9.3 maps preparing, verifying and
// backoff onto ben:claimed too, so the label cycles within one claim. Keying
// on every transition would repost the claim milestone on each re-entry.
func TestCommentPostsOneClaimedAcrossLabelReentry(t *testing.T) {
	f := newFakeGitHub(t)
	issue := claimedIssue(t, f)
	adapter := f.adapter(t)
	claimed := core.MilestoneComment{Milestone: core.MilestoneClaimed}

	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, claimed); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	// running, then back to claimed for backoff — same claim throughout.
	issue.unlabel("ben:claimed")
	issue.label("ben:running")
	issue.unlabel("ben:running")
	issue.label("ben:claimed")
	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, claimed); err != nil {
		t.Fatalf("Comment after re-entry: %v", err)
	}

	if got := issue.currentComments(); len(got) != 1 {
		t.Fatalf("ben:claimed re-entry left %d claim comments, want 1 — that is the per-tick noise §8.4 forbids", len(got))
	}
}

// A later claim on the same issue is a new occurrence, not a duplicate
// suppressed by the previous cycle's marker.
func TestCommentPostsAgainInALaterClaimCycle(t *testing.T) {
	f := newFakeGitHub(t)
	issue := claimedIssue(t, f)
	adapter := f.adapter(t)

	claimed := core.MilestoneComment{Milestone: core.MilestoneClaimed}
	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, claimed); err != nil {
		t.Fatalf("first cycle Comment: %v", err)
	}

	// Released and re-claimed: a fresh cycle, freshly projected.
	issue.unlabel("ben:claimed")
	issue.unassign(testLogin)
	issue.assign(testLogin)
	issue.label("ben:claimed")

	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, claimed); err != nil {
		t.Fatalf("second cycle Comment: %v", err)
	}
	got := issue.currentComments()
	if len(got) != 2 {
		t.Fatalf("got %d comments across two claim cycles, want 2", len(got))
	}
}

// The publish milestone belongs to the transition that cleared the
// projection at done (SPEC §8.4).
func TestCommentPublishedAnchorsOnTheClearingTransition(t *testing.T) {
	f := newFakeGitHub(t)
	issue := claimedIssue(t, f)
	adapter := f.adapter(t)
	published := core.MilestoneComment{Milestone: core.MilestonePublished, PRURL: "https://example.test/pull/9"}

	// Before the projection is cleared there is nothing to anchor to.
	err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, published)
	if !errors.Is(err, ErrNoMilestoneOccurrence) {
		t.Fatalf("error = %v, want ErrNoMilestoneOccurrence before done clears the labels", err)
	}

	issue.unlabel("ben:claimed") // done
	for range 2 {
		if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, published); err != nil {
			t.Fatalf("Comment: %v", err)
		}
	}
	if got := issue.currentComments(); len(got) != 1 {
		t.Fatalf("got %d publish comments, want 1", len(got))
	}
}

// Suppressing duplicates is the whole job, so the read that decides it cannot
// be answered from our own cache.
func TestCommentMarkerLookupBypassesTheCache(t *testing.T) {
	f := newFakeGitHub(t)
	claimedIssue(t, f)
	adapter := f.adapter(t)

	claimed := core.MilestoneComment{Milestone: core.MilestoneClaimed}
	for range 2 {
		if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, claimed); err != nil {
			t.Fatalf("Comment: %v", err)
		}
	}
	reads := f.calls("GET", "/comments")
	if len(reads) != 2 {
		t.Fatalf("read the comment list %d times, want 2", len(reads))
	}
	for i, r := range reads {
		if r.IfNoneMatch != "" || !r.Billed {
			t.Errorf("comment read %d was conditional or cache-served: %+v", i, r)
		}
	}
}

// SPEC §9.10 gate 3: an issue assigned to us whose assignment event the log
// has lost must still be parked *and* commented. Anchoring needs-review on
// its own labeling — not on the claim cycle — is what makes that reachable.
func TestCommentNeedsReviewWithoutAnAssignmentEvent(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.serve(f)
	issue.assign(testLogin)
	issue.label("ben:needs-review")
	// Event retention or a transfer lost the assignment; the label stands.
	issue.dropAssignmentEvents()

	parked := core.MilestoneComment{Milestone: core.MilestoneNeedsReview, Detail: "claim cannot be accounted for"}
	if err := f.adapter(t).Comment(context.Background(), core.Issue{Identifier: "1"}, parked); err != nil {
		t.Fatalf("gate 3 must be able to comment: %v", err)
	}
	if got := issue.currentComments(); len(got) != 1 {
		t.Fatalf("got %d comments, want the needs-review milestone posted", len(got))
	}
}

// A milestone that cannot name its occurrence cannot be idempotent, so it is
// a refusal rather than a duplicate waiting to happen.
func TestCommentRefusesWithoutAnAnchoringTransition(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.serve(f)
	issue.assign(testLogin) // assigned, but nothing projected yet

	err := f.adapter(t).Comment(context.Background(), core.Issue{Identifier: "1"}, core.MilestoneComment{Milestone: core.MilestoneClaimed})
	if !errors.Is(err, ErrNoMilestoneOccurrence) {
		t.Fatalf("error = %v, want ErrNoMilestoneOccurrence", err)
	}
	if posts := f.calls("POST", "/comments"); len(posts) != 0 {
		t.Errorf("posted anyway: %v", posts)
	}
}

func TestCommentRejectsUnknownMilestones(t *testing.T) {
	f := newFakeGitHub(t)
	claimedIssue(t, f)

	err := f.adapter(t).Comment(context.Background(), core.Issue{Identifier: "1"}, core.MilestoneComment{Milestone: "progress"})
	if !errors.Is(err, ErrUnknownMilestone) {
		t.Fatalf("error = %v, want ErrUnknownMilestone", err)
	}
	if posts := f.calls("POST", "/comments"); len(posts) != 0 {
		t.Errorf("a refused milestone must post nothing: %v", posts)
	}
}

func TestFindPR(t *testing.T) {
	const branch = "ben/issue-1-9f2a"

	openPR := &gh.PullRequest{
		Number:  gh.Ptr(9),
		State:   gh.Ptr("open"),
		HTMLURL: gh.Ptr("https://github.com/acme/widgets/pull/9"),
		Head:    &gh.PullRequestBranch{Ref: gh.Ptr(branch)},
		Base:    &gh.PullRequestBranch{Ref: gh.Ptr("release/v2")},
	}
	mergedPR := &gh.PullRequest{
		Number:   gh.Ptr(8),
		State:    gh.Ptr("closed"),
		MergedAt: &gh.Timestamp{Time: time.Now()},
		HTMLURL:  gh.Ptr("https://github.com/acme/widgets/pull/8"),
		Head:     &gh.PullRequestBranch{Ref: gh.Ptr(branch)},
	}
	otherBranch := &gh.PullRequest{
		Number:  gh.Ptr(7),
		State:   gh.Ptr("open"),
		HTMLURL: gh.Ptr("https://github.com/acme/widgets/pull/7"),
		Head:    &gh.PullRequestBranch{Ref: gh.Ptr("ben/issue-2-0000")},
	}

	closedPR := &gh.PullRequest{
		Number:  gh.Ptr(6),
		State:   gh.Ptr("closed"),
		HTMLURL: gh.Ptr("https://github.com/acme/widgets/pull/6"),
		Head:    &gh.PullRequestBranch{Ref: gh.Ptr(branch)},
	}

	tests := []struct {
		name       string
		prs        []*gh.PullRequest
		wantNumber int
		wantState  string
	}{
		{"open pull request is the evidence", []*gh.PullRequest{openPR}, 9, "open"},
		{"open wins over an older merged one", []*gh.PullRequest{mergedPR, openPR}, 9, "open"},
		// SPEC §9.7 names an open PR. A rejected attempt on the same branch
		// must not satisfy a caller that only checks for non-nil.
		{"closed pull request is not evidence", []*gh.PullRequest{closedPR}, 0, ""},
		{"merged pull request is not evidence", []*gh.PullRequest{mergedPR}, 0, ""},
		{"no pull request for the branch", []*gh.PullRequest{otherBranch}, 0, ""},
		{"nothing at all", nil, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handle("GET /api/v3/repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, r, tt.prs)
			})

			pr, err := f.adapter(t).FindPR(context.Background(), core.Issue{Identifier: "1"}, branch)
			if err != nil {
				t.Fatalf("FindPR: %v", err)
			}
			if tt.wantNumber == 0 {
				if pr != nil {
					t.Fatalf("FindPR() = %+v, want nil", pr)
				}
				return
			}
			if pr == nil {
				t.Fatal("FindPR() = nil, want the published pull request")
			}
			if pr.Number != tt.wantNumber || pr.State != tt.wantState || pr.Branch != branch || pr.BaseBranch != "release/v2" {
				t.Errorf("FindPR() = %+v, want #%d %s from %s to release/v2", pr, tt.wantNumber, tt.wantState, branch)
			}
		})
	}
}

func TestFindPRRefusesMultipleExactHeadCandidatesRegardlessOfTargetOrOrder(t *testing.T) {
	const branch = "ben/issue-1-9f2a"
	pr := func(number int, target string) *gh.PullRequest {
		return &gh.PullRequest{
			Number: gh.Ptr(number), State: gh.Ptr("open"),
			HTMLURL: gh.Ptr(fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number)),
			Head:    &gh.PullRequestBranch{Ref: gh.Ptr(branch)},
			Base:    &gh.PullRequestBranch{Ref: gh.Ptr(target)},
		}
	}
	correct, wrong := pr(10, "main"), pr(11, "unprotected")
	for _, tc := range []struct {
		name  string
		pages [][]*gh.PullRequest
	}{
		{name: "correct then wrong", pages: [][]*gh.PullRequest{{correct, wrong}}},
		{name: "wrong then correct", pages: [][]*gh.PullRequest{{wrong, correct}}},
		{name: "second candidate on another page", pages: [][]*gh.PullRequest{{correct}, {wrong}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handle("GET /api/v3/repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
				page := 1
				if r.URL.Query().Get("page") == "2" {
					page = 2
				}
				if page < len(tc.pages) {
					w.Header().Set("Link", fmt.Sprintf(`<%s%s&page=%d>; rel="next"`, f.srv.URL, r.URL.RequestURI(), page+1))
				}
				writeJSON(w, r, tc.pages[page-1])
			})

			got, err := f.adapter(t).FindPR(context.Background(), core.Issue{Identifier: "1"}, branch)
			if !errors.Is(err, core.ErrPRAmbiguous) || got != nil {
				t.Fatalf("FindPR = %+v, %v; want nil, ErrPRAmbiguous", got, err)
			}
		})
	}
}

func TestFindPRRequiresABranch(t *testing.T) {
	f := newFakeGitHub(t)
	if _, err := f.adapter(t).FindPR(context.Background(), core.Issue{Identifier: "1"}, ""); err == nil {
		t.Fatal("FindPR without a branch must refuse rather than guess one")
	}
}

func TestIssueIdentifierMustBeANumber(t *testing.T) {
	f := newFakeGitHub(t)
	a := f.adapter(t)
	issue := core.Issue{Identifier: "PROJ-4"}

	if _, err := a.Claim(context.Background(), issue); err == nil {
		t.Error("Claim accepted a non-GitHub identifier")
	}
	if err := a.SetStateLabels(context.Background(), issue, core.StateLabelClaimed); err == nil {
		t.Error("SetStateLabels accepted a non-GitHub identifier")
	}
}
