// Package github is the v1 tracker adapter (SPEC §8): a normalized read
// kernel over GitHub Issues plus the closed write set of queue mechanics —
// claim, release, state labels, milestone comments. It writes nothing else.
// The agent writes all content; issues close when a human merges the PR
// (SPEC §8.1).
//
// This package owns the go-github dependency for the whole module
// (internal/arch enforces it).
package github

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v90/github"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// defaultHTTPTimeout bounds a single API call. A poll that hangs forever
// stalls the whole tick loop.
const defaultHTTPTimeout = 30 * time.Second

// perPage is the list page size. 100 is GitHub's maximum: fewer pages means
// fewer conditional requests to revalidate each tick.
const perPage = 100

// requestControl outlives one adapter generation. The allowance is shared at
// the API endpoint because readiness must spend a request before a rotated
// token can prove which account it belongs to. Server-directed backoff is
// narrower: each credential *authority* gets its own gate, retained even when a
// failed reload candidate is discarded.
//
// Together those two scopes are SPEC §8.5's key — **(API endpoint, credential
// source authority)**. The endpoint half is the control itself, which a
// successor adopts only from a predecessor addressing the same endpoint
// (ContinueRequestControl); the authority half is this map.
//
// Keyed by authority and not by the token, which is the whole of amendment 11.
// An authority is stable across rotation and a token is not, so a token-keyed
// gate abandons its backoff on every refresh — precisely when a server has just
// asked the daemon to slow down — and accumulates one entry per rotation for the
// life of the process. With a source that mints a credential every fifty
// minutes, that is not a theoretical leak.
type requestControl struct {
	budget *requestBudget

	mu    sync.Mutex
	gates map[string]*rateGate
}

func newRequestControl(budget *requestBudget) *requestControl {
	return &requestControl{budget: budget, gates: make(map[string]*rateGate)}
}

// gateFor returns the backoff gate for one credential authority. The authority
// is non-secret by construction (core.SourceDescriptor.Authority), so unlike the
// token it replaced it can be held as a plain map key for the life of the
// process.
func (c *requestControl) gateFor(authority string) *rateGate {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gate := c.gates[authority]; gate != nil {
		return gate
	}
	gate := newRateGate(nil)
	c.gates[authority] = gate
	return gate
}

// Adapter implements core.TrackerAdapter against GitHub.
type Adapter struct {
	client *gh.Client
	// http is the same client go-github rides, kept because §9.5's content read
	// has no REST equivalent and is issued directly (see graphql.go). One client
	// means one bearer credential, one timeout, and one rate gate — #47's
	// finding is that credential paths multiply quietly. It is also the same
	// budget: the transport below meters every exchange that client makes,
	// GraphQL included (SPEC §8.5).
	http       *http.Client
	graphqlURL string
	cache      *condCache
	gate       *rateGate
	budget     *requestBudget
	control    *requestControl
	// transport owns the two request-control pointers used below condCache. A
	// rebuild may replace them before Ready so the new adapter continues its
	// predecessor's credential-scoped budget and backoff.
	transport *budgetTransport
	auth      *authTransport
	cfg       settings
	host      string

	mu sync.Mutex
	// cred is the tracker credential's source, bound at construction and never
	// nil (core.TrackerOptions.Credential). Every request obtains its credential
	// through it at the moment it is needed (authTransport).
	cred core.Source
	// principal is the configured claim assignee, or the token's login when the
	// key is omitted. The former is populated at construction; the latter is
	// resolved once and reused (SPEC §8.4).
	//
	// A construction-time binding or request-saving cache, and nothing else. It
	// exists before Ready finishes — immediately for a configured assignee,
	// part-way through Ready for the fallback — so it is deliberately *not* what
	// ClaimPrincipal answers from. See readyPrincipal.
	principal string
	// readyPrincipal and cloneURL are what Ready established (SPEC §6.2, §8.4,
	// §10.2): the login claims are assigned to, and the server's own clone URL.
	// Both are set together and only on a successful Ready, so their presence
	// *is* the readiness precondition ClaimPrincipal and Repository check — no
	// second resolution path, and no answer attesting to a world nobody finished
	// probing.
	//
	// The credential that reached it is deliberately **not** among them. It used
	// to be, and that is the lifetime bug: a value captured here is the string a
	// rotation leaves behind, so every base fetch after one would authenticate
	// with a credential nothing could refresh, and redaction would scrub the
	// stale value while the live one flowed through git's stderr. What Repository
	// hands out now is the source itself.
	readyPrincipal string
	cloneURL       string
}

// Compile-time proof that the adapter satisfies the contract the orchestrator
// programs against, and the §9.5 content seam beside it.
var (
	_ core.TrackerAdapter        = (*Adapter)(nil)
	_ core.ContentApprovalSource = (*Adapter)(nil)
)

// And the §8.5 request budget, which the orchestrator discovers by assertion:
// nothing fails to compile when a discovered capability stops matching, so the
// declared interface is named here rather than left to the assertion site to
// describe. Without it the per-tick window would silently never be opened.
var _ core.RequestBudget = (*Adapter)(nil)

// Request-control continuity is optional at the core boundary. Naming both
// halves here keeps a signature drift from silently giving every reload a fresh
// allowance: the successor half adopts a predecessor's controls, the domain half
// is what assembly retains a discarded candidate under.
var (
	_ core.RequestControlSuccessor = (*Adapter)(nil)
	_ core.RequestControlDomain    = (*Adapter)(nil)
)

// New validates opts and builds the adapter, binding them to the instance
// (SPEC §5.7). It performs no network I/O and reads no environment: startup must
// be able to refuse a bad config without reaching GitHub, and every credential
// is obtained from the bound source at the moment it is needed.
func New(opts core.TrackerOptions) (*Adapter, error) {
	set, err := parseOptions(opts)
	if err != nil {
		return nil, err
	}

	// The transport starts this timeout only after budget admission. A history
	// continuation may deliberately wait for the next request window; charging
	// that wait against the network timeout would discard the pages it preserved.
	httpClient := &http.Client{}
	budget := newRequestBudget(nil)
	control := newRequestControl(budget)
	gate := control.gateFor(opts.Credential.Descriptor().Authority)
	// The budget sits below the conditional cache: it sees both the outgoing
	// If-None-Match and the origin's 304, before the cache replays that response
	// as a 200 for go-github (SPEC §8.5, budget.go).
	transport := &budgetTransport{next: httpClient.Transport, budget: budget, gate: gate}
	cache := newCondCache(transport)
	// Outermost, deliberately: an initial credential failure must cost nothing
	// below it. Nothing is charged to the request budget, no conditional cache
	// entry is consulted, and — the point of the whole arrangement — no request
	// reaches GitHub at all. A request that waits for budget admission fetches
	// again immediately before the network; that second failure returns the
	// unused reservation and is just as unable to reach GitHub.
	auth := &authTransport{next: cache, source: opts.Credential}
	transport.refreshCredential = auth.authenticate
	httpClient.Transport = auth

	clientOpts := []gh.ClientOptionsFunc{
		gh.WithHTTPClient(httpClient),
		gh.WithUserAgent("ben"),
		// go-github's client-side preflight refuses every request once it
		// has seen X-RateLimit-Remaining: 0. That would black out precisely
		// the polls that cost nothing — a conditional request answered 304 is
		// free and served even at the limit. rateGate is the single
		// authority instead, and it closes only on a refusal the server
		// actually sent (SPEC §8.5).
		gh.WithDisableRateLimitCheck(),
	}
	if set.apiURL != "" {
		clientOpts = append(clientOpts, gh.WithEnterpriseURLs(set.apiURL, set.apiURL))
	}
	client, err := gh.NewClient(clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("building github client: %w", err)
	}
	// Derived from the same api_url go-github was given, at construction, so a
	// URL that cannot name a GraphQL endpoint is a structural refusal rather than
	// a per-claim failure (SPEC §5.7).
	graphqlURL, err := graphqlEndpoint(set.apiURL)
	if err != nil {
		return nil, err
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return &Adapter{
		client: client, http: httpClient, graphqlURL: graphqlURL,
		cache: cache, gate: gate, budget: budget, control: control,
		transport: transport, auth: auth, cfg: set, host: host,
		cred:      opts.Credential,
		principal: set.claimAssignee,
	}, nil
}

// RequestControlKey is the API endpoint this adapter addresses, canonicalized —
// and literally the same call ContinueRequestControl compares below, so what
// assembly retains and what a successor will accept cannot disagree.
//
// No credential appears in it: userinfo is refused at load
// (ErrAPIURLNotAnEndpoint) and stripped here regardless, the budget is shared
// across an endpoint's tokens, and server-directed backoff is keyed inside the
// control by a one-way token identity (requestControl.gateFor) that never leaves
// it.
func (a *Adapter) RequestControlKey() string { return requestControlKey(a.client.BaseURL()) }

// requestControlKey reduces a client base URL to the endpoint it addresses, so
// two spellings of one endpoint cannot hold two allowances. Config refuses the
// components that address nothing at all; what stays legal is a host's case and a
// port that is its scheme's default, and both name the same server.
//
// Merging too much is the safe direction, and deliberately the one taken when in
// doubt: two endpoints sharing one allowance spend fewer requests than the pair
// is entitled to, while two allowances for one endpoint spend more than that
// endpoint permits — the bound this layer exists to hold (SPEC §8.5).
func requestControlKey(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		// Never merge on a URL that could not be read.
		return base
	}
	u.User = nil
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	u.Scheme = strings.ToLower(u.Scheme)
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == defaultPort(u.Scheme) {
		port = ""
	}
	switch {
	case port != "":
		u.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		// An IPv6 literal, unbracketed by Hostname().
		u.Host = "[" + host + "]"
	default:
		u.Host = host
	}
	return u.String()
}

func defaultPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// ContinueRequestControl adopts the first compatible predecessor's
// endpoint-scoped controls before Ready. It performs no I/O; a token is resolved
// later and selects its credential-specific gate from the shared control.
//
// Compatibility is the canonical API endpoint, so a candidate that moves the
// daemon to a new one skips the published generation and continues from a
// discarded candidate of its own endpoint instead — the generation that already
// spent requests there, and already heard whatever Retry-After it sent.
func (a *Adapter) ContinueRequestControl(previous ...core.TrackerAdapter) {
	key := a.RequestControlKey()
	for _, candidate := range previous {
		predecessor, ok := candidate.(*Adapter)
		if !ok || predecessor == a || predecessor.RequestControlKey() != key {
			continue
		}
		a.control = predecessor.control
		a.budget = predecessor.control.budget
		a.transport.budget = predecessor.control.budget
		// Re-selected from the adopted control, or the gate this adapter built
		// for itself would keep the predecessor's endpoint budget while
		// forgetting the Retry-After that endpoint just sent.
		a.selectRequestGate()
		return
	}
}

// selectRequestGate binds the authority-scoped backoff. Because the map belongs
// to requestControl, a failed candidate's Retry-After survives for the next
// candidate using the same credential — and survives a rotation, because an
// authority does not move when a token does (SPEC §8.5, amendment 11).
func (a *Adapter) selectRequestGate() {
	gate := a.control.gateFor(a.cred.Descriptor().Authority)
	a.gate = gate
	a.transport.gate = gate
}

// admit applies a standing server rate-limit refusal before a call does any
// local work. The request budget admits at the transport boundary instead:
// that is the only place every request, including concurrent requests and
// cache-generated conditional probes, can be reserved exactly once.
func (a *Adapter) admit() error {
	return a.gate.check()
}

// authTransport obtains the bearer credential from the bound source and applies
// it to every request (SPEC §10.2).
//
// **Per request, not per generation.** This is the one place the tracker's
// credential is read, and it is read at the moment the request needs it: a
// credential captured once would be the string a rotation leaves behind, and
// every poll after that rotation would fail with nothing able to refresh it.
// Caching belongs to the source, which is what knows the deadline.
//
// A source failure returns here, **before** `next` is called, which is what
// makes "no source failure falls through to a different credential" a property
// of the wiring rather than of each call site. There is no unauthenticated path
// through this transport to fall through *to*.
type authTransport struct {
	next   http.RoundTripper
	source core.Source
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req, err := t.authenticate(req)
	if err != nil {
		return nil, err
	}
	return t.next.RoundTrip(req)
}

func (t *authTransport) authenticate(req *http.Request) (*http.Request, error) {
	tok, err := t.source.Fetch(req.Context(), core.PurposeTracker)
	if err != nil {
		return nil, err
	}
	if tok.Value == "" {
		return nil, emptyCredential(t.source)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+tok.Value)
	return req, nil
}

// emptyCredential is the boundary's refusal of a source that reported success
// with no credential — permanent, and raised before any use (SPEC §10.2).
func emptyCredential(src interface{ Descriptor() core.SourceDescriptor }) error {
	return &core.CredentialError{
		Class:     core.CredentialPermanent,
		Authority: src.Descriptor().Authority,
		Err:       fmt.Errorf("%w: %w", ErrMissingToken, core.ErrCredentialEmpty),
	}
}

// Ready reports whether the bound configuration can operate now (SPEC §5.7):
// the credential resolves and the tracker answers for it. This is where an
// omitted token meets its documented fallback — $GITHUB_TOKEN is read here,
// not at parse, because an unresolved credential is the world not being set
// up, not the config being wrong (SPEC §5.8, §8.4). It is also the only place
// the fallback is read: everything downstream, Repository included, uses what
// readiness established.
func (a *Adapter) Ready(ctx context.Context) error {
	// Invalidated before anything is attempted, so what the accessors report is
	// this attempt's verdict and never the previous one's. Readiness is a
	// point-in-time fact: a credential can be revoked, a repository made private,
	// a host taken away, and a Ready that then fails must leave the adapter
	// refusing rather than serving a principal and a clone URL from the last time
	// the world was in order (SPEC §5.7; core.ClaimPrincipalSource).
	//
	// Publishing only on success is not enough on its own — that leaves the
	// *previous* success standing, which is the same defect one call later. Fail
	// closed in the window instead: a concurrent reader during a re-check gets
	// ErrNotReady, which costs a retry, where the other direction costs a claim
	// written as an account this token may no longer be.
	a.mu.Lock()
	a.readyPrincipal, a.cloneURL = "", ""
	a.mu.Unlock()

	// One exchange, before any GitHub request, so a misconfigured source is a
	// loud startup refusal rather than a poll that mysteriously 401s. The token
	// is discarded: what readiness establishes is that the source answers, and
	// every request below obtains its own through the transport.
	probe, err := a.cred.Fetch(ctx, core.PurposeTracker)
	if err != nil {
		return err
	}
	if probe.Value == "" {
		return emptyCredential(a.cred)
	}

	// Two reads, each attesting to something the first poll would otherwise
	// discover mid-tick. With no configured assignee they are the authenticated
	// login and repository visibility. With one, claimPrincipal is already
	// populated and the second read below proves that account assignable after
	// the repository itself has answered (SPEC §8.4).
	principal, err := a.claimPrincipal(ctx)
	if err != nil {
		return err
	}
	if err := a.admit(); err != nil {
		return err
	}
	repo, _, err := a.client.Repositories.Get(withoutCache(ctx), a.cfg.owner, a.cfg.repo)
	if err != nil {
		return fmt.Errorf("reaching repository %s/%s: %w", a.cfg.owner, a.cfg.repo, a.gate.observe(err))
	}
	// The read that proves the repository visible also states its clone URL,
	// so the base clone (SPEC §6.2) fetches from the server's own answer
	// rather than from a URL BEN assembled out of the API URL. GitHub Enterprise
	// admins choose both hostnames, and they need not be related: any rule for
	// deriving one from the other can point the credential at the wrong host.
	clone := repo.GetCloneURL()
	if clone == "" {
		return fmt.Errorf("repository %s/%s reports no clone URL: the base clone (SPEC §6.2) would have no remote to fetch from", a.cfg.owner, a.cfg.repo)
	}
	if a.cfg.claimAssignee != "" {
		if err := a.checkClaimPrincipal(ctx, principal); err != nil {
			return err
		}
	}
	// Published together, and only here — the one point at which every check
	// above has passed. A value set on the way through, as the principal cache
	// is, would answer for a readiness that then failed at the repository probe
	// (SPEC §5.7, §5.8).
	a.mu.Lock()
	a.readyPrincipal, a.cloneURL = strings.ToLower(principal), clone
	a.mu.Unlock()
	return nil
}

// Fetch is the queue read: every issue carrying the required labels,
// normalized, with the §8.3 `dispatchable` verdict. Label filtering happens
// server-side on the core budget; the Search API stays out of the poll path
// entirely (SPEC §8.5).
//
// Its filters make it the wrong read for issues BEN already owns — a closed
// issue, or one whose queue label a human pulled, is absent by construction.
// Get and FetchAssigned are those reads (SPEC §9.8, §9.10).
func (a *Adapter) Fetch(ctx context.Context) ([]core.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		Labels: a.cfg.requiredLabels,
		State:  a.listState(),
		// Oldest first: the orchestrator dispatches FIFO by age (SPEC §9.5),
		// and a stable order keeps polls diffable.
		Sort:        "created",
		Direction:   "asc",
		ListOptions: gh.ListOptions{PerPage: perPage},
	}
	return a.list(ctx, opts, true)
}

// ClaimedByPrincipal is the first recovery read (SPEC §9.10 step 1): every
// issue our claim principal holds. Startup has no local record to consult, so
// these are the candidates it classifies from positive evidence.
//
// Deliberately unfiltered — any state, any labels or none. Fetch's filters
// structurally cannot serve recovery: the claims most in need of cleanup are
// exactly the ones that have left the queue partition, closed under a merged
// PR or stripped of their queue label by a human.
//
// Cache-bypassing: an answer served from our own ETag cache would attest to
// nothing about the world we are trying to reconstruct. No `dispatchable`
// verdict either — recovery routes each candidate by the §9.10 table, which
// is a different question.
func (a *Adapter) ClaimedByPrincipal(ctx context.Context) ([]core.Issue, error) {
	return a.principalAssignments(ctx, false)
}

// HeldClaims asks the same question on the steady-state path — the §9.8
// held-claim sweep that releases a retained `done` claim once its issue closes
// — and differs in exactly one respect: it is ETag-conditional. A review
// backlog then costs one request per tick and, 304s being unbilled, no core
// budget. A Get per held claim would cost one per claim per tick, and the held
// set grows with human review latency, which is the one quantity the daemon
// does not control.
//
// Separate from ClaimedByPrincipal rather than a flag on it because the cache
// posture is part of the contract (SPEC §8.2): recovery MUST read origin, and
// one method serving both postures could serve it a cached answer.
func (a *Adapter) HeldClaims(ctx context.Context) ([]core.Issue, error) {
	return a.principalAssignments(ctx, true)
}

// principalAssignments is the query both reads share: every issue the
// principal holds, any state, any labels, no dispatchability verdict. Only the
// cache posture differs.
func (a *Adapter) principalAssignments(ctx context.Context, conditional bool) ([]core.Issue, error) {
	principal, err := a.claimPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	opts := &gh.IssueListByRepoOptions{
		Assignee:    principal,
		State:       "all",
		Sort:        "created",
		Direction:   "asc",
		ListOptions: gh.ListOptions{PerPage: perPage},
	}
	if !conditional {
		ctx = withoutCache(ctx)
	}
	return a.list(ctx, opts, false)
}

// ClaimHistory is the second recovery read (SPEC §9.10 step 2): the issue's
// ordered assignment and label changes, which is what separates a claim
// killed before its label was projected from one that ran to a
// label-clearing terminal. Both shapes read as assigned-with-no-state-label;
// only the log tells them apart.
//
// The raw provider payload stops here (invariant 6): the core sees the closed
// ClaimEvent shape, in tracker order.
//
// A 404 is core.ErrIssueNotFound, as it is on Get: the change-log endpoint names
// one issue, so GitHub's answer is about that issue and the caller can route on
// it. The classification is made *here* rather than in issueEvents, which two
// writes also walk — Claim's race arbitration and Comment's occurrence lookup —
// and a write must not conclude "gone" from a refusal (core.ErrIssueNotFound).
func (a *Adapter) ClaimHistory(ctx context.Context, issue core.Issue) ([]core.ClaimEvent, error) {
	if err := a.admit(); err != nil {
		return nil, err
	}
	number, err := issueNumber(issue)
	if err != nil {
		return nil, err
	}
	events, err := a.issueEvents(ctx, number, issueEventOptions{wait: true})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: #%d", core.ErrIssueNotFound, number)
		}
		return nil, err
	}

	out := make([]core.ClaimEvent, 0, len(events))
	for _, ev := range events {
		kind, subject, ok := claimEventKind(ev)
		if !ok {
			continue
		}
		out = append(out, core.ClaimEvent{
			Kind:    kind,
			Actor:   ev.GetActor().GetLogin(),
			Subject: subject,
			At:      ev.GetCreatedAt().Time,
			ID:      ev.GetID(),
		})
	}
	return out, nil
}

// claimEventKind projects the provider's open-ended event vocabulary onto the
// six kinds the core reads. Everything else is noise for this purpose.
func claimEventKind(ev *gh.IssueEvent) (core.ClaimEventKind, string, bool) {
	switch ev.GetEvent() {
	case "assigned":
		return core.ClaimEventAssigned, ev.GetAssignee().GetLogin(), true
	case "unassigned":
		return core.ClaimEventUnassigned, ev.GetAssignee().GetLogin(), true
	case "labeled":
		return core.ClaimEventLabeled, ev.GetLabel().GetName(), true
	case "unlabeled":
		return core.ClaimEventUnlabeled, ev.GetLabel().GetName(), true
	// The state pair names no assignee and no label. It is here because a
	// close survives a reopen in the log but not in the issue's state, and the
	// held-claim sweep releases on the close (SPEC §8.2, §9.8).
	case "closed":
		return core.ClaimEventClosed, "", true
	case "reopened":
		return core.ClaimEventReopened, "", true
	default:
		return "", "", false
	}
}

// Get is the reconciliation read (SPEC §9.8): one issue as it stands now,
// unfiltered. `dispatchable` is not computed — an issue BEN is running is
// assigned to BEN and would read as non-dispatchable regardless, which says
// nothing about whether the run should continue.
func (a *Adapter) Get(ctx context.Context, identifier string) (*core.Issue, error) {
	if err := a.admit(); err != nil {
		return nil, err
	}
	number, err := issueNumber(core.Issue{Identifier: identifier})
	if err != nil {
		return nil, err
	}
	raw, _, err := a.client.Issues.Get(ctx, a.cfg.owner, a.cfg.repo, number)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: #%d", core.ErrIssueNotFound, number)
		}
		return nil, fmt.Errorf("reading issue #%d: %w", number, a.gate.observe(err))
	}
	issue := normalize(raw)
	return &issue, nil
}

// list walks a paginated issue listing. withVerdict computes §8.3
// dispatchability, which costs a dependency request per candidate and only
// means anything on the queue read.
func (a *Adapter) list(ctx context.Context, opts *gh.IssueListByRepoOptions, withVerdict bool) ([]core.Issue, error) {
	if err := a.admit(); err != nil {
		return nil, err
	}

	var issues []core.Issue
	for {
		page, resp, err := a.client.Issues.ListByRepo(ctx, a.cfg.owner, a.cfg.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing issues: %w", a.gate.observe(err))
		}
		for _, raw := range page {
			// The issues endpoint returns pull requests too; they are not
			// work items.
			if raw.IsPullRequest() {
				continue
			}
			issue := normalize(raw)
			if withVerdict {
				if err := a.applyVerdict(ctx, raw, &issue); err != nil {
					return nil, err
				}
			}
			issues = append(issues, issue)
		}
		if resp.NextPage == 0 {
			return issues, nil
		}
		// Qualified: IssueListByRepoOptions embeds both the cursor and the
		// offset pagination structs, and both have a Page.
		opts.ListOptions.Page = resp.NextPage
	}
}

func (a *Adapter) applyVerdict(ctx context.Context, raw *gh.Issue, issue *core.Issue) error {
	// Blockers cost a request each, so only ask where the answer can change
	// the verdict: an issue that already failed a cheap check is not
	// dispatchable regardless, and GitHub's dependency summary settles the
	// zero case for free (SPEC §8.5).
	eligible := eligibleIgnoringBlockers(*issue, a.cfg.requiredLabels, a.cfg.activeStates)
	if eligible && mayHaveBlockers(raw) {
		// This fan-out is per candidate, so it is where a queue read stops being
		// one request and starts scaling with the queue. The transport budget
		// bounds the concurrent requests before they reach GitHub, and each
		// completed entry stays cached for free revalidation on the next tick
		// (SPEC §8.5, budget.go).
		blockers, err := a.listBlockedBy(ctx, raw.GetNumber())
		if err != nil {
			// Fail closed: unknown dependencies must never read as "no
			// dependencies" and release work early.
			return fmt.Errorf("listing blockers of issue #%d: %w", raw.GetNumber(), err)
		}
		issue.Blockers = blockers
	}
	issue.Dispatchable = eligible && !hasOpenBlocker(*issue)
	return nil
}

// listState maps active_states onto the list endpoint's coarse filter. Asking
// for exactly what we accept keeps closed issues off the wire in the common
// case; anything else is filtered client-side by eligibleIgnoringBlockers.
func (a *Adapter) listState() string {
	if len(a.cfg.activeStates) == 1 && strings.EqualFold(a.cfg.activeStates[0], "open") {
		return "open"
	}
	return "all"
}

// mayHaveBlockers reads GitHub's dependency summary. Absent (older GHE, or an
// endpoint that omits it) means "unknown" — ask.
func mayHaveBlockers(raw *gh.Issue) bool {
	s := raw.IssueDependenciesSummary
	return s == nil || s.TotalBlockedBy == nil || *s.TotalBlockedBy > 0
}

func (a *Adapter) listBlockedBy(ctx context.Context, number int) ([]core.Blocker, error) {
	var all []*gh.Issue
	opts := &gh.ListOptions{PerPage: perPage}
	for {
		page, resp, err := a.client.Issues.ListBlockedBy(ctx, a.cfg.owner, a.cfg.repo, int64(number), opts)
		if err != nil {
			return nil, a.gate.observe(err)
		}
		all = append(all, page...)
		if resp.NextPage == 0 {
			return normalizeBlockers(all), nil
		}
		opts.Page = resp.NextPage
	}
}

// FindPR returns the open pull request published on branch — the third leg of
// the §9.7 evidence check, which names an *open* PR specifically. A closed or
// merged PR on the same branch is not that evidence: a rejected PR from an
// earlier attempt would otherwise satisfy a caller that only checks for
// non-nil. Only open PRs are asked for, so only open PRs can be returned.
func (a *Adapter) FindPR(ctx context.Context, issue core.Issue, branch string) (*core.PR, error) {
	if branch == "" {
		return nil, errors.New("FindPR requires the workspace branch")
	}
	if err := a.admit(); err != nil {
		return nil, err
	}

	opts := &gh.PullRequestListOptions{
		State:       "open",
		Head:        a.cfg.owner + ":" + branch,
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: perPage},
	}
	prs, _, err := a.client.PullRequests.List(ctx, a.cfg.owner, a.cfg.repo, opts)
	if err != nil {
		return nil, fmt.Errorf("listing pull requests for %s: %w", branch, a.gate.observe(err))
	}

	for _, pr := range prs {
		// The head filter is a server-side convenience, not a guarantee we
		// let decide evidence.
		if pr.GetHead().GetRef() != branch || pr.GetState() != "open" {
			continue
		}
		return &core.PR{
			Number: pr.GetNumber(),
			URL:    pr.GetHTMLURL(),
			State:  "open",
			Branch: branch,
		}, nil
	}
	return nil, nil
}

// Claim assigns the issue to the token's identity and verifies by read-back:
// GitHub answers 201 even when the assignment silently did not happen, and it
// has no conditional writes, so read-after-write is the only way to know
// (SPEC §8.4).
//
// Every path out of here leaves the issue either verifiably ours or
// verifiably not ours. An assignment we cannot account for is the worst
// outcome available: recovery reads assigned-with-no-state-label as
// published-awaiting-review and leaves it alone forever (SPEC §9.10 step 3).
//
// Everything before the assignment refuses through notAttempted, because the
// caller's remedy for a claim error is a release — a write, on an issue this
// adapter has just said it has no capacity or no permission to write to
// (core.ErrClaimNotAttempted).
func (a *Adapter) Claim(ctx context.Context, issue core.Issue) (bool, error) {
	if err := a.admit(); err != nil {
		return false, notAttempted(err)
	}
	number, err := issueNumber(issue)
	if err != nil {
		return false, notAttempted(err)
	}
	principal, err := a.claimPrincipal(ctx)
	if err != nil {
		return false, notAttempted(err)
	}
	lease, err := a.budget.reserveLease(claimLeaseCosts[:]...)
	if err != nil {
		return false, notAttempted(err)
	}
	defer lease.close()
	ctx = useClaimLeaseBudget(ctx)
	// The unwind takes the stage reserved for a write, whichever earlier stage it
	// follows. Reached from the assignment failure below, the read-back's
	// reservation is still untaken — and spending that one would charge this
	// DELETE as a read (claimStage).
	release := func() error {
		releaseCtx, _, err := lease.requestContext(ctx, claimStageRelease, false)
		if err != nil {
			return err
		}
		return a.releaseAs(releaseCtx, number, principal)
	}

	assignCtx, assignment, err := lease.requestContext(ctx, claimStageAssign, true)
	if err != nil {
		return false, notAttempted(err)
	}
	if _, _, err := a.client.Issues.AddAssignees(assignCtx, a.cfg.owner, a.cfg.repo, number, []string{principal}); err != nil {
		assignErr := fmt.Errorf("assigning issue #%d: %w", number, a.gate.observe(err))
		// A network or redirect failure does not prove the write missed GitHub.
		// If an exchange was attempted, unwind through the protected stage. A
		// gate refusal before the first exchange needs no write of its own —
		// and, having never left this process, is the one assignment failure
		// that can say so.
		if !assignment.wasAttempted() {
			return false, notAttempted(assignErr)
		}
		if relErr := release(); relErr != nil {
			return false, errors.Join(assignErr, fmt.Errorf("unwinding uncertain assignment: %w", relErr))
		}
		return false, assignErr
	}

	// Uncached: a conditional GET answered from our own cache would verify
	// nothing.
	verifyCtx, _, err := lease.requestContext(ctx, claimStageVerify, false)
	if err != nil {
		return false, err
	}
	fresh, _, err := a.client.Issues.Get(withoutCache(verifyCtx), a.cfg.owner, a.cfg.repo, number)
	if err != nil {
		// We wrote and cannot confirm what we wrote. Unwind rather than
		// strand the issue in an assignment nobody is tracking.
		verifyErr := fmt.Errorf("verifying claim on issue #%d: %w", number, a.gate.observe(err))
		if relErr := release(); relErr != nil {
			return false, errors.Join(verifyErr, fmt.Errorf("unwinding unverifiable claim: %w", relErr))
		}
		return false, verifyErr
	}
	assignees := normalize(fresh).Assignees

	if !containsFold(assignees, principal) {
		// The write did not stick. There is no claim of ours to release.
		return false, nil
	}
	if len(assignees) == 1 {
		return true, nil
	}

	// Contested. "Exactly one wins; the loser releases" (SPEC §12.3 scenario
	// 2) needs a winner every claimant computes the same way — releasing
	// whenever we see company would leave races with no winner at all.
	won, err := a.wonRace(ctx, number, assignees, principal)
	if err != nil {
		// Cannot establish an order, so cannot know we are the winner. Yield:
		// a wasted round beats two daemons on one branch.
		if relErr := release(); relErr != nil {
			return false, errors.Join(err, fmt.Errorf("releasing unresolved claim: %w", relErr))
		}
		return false, nil
	}
	if !won {
		if err := release(); err != nil {
			return false, fmt.Errorf("releasing lost claim on issue #%d: %w", number, err)
		}
		return false, nil
	}
	return true, nil
}

// wonRace decides a contested claim by assignment order: whoever was assigned
// first owns it. GitHub's issue events carry that order, so every claimant
// reading them reaches the same verdict — including the daemon that read back
// a sole assignment and only later discovered company.
//
// Ordering is by (created_at, event id): event timestamps are
// second-granularity, and two daemons racing land inside the same second
// routinely. Event ids are monotonic, so they break the tie truthfully rather
// than arbitrarily.
func (a *Adapter) wonRace(ctx context.Context, number int, assignees []string, principal string) (bool, error) {
	held, seen, err := a.assignmentOrder(ctx, number)
	if err != nil {
		return false, err
	}
	if _, ours := held[strings.ToLower(principal)]; !ours {
		// Our assignment is already gone — someone unassigned us between the
		// read-back and now. Nothing to win.
		return false, nil
	}

	var winner string
	var best assignEvent
	for _, login := range assignees {
		key := strings.ToLower(login)
		at, ok := held[key]
		if !ok {
			// The log is more current than the read-back. A login it has
			// seen and since released has withdrawn from the race — that is
			// the losing daemon getting out of the way, and yielding to it
			// would leave the race with no winner at all.
			if seen[key] {
				continue
			}
			// Never seen: the log cannot account for a party that is
			// nonetheless assigned (retention, a transfer). Refuse to guess.
			return false, fmt.Errorf("issue #%d: no assignment event for %q, cannot order the claim race", number, login)
		}
		if winner == "" || at.before(best) {
			winner, best = login, at
		}
	}
	return strings.EqualFold(winner, principal), nil
}

// assignEvent is when a login became — and stayed — assigned.
type assignEvent struct {
	at time.Time
	id int64
}

func (e assignEvent) before(other assignEvent) bool {
	if !e.at.Equal(other.at) {
		return e.at.Before(other.at)
	}
	return e.id < other.id
}

// assignmentOrder replays the issue's assign/unassign events. held maps each
// login with a standing assignment to when that assignment began; seen covers
// every login the log mentions at all, which is what separates "released it
// again" from "the log never knew about them". Every cached page is
// revalidated at the origin; retaining the pages lets a later attempt advance
// through a history longer than one request window without waiting assigned.
func (a *Adapter) assignmentOrder(ctx context.Context, number int) (held map[string]assignEvent, seen map[string]bool, err error) {
	events, err := a.issueEvents(ctx, number, issueEventOptions{revalidate: true, refuse: true})
	if err != nil {
		return nil, nil, err
	}
	held, seen = replayAssignments(events)
	return held, seen, nil
}

// replayAssignments is the pure half, over events already in tracker order.
func replayAssignments(events []*gh.IssueEvent) (held map[string]assignEvent, seen map[string]bool) {
	held, seen = map[string]assignEvent{}, map[string]bool{}
	for _, ev := range events {
		login := strings.ToLower(ev.GetAssignee().GetLogin())
		if login == "" {
			continue
		}
		switch ev.GetEvent() {
		case "assigned":
			seen[login] = true
			// Only the start of the current streak counts: a login assigned,
			// unassigned, then assigned again queues behind whoever held it
			// throughout.
			if _, ok := held[login]; !ok {
				held[login] = assignEvent{ev.GetCreatedAt().Time, ev.GetID()}
			}
		case "unassigned":
			seen[login] = true
			delete(held, login)
		}
	}
	return held, seen
}

// issueEvents reads the issue's whole change log in tracker order. Recovery
// bypasses the cache because its contract requires a cache-bypassing read.
// Claim arbitration and milestone occurrences instead revalidate every page by
// ETag: the origin still attests to the facts, while retaining unchanged pages
// allows bounded progress and avoids billing old history repeatedly (SPEC
// §8.2, §8.4).
//
// The endpoint's order is documented as ascending, but a verdict every daemon
// must agree on cannot rest on that, so it is sorted here by
// (created_at, event id).
type issueEventOptions struct {
	wait       bool
	revalidate bool
	refuse     bool
}

func (a *Adapter) issueEvents(ctx context.Context, number int, options issueEventOptions) ([]*gh.IssueEvent, error) {
	var events []*gh.IssueEvent
	opts := &gh.ListOptions{PerPage: perPage}
	for {
		pageCtx := ctx
		if options.wait && opts.Page != 0 {
			pageCtx = waitForRequestBudget(pageCtx)
		}
		if options.refuse {
			pageCtx = refuseConditionalRequestBudget(pageCtx)
		}
		if !options.revalidate {
			pageCtx = withoutCache(pageCtx)
		}
		page, resp, err := a.client.Issues.ListIssueEvents(pageCtx, a.cfg.owner, a.cfg.repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("reading change log of issue #%d: %w", number, a.gate.observe(err))
		}
		events = append(events, page...)
		if resp.NextPage == 0 {
			// A cached terminal page can gain a successor without changing its
			// body: exactly perPage old events still occupy it after an append, and
			// a body-derived ETag can therefore answer 304 with the old no-Link
			// header. Probe the following page whenever that cached boundary is
			// full so a newly appended milestone transition cannot be hidden.
			fromCache := resp.Response != nil && resp.Response.Header.Get("X-From-Cache") == "1"
			if options.revalidate && fromCache && len(page) == perPage {
				pageNumber := opts.Page
				if pageNumber == 0 {
					pageNumber = 1
				}
				opts.Page = pageNumber + 1
				continue
			}
			break
		}
		// Recovery holds raw pages only in this call, so it waits across request
		// windows. Milestone pages are ETag-cached and can return: the next tick
		// revalidates the same prefix for free. Claim arbitration also returns
		// promptly because it is already assigned and must preserve its reserved
		// release request (SPEC §8.4, §9.10).
		opts.Page = resp.NextPage
	}

	slices.SortFunc(events, func(x, y *gh.IssueEvent) int {
		return assignEvent{x.GetCreatedAt().Time, x.GetID()}.compare(assignEvent{y.GetCreatedAt().Time, y.GetID()})
	})
	return events, nil
}

func (e assignEvent) compare(other assignEvent) int {
	switch {
	case e.before(other):
		return -1
	case other.before(e):
		return 1
	default:
		return 0
	}
}

// Release drops this daemon's claim. It is idempotent: removing an assignee
// that is not there is a no-op on GitHub's side.
func (a *Adapter) Release(ctx context.Context, issue core.Issue) error {
	if err := a.admit(); err != nil {
		return err
	}
	number, err := issueNumber(issue)
	if err != nil {
		return err
	}
	principal, err := a.claimPrincipal(ctx)
	if err != nil {
		return err
	}
	return a.releaseAs(ctx, number, principal)
}

func (a *Adapter) releaseAs(ctx context.Context, number int, principal string) error {
	if _, _, err := a.client.Issues.RemoveAssignees(ctx, a.cfg.owner, a.cfg.repo, number, []string{principal}); err != nil {
		return fmt.Errorf("unassigning issue #%d: %w", number, a.gate.observe(err))
	}
	return nil
}

// SetStateLabels projects orchestrator state onto exactly one `ben:*` label
// (SPEC §9.3): a set-to, not an add-to.
//
// Two properties the obvious implementation does not have.
//
// Exact: the labels to clear come from the tracker's own answer, never from
// issue.Labels. A caller holding a view from an earlier tick would otherwise
// leave the label it did not know about — `ben:claimed` surviving alongside
// `ben:running`.
//
// Crash-safe: the new label goes on before the old one comes off. A daemon
// killed between the two writes leaves two state labels, which recovery reads
// as an orphan and re-runs (§9.10 step 2). Removing first would leave none,
// and no state label on an assigned issue means published-awaiting-review
// (§9.10 step 3) — a run abandoned, silently, as if it had succeeded.
func (a *Adapter) SetStateLabels(ctx context.Context, issue core.Issue, label core.StateLabel) error {
	if err := a.admit(); err != nil {
		return err
	}
	number, err := issueNumber(issue)
	if err != nil {
		return err
	}
	want, err := stateLabelName(label)
	if err != nil {
		return fmt.Errorf("%w: %q", err, label)
	}

	current, err := a.projectLabel(ctx, number, want)
	if err != nil {
		return err
	}
	for _, existing := range current {
		if !isStateLabel(existing) || (want != "" && strings.EqualFold(existing, want)) {
			continue
		}
		if _, err := a.client.Issues.RemoveLabelForIssue(ctx, a.cfg.owner, a.cfg.repo, number, existing); err != nil {
			// A label another actor removed first is the state we wanted.
			if isNotFound(err) {
				continue
			}
			return fmt.Errorf("removing label %q from issue #%d: %w", existing, number, a.gate.observe(err))
		}
	}
	return nil
}

// projectLabel writes the target label if there is one and returns the
// issue's labels as the tracker now reports them. Adding a label returns the
// full set, so the common path costs no extra request; only the terminal
// "clear the projection" case has to ask.
func (a *Adapter) projectLabel(ctx context.Context, number int, want string) ([]string, error) {
	if want != "" {
		// Idempotent on GitHub: adding a label the issue already carries
		// changes nothing and still answers with the whole set.
		labels, _, err := a.client.Issues.AddLabelsToIssue(ctx, a.cfg.owner, a.cfg.repo, number, []string{want})
		if err != nil {
			return nil, fmt.Errorf("adding label %q to issue #%d: %w", want, number, a.gate.observe(err))
		}
		return labelNames(labels), nil
	}

	var all []*gh.Label
	opts := &gh.ListOptions{PerPage: perPage}
	for {
		// Uncached: a stale label set would leave the projection standing at
		// exactly the moment §9.3 says it must come down.
		page, resp, err := a.client.Issues.ListLabelsByIssue(withoutCache(ctx), a.cfg.owner, a.cfg.repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("reading labels of issue #%d: %w", number, a.gate.observe(err))
		}
		all = append(all, page...)
		if resp.NextPage == 0 {
			return labelNames(all), nil
		}
		opts.Page = resp.NextPage
	}
}

func labelNames(labels []*gh.Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.GetName())
	}
	return out
}

// Comment posts one of the four milestone comments (SPEC §8.4). Anything
// outside that set is a refusal, not prose.
//
// Idempotent per milestone kind and claim cycle. Each comment carries a
// marker, and one already on the issue makes this a no-op. That is what lets
// recovery *complete* an interrupted terminal projection: a kill between the
// label write and the comment would otherwise leave a milestone that can
// never be written, because the projection it belonged to is finished and no
// later transition will re-attempt it. Re-issuing has to be free.
func (a *Adapter) Comment(ctx context.Context, issue core.Issue, c core.MilestoneComment) error {
	if err := a.admit(); err != nil {
		return err
	}
	number, err := issueNumber(issue)
	if err != nil {
		return err
	}
	principal, err := a.claimPrincipal(ctx)
	if err != nil {
		return err
	}
	body, err := renderMilestone(c, a.identity(), principal)
	if err != nil {
		return err
	}

	anchor, err := a.milestoneOccurrence(ctx, number, c.Milestone, principal)
	if err != nil {
		return err
	}
	marker := milestoneMarker(c.Milestone, anchor.id)
	posted, err := a.hasMarker(ctx, number, marker, anchor.at)
	if err != nil {
		return err
	}
	if posted {
		return nil
	}

	// The marker walk is uncached correctness evidence. Once it completes, keep
	// its write attached to the same attempt rather than make a later retry repeat
	// those pages. The event walk above needs no such wait: its pages are
	// ETag-revalidated, so a budget refusal releases the serial effects worker and
	// the next tick resumes through free 304s.
	ctx = waitForRequestBudget(ctx)
	body += "\n" + marker + "\n"
	if _, _, err := a.client.Issues.CreateComment(ctx, a.cfg.owner, a.cfg.repo, number, &gh.IssueComment{Body: &body}); err != nil {
		return fmt.Errorf("commenting on issue #%d: %w", number, a.gate.observe(err))
	}
	return nil
}

// milestoneMarker is the machine-readable identity of one occurrence of one
// milestone. An HTML comment: legible to us, invisible in rendered markdown,
// and not prose (SPEC §3.3).
func milestoneMarker(m core.Milestone, occurrence int64) string {
	return fmt.Sprintf("<!-- ben:milestone kind=%s occurrence=%d -->", m, occurrence)
}

// milestoneAnchor is the label transition one occurrence of one milestone
// belongs to: the event id that names the occurrence, and when it happened.
//
// The timestamp is carried because it bounds the duplicate check. A comment
// bearing this marker cannot predate the transition the marker names — the
// milestone is written after the label (§9.2) — so the anchor is where the
// comment list has to be read from, and everything before it is the issue's own
// discussion (see hasMarker).
type milestoneAnchor struct {
	id int64
	at time.Time
}

func anchorOf(ev *gh.IssueEvent) milestoneAnchor {
	return milestoneAnchor{id: ev.GetID(), at: ev.GetCreatedAt().Time}
}

// milestoneOccurrence identifies which occurrence of a milestone this is, as
// the label transition that defines it (SPEC §8.4).
//
// The anchor differs per kind because the four recur differently, and no
// single key works for all of them. Keying on the claim cycle would suppress
// a legitimate second needs-review — re-queueing retains the assignment
// (§9.2), so a second failure lands in the same cycle. Keying on every
// transition would spam claimed — §9.3 maps preparing, verifying and backoff
// onto ben:claimed too, so the label cycles within one claim.
//
// Every milestone is posted after the label write that anchors it (§9.2), so
// the anchor is always already on the log by the time we look.
func (a *Adapter) milestoneOccurrence(ctx context.Context, number int, m core.Milestone, principal string) (milestoneAnchor, error) {
	events, err := a.issueEvents(ctx, number, issueEventOptions{revalidate: true})
	if err != nil {
		return milestoneAnchor{}, err
	}

	switch m {
	case core.MilestoneClaimed:
		// The *first* ben:claimed of this claim cycle — later re-entries are
		// the same claim, not a new one. Scoping to the cycle is what makes
		// "first" meaningful, so this is the one kind that needs the claim.
		held, _ := replayAssignments(events)
		start, ok := held[strings.ToLower(principal)]
		if !ok {
			return milestoneAnchor{}, fmt.Errorf("%w: issue #%d has no standing assignment to %s to scope its claim milestone",
				ErrNoMilestoneOccurrence, number, principal)
		}
		for _, ev := range events {
			if ev.GetID() >= start.id && labeledWith(ev, "labeled", string(core.StateLabelClaimed)) {
				return anchorOf(ev), nil
			}
		}
	case core.MilestoneNeedsReview:
		// Each parking is its own occurrence: a human re-queues by removing
		// the label, and the next failure owes a fresh comment.
		if anchor, ok := lastTransition(events, "labeled", string(core.StateLabelNeedsReview)); ok {
			return anchor, nil
		}
	case core.MilestoneFailed:
		if anchor, ok := lastTransition(events, "labeled", string(core.StateLabelFailed)); ok {
			return anchor, nil
		}
	case core.MilestonePublished:
		// done clears the projection; the transition that removed it is what
		// the publish comment belongs to.
		if anchor, ok := lastTransition(events, "unlabeled", string(core.StateLabelClaimed), string(core.StateLabelRunning)); ok {
			return anchor, nil
		}
	default:
		return milestoneAnchor{}, fmt.Errorf("%w: %q", ErrUnknownMilestone, m)
	}
	return milestoneAnchor{}, fmt.Errorf("%w: issue #%d carries no label transition anchoring the %q milestone",
		ErrNoMilestoneOccurrence, number, m)
}

// lastTransition returns the most recent kind-transition naming any of the given
// state labels. Events arrive ordered, so the last match wins.
func lastTransition(events []*gh.IssueEvent, kind string, labels ...string) (milestoneAnchor, bool) {
	var anchor milestoneAnchor
	var found bool
	for _, ev := range events {
		if labeledWith(ev, kind, labels...) {
			anchor, found = anchorOf(ev), true
		}
	}
	return anchor, found
}

func labeledWith(ev *gh.IssueEvent, kind string, labels ...string) bool {
	if ev.GetEvent() != kind {
		return false
	}
	for _, l := range labels {
		if strings.EqualFold(ev.GetLabel().GetName(), statePrefix+l) {
			return true
		}
	}
	return false
}

// hasMarker reports whether the issue already carries this milestone.
//
// Read from the anchor forward, not from the beginning of the issue. The cost of
// the whole-list walk this replaces grew with the issue's discussion — unbounded,
// on a path taken up to four times per issue per occurrence, and billed every
// time (SPEC §8.5). What the anchor bounds it to is the only window a duplicate
// could be in: this marker names a label transition, and the milestone carrying
// it is written after that transition (§9.2), so no comment before it can bear
// the marker.
//
// The window is opened a second early because that boundary is not worth
// trusting: GitHub documents `since` as "last updated after the given time"
// while comparing it inclusively in practice, and the anchor's timestamp arrives
// floored to the second, so a milestone posted in the anchor's own second sits
// exactly on the line. One second of slack means neither reading of the filter
// can hide our own marker; it costs one extra second of comments to scan, and a
// missed marker would cost a duplicate milestone (SPEC §8.4).
//
// An anchor whose event carried no timestamp falls back to the whole list: the
// bound is only as good as the fact it rests on, and a window that cannot be
// justified is worse than a walk that is merely expensive.
//
// Uncached, for the usual reason: a cached comment list would let a duplicate
// through, and suppressing duplicates is the entire job.
func (a *Adapter) hasMarker(ctx context.Context, number int, marker string, anchoredAt time.Time) (bool, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: perPage}}
	if !anchoredAt.IsZero() {
		since := anchoredAt.Add(-time.Second)
		opts.Since = &since
	}
	for {
		pageCtx := ctx
		if opts.Page != 0 {
			pageCtx = waitForRequestBudget(pageCtx)
		}
		page, resp, err := a.client.Issues.ListComments(withoutCache(pageCtx), a.cfg.owner, a.cfg.repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("reading comments of issue #%d: %w", number, a.gate.observe(err))
		}
		for _, comment := range page {
			if strings.Contains(comment.GetBody(), marker) {
				return true, nil
			}
		}
		if resp.NextPage == 0 {
			return false, nil
		}
		opts.Page = resp.NextPage
	}
}

// identity is the multi-daemon-ready daemon name carried in claim comments
// (SPEC §8.4).
func (a *Adapter) identity() string {
	return a.host + "/" + a.cfg.workflowKey
}

// checkClaimPrincipal proves a configured machine user can be assigned in this
// repository. GitHub deliberately gives every negative answer as 404, including
// an account that does not exist, so BEN preserves that one verdict and does not
// spend another request inventing a distinction with the same remedy.
func (a *Adapter) checkClaimPrincipal(ctx context.Context, principal string) error {
	if err := a.admit(); err != nil {
		return err
	}
	assignable, _, err := a.client.Issues.IsAssignee(withoutCache(ctx), a.cfg.owner, a.cfg.repo, principal)
	if err != nil {
		return fmt.Errorf("%w: %q in %s/%s: %w", ErrClaimPrincipalProbe, principal, a.cfg.owner, a.cfg.repo, a.gate.observe(err))
	}
	if !assignable {
		return fmt.Errorf("%w: %q in %s/%s", ErrClaimPrincipalNotAssignable, principal, a.cfg.owner, a.cfg.repo)
	}
	return nil
}

// claimPrincipal resolves and caches the configured assignee or token login.
//
// It gates itself rather than trusting callers to: it issues a request, and a
// caller that gates only its own work would spend one here on every retry
// while the window is open.
func (a *Adapter) claimPrincipal(ctx context.Context) (string, error) {
	a.mu.Lock()
	cached := a.principal
	a.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	if err := a.admit(); err != nil {
		return "", err
	}

	user, _, err := a.client.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoPrincipal, a.gate.observe(err))
	}
	login := user.GetLogin()
	if login == "" {
		return "", fmt.Errorf("%w: the authenticated user has no login (a GitHub App installation token cannot be an assignee — see SPEC §8.4)", ErrNoPrincipal)
	}

	a.mu.Lock()
	a.principal = login
	a.mu.Unlock()
	return login, nil
}

func issueNumber(issue core.Issue) (int, error) {
	n, err := strconv.Atoi(issue.Identifier)
	if err != nil {
		return 0, fmt.Errorf("issue identifier %q is not a GitHub issue number", issue.Identifier)
	}
	return n, nil
}

// isNotFound reports the status, and deliberately not a verdict: what a 404
// means depends on which call got it. On a read naming one issue it is
// core.ErrIssueNotFound (Get, ClaimHistory); on the label DELETE it is a label
// another actor removed first, which is the state SetStateLabels wanted. The two
// call sites read the same predicate and conclude different things, which is why
// this cannot be a mapping.
func isNotFound(err error) bool {
	var errResp *gh.ErrorResponse
	return errors.As(err, &errResp) && errResp.Response != nil && errResp.Response.StatusCode == http.StatusNotFound
}
