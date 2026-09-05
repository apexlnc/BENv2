package reviewctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

// Forge is everything the controller does to GitHub, listed in one place so
// the permission model is readable as a type.
//
// Six reads and five writes, and the writes are the whole authority: publish a
// COMMENT review, remove one assignee, remove the required label, add an
// informational one, post a comment. There is no approve, no merge, no close,
// no required-label *addition* and no `ben:*` write, because there is no
// method for one — which is a stronger statement than a token scope, since a
// misconfigured token is an operator error and a missing method is a compile
// error.
type Forge interface {
	Issue(ctx context.Context, number int) (review.Issue, error)
	Comments(ctx context.Context, number int) ([]review.Comment, error)
	Events(ctx context.Context, number int) ([]review.Event, error)
	PullRequest(ctx context.Context, number int) (*review.PullRequest, error)
	Reviews(ctx context.Context, number int) ([]review.Review, error)
	Diff(ctx context.Context, base, head string) (string, error)

	PublishReview(ctx context.Context, pr int, commitID, body string) error
	Unassign(ctx context.Context, issue int, login string) error
	RemoveLabel(ctx context.Context, issue int, label string) error
	AddLabel(ctx context.Context, issue int, label string) error
	PostComment(ctx context.Context, issue int, body string) error

	// Candidates advances the reconciliation scan and returns its next bounded
	// slice: across a complete scan, every issue this controller may owe
	// something to, whether or not the required label still stands.
	Candidates(ctx context.Context, label string) ([]int, error)
}

// requestClass is the operation whose successful response can prove that a
// prior secondary-limit failure of the same operation has recovered. Keeping
// these explicit avoids deriving trust-relevant identity from dynamic paths.
type requestClass uint8

const (
	requestCandidates requestClass = iota + 1
	requestIssue
	requestComments
	requestEvents
	requestPullRequest
	requestReviews
	requestDiff
	requestPublishReview
	requestUnassign
	requestRemoveLabel
	requestAddLabel
	requestPostComment
)

type requestClassContextKey struct{}

func classFromContext(ctx context.Context) requestClass {
	class, _ := ctx.Value(requestClassContextKey{}).(requestClass)
	return class
}

// client is the stdlib REST implementation.
//
// Hand-rolled rather than go-github because `internal/arch` gives that
// dependency exactly one owner — `internal/tracker`, the daemon's adapter —
// and this binary is not the daemon. The alternative was widening a boundary
// rule (SPEC §3.6) for a client that needs eleven endpoints, no caching and no
// conditional requests, none of which this process lives long enough to
// benefit from. Rate discipline it does need — the daemon runs this client on
// every sweep for its whole lifetime — and that is gate.go (#239).
type client struct {
	http       *http.Client
	base       string // API root, no trailing slash
	credential func(context.Context) (string, error)
	owner      string
	repo       string
	gate       *gate
	now        func() time.Time
	log        func(string, ...any)

	// candidateMu makes the pagination cursor below one serial scan even if a
	// caller accidentally overlaps scheduled sweeps. Controller already
	// serializes them; keeping the invariant here makes the client complete.
	candidateMu sync.Mutex
	candidates  candidateScan
}

func NewClient(base, token, owner, repo string, timeout time.Duration) *client {
	return NewCredentialClient(base, func(context.Context) (string, error) { return token, nil }, owner, repo, timeout)
}

// NewCredentialClient resolves the controller credential for every request.
// A daemon keeps this client for its whole lifetime while an STS token does
// not; making freshness part of the request boundary prevents one token minted
// at startup from becoming every future sweep's permanent 401.
func NewCredentialClient(
	base string, credential func(context.Context) (string, error), owner, repo string, timeout time.Duration,
) *client {
	if credential == nil {
		credential = func(context.Context) (string, error) { return "", nil }
	}
	g := newGate(time.Now)
	return &client{
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// The redirect response is otherwise hidden by Client.Do. Learn
				// any exhausted window before deciding whether another exchange
				// may leave the process, then reserve that exchange separately.
				if req.Response != nil {
					g.observe(classFromContext(req.Context()), req.Response.StatusCode, req.Response.Header, nil)
				}
				// Preserve net/http's default redirect bound while making its
				// otherwise implicit follow-up requests visible to the gate.
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return g.acquire(req.Context())
			},
		},
		base:       strings.TrimSuffix(base, "/"),
		credential: credential,
		owner:      owner,
		repo:       repo,
		gate:       g,
		now:        time.Now,
		log:        func(string, ...any) {},
	}
}

// WithLog routes the client's own diagnostics — what a sweep excluded and why
// — and returns the client for assembly-time chaining. The default is silence,
// which is fine for reads but not for exclusions: a silently truncated sweep
// reads as "covered everything" (#239).
func (c *client) WithLog(log func(string, ...any)) *client {
	if log != nil {
		c.log = log
	}
	return c
}

// resetSweepBudget gives a discovery-free retry the same allowance an ordinary
// Candidates-led sweep receives. It is scheduling state, not forge authority,
// so it deliberately stays outside Forge.
func (c *client) resetSweepBudget() { c.gate.beginSweep() }

// There is deliberately no retry here. Event delivery is a wake-up and the
// scheduled sweep is the reconciler (#11), so a transient failure is already
// covered by a mechanism that has to exist anyway; adding a second one would
// mean two places that decide how long to keep trying.
func (c *client) do(
	ctx context.Context, class requestClass, method, path, accept string, body any,
) ([]byte, http.Header, error) {
	// Reserve before the network, refuse while a declared backoff stands
	// (gate.go). The refusal costs no request, which is the point: after
	// exhaustion, the retry is the next sweep, not the next candidate.
	if err := c.gate.acquire(ctx); err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}

	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		payload = bytes.NewReader(buf)
	}

	requestCtx := context.WithValue(ctx, requestClassContextKey{}, class)
	req, err := http.NewRequestWithContext(requestCtx, method, c.base+path, payload)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	token, err := c.credential(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: resolving the controller credential: %w", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Bounded: a controller that streams an unbounded response into memory on
	// a malformed reply is a denial of service against its own workflow run.
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	// Headers and status are already complete even when the body is not. Learn
	// their refusal before returning a read error, or a truncated 429 would be
	// treated as an ordinary per-candidate failure and the sweep would continue
	// issuing requests under the server's declared backoff.
	limited := c.gate.observe(class, resp.StatusCode, resp.Header, data)
	if readErr != nil {
		if limited {
			return nil, nil, fmt.Errorf("%w: %s %s: reading response: %w", ErrRateLimited, method, path, readErr)
		}
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The body carries GitHub's own message, which names the missing
		// permission; the token never appears in it.
		he := &httpError{status: resp.StatusCode, method: method, path: path, body: strings.TrimSpace(string(data))}
		if limited {
			// The forge named its own backoff. Marked here so the response
			// that declared it and every refusal made under it carry the same
			// error, and the sweep stops at the first rather than the second.
			return nil, nil, fmt.Errorf("%w: %w", ErrRateLimited, he)
		}
		return nil, nil, he
	}
	c.gate.succeed(class)
	return data, resp.Header, nil
}

// httpError keeps the status alongside the message because one status means
// something the others do not: 404 is a fact about the world (this pull
// request is gone), while everything else is a failed read, and the two must
// never route the same way.
type httpError struct {
	status       int
	method, path string
	body         string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s %s: %d: %s", e.method, e.path, e.status, e.body)
}

func isNotFound(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.status == http.StatusNotFound
}

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }

const acceptJSON = "application/vnd.github+json"

// errPagesDone lets a page callback end pagination cleanly: the pages walked
// so far are the whole answer by construction — a recency-sorted list crossed
// its horizon — which is different from a failed read.
var errPagesDone = errors.New("pagination complete")

// paged walks every page before returning, because everything the reducer does
// with these lists is a filter or a count. A first page of thirty comments
// that happens to exclude the newest milestone is not a smaller answer; it is
// a different one. The one exception is errPagesDone, which a callback may
// only return when the ordering of the list makes the remaining pages
// irrelevant by construction.
func (c *client) paged(ctx context.Context, class requestClass, path string, each func([]byte) error) error {
	next := path + join(path) + "per_page=100"
	for next != "" {
		data, header, err := c.do(ctx, class, http.MethodGet, next, acceptJSON, nil)
		if err != nil {
			return err
		}
		if err := each(data); errors.Is(err, errPagesDone) {
			return nil
		} else if err != nil {
			return err
		}
		link, err := nextLink(header.Get("Link"), c.base)
		if err != nil {
			return fmt.Errorf("GET %s: %w", next, err)
		}
		next = link
	}
	return nil
}

func join(path string) string {
	if strings.Contains(path, "?") {
		return "&"
	}
	return "?"
}

// ErrPageLinkOffRoot is a rel="next" link this client refuses to follow. It is
// a failed read and never a completed one: silently ending the walk would hand
// the reducer a first page in place of the whole list, and every answer it
// derives from one — is this occurrence routed, how many distinct heads has it
// reviewed — is a different answer on a prefix, not a smaller one.
var ErrPageLinkOffRoot = errors.New("reviewctl: the rel=\"next\" link is not under the API root")

// nextLink extracts the rel="next" target from a Link header and returns it as
// a path relative to the API root, so pagination cannot be walked off onto
// another host by a crafted header.
//
// Three answers, which is why this returns an error at all: a next page
// (path, nil), no next page ("", nil), and a next page that exists but is
// refused ("", ErrPageLinkOffRoot). Only the middle one ends a walk.
func nextLink(link, base string) (string, error) {
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		target := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		isNext := false
		for _, attr := range segs[1:] {
			if strings.TrimSpace(attr) == `rel="next"` {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		// The remainder must be an absolute path, not merely share the root's
		// characters: `https://api.github.com.evil.example/x` has the API root
		// as a string prefix, and returning `.evil.example/x` would send the
		// next request — Authorization header and all — to another host.
		rest, ok := strings.CutPrefix(target, base)
		if !ok || !strings.HasPrefix(rest, "/") {
			return "", fmt.Errorf("%w (%q, root %q)", ErrPageLinkOffRoot, target, base)
		}
		return rest, nil
	}
	return "", nil
}

func (c *client) repoPath(format string, args ...any) string {
	return fmt.Sprintf("/repos/%s/%s", c.owner, c.repo) + fmt.Sprintf(format, args...)
}

type apiUser struct {
	Login string `json:"login"`
}

type apiLabel struct {
	Name string `json:"name"`
}

type apiIssue struct {
	Number      int        `json:"number"`
	State       string     `json:"state"`
	Labels      []apiLabel `json:"labels"`
	Assignees   []apiUser  `json:"assignees"`
	PullRequest *struct{}  `json:"pull_request"`
}

func (c *client) Issue(ctx context.Context, number int) (review.Issue, error) {
	data, _, err := c.do(ctx, requestIssue, http.MethodGet, c.repoPath("/issues/%d", number), acceptJSON, nil)
	if err != nil {
		return review.Issue{}, err
	}
	var raw apiIssue
	if err := json.Unmarshal(data, &raw); err != nil {
		return review.Issue{}, err
	}
	out := review.Issue{Number: raw.Number, Closed: raw.State != "open"}
	for _, l := range raw.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	for _, a := range raw.Assignees {
		out.Assignees = append(out.Assignees, a.Login)
	}
	return out, nil
}

type apiComment struct {
	ID        int64     `json:"id"`
	User      apiUser   `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func (c *client) Comments(ctx context.Context, number int) ([]review.Comment, error) {
	var out []review.Comment
	err := c.paged(ctx, requestComments, c.repoPath("/issues/%d/comments", number), func(page []byte) error {
		var raw []apiComment
		if err := json.Unmarshal(page, &raw); err != nil {
			return err
		}
		for _, r := range raw {
			out = append(out, review.Comment{ID: r.ID, Author: r.User.Login, Body: r.Body, CreatedAt: r.CreatedAt})
		}
		return nil
	})
	return out, err
}

type apiEvent struct {
	ID        int64     `json:"id"`
	Event     string    `json:"event"`
	Actor     apiUser   `json:"actor"`
	Assignee  apiUser   `json:"assignee"`
	Label     apiLabel  `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

func (c *client) Events(ctx context.Context, number int) ([]review.Event, error) {
	var out []review.Event
	err := c.paged(ctx, requestEvents, c.repoPath("/issues/%d/events", number), func(page []byte) error {
		var raw []apiEvent
		if err := json.Unmarshal(page, &raw); err != nil {
			return err
		}
		for _, r := range raw {
			out = append(out, review.Event{
				ID: r.ID, Type: r.Event, Actor: r.Actor.Login,
				Assignee: r.Assignee.Login, Label: r.Label.Name, CreatedAt: r.CreatedAt,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return review.SortEvents(out), nil
}

type apiPR struct {
	Number    int       `json:"number"`
	HTMLURL   string    `json:"html_url"`
	State     string    `json:"state"`
	Merged    bool      `json:"merged"`
	Body      string    `json:"body"`
	Head      apiRef    `json:"head"`
	Base      apiRef    `json:"base"`
	Draft     bool      `json:"draft"`
	User      apiUser   `json:"user"`
	UpdatedAt time.Time `json:"updated_at"`
}

// apiRef carries the ref's repository so the sweep can tell a branch in the
// target repository from one in a fork — the ref name alone cannot (#239).
// Repo is a pointer because GitHub sends null for a deleted fork.
type apiRef struct {
	Ref  string   `json:"ref"`
	SHA  string   `json:"sha"`
	Repo *apiRepo `json:"repo"`
}

type apiRepo struct {
	FullName string `json:"full_name"`
}

func (c *client) PullRequest(ctx context.Context, number int) (*review.PullRequest, error) {
	data, _, err := c.do(ctx, requestPullRequest, http.MethodGet, c.repoPath("/pulls/%d", number), acceptJSON, nil)
	if err != nil {
		return nil, err
	}
	var raw apiPR
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return &review.PullRequest{
		Number:  raw.Number,
		URL:     raw.HTMLURL,
		Closed:  raw.State != "open",
		Merged:  raw.Merged,
		Branch:  raw.Head.Ref,
		Head:    raw.Head.SHA,
		Base:    raw.Base.Ref,
		BaseSHA: raw.Base.SHA,
		Body:    raw.Body,
	}, nil
}

type apiReview struct {
	ID          int64     `json:"id"`
	User        apiUser   `json:"user"`
	Body        string    `json:"body"`
	CommitID    string    `json:"commit_id"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
}

func (c *client) Reviews(ctx context.Context, number int) ([]review.Review, error) {
	var out []review.Review
	err := c.paged(ctx, requestReviews, c.repoPath("/pulls/%d/reviews", number), func(page []byte) error {
		var raw []apiReview
		if err := json.Unmarshal(page, &raw); err != nil {
			return err
		}
		for _, r := range raw {
			out = append(out, review.Review{
				ID: r.ID, Author: r.User.Login, Body: r.Body,
				CommitID: r.CommitID, State: r.State, SubmittedAt: r.SubmittedAt,
			})
		}
		return nil
	})
	return out, err
}

// Diff asks for the three-dot comparison — the same diff the pull request
// shows — pinned to both endpoint SHAs rather than either branch name. Naming
// a ref would race a push or retarget landing between the decision and the
// read, and the whole round is anchored to one exact comparison.
func (c *client) Diff(ctx context.Context, base, head string) (string, error) {
	path := c.repoPath("/compare/%s...%s", url.PathEscape(base), url.PathEscape(head))
	data, _, err := c.do(ctx, requestDiff, http.MethodGet, path, "application/vnd.github.diff", nil)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// PublishReview is the one review event the controller may emit. The literal
// is not a parameter on purpose: APPROVE would satisfy a branch rule that a
// human code owner is supposed to satisfy, and CHANGES_REQUESTED would hold a
// merge gate this controller has no authority to hold.
func (c *client) PublishReview(ctx context.Context, pr int, commitID, body string) error {
	_, _, err := c.do(ctx, requestPublishReview, http.MethodPost, c.repoPath("/pulls/%d/reviews", pr), acceptJSON, map[string]any{
		"commit_id": commitID,
		"body":      body,
		"event":     "COMMENT",
	})
	return err
}

func (c *client) Unassign(ctx context.Context, issue int, login string) error {
	_, _, err := c.do(ctx, requestUnassign, http.MethodDelete, c.repoPath("/issues/%d/assignees", issue), acceptJSON, map[string]any{
		"assignees": []string{login},
	})
	return err
}

func (c *client) RemoveLabel(ctx context.Context, issue int, label string) error {
	_, _, err := c.do(ctx, requestRemoveLabel, http.MethodDelete, c.repoPath("/issues/%d/labels/%s", issue, url.PathEscape(label)), acceptJSON, nil)
	return err
}

func (c *client) AddLabel(ctx context.Context, issue int, label string) error {
	_, _, err := c.do(ctx, requestAddLabel, http.MethodPost, c.repoPath("/issues/%d/labels", issue), acceptJSON, map[string]any{
		"labels": []string{label},
	})
	return err
}

func (c *client) PostComment(ctx context.Context, issue int, body string) error {
	_, _, err := c.do(ctx, requestPostComment, http.MethodPost, c.repoPath("/issues/%d/comments", issue), acceptJSON, map[string]any{
		"body": body,
	})
	return err
}

// candidateHorizon bounds the closed half of the sweep's candidate set.
// Closed and merged PRs are candidates at all only because a route mutation
// can land and the PR merge before its marker does (see Candidates); the next
// sweep repairs that gap in minutes, so the horizon only has to outlast the
// longest plausible controller outage — not repository history. Thirty days
// is that with a wide margin, and it is what makes a sweep's request count
// independent of repository age (#239).
const candidateHorizon = 30 * 24 * time.Hour

type candidatePhase uint8

const (
	candidateIssues candidatePhase = iota
	candidateOpenPulls
	candidateClosedPulls
)

// candidateScan is the server-issued continuation of one logical union. It
// survives scheduled sweeps so discovery can yield before consuming the whole
// request allowance without starting at page one again. The accumulated seen
// set preserves the union's de-duplication across those slices.
type candidateScan struct {
	label     string
	phase     candidatePhase
	next      string
	seen      map[int]bool
	horizon   time.Time
	forkHeads int
	crossed   bool
}

// Candidates advances one logical union of two lists, and needs both.
//
// The labelled issues are the live queue. The `ben/<n>` pull requests are the
// recovery set: a route mutation may land and the PR may merge before its
// marker does, and an issue whose label the controller already removed is
// invisible to the first list. Open PRs are taken whole; closed ones only
// back to candidateHorizon, walked in update-recency order so history costs
// nothing to skip.
//
// Both lists take only PRs whose head lives in this repository. BEN publishes
// by pushing `ben/<n>` to origin (SPEC §9.7), so a fork-head PR is never a
// BEN publication — and any account that can fork can name a branch `ben/<n>`,
// which would otherwise make the candidate set attacker-extensible (#239).
//
// Discovery and reconciliation share one hard sweep budget. Each call yields
// once discovery reservations reach half of it and retains the server's next
// links for the following call. Returning a slice of the union is deliberate:
// the controller retains unvisited candidates, while the cursor guarantees
// that a list larger than one sweep's budget eventually reaches later pages.
func (c *client) Candidates(ctx context.Context, label string) ([]int, error) {
	c.candidateMu.Lock()
	defer c.candidateMu.Unlock()

	// Discovery is the first request of every sweep (Controller.Sweep), so it
	// is where the per-sweep budget re-opens.
	c.resetSweepBudget()
	_, sweepBudget := c.gate.sweepUsage()
	discoveryLimit := sweepBudget / 2
	if discoveryLimit < 1 {
		discoveryLimit = 1
	}

	if c.candidates.seen == nil || c.candidates.label != label {
		c.startCandidateScan(label)
	}

	var out []int
	for {
		data, header, err := c.do(ctx, requestCandidates, http.MethodGet, c.candidates.next, acceptJSON, nil)
		if err != nil {
			return out, err
		}
		numbers, forkHeads, crossed, err := c.decodeCandidatePage(data)
		if err != nil {
			return out, err
		}

		var next string
		if !crossed {
			next, err = nextLink(header.Get("Link"), c.base)
			if err != nil {
				return out, fmt.Errorf("GET %s: %w", c.candidates.next, err)
			}
		}

		// Commit a page only after both its body and continuation are valid.
		// A malformed Link must not advance the cursor or mark candidates seen
		// before the caller has received them.
		for _, n := range numbers {
			if n > 0 && !c.candidates.seen[n] {
				c.candidates.seen[n] = true
				out = append(out, n)
			}
		}
		c.candidates.forkHeads += forkHeads
		c.candidates.crossed = c.candidates.crossed || crossed
		c.candidates.next = next

		if next == "" && c.advanceCandidateScan() {
			return out, nil
		}
		used, _ := c.gate.sweepUsage()
		if used >= discoveryLimit {
			return out, nil
		}
	}
}

func (c *client) startCandidateScan(label string) {
	c.candidates = candidateScan{
		label: label, phase: candidateIssues, seen: map[int]bool{},
		horizon: c.now().Add(-candidateHorizon),
	}
	c.candidates.next = c.firstCandidatePage(candidateIssues, label)
}

// advanceCandidateScan selects the next source. It returns true only after the
// whole union has been traversed and resets the cursor for the next cycle.
func (c *client) advanceCandidateScan() bool {
	s := &c.candidates
	if s.phase < candidateClosedPulls {
		s.phase++
		s.next = c.firstCandidatePage(s.phase, s.label)
		return false
	}
	if s.forkHeads > 0 {
		c.log("review sweep: excluded %d ben/<n> pull request(s) whose head is not in %s/%s", s.forkHeads, c.owner, c.repo)
	}
	if s.crossed {
		c.log("review sweep: closed pull requests not updated since %s were not scanned", s.horizon.UTC().Format(time.RFC3339))
	}
	c.candidates = candidateScan{}
	return true
}

func (c *client) firstCandidatePage(phase candidatePhase, label string) string {
	var path string
	switch phase {
	case candidateIssues:
		path = c.repoPath("/issues?state=open&labels=%s", url.QueryEscape(label))
	case candidateOpenPulls:
		path = c.repoPath("/pulls?state=open")
	case candidateClosedPulls:
		path = c.repoPath("/pulls?state=closed&sort=updated&direction=desc")
	}
	return path + join(path) + "per_page=100"
}

// decodeCandidatePage is pure with respect to the scan cursor: Candidates can
// validate the page's Link before committing either half of the response.
func (c *client) decodeCandidatePage(data []byte) (numbers []int, forkHeads int, crossed bool, err error) {
	if c.candidates.phase == candidateIssues {
		var raw []apiIssue
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, 0, false, err
		}
		for _, r := range raw {
			if r.PullRequest == nil { // the issues endpoint returns pull requests too
				numbers = append(numbers, r.Number)
			}
		}
		return numbers, 0, false, nil
	}

	var raw []apiPR
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, 0, false, err
	}
	for _, r := range raw {
		if c.candidates.phase == candidateClosedPulls && r.UpdatedAt.Before(c.candidates.horizon) {
			// Sorted by update recency, so everything after this is older
			// still; the remaining pages are irrelevant by construction.
			crossed = true
			break
		}
		if !c.localHead(r) {
			if _, ok := issueOfBranch(r.Head.Ref); ok {
				forkHeads++
			}
			continue
		}
		if n, ok := issueOfBranch(r.Head.Ref); ok {
			numbers = append(numbers, n)
		}
	}
	return numbers, forkHeads, crossed, nil
}

// localHead reports whether the PR's head branch lives in the target
// repository. A nil head repo is a deleted fork: excluded for the same reason
// a live one is.
func (c *client) localHead(r apiPR) bool {
	return r.Head.Repo != nil && strings.EqualFold(r.Head.Repo.FullName, c.owner+"/"+c.repo)
}

// issueOfBranch is the sweep's half of the `ben/<n>` convention. The reducer
// re-derives and re-checks it against the issue and the PR body; this only
// decides which issues are worth looking at.
func issueOfBranch(ref string) (int, bool) {
	rest, ok := strings.CutPrefix(ref, "ben/")
	if !ok {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(rest, "%d", &n); err != nil || n <= 0 || fmt.Sprint(n) != rest {
		return 0, false
	}
	return n, true
}
