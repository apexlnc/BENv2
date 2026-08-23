package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v90/github"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The §9.5 content read, and the one thing in this adapter REST cannot answer.
//
// GitHub's REST issue payload carries no edit fact at all: not whether a body
// was edited, and not when. `updated_at` moves for a label, an assignment, a
// comment — everything — so it cannot separate "someone edited the text" from
// "someone triaged it". GraphQL's `lastEditedAt` is the only field that says it.
//
// Hand-rolled over the adapter's own authenticated client rather than through
// `shurcooL/githubv4`, decided on #49: this package already owns net/http and
// the transport that carries the bearer token, the cache posture, and the rate
// gate, so one more request adds no module, no SPEC Appendix A entry, and no
// internal/arch allowlist change. What it costs is this file.

// defaultGraphQLURL is github.com's GraphQL endpoint. GitHub Enterprise serves
// it at <host>/api/graphql beside the REST <host>/api/v3.
const defaultGraphQLURL = "https://api.github.com/graphql"

// contentApprovalQuery reads both halves of §9.5's content fact in one
// response, plus the content itself.
//
// `lastEditedAt` alone would pass a retitled issue. Measured against issue #39:
// a title-only rename left `lastEditedAt` null and `userContentEdits.totalCount`
// at 0, while a `RenamedTitleEvent` recorded the change — so the rename event is
// not a second opinion on the body fact, it is the *only* evidence a title
// moved. Both are read; neither is trusted to cover the other.
//
// `last: 1` because only the most recent rename can be after the approving
// instant: an earlier one is dominated by it.
const contentApprovalQuery = `query($owner:String!,$repo:String!,$number:Int!){
  repository(owner:$owner,name:$repo){
    issue(number:$number){
      title
      body
      lastEditedAt
      timelineItems(last:1,itemTypes:[RENAMED_TITLE_EVENT]){
        nodes{ ... on RenamedTitleEvent { createdAt } }
      }
    }
  }
}`

// graphqlEndpoint derives the GraphQL URL from the configured REST api_url.
//
// It mirrors go-github's own WithEnterpriseURLs rule so the two cannot point at
// different servers: that function appends `api/v3/` unless the path already
// ends in it or the host is an `api.` host, which is exactly the distinction
// between GitHub Enterprise (REST at <host>/api/v3, GraphQL at <host>/api/graphql)
// and github.com (REST at api.github.com, GraphQL at api.github.com/graphql).
//
// **The path is read before the host, and the order is the whole rule.** An
// explicit `/api/v3` suffix names the Enterprise layout outright — the operator
// has written down where REST lives — while an `api.` host is only a heuristic
// about which product this is. Reading the host first turns
// `https://api.ghe.example.com/api/v3/` into `/api/v3/graphql`, which is an
// Enterprise server addressed as though it were github.com: every content read
// 404s, and §9.5 parks the whole queue for want of an edit fact. go-github does
// not have this bug because its two conditions are `||`-ed into one decision
// about whether to *append*; here they select between two different layouts, so
// they have to be ordered.
//
// Derived rather than configured: a second URL in the provider block would be a
// second thing to get wrong, and an operator who pointed it at another host
// would have BEN read one server's approval facts about another server's issue.
func graphqlEndpoint(apiURL string) (string, error) {
	if apiURL == "" {
		return defaultGraphQLURL, nil
	}
	u, err := url.Parse(apiURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", &core.ConfigValueError{Field: "tracker.provider.api_url", Value: apiURL, Err: ErrInvalidAPIURL}
	}
	base := strings.TrimSuffix(u.Path, "/")
	switch {
	case strings.HasSuffix(base, "/api/v3"):
		u.Path = strings.TrimSuffix(base, "/api/v3") + "/api/graphql"
	case strings.HasPrefix(u.Host, "api.") || strings.Contains(u.Host, ".api."):
		u.Path = base + "/graphql"
	default:
		u.Path = base + "/api/graphql"
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// contentApprovalResponse is the shape the §9.5 query answers with.
type contentApprovalResponse struct {
	Data struct {
		Repository *struct {
			Issue *struct {
				Title         string     `json:"title"`
				Body          string     `json:"body"`
				LastEditedAt  *time.Time `json:"lastEditedAt"`
				TimelineItems struct {
					Nodes []struct {
						CreatedAt *time.Time `json:"createdAt"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"issue"`
		} `json:"repository"`
	} `json:"data"`
	Errors []graphQLErrorEntry `json:"errors"`
}

// graphQLErrorEntry is one entry of a GraphQL `errors` array. Type identifies a
// primary rate refusal; a secondary refusal may instead be stated only in the
// message. GraphQL is where that distinction lives: the REST half of this
// adapter reads it from the response go-github classified.
type graphQLErrorEntry struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// rateLimitedType is the GraphQL error type GitHub reports a *primary* rate
// limit with — inside an HTTP 200, unlike every REST refusal this adapter sees.
const rateLimitedType = "RATE_LIMITED"

// graphQLError turns an `errors` array into the error the rate gate can read.
//
// An entry that explicitly names a secondary limit is shaped as go-github's own
// AbuseRateLimitError. A RATE_LIMITED entry that does not is shaped as its
// RateLimitError, carrying the reset the response's headers state. Those are the
// types rateGate.classify understands — the same routing a REST refusal gets,
// arrived at from a 200. Every other error is plain: a permission problem must
// not close the door on the whole tracker.
func graphQLError(resp *http.Response, entries []graphQLErrorEntry) error {
	message := entries[0].Message
	if graphQLErrorsNameSecondaryLimit(entries) {
		return fmt.Errorf("%w: %w", ErrGraphQL, &gh.AbuseRateLimitError{
			Response: resp,
			Message:  "graphql request refused: secondary rate limit",
		})
	}
	for _, e := range entries {
		if !strings.EqualFold(e.Type, rateLimitedType) {
			continue
		}
		rl := &gh.RateLimitError{Response: resp, Message: e.Message}
		if v := resp.Header.Get(gh.HeaderRateReset); v != "" {
			if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
				rl.Rate.Reset = gh.Timestamp{Time: time.Unix(epoch, 0)}
			}
		}
		// A reset the response did not state leaves Rate.Reset zero, which
		// classify turns into the minimum wait rather than none: the gate closing
		// briefly is the fail-closed direction, and the poll interval dominates it
		// anyway. Inventing a longer wait from nothing would be a guess about the
		// server's budget.
		return fmt.Errorf("%w: %w", ErrGraphQL, rl)
	}
	return fmt.Errorf("%w: %s", ErrGraphQL, message)
}

// graphQLHTTPError keeps a non-200 response's provider prose out of operator
// logs while preserving the one fact the rate gate needs from it. GitHub may
// identify a GraphQL secondary limit only in the JSON body, with no Retry-After
// and a positive primary budget; replacing that body with a wholly generic
// ErrorResponse would make the limit indistinguishable from a permission 403.
func graphQLHTTPError(resp *http.Response, raw []byte) *gh.ErrorResponse {
	message := fmt.Sprintf("graphql request refused with status %d", resp.StatusCode)
	if graphQLBodyNamesSecondaryLimit(raw) {
		message += ": secondary rate limit"
	}
	return &gh.ErrorResponse{Response: resp, Message: message}
}

func graphQLBodyNamesSecondaryLimit(raw []byte) bool {
	var body struct {
		Message          string              `json:"message"`
		DocumentationURL string              `json:"documentation_url"`
		Errors           []graphQLErrorEntry `json:"errors"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return false
	}
	if namesSecondaryLimit(&gh.ErrorResponse{
		Message:          body.Message,
		DocumentationURL: body.DocumentationURL,
	}) {
		return true
	}
	return graphQLErrorsNameSecondaryLimit(body.Errors)
}

func graphQLErrorsNameSecondaryLimit(entries []graphQLErrorEntry) bool {
	for _, entry := range entries {
		if namesSecondaryLimit(&gh.ErrorResponse{Message: entry.Message}) {
			return true
		}
	}
	return false
}

// ContentApproval reads the issue's author-controlled content together with
// when it was last edited (SPEC §9.5, core.ContentApprovalSource).
//
// Both facts from one response on purpose: the orchestrator pins the content
// *because* the edit time says nothing has touched it since a labeler approved
// it, and a pin taken from an earlier read would leave a window between the two
// in which an edit is invisible to both.
//
// Uncached, for the reason every attesting read in this adapter is uncached: a
// 304 from our own ETag cache would attest to nothing about the world. The
// request is a POST, which condCache does not cache at all, so this is a
// property of the transport rather than a flag — stated here because a future
// caching GraphQL client would break §9.5 silently.
func (a *Adapter) ContentApproval(ctx context.Context, issue core.Issue) (core.ContentApproval, error) {
	if err := a.gate.check(); err != nil {
		return core.ContentApproval{}, err
	}
	number, err := issueNumber(issue)
	if err != nil {
		return core.ContentApproval{}, err
	}

	var body contentApprovalResponse
	resp, err := a.graphql(ctx, contentApprovalQuery, map[string]any{
		"owner": a.cfg.owner, "repo": a.cfg.repo, "number": number,
	}, &body)
	if err != nil {
		return core.ContentApproval{}, fmt.Errorf("reading the approval facts of issue #%d: %w", number, err)
	}
	if len(body.Errors) > 0 {
		// A partial response is still a refusal here. GraphQL answers 200 with an
		// `errors` array for a permission or rate problem, and the zero
		// core.ContentEdit that a silently-ignored one would leave behind is
		// `unknown`, which parks — but it parks for the wrong stated reason, and
		// an operator reading "the tracker cannot say" needs the message that
		// says why.
		//
		// **A primary rate limit arrives here, not as a status code.** GraphQL
		// reports it as `type: RATE_LIMITED` inside a 200 — only secondary limits
		// get a 403 or 429 — so a refusal read from the status alone leaves the
		// gate open and BEN spends a request per claim and per re-dispatch
		// against a budget the server has already said is gone (SPEC §8.5).
		return core.ContentApproval{}, fmt.Errorf("reading the approval facts of issue #%d: %w",
			number, a.gate.observe(graphQLError(resp, body.Errors)))
	}
	if body.Data.Repository == nil || body.Data.Repository.Issue == nil {
		return core.ContentApproval{}, fmt.Errorf("%w: #%d", core.ErrIssueNotFound, number)
	}

	iss := body.Data.Repository.Issue
	out := core.ContentApproval{Content: core.IssueContent{Title: iss.Title, Body: iss.Body}}
	// Both nulls together are the tracker positively stating "never edited": the
	// issue has neither a body edit nor a rename in its whole history. That is a
	// fact, and the only shape entitled to be one — every other path through this
	// function either states an instant or returns an error, and the zero value
	// nobody sets means unknown (BUILD.md decision 15).
	var latest time.Time
	if iss.LastEditedAt != nil {
		latest = *iss.LastEditedAt
	}
	for _, n := range iss.TimelineItems.Nodes {
		if n.CreatedAt != nil && n.CreatedAt.After(latest) {
			latest = *n.CreatedAt
		}
	}
	if latest.IsZero() {
		out.Edit = core.ContentEdit{Status: core.ContentEditNever}
		return out, nil
	}
	out.Edit = core.ContentEdit{Status: core.ContentEditAt, At: latest}
	return out, nil
}

// graphql posts one query on the adapter's authenticated client and decodes the
// response into out.
//
// It returns the response alongside the error so the caller can read its headers:
// a GraphQL primary rate limit is a 200 whose `errors` array says RATE_LIMITED,
// and the wait it asks for is in the headers of that same 200 (see graphQLError).
// The body is already consumed and closed by then; only the header map is live.
//
// The client is the same one every REST call rides: one bearer credential, one
// timeout, one rate gate. Building a second would mean a second place the token
// is applied, and #47's finding is that credential paths multiply quietly.
func (a *Adapter) graphql(ctx context.Context, query string, vars map[string]any, out any) (*http.Response, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, fmt.Errorf("%w: encoding the query: %w", ErrGraphQL, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.graphqlURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGraphQL, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ben")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGraphQL, a.gate.observe(err))
	}
	defer resp.Body.Close()
	// Bounded: this is a response BEN parses, and an unbounded read of a body a
	// misconfigured endpoint returns is memory the daemon does not control.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGraphQLResponse))
	if err != nil {
		return resp, fmt.Errorf("%w: reading the response: %w", ErrGraphQL, err)
	}
	if resp.StatusCode != http.StatusOK {
		// Shaped as go-github's own ErrorResponse, because that is the type
		// rateGate.classify reads: a GraphQL *secondary* limit carries the same
		// 403-or-429, Retry-After and spent budget a REST one does, and routing it
		// through the same gate closes the same window the poll loop already honors
		// (SPEC §8.5). The primary limit does not come this way — see graphQLError.
		// The body's explicit secondary-limit signal is retained even when those
		// headers are absent; every other 403 is left as a permission refusal (#198).
		// Provider prose is deliberately not echoed because this reaches an operator
		// log — graphQLHTTPError reduces it to that closed classification.
		return resp, fmt.Errorf("%w: %w", ErrGraphQL, a.gate.observe(graphQLHTTPError(resp, raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp, fmt.Errorf("%w: decoding the response: %w", ErrGraphQL, err)
	}
	return resp, nil
}

// maxGraphQLResponse bounds one decoded response. An issue body is capped by
// GitHub at 64 KiB; a megabyte is room for that plus the envelope and no room
// for an endpoint that answers with something else entirely.
const maxGraphQLResponse = 1 << 20
