package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

type remotePRAnswer struct {
	nodes                                            []remotePRAnswerNode
	more, absentRepo, absentIssue                    bool
	omitPRTotalCount, omitPRHasNextPage, omitPRNodes bool
	errMessage, errType                              string
	status                                           int
}

type remotePRAnswerNode struct {
	number                                                   int
	url, state, head, oid, base                              string
	baseRepo, headRepo                                       string
	closing                                                  []remoteClosingAnswer
	moreClosing, noBaseRepo, noHeadRepo, noClosingConnection bool
	omitClosingTotalCount, omitClosingHasNextPage            bool
	omitClosingNodes                                         bool
}

type remoteClosingAnswer struct {
	number  int
	repo    string
	nilRepo bool
}

func validRemotePRAnswer() remotePRAnswer {
	return remotePRAnswer{nodes: []remotePRAnswerNode{{
		number:   17,
		url:      "https://github.test/acme/widgets/pull/17",
		state:    "OPEN",
		head:     "ben/7",
		oid:      "1111111111111111111111111111111111111111",
		base:     "main",
		baseRepo: "acme/widgets",
		headRepo: "acme/widgets",
		closing:  []remoteClosingAnswer{{number: 7, repo: "acme/widgets"}},
	}}}
}

func (f *fakeGitHub) handleRemotePRGraphQL(answer func() remotePRAnswer) {
	f.handle("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, field := range []string{"headRefOid", "baseRepository", "headRepository", "closingIssuesReferences"} {
			if !strings.Contains(payload.Query, field) {
				f.t.Errorf("remote PR query omitted %s:\n%s", field, payload.Query)
			}
		}
		wantVars := map[string]any{"owner": testOwner, "repo": testRepo, "branch": "ben/7", "issue": float64(7)}
		for key, want := range wantVars {
			if got := payload.Variables[key]; got != want {
				f.t.Errorf("query variable %s = %v, want %v", key, got, want)
			}
		}

		a := answer()
		if a.status != 0 {
			w.WriteHeader(a.status)
			return
		}
		if a.errMessage != "" {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test server
				"data":   map[string]any{"repository": nil},
				"errors": []map[string]any{{"type": a.errType, "message": a.errMessage}},
			})
			return
		}
		if a.absentRepo {
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": nil}}) //nolint:errcheck // test server
			return
		}

		var issue any = map[string]any{"number": 7}
		if a.absentIssue {
			issue = nil
		}
		nodes := make([]map[string]any, 0, len(a.nodes))
		for _, node := range a.nodes {
			var baseRepository any = map[string]any{"nameWithOwner": node.baseRepo}
			if node.noBaseRepo {
				baseRepository = nil
			}
			var headRepository any = map[string]any{"nameWithOwner": node.headRepo}
			if node.noHeadRepo {
				headRepository = nil
			}
			closing := make([]map[string]any, 0, len(node.closing))
			for _, item := range node.closing {
				var repository any = map[string]any{"nameWithOwner": item.repo}
				if item.nilRepo {
					repository = nil
				}
				closing = append(closing, map[string]any{"number": item.number, "repository": repository})
			}
			var closingConnection any
			if !node.noClosingConnection {
				pageInfo := map[string]any{"hasNextPage": node.moreClosing}
				if node.omitClosingHasNextPage {
					delete(pageInfo, "hasNextPage")
				}
				connection := map[string]any{
					"totalCount": len(closing), "pageInfo": pageInfo, "nodes": closing,
				}
				if node.omitClosingTotalCount {
					delete(connection, "totalCount")
				}
				if node.omitClosingNodes {
					delete(connection, "nodes")
				}
				closingConnection = connection
			}
			nodes = append(nodes, map[string]any{
				"number": node.number, "url": node.url, "state": node.state,
				"headRefName": node.head, "headRefOid": node.oid, "baseRefName": node.base,
				"baseRepository": baseRepository, "headRepository": headRepository,
				"closingIssuesReferences": closingConnection,
			})
		}
		pageInfo := map[string]any{"hasNextPage": a.more}
		if a.omitPRHasNextPage {
			delete(pageInfo, "hasNextPage")
		}
		pullRequests := map[string]any{
			"totalCount": len(nodes), "pageInfo": pageInfo, "nodes": nodes,
		}
		if a.omitPRTotalCount {
			delete(pullRequests, "totalCount")
		}
		if a.omitPRNodes {
			delete(pullRequests, "nodes")
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test server
			"data": map[string]any{"repository": map[string]any{
				"issue": issue, "pullRequests": pullRequests,
			}},
		})
	})
}

func remotePRRequest() core.RemotePRQuery {
	return core.RemotePRQuery{
		Issue: core.Issue{Identifier: "7"}, Repository: remotePRRepositoryIdentity, Branch: "ben/7",
	}
}

const remotePRCloneURL = "https://github.test:8443/acme/widgets.git"

var remotePRRepositoryIdentity = mustRepositoryIdentity(remotePRCloneURL)

// readyRemotePRAdapter establishes the clone identity RemotePR is required to
// bind to, then clears the readiness probes from request-count assertions.
func readyRemotePRAdapter(t *testing.T, f *fakeGitHub) *Adapter {
	t.Helper()
	f.serveRepoWithCloneURL(remotePRCloneURL)
	a := f.adapter(t)
	if err := a.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	f.reset()
	return a
}

func TestRemotePRReadsCompleteIndependentFacts(t *testing.T) {
	f := newFakeGitHub(t)
	answer := validRemotePRAnswer()
	// A cross-repository reference is not this repository's issue identifier,
	// and a duplicate same-repository reference is normalized once.
	answer.nodes[0].closing = append(answer.nodes[0].closing,
		remoteClosingAnswer{number: 7, repo: "acme/widgets"},
		remoteClosingAnswer{number: 9, repo: "someone/else"})
	f.handleRemotePRGraphQL(func() remotePRAnswer { return answer })

	got, err := readyRemotePRAdapter(t, f).RemotePR(t.Context(), remotePRRequest())
	if err != nil {
		t.Fatalf("RemotePR: %v", err)
	}
	if got == nil {
		t.Fatal("RemotePR returned nil")
	}
	want := core.RemotePR{
		Number: 17, URL: "https://github.test/acme/widgets/pull/17", State: "open",
		Repository: remotePRRepositoryIdentity, HeadRepository: remotePRRepositoryIdentity,
		HeadBranch: "ben/7", HeadSHA: answer.nodes[0].oid, BaseBranch: "main",
		LinkStatus: core.RemoteIssueLinkStated, LinkedIssues: []string{"7"},
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("RemotePR = %+v, want %+v", *got, want)
	}
}

func TestRemotePRStatesAbsenceAndAnEmptyLinkEnumeration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer remotePRAnswer
		nilPR  bool
	}{
		{"no open pull request", remotePRAnswer{}, true},
		{"pull request closes nothing", func() remotePRAnswer {
			a := validRemotePRAnswer()
			a.nodes[0].closing = nil
			return a
		}(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handleRemotePRGraphQL(func() remotePRAnswer { return tc.answer })
			got, err := readyRemotePRAdapter(t, f).RemotePR(t.Context(), remotePRRequest())
			if err != nil {
				t.Fatal(err)
			}
			if tc.nilPR {
				if got != nil {
					t.Fatalf("RemotePR = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.LinkStatus != core.RemoteIssueLinkStated || len(got.LinkedIssues) != 0 {
				t.Fatalf("RemotePR = %+v, want a stated empty link enumeration", got)
			}
		})
	}
}

func TestRemotePRRefusesAmbiguousOrIncompleteEnumeration(t *testing.T) {
	valid := validRemotePRAnswer()
	tests := []struct {
		name   string
		answer remotePRAnswer
		want   error
	}{
		{"two candidates", remotePRAnswer{nodes: append(slices.Clone(valid.nodes), valid.nodes[0])}, core.ErrRemotePRAmbiguous},
		{"another candidate page", remotePRAnswer{nodes: valid.nodes, more: true}, core.ErrRemotePRAmbiguous},
		{"pull-request count omitted", remotePRAnswer{omitPRTotalCount: true}, core.ErrRemotePRIncomplete},
		{"pull-request page verdict omitted", remotePRAnswer{omitPRHasNextPage: true}, core.ErrRemotePRIncomplete},
		{"pull-request nodes omitted", remotePRAnswer{omitPRNodes: true}, core.ErrRemotePRIncomplete},
		{"another closing-issue page", func() remotePRAnswer {
			a := validRemotePRAnswer()
			a.nodes[0].moreClosing = true
			return a
		}(), core.ErrRemotePRIncomplete},
		{"closing-issue count omitted", func() remotePRAnswer {
			a := validRemotePRAnswer()
			a.nodes[0].closing = nil
			a.nodes[0].omitClosingTotalCount = true
			return a
		}(), core.ErrRemotePRIncomplete},
		{"closing-issue page verdict omitted", func() remotePRAnswer {
			a := validRemotePRAnswer()
			a.nodes[0].closing = nil
			a.nodes[0].omitClosingHasNextPage = true
			return a
		}(), core.ErrRemotePRIncomplete},
		{"closing-issue nodes omitted", func() remotePRAnswer {
			a := validRemotePRAnswer()
			a.nodes[0].closing = nil
			a.nodes[0].omitClosingNodes = true
			return a
		}(), core.ErrRemotePRIncomplete},
		{"malformed closing issue", func() remotePRAnswer {
			a := validRemotePRAnswer()
			a.nodes[0].closing[0].nilRepo = true
			return a
		}(), core.ErrRemotePRIncomplete},
		{"closing-issue enumeration omitted", func() remotePRAnswer {
			a := validRemotePRAnswer()
			a.nodes[0].noClosingConnection = true
			return a
		}(), core.ErrRemotePRIncomplete},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handleRemotePRGraphQL(func() remotePRAnswer { return tc.answer })
			got, err := readyRemotePRAdapter(t, f).RemotePR(t.Context(), remotePRRequest())
			if !errors.Is(err, tc.want) {
				t.Fatalf("RemotePR = %+v, %v; want %v", got, err, tc.want)
			}
			if got != nil {
				t.Fatalf("a refused enumeration returned %+v", got)
			}
		})
	}
}

func TestRemotePRPreservesForkIdentity(t *testing.T) {
	f := newFakeGitHub(t)
	answer := validRemotePRAnswer()
	answer.nodes[0].headRepo = "someone/fork"
	f.handleRemotePRGraphQL(func() remotePRAnswer { return answer })
	got, err := readyRemotePRAdapter(t, f).RemotePR(t.Context(), remotePRRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got.HeadRepository, "/github.test:8443/someone/fork") || got.HeadRepository == got.Repository {
		t.Fatalf("fork identity = %q beside base %q", got.HeadRepository, got.Repository)
	}
}

func TestRemotePRRefusesProviderFailuresAndMalformedFacts(t *testing.T) {
	tests := []struct {
		name   string
		answer remotePRAnswer
		want   error
	}{
		{"GraphQL error", remotePRAnswer{errType: "FORBIDDEN", errMessage: "denied"}, ErrGraphQL},
		{"non-200", remotePRAnswer{status: http.StatusInternalServerError}, ErrGraphQL},
		{"repository absent", remotePRAnswer{absentRepo: true}, ErrGraphQL},
		{"issue absent", remotePRAnswer{absentIssue: true}, core.ErrIssueNotFound},
		{"head omitted", func() remotePRAnswer {
			a := validRemotePRAnswer()
			a.nodes[0].oid = ""
			return a
		}(), ErrGraphQL},
		{"head repository omitted", func() remotePRAnswer {
			a := validRemotePRAnswer()
			a.nodes[0].noHeadRepo = true
			return a
		}(), ErrGraphQL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handleRemotePRGraphQL(func() remotePRAnswer { return tc.answer })
			got, err := readyRemotePRAdapter(t, f).RemotePR(t.Context(), remotePRRequest())
			if !errors.Is(err, tc.want) {
				t.Fatalf("RemotePR = %+v, %v; want %v", got, err, tc.want)
			}
			if got != nil {
				t.Fatalf("a refused response returned %+v", got)
			}
		})
	}
}

func TestRemotePRBindsRepositoryToReadyCloneBeforeARequest(t *testing.T) {
	for _, repository := range []string{
		"github.test:8443/acme/another",
		"github.test:9443/acme/widgets",
		"elsewhere.test:8443/acme/widgets",
	} {
		t.Run(repository, func(t *testing.T) {
			f := newFakeGitHub(t)
			a := readyRemotePRAdapter(t, f)
			q := remotePRRequest()
			q.Repository = repository
			if got, err := a.RemotePR(t.Context(), q); err == nil || got != nil {
				t.Fatalf("RemotePR = %+v, %v; want a refusal", got, err)
			}
			requests, _ := f.snapshot()
			if len(requests) != 0 {
				t.Fatalf("a locally invalid query made %d request(s)", len(requests))
			}
		})
	}
}
