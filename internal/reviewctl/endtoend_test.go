package reviewctl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

// One cycle over real HTTP, from the controller's entry point down to the JSON
// GitHub returns.
//
// The driver's own tests use a fake forge, which is where the interesting
// decisions live; what they cannot see is the layer between the wire and the
// reducer. A field read from the wrong JSON key — `state` instead of `merged`,
// `user.login` instead of `actor.login` — produces a perfectly sensible
// decision about a world that is not there.
func TestTheControllerDrivesOneCycleOverHTTP(t *testing.T) {
	g := &fakeGitHub{
		t:         t,
		assignees: []string{fxPrincipal},
		labels:    []string{fxQueue},
	}
	srv := httptest.NewServer(g)
	defer srv.Close()

	// The interrupted state the reducer has to finish: a review is published
	// and no route has been recorded, so `run` owes an unassignment and a
	// marker — and owes them without a reviewer configured.
	g.reviewBody = review.ReviewBody(review.ReviewMarker{
		Occurrence: 9001, Claim: fxEpoch, Approval: fxApproval,
		Head: head1, Base: base1, Verdict: review.VerdictChangesRequested,
	}, "findings")

	// No reviewer: the controller owes an unassignment and a marker for a review
	// that is already published, and it owes them without opening a round.
	c, err := New(Options{
		Policy: fxConfig(),
		Forge:  NewClient(srv.URL, "secret", "acme", "ben", 5*time.Second),
		Log:    t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background(), fxIssue); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(g.assignees) != 0 {
		t.Errorf("assignees = %v, want the claim handed back", g.assignees)
	}
	if !contains(g.labels, fxQueue) {
		t.Error("the human's required label was removed on a revise route")
	}
	if len(g.posted) != 1 {
		t.Fatalf("posted %d comments, want the one route marker", len(g.posted))
	}
	m, err := review.ParseRouteMarker(g.posted[0])
	if err != nil {
		t.Fatalf("the posted comment carries no route marker: %v\n%s", err, g.posted[0])
	}
	if m.Outcome != review.OutcomeRevise || m.Head != head1 || m.Occurrence != 9001 || m.Claim != fxEpoch {
		t.Errorf("route marker = %+v, want a revise of occurrence 9001 at head1 under epoch %d", m, fxEpoch)
	}
	if g.published != 0 {
		t.Errorf("published %d reviews; the one on the pull request was already there", g.published)
	}
}

func TestClientReadsTranslateTheWire(t *testing.T) {
	g := &fakeGitHub{t: t, assignees: []string{fxPrincipal}, labels: []string{fxQueue, "bug"}}
	srv := httptest.NewServer(g)
	defer srv.Close()

	c := NewClient(srv.URL, "secret", "acme", "ben", 5*time.Second)
	ctx := context.Background()

	issue, err := c.Issue(ctx, 11)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Number != 11 || issue.Closed || len(issue.Labels) != 2 || issue.Assignees[0] != fxPrincipal {
		t.Errorf("issue = %+v", issue)
	}

	pr, err := c.PullRequest(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != fxPRNumber || pr.URL != fxPRURL || pr.Branch != "ben/11" || pr.Head != head1 ||
		pr.Base != "main" || pr.BaseSHA != base1 || pr.Closed || pr.Merged {
		t.Errorf("pull request = %+v", pr)
	}

	events, err := c.Events(ctx, 11)
	if err != nil {
		t.Fatal(err)
	}
	if got := review.ClaimEpoch(events, fxPrincipal); got != fxEpoch {
		t.Errorf("claim epoch = %d, want %d — the actor and assignee keys must not be crossed", got, fxEpoch)
	}

	diff, err := c.Diff(ctx, base1, head1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(diff, "diff --git") {
		t.Errorf("diff = %q, want the raw patch rather than JSON", diff)
	}

	// A merged pull request must read as one; `state` alone says "closed" and
	// the reducer refuses either way, but only `merged` distinguishes them.
	g.merged = true
	pr, err = c.PullRequest(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Merged {
		t.Error("a merged pull request did not read as merged")
	}
}

// fakeGitHub serves the eleven endpoints the controller uses, and applies the
// writes so a second read reflects them — including the change-log events a
// real write produces, which is what the reducer resumes from.
type fakeGitHub struct {
	t *testing.T

	assignees  []string
	labels     []string
	reviewBody string
	merged     bool

	posted    []string
	published int

	unassignedAt time.Time
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")

	switch r.Method + " " + r.URL.Path {
	case "GET /repos/acme/ben/issues/11":
		g.write(w, map[string]any{
			"number": 11, "state": "open",
			"labels":    labelObjects(g.labels),
			"assignees": userObjects(g.assignees),
		})

	case "GET /repos/acme/ben/issues/11/comments":
		comments := []map[string]any{{
			"id": 1, "user": map[string]any{"login": fxTracker},
			"body": "**BEN published a pull request.**\n\n- pull request: " + fxPRURL +
				"\n- daemon: `d`\n<!-- ben:milestone kind=published occurrence=9001 -->\n",
			"created_at": "2026-08-21T12:05:00Z",
		}}
		// A posted comment is on the issue from then on, which is what makes
		// the route marker an idempotency key rather than a log line.
		for i, body := range g.posted {
			comments = append(comments, map[string]any{
				"id": 100 + i, "user": map[string]any{"login": fxController},
				"body": body, "created_at": "2026-08-21T12:30:00Z",
			})
		}
		g.write(w, comments)

	case "GET /repos/acme/ben/issues/11/events":
		events := []map[string]any{
			{"id": 900, "event": "labeled", "actor": map[string]any{"login": "a-human"},
				"label": map[string]any{"name": fxQueue}, "created_at": "2026-08-21T12:00:00Z"},
			{"id": fxEpoch, "event": "assigned", "actor": map[string]any{"login": fxTracker},
				"assignee": map[string]any{"login": fxPrincipal}, "created_at": "2026-08-21T12:01:00Z"},
			{"id": 9001, "event": "unlabeled", "actor": map[string]any{"login": fxTracker},
				"label": map[string]any{"name": "ben:claimed"}, "created_at": "2026-08-21T12:04:00Z"},
		}
		if !g.unassignedAt.IsZero() {
			events = append(events, map[string]any{
				"id": 9500, "event": "unassigned", "actor": map[string]any{"login": fxController},
				"assignee":   map[string]any{"login": fxPrincipal},
				"created_at": g.unassignedAt.UTC().Format(time.RFC3339),
			})
		}
		g.write(w, events)

	case "GET /repos/acme/ben/pulls/42":
		state := "open"
		if g.merged {
			state = "closed"
		}
		g.write(w, map[string]any{
			"number": fxPRNumber, "html_url": fxPRURL, "state": state, "merged": g.merged,
			"body": "Fixes #11",
			"head": map[string]any{"ref": "ben/11", "sha": head1},
			"base": map[string]any{"ref": "main", "sha": base1},
		})

	case "GET /repos/acme/ben/pulls/42/reviews":
		if g.reviewBody == "" {
			g.write(w, []map[string]any{})
			return
		}
		g.write(w, []map[string]any{{
			"id": 7, "user": map[string]any{"login": fxController}, "body": g.reviewBody,
			"commit_id": head1, "state": review.ReviewStateCommented,
			"submitted_at": "2026-08-21T12:10:00Z",
		}})

	case "GET /repos/acme/ben/compare/" + base1 + "..." + head1:
		io.WriteString(w, "diff --git a/x b/x\n")

	case "POST /repos/acme/ben/pulls/42/reviews":
		g.published++
		g.write(w, map[string]any{"id": 8})

	case "DELETE /repos/acme/ben/issues/11/assignees":
		var payload struct {
			Assignees []string `json:"assignees"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			g.t.Errorf("unassign payload: %v", err)
		}
		for _, login := range payload.Assignees {
			g.assignees = remove(g.assignees, login)
		}
		// A real unassignment lands in the change log too. Give it the same
		// second as the review: GitHub's timestamps cannot order these writes,
		// so recovery must use the event log's claim-relative order.
		g.unassignedAt = time.Date(2026, 8, 21, 12, 10, 0, 0, time.UTC)
		g.write(w, map[string]any{"number": 11})

	case "POST /repos/acme/ben/issues/11/comments":
		var payload struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			g.t.Errorf("comment payload: %v", err)
		}
		g.posted = append(g.posted, payload.Body)
		g.write(w, map[string]any{"id": 99})

	default:
		g.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "no route", http.StatusNotFound)
	}
}

func (g *fakeGitHub) write(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		g.t.Error(err)
	}
}

func labelObjects(names []string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

func userObjects(logins []string) []map[string]any {
	out := make([]map[string]any, 0, len(logins))
	for _, l := range logins {
		out = append(out, map[string]any{"login": l})
	}
	return out
}

func remove(all []string, want string) []string {
	var kept []string
	for _, v := range all {
		if !strings.EqualFold(v, want) {
			kept = append(kept, v)
		}
	}
	return kept
}

func contains(all []string, want string) bool {
	for _, v := range all {
		if v == want {
			return true
		}
	}
	return false
}
