package review

import (
	"strings"
	"testing"
)

func TestParsePRURL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		owner  string
		repo   string
		number int
		bad    bool
	}{
		{name: "canonical", raw: "https://github.com/acme/ben/pull/42", owner: "acme", repo: "ben", number: 42},
		{name: "trailing space is trimmed", raw: " https://github.com/acme/ben/pull/42 ", owner: "acme", repo: "ben", number: 42},
		{name: "GitHub Enterprise host", raw: "https://github.corp.example/acme/ben/pull/42", owner: "acme", repo: "ben", number: 42},
		{name: "an api url is not a web url", raw: "https://api.github.com/repos/acme/ben/pulls/42", bad: true},
		{name: "an anchor is not the pull request", raw: "https://github.com/acme/ben/pull/42#issuecomment-1", bad: true},
		{name: "the issue path is not the pull path", raw: "https://github.com/acme/ben/issues/42", bad: true},
		{name: "plain http", raw: "http://github.com/acme/ben/pull/42", bad: true},
		{name: "no number", raw: "https://github.com/acme/ben/pull/", bad: true},
		{name: "empty", raw: "", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, number, err := ParsePRURL(tc.raw)
			if tc.bad {
				if err == nil {
					t.Fatalf("ParsePRURL(%q) = %s/%s#%d, want a refusal", tc.raw, owner, repo, number)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePRURL(%q): %v", tc.raw, err)
			}
			if owner != tc.owner || repo != tc.repo || number != tc.number {
				t.Errorf("= %s/%s#%d, want %s/%s#%d", owner, repo, number, tc.owner, tc.repo, tc.number)
			}
		})
	}
}

func TestClosingIssues(t *testing.T) {
	cfg := fxConfig()
	for _, tc := range []struct {
		name    string
		body    string
		want    []int
		foreign bool
	}{
		{name: "the template's own form", body: "Fixes #11", want: []int{11}},
		{name: "case insensitive", body: "FIXES #11", want: []int{11}},
		{name: "other keywords", body: "Closes #11", want: []int{11}},
		{name: "resolved", body: "resolved #11", want: []int{11}},
		{name: "qualified, same repo", body: "Fixes acme/ben#11", want: []int{11}},
		{name: "repeated is one", body: "Fixes #11 and fixes #11", want: []int{11}},
		{name: "two issues", body: "Fixes #11\nCloses #12", want: []int{11, 12}},
		{name: "another repository does not count", body: "Fixes other/repo#11", foreign: true},
		{name: "a bare reference is not a closing keyword", body: "See #11"},
		{name: "a keyword inside another word is not a keyword", body: "This prefixes #11 nicely"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, foreign := closingIssues(cfg, tc.body)
			if foreign != tc.foreign {
				t.Errorf("foreign = %v, want %v", foreign, tc.foreign)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("issues = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("issues = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The link is read off one structured field line and never off the headline,
// so rewording SPEC §8.4's prose cannot silently stop the controller — and
// cannot silently start it either. internal/tracker/github pins the producing
// side of this same coupling.
func TestPublishedPRLink(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "the rendered milestone",
			body: "**BEN published a pull request.**\n\n- pull request: https://github.com/acme/ben/pull/42\n- daemon: `d`\n",
			want: "https://github.com/acme/ben/pull/42",
		},
		{
			name: "a headline alone carries no link",
			body: "**BEN published a pull request.**\n\n- daemon: `d`\n",
		},
		{
			name: "a url loose in prose is not the field",
			body: "see https://github.com/acme/ben/pull/99 for details\n",
		},
		{
			name: "an empty field is no field",
			body: "- pull request: \n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := publishedPRLink(tc.body)
			if (tc.want == "") == ok {
				t.Fatalf("publishedPRLink ok = %v, want %v", ok, tc.want != "")
			}
			if got != tc.want {
				t.Errorf("link = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLatestPublishedIgnoresEverythingElse(t *testing.T) {
	cfg := fxConfig()
	comments := []Comment{
		{ID: 1, Author: "a-human", Body: milestone(0, occ3, fxPRURL, at(0)).Body, CreatedAt: at(0)},
		milestone(2, occ1, fxPRURL, at(10)),
		{ID: 3, Author: fxTracker, Body: "<!-- ben:milestone kind=claimed occurrence=9500 -->", CreatedAt: at(11)},
		milestone(4, occ2, fxPRURL, at(20)),
	}

	got, ok := LatestPublished(cfg, comments)
	if !ok {
		t.Fatal("LatestPublished found nothing")
	}
	if got.Occurrence != occ2 {
		t.Errorf("occurrence = %d, want %d — the newest published milestone by the tracker identity", got.Occurrence, occ2)
	}
	if got.PRURL != fxPRURL {
		t.Errorf("url = %q, want %q", got.PRURL, fxPRURL)
	}
}

func TestValidateSubject(t *testing.T) {
	cfg := fxConfig()
	trigger := Trigger{Occurrence: occ1, PRURL: fxPRURL, At: at(10)}

	for _, tc := range []struct {
		name string
		pr   func() *PullRequest
		want string // substring of the refusal; empty means it must pass
	}{
		{name: "the ordinary case", pr: func() *PullRequest { return fxPR(head1) }},
		{name: "no pull request", pr: func() *PullRequest { return nil }, want: "could not be read"},
		{
			name: "a head that is not a sha",
			pr:   func() *PullRequest { p := fxPR(head1); p.Head = "HEAD"; return p },
			want: "not a commit sha",
		},
		{
			name: "two closing keywords",
			pr:   func() *PullRequest { p := fxPR(head1); p.Body = "Fixes #11\nCloses #12"; return p },
			want: "closes 2 issues",
		},
		{
			name: "a local and foreign closing keyword",
			pr:   func() *PullRequest { p := fxPR(head1); p.Body = "Fixes #11\nCloses other/repo#12"; return p },
			want: "also closes an issue in another repository",
		},
		{
			name: "the API URL differs from the milestone",
			pr: func() *PullRequest {
				p := fxPR(head1)
				p.URL = "https://github.corp.example/acme/ben/pull/42"
				return p
			},
			want: "does not exactly match",
		},
		{
			name: "a base that is not a sha",
			pr:   func() *PullRequest { p := fxPR(head1); p.BaseSHA = "main"; return p },
			want: "base \"main\", which is not a commit sha",
		},
		{
			name: "a branch that does not resolve",
			pr:   func() *PullRequest { p := fxPR(head1); p.Branch = "ben/nope"; return p },
			want: "not the canonical",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSubject(cfg, trigger, tc.pr())
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateSubject: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateSubjectAcceptsTheConfiguredGitHubEnterpriseHost(t *testing.T) {
	const enterpriseURL = "https://github.corp.example/acme/ben/pull/42"
	pr := fxPR(head1)
	pr.URL = enterpriseURL
	trigger := Trigger{Occurrence: occ1, PRURL: enterpriseURL, At: at(10)}
	if err := validateSubject(fxConfig(), trigger, pr); err != nil {
		t.Fatalf("validateSubject: %v", err)
	}
}

func TestValidateSubjectDoesNotFollowASameRepoURLOnAnotherHost(t *testing.T) {
	trigger := Trigger{Occurrence: occ1, PRURL: "https://evil.example/acme/ben/pull/42", At: at(10)}
	err := validateSubject(fxConfig(), trigger, fxPR(head1))
	if err == nil || !strings.Contains(err.Error(), "does not exactly match") {
		t.Fatalf("error = %v, want the foreign host refused against the forge's canonical URL", err)
	}
}
