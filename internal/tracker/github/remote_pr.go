package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/gitremote"
)

// remotePRQuery reads every forge-owned fact the remote publication verifier
// needs in one authenticated observation. The issue is read beside the pull
// request so a closing reference cannot bind a claim to a number that is not an
// issue in the configured repository.
const remotePRQuery = `query($owner:String!,$repo:String!,$branch:String!,$issue:Int!){
  repository(owner:$owner,name:$repo){
    issue(number:$issue){number}
    pullRequests(first:2,states:[OPEN],headRefName:$branch){
      totalCount
      pageInfo{hasNextPage}
      nodes{
        number
        url
        state
        headRefName
        headRefOid
        baseRefName
        baseRepository{nameWithOwner}
        headRepository{nameWithOwner}
        closingIssuesReferences(first:100){
          totalCount
          pageInfo{hasNextPage}
          nodes{number repository{nameWithOwner}}
        }
      }
    }
  }
}`

type remotePRResponse struct {
	Data struct {
		Repository *struct {
			Issue *struct {
				Number int `json:"number"`
			} `json:"issue"`
			PullRequests *struct {
				TotalCount *int            `json:"totalCount"`
				PageInfo   *pageInfo       `json:"pageInfo"`
				Nodes      *[]remotePRNode `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	} `json:"data"`
	Errors []graphQLErrorEntry `json:"errors"`
}

type pageInfo struct {
	HasNextPage *bool `json:"hasNextPage"`
}

type remotePRNode struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	State          string `json:"state"`
	HeadRefName    string `json:"headRefName"`
	HeadRefOID     string `json:"headRefOid"`
	BaseRefName    string `json:"baseRefName"`
	BaseRepository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"baseRepository"`
	HeadRepository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"headRepository"`
	ClosingIssuesReferences *struct {
		TotalCount *int      `json:"totalCount"`
		PageInfo   *pageInfo `json:"pageInfo"`
		Nodes      *[]struct {
			Number     int `json:"number"`
			Repository *struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"repository"`
		} `json:"nodes"`
	} `json:"closingIssuesReferences"`
}

// RemotePR implements core.RemotePRSource with GitHub's GraphQL API. REST's
// pull-request list omits the closing-issue relationship, so it cannot provide
// the positive issue binding remote verification requires.
func (a *Adapter) RemotePR(ctx context.Context, q core.RemotePRQuery) (*core.RemotePR, error) {
	if err := a.admit(); err != nil {
		return nil, err
	}
	if q.Branch == "" || q.Repository == "" {
		return nil, errors.New("RemotePR requires a repository and branch")
	}
	issue, err := issueNumber(q.Issue)
	if err != nil {
		return nil, err
	}
	readyRepository, err := a.Repository(ctx)
	if err != nil {
		return nil, err
	}
	repositoryIdentity, err := gitremote.RepositoryIdentity(readyRepository.RemoteURL)
	if err != nil {
		return nil, fmt.Errorf("RemotePR cannot identify the repository established by Ready: %w", err)
	}
	configured := a.cfg.owner + "/" + a.cfg.repo
	if !repositoryIdentityNames(repositoryIdentity, configured) {
		return nil, errors.New("RemotePR repository established by Ready does not identify the configured repository")
	}
	if q.Repository != repositoryIdentity {
		return nil, errors.New("RemotePR repository is not the repository established by Ready")
	}

	var body remotePRResponse
	resp, err := a.graphql(ctx, remotePRQuery, map[string]any{
		"owner": a.cfg.owner, "repo": a.cfg.repo, "branch": q.Branch, "issue": issue,
	}, &body)
	if err != nil {
		return nil, fmt.Errorf("reading the pull request for %s: %w", q.Branch, err)
	}
	if len(body.Errors) > 0 {
		return nil, fmt.Errorf("reading the pull request for %s: %w",
			q.Branch, a.gate.observe(graphQLError(resp, body.Errors)))
	}
	if body.Data.Repository == nil {
		return nil, fmt.Errorf("%w: the configured repository was absent from the response", ErrGraphQL)
	}
	repository := body.Data.Repository
	if repository.Issue == nil || repository.Issue.Number != issue {
		return nil, fmt.Errorf("%w: #%d", core.ErrIssueNotFound, issue)
	}
	prs := repository.PullRequests
	if prs == nil || prs.TotalCount == nil || prs.PageInfo == nil ||
		prs.PageInfo.HasNextPage == nil || prs.Nodes == nil || *prs.TotalCount < 0 {
		return nil, fmt.Errorf("%w: the pull-request enumeration was absent or incomplete", core.ErrRemotePRIncomplete)
	}
	prNodes := *prs.Nodes
	if *prs.PageInfo.HasNextPage || *prs.TotalCount > 1 || len(prNodes) > 1 {
		return nil, fmt.Errorf("%w: branch %s", core.ErrRemotePRAmbiguous, q.Branch)
	}
	if *prs.TotalCount != len(prNodes) {
		return nil, fmt.Errorf("%w: the pull-request count did not match the returned candidates", core.ErrRemotePRIncomplete)
	}
	if len(prNodes) == 0 {
		return nil, nil
	}

	node := prNodes[0]
	if err := validateRemotePRNode(node, configured, q.Branch); err != nil {
		return nil, err
	}
	closingIssues := node.ClosingIssuesReferences
	if closingIssues == nil || closingIssues.TotalCount == nil || closingIssues.PageInfo == nil ||
		closingIssues.PageInfo.HasNextPage == nil || closingIssues.Nodes == nil || *closingIssues.TotalCount < 0 {
		return nil, fmt.Errorf("%w: pull request #%d has more closing issues than one response contained",
			core.ErrRemotePRIncomplete, node.Number)
	}
	closingNodes := *closingIssues.Nodes
	if *closingIssues.PageInfo.HasNextPage || *closingIssues.TotalCount != len(closingNodes) {
		return nil, fmt.Errorf("%w: pull request #%d has more closing issues than one response contained",
			core.ErrRemotePRIncomplete, node.Number)
	}

	linked := make([]string, 0, len(closingNodes))
	seen := make(map[int]bool, len(closingNodes))
	for _, closing := range closingNodes {
		if closing.Number <= 0 || closing.Repository == nil || closing.Repository.NameWithOwner == "" {
			return nil, fmt.Errorf("%w: pull request #%d returned an incomplete closing-issue reference",
				core.ErrRemotePRIncomplete, node.Number)
		}
		if !strings.EqualFold(closing.Repository.NameWithOwner, configured) || seen[closing.Number] {
			continue
		}
		seen[closing.Number] = true
		linked = append(linked, strconv.Itoa(closing.Number))
	}

	return &core.RemotePR{
		Number:         node.Number,
		URL:            node.URL,
		State:          "open",
		Repository:     repositoryIdentity,
		HeadRepository: repositoryIdentityFor(repositoryIdentity, configured, node.HeadRepository.NameWithOwner),
		HeadBranch:     node.HeadRefName,
		HeadSHA:        node.HeadRefOID,
		BaseBranch:     node.BaseRefName,
		LinkStatus:     core.RemoteIssueLinkStated,
		LinkedIssues:   linked,
	}, nil
}

func validateRemotePRNode(node remotePRNode, configured, branch string) error {
	u, urlErr := url.Parse(node.URL)
	switch {
	case node.Number <= 0,
		invalidRemotePRURL(u, urlErr),
		!strings.EqualFold(node.State, "OPEN"),
		node.HeadRefName != branch,
		node.HeadRefOID == "",
		node.BaseRefName == "",
		node.BaseRepository == nil,
		node.HeadRepository == nil:
		return fmt.Errorf("%w: the response contained incomplete or contradictory pull-request facts", ErrGraphQL)
	case !strings.EqualFold(node.BaseRepository.NameWithOwner, configured):
		return fmt.Errorf("%w: the response named a different base repository", ErrGraphQL)
	case node.HeadRepository.NameWithOwner == "":
		return fmt.Errorf("%w: the response omitted the head repository", ErrGraphQL)
	}
	return nil
}

func invalidRemotePRURL(u *url.URL, err error) bool {
	return err != nil ||
		!(strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) ||
		u.Host == "" || u.User != nil
}

// repositoryIdentityNames checks only the path half of an identity already
// derived from Ready's clone URL. Host authority is bound separately by exact
// equality with the caller's independently derived identity.
func repositoryIdentityNames(identity, nameWithOwner string) bool {
	want := strings.ToLower(nameWithOwner)
	got := strings.ToLower(identity)
	return got == want || strings.HasSuffix(got, "/"+want) || strings.HasSuffix(got, ":"+want)
}

// repositoryIdentityFor keeps the canonical identity's host (and port) while
// replacing its nameWithOwner suffix. A fork must compare unequal to the base;
// returning only the fork's name is the safe fallback for an unfamiliar shape.
func repositoryIdentityFor(base, configured, actual string) string {
	if strings.EqualFold(actual, configured) {
		return base
	}
	lowerBase := strings.ToLower(base)
	lowerConfigured := strings.ToLower(configured)
	if strings.HasSuffix(lowerBase, lowerConfigured) {
		start := len(base) - len(configured)
		if start == 0 || base[start-1] == '/' || base[start-1] == ':' {
			return base[:start] + actual
		}
	}
	return actual
}
