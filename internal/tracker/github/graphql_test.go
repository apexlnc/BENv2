package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v90/github"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The GraphQL endpoint is derived from the same api_url go-github is given, so
// the REST and GraphQL halves of one adapter cannot address different servers.
//
// The rows mirror go-github's WithEnterpriseURLs rule (github.go): append
// `api/v3/` unless the path already ends in it or the host is an `api.` host.
// A row that disagrees with that function is a bug in this table or in the
// derivation, and either way the two must be read together.
func TestGraphQLEndpointDerivation(t *testing.T) {
	for _, tc := range []struct {
		name, apiURL, want string
	}{
		{"github.com by default", "", defaultGraphQLURL},
		{"an api. host serves graphql at the root", "https://api.github.com/", "https://api.github.com/graphql"},
		{"a GHES host, bare", "https://ghe.example.com/", "https://ghe.example.com/api/graphql"},
		{"a GHES host with the REST suffix spelled out", "https://ghe.example.com/api/v3/", "https://ghe.example.com/api/graphql"},
		{"a GHES host with the REST suffix and no trailing slash", "https://ghe.example.com/api/v3", "https://ghe.example.com/api/graphql"},
		{"a GHES host behind a path prefix", "https://example.com/github/api/v3/", "https://example.com/github/api/graphql"},
		// The path wins over the host heuristic, and this row is why the two are
		// ordered rather than `||`-ed. An Enterprise install on an `api.`-prefixed
		// hostname is ordinary; reading the host first addresses it as though it
		// were github.com — `/api/v3/graphql`, which 404s — and §9.5 then parks
		// every issue in the queue for want of an edit fact.
		{"an api-prefixed GHES host with the REST suffix", "https://api.ghe.example.com/api/v3/", "https://api.ghe.example.com/api/graphql"},
		{"an api-prefixed GHES host, no suffix, is still a heuristic call", "https://api.ghe.example.com/", "https://api.ghe.example.com/graphql"},
		{"a test server with no path at all", "http://127.0.0.1:8080", "http://127.0.0.1:8080/api/graphql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := graphqlEndpoint(tc.apiURL)
			if err != nil {
				t.Fatalf("graphqlEndpoint(%q): %v", tc.apiURL, err)
			}
			if got != tc.want {
				t.Errorf("graphqlEndpoint(%q) = %q, want %q", tc.apiURL, got, tc.want)
			}
		})
	}
}

// graphQLIssue is what the fake server answers a content-approval query with.
type graphQLIssue struct {
	title, body string
	// lastEditedAt and renamedAt are RFC3339 strings, or "" for JSON null.
	lastEditedAt, renamedAt string
	// absent makes `repository.issue` null: deleted, transferred, or invisible.
	absent bool
	// errMessage puts an entry in the `errors` array of an otherwise-200 answer,
	// which is how GraphQL reports a permission or rate problem. With a nonzero
	// status it is instead the top-level error message. errType is an errors-array
	// entry's `type`, and it is the only thing separating the two.
	errMessage, errType string
	// status overrides the HTTP status.
	status int
}

// handleGraphQL installs the §9.5 content read on the fake server.
//
// The response is assembled as JSON text rather than through Go structs, and
// deliberately: the adapter's own decoding is what this exercises, so a shared
// struct definition on both sides would agree with itself about a shape GitHub
// need not produce — a null `lastEditedAt`, an empty `nodes` array, a null
// `issue` under a non-null `repository`.
func (f *fakeGitHub) handleGraphQL(answer func() graphQLIssue) {
	f.handle("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		f.t.Helper()
		a := answer()
		w.Header().Set("Content-Type", "application/json")
		if a.status != 0 {
			message := a.errMessage
			if message == "" {
				message = "nope"
			}
			w.WriteHeader(a.status)
			w.Write([]byte(`{"message":` + jsonString(message) + `}`)) //nolint:errcheck // test server
			return
		}
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The query has to ask for both facts, or the fixtures below prove
		// nothing about what a real server would return.
		for _, want := range []string{"lastEditedAt", "RENAMED_TITLE_EVENT"} {
			if !strings.Contains(payload.Query, want) {
				f.t.Errorf("the content-approval query does not ask for %s:\n%s", want, payload.Query)
			}
		}
		if got := payload.Variables["number"]; got != float64(7) {
			f.t.Errorf("query variable number = %v, want the issue number", got)
		}

		switch {
		case a.errMessage != "":
			w.Write([]byte(`{"data":{"repository":null},"errors":[{"type":` + //nolint:errcheck // test server
				jsonString(a.errType) + `,"message":` + jsonString(a.errMessage) + `}]}`))
		case a.absent:
			w.Write([]byte(`{"data":{"repository":{"issue":null}}}`)) //nolint:errcheck // test server
		default:
			w.Write([]byte(`{"data":{"repository":{"issue":{` + //nolint:errcheck // test server
				`"title":` + jsonString(a.title) +
				`,"body":` + jsonString(a.body) +
				`,"lastEditedAt":` + jsonTimeOrNull(a.lastEditedAt) +
				`,"timelineItems":{"nodes":[` + renameNode(a.renamedAt) + `]}}}}}`))
		}
	})
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func jsonTimeOrNull(s string) string {
	if s == "" {
		return "null"
	}
	return jsonString(s)
}

func renameNode(at string) string {
	if at == "" {
		return ""
	}
	return `{"createdAt":` + jsonString(at) + `}`
}

// The three shapes an edit fact takes, and the one that is a positive "never".
//
// The rename row is the one that matters most: measured against issue #39, a
// title-only rename leaves `lastEditedAt` null and `userContentEdits` at 0, so
// the RenamedTitleEvent is the *only* evidence the title moved. A check built on
// `lastEditedAt` alone reads that row as "never edited" and dispatches a title
// nobody approved.
func TestContentApprovalReadsBothHalvesOfTheEditFact(t *testing.T) {
	edited := "2026-08-13T12:00:00Z"
	renamed := "2026-08-13T13:00:00Z"

	for _, tc := range []struct {
		name   string
		answer graphQLIssue
		want   core.ContentEdit
	}{
		{
			name:   "never edited",
			answer: graphQLIssue{title: "t", body: "b"},
			want:   core.ContentEdit{Status: core.ContentEditNever},
		},
		{
			name:   "a body edit",
			answer: graphQLIssue{title: "t", body: "b", lastEditedAt: edited},
			want:   core.ContentEdit{Status: core.ContentEditAt, At: mustTime(t, edited)},
		},
		{
			// The #39 fixture.
			name:   "a title-only rename, which lastEditedAt does not see",
			answer: graphQLIssue{title: "t", body: "b", renamedAt: renamed},
			want:   core.ContentEdit{Status: core.ContentEditAt, At: mustTime(t, renamed)},
		},
		{
			// Both, and the later one governs: the question is when the content
			// last moved, not which half moved.
			name:   "both, latest wins",
			answer: graphQLIssue{title: "t", body: "b", lastEditedAt: edited, renamedAt: renamed},
			want:   core.ContentEdit{Status: core.ContentEditAt, At: mustTime(t, renamed)},
		},
		{
			name:   "both, with the body edit later",
			answer: graphQLIssue{title: "t", body: "b", lastEditedAt: renamed, renamedAt: edited},
			want:   core.ContentEdit{Status: core.ContentEditAt, At: mustTime(t, renamed)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handleGraphQL(func() graphQLIssue { return tc.answer })
			a := f.adapter(t)

			got, err := a.ContentApproval(t.Context(), core.Issue{Identifier: "7"})
			if err != nil {
				t.Fatalf("ContentApproval: %v", err)
			}
			if got.Edit.Status != tc.want.Status || !got.Edit.At.Equal(tc.want.At) {
				t.Errorf("edit = %+v, want %+v", got.Edit, tc.want)
			}
			// The content rides with the fact, from one response: the caller pins
			// these bytes *because* of that edit time.
			if got.Content.Title != tc.answer.title || got.Content.Body != tc.answer.body {
				t.Errorf("content = %+v, want the issue's title and body", got.Content)
			}
		})
	}
}

// Every way the read can fail to produce an answer is an error, never a
// silently-unknown edit fact that some later caller might read as safe.
func TestContentApprovalRefusesRatherThanGuessing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer graphQLIssue
		want   error
	}{
		{"a GraphQL errors array", graphQLIssue{errMessage: "Resource not accessible by integration"}, ErrGraphQL},
		{"a non-200", graphQLIssue{status: http.StatusInternalServerError}, ErrGraphQL},
		{"the issue is gone", graphQLIssue{absent: true}, core.ErrIssueNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handleGraphQL(func() graphQLIssue { return tc.answer })
			a := f.adapter(t)

			got, err := a.ContentApproval(t.Context(), core.Issue{Identifier: "7"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("ContentApproval error = %v, want %v", err, tc.want)
			}
			if got.Edit.Status != core.ContentEditUnknown {
				t.Errorf("a refusal returned edit status %s; a failed read states nothing", got.Edit.Status)
			}
		})
	}
}

// A GraphQL rate refusal closes the same window a REST one does (SPEC §8.5).
// Two endpoints with one budget need one gate, or the poll loop honors a limit
// on one half while the other keeps spending against it.
//
// GraphQL can state a secondary limit in either a 403 body or a 200 `errors`
// array. A primary limit also arrives inside a 200, whose error type says
// `RATE_LIMITED`; the message is what separates a secondary 200 from it. A gate
// driven off the status or type alone misses one of those shapes and spends a
// request per claim and per re-dispatch against a budget the server has already
// said is unavailable.
func TestContentApprovalRefusalClosesTheRateGate(t *testing.T) {
	const providerOnlyDetail = "provider-only-detail-must-not-reach-the-log"
	for _, tc := range []struct {
		name           string
		limit          func(*fakeGitHub, *graphQLIssue)
		wantSecondary  bool
		bodyOnlyStatus int
		redacted       string
	}{
		{
			name: "a secondary limit carrying Retry-After",
			limit: func(f *fakeGitHub, _ *graphQLIssue) {
				f.rateLimitAll(limitResponse{secondary: true, retryAfterSeconds: 60})
			},
			wantSecondary: true,
		},
		{
			name: "a secondary limit stated only in the response body",
			limit: func(_ *fakeGitHub, answer *graphQLIssue) {
				*answer = graphQLIssue{
					status:     http.StatusForbidden,
					errMessage: "You have exceeded a secondary rate limit. " + providerOnlyDetail,
				}
			},
			wantSecondary:  true,
			bodyOnlyStatus: http.StatusForbidden,
			redacted:       providerOnlyDetail,
		},
		{
			name: "a secondary limit inside a 200 errors array",
			limit: func(_ *fakeGitHub, answer *graphQLIssue) {
				*answer = graphQLIssue{
					errType:    "FORBIDDEN",
					errMessage: "You have exceeded a secondary rate limit. " + providerOnlyDetail,
				}
			},
			wantSecondary:  true,
			bodyOnlyStatus: http.StatusOK,
			redacted:       providerOnlyDetail,
		},
		{
			name: "a primary limit, inside a 200",
			limit: func(_ *fakeGitHub, answer *graphQLIssue) {
				*answer = graphQLIssue{errType: "RATE_LIMITED", errMessage: "API rate limit exceeded"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			answer := graphQLIssue{title: "t", body: "b"}
			f.handleGraphQL(func() graphQLIssue { return answer })
			a := f.adapter(t)
			tc.limit(f, &answer)

			_, err := a.ContentApproval(t.Context(), core.Issue{Identifier: "7"})
			var observed *RateLimitError
			if !errors.As(err, &observed) {
				t.Fatalf("ContentApproval error = %v, want a *RateLimitError", err)
			}
			if observed.Secondary != tc.wantSecondary {
				t.Errorf("Secondary = %v, want %v", observed.Secondary, tc.wantSecondary)
			}
			if tc.bodyOnlyStatus != 0 {
				var response *http.Response
				var errResp *gh.ErrorResponse
				var abuse *gh.AbuseRateLimitError
				switch {
				case errors.As(err, &errResp):
					response = errResp.Response
				case errors.As(err, &abuse):
					response = abuse.Response
				}
				if response == nil {
					t.Fatalf("ContentApproval error = %v, want its GitHub response", err)
				}
				if response.StatusCode != tc.bodyOnlyStatus {
					t.Fatalf("response status = %d, want %d", response.StatusCode, tc.bodyOnlyStatus)
				}
				if got := response.Header.Get("Retry-After"); got != "" {
					t.Fatalf("Retry-After = %q, want the body to be the only secondary-limit signal", got)
				}
				remaining, convErr := strconv.Atoi(response.Header.Get(gh.HeaderRateRemaining))
				if convErr != nil || remaining <= 0 {
					t.Fatalf("X-RateLimit-Remaining = %q, want a positive primary budget",
						response.Header.Get(gh.HeaderRateRemaining))
				}
				epoch, convErr := strconv.ParseInt(response.Header.Get(gh.HeaderRateReset), 10, 64)
				if convErr != nil || !time.Unix(epoch, 0).After(time.Now()) {
					t.Fatalf("X-RateLimit-Reset = %q, want ordinary future metadata",
						response.Header.Get(gh.HeaderRateReset))
				}
			}
			if tc.redacted != "" && strings.Contains(err.Error(), tc.redacted) {
				t.Errorf("ContentApproval error echoed provider prose: %v", err)
			}

			// The gate is now shut, so an unrelated REST read refuses without a
			// request — which is the whole point of closing it.
			before, _ := f.snapshot()
			var rl *RateLimitError
			if _, err := a.Get(t.Context(), "7"); !errors.As(err, &rl) {
				t.Fatalf("Get error = %v, want a *RateLimitError from the standing refusal", err)
			}
			if after, _ := f.snapshot(); len(after) != len(before) {
				t.Errorf("the standing refusal still spent %d request(s)", len(after)-len(before))
			}
		})
	}
}

// Every other GraphQL error is a refusal for this read and nothing more. A
// permission problem on one issue must not shut the door on the whole tracker:
// the gate exists to honor a budget the server named, not to react to any
// failure.
func TestANonRateGraphQLErrorLeavesTheGateOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer graphQLIssue
	}{
		{"inside a 200 errors array", graphQLIssue{errType: "FORBIDDEN", errMessage: "Resource not accessible by integration"}},
		{"inside a 403 response body", graphQLIssue{status: http.StatusForbidden, errMessage: "Resource not accessible by integration"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handleGraphQL(func() graphQLIssue { return tc.answer })
			a := f.adapter(t)

			if _, err := a.ContentApproval(t.Context(), core.Issue{Identifier: "7"}); !errors.Is(err, ErrGraphQL) {
				t.Fatalf("ContentApproval error = %v, want %v", err, ErrGraphQL)
			}
			var rl *RateLimitError
			if _, err := a.Get(t.Context(), "7"); errors.As(err, &rl) {
				t.Errorf("Get was refused by the rate gate after a permission error: %v", err)
			}
		})
	}
}

// The content read must not be answerable from BEN's own ETag cache: it attests
// to the world, and a 304 attests to nothing (SPEC §8.2's cache posture).
func TestContentApprovalIsNotServedFromCache(t *testing.T) {
	f := newFakeGitHub(t)
	answer := graphQLIssue{title: "t", body: "b"}
	f.handleGraphQL(func() graphQLIssue { return answer })
	a := f.adapter(t)

	if _, err := a.ContentApproval(t.Context(), core.Issue{Identifier: "7"}); err != nil {
		t.Fatalf("ContentApproval: %v", err)
	}
	// The world moves, and the second read has to see it.
	answer.lastEditedAt = "2026-08-13T12:00:00Z"
	got, err := a.ContentApproval(t.Context(), core.Issue{Identifier: "7"})
	if err != nil {
		t.Fatalf("ContentApproval: %v", err)
	}
	if got.Edit.Status != core.ContentEditAt {
		t.Errorf("edit = %+v, want the second read to see the edit rather than a cached answer", got.Edit)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
