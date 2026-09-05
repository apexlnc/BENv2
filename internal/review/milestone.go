package review

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Trigger is one delivery: BEN said, in a comment the controller can attribute
// and date, that it published a pull request for this issue.
type Trigger struct {
	Occurrence int64     // the state-label transition id from the milestone marker
	PRURL      string    // the link the milestone carries, exactly as written
	At         time.Time // when the milestone was posted
	CommentID  int64
}

// LatestPublished picks the newest published milestone authored by the tracker
// identity. It is the subject the observer must fetch. The reducer separately
// walks older publications to repair a route whose mutation landed before a
// newer occurrence overtook it; an unfinished older occurrence is never
// reviewed again.
func LatestPublished(cfg Config, comments []Comment) (Trigger, bool) {
	all := publishedMilestones(cfg, comments)
	if len(all) == 0 {
		return Trigger{}, false
	}
	return all[len(all)-1], true
}

// publishedMilestones returns one trusted trigger per occurrence in occurrence
// order. An occurrence is the state-transition id and therefore the delivery
// order; comment pagination order is not an authority.
func publishedMilestones(cfg Config, comments []Comment) []Trigger {
	seen := map[int64]bool{}
	var out []Trigger
	for _, c := range comments {
		if !eqFold(c.Author, cfg.TrackerAuthor) {
			continue
		}
		occ, err := PublishedOccurrence(c.Body)
		if err != nil {
			continue
		}
		url, ok := publishedPRLink(c.Body)
		if !ok {
			// A published milestone with no link asserts evidence it does not
			// carry; the renderer refuses to produce one (SPEC §8.4), so this
			// is a body that has been edited or forged. Skipping it rather
			// than failing the whole reduction keeps one bad comment from
			// wedging an issue.
			continue
		}
		if seen[occ] {
			continue
		}
		seen[occ] = true
		out = append(out, Trigger{Occurrence: occ, PRURL: url, At: c.CreatedAt, CommentID: c.ID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Occurrence < out[j].Occurrence })
	return out
}

// prLinkField is the one structured line the controller reads out of a
// milestone body, written by field() in internal/tracker/github/comment.go.
//
// The marker says *that* a pull request was published and which occurrence
// this is; it does not carry the link, so the link has to come from the body.
// This is the narrowest thing that works: a fixed field prefix at the start of
// a line, never the headline and never a URL found loose in the prose. The
// tracker adapter pins this exact rendering in a test citing #11, so the two
// halves of the coupling each have an anchor.
const prLinkField = "- pull request: "

func publishedPRLink(body string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, prLinkField); ok {
			rest = strings.TrimSpace(rest)
			if rest != "" {
				return rest, true
			}
		}
	}
	return "", false
}

var prPathNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ParsePRURL splits the canonical shape of a pull request web URL. The host is
// deliberately not hard-coded to github.com: GitHub Enterprise Server uses
// the same path on the deployment's own host. validateSubject separately
// requires the whole URL to equal the canonical html_url returned by the
// configured forge, so accepting the shape here does not trust another host.
func ParsePRURL(raw string) (owner, repo string, number int, err error) {
	u, parseErr := url.Parse(strings.TrimSpace(raw))
	if parseErr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return "", "", 0, fmt.Errorf("%q is not a pull request URL", raw)
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 5 || parts[0] != "" || parts[3] != "pull" ||
		!prPathNameRe.MatchString(parts[1]) || !prPathNameRe.MatchString(parts[2]) {
		return "", "", 0, fmt.Errorf("%q is not a pull request URL", raw)
	}
	number, err = strconv.Atoi(parts[4])
	if err != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("%q carries no pull request number", raw)
	}
	return parts[1], parts[2], number, nil
}

// closingKeywordRe matches GitHub's closing keywords over a same-repository
// issue reference, in the two spellings BEN's prompt template and a human
// might use.
var closingKeywordRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+(?:([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+))?#([0-9]+)\b`)

// closingIssues returns every distinct issue a body promises to close in the
// given repository, plus whether any reference named a different repository.
func closingIssues(cfg Config, body string) (issues []int, foreign bool) {
	seen := map[int]bool{}
	for _, m := range closingKeywordRe.FindAllStringSubmatch(body, -1) {
		if m[1] != "" && !(eqFold(m[1], cfg.Owner) && eqFold(m[2], cfg.Repo)) {
			foreign = true
			continue
		}
		n, err := strconv.Atoi(m[3])
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		issues = append(issues, n)
	}
	return issues, foreign
}

// branchIssue reads the issue number back out of a canonical `ben/<n>` branch.
func branchIssue(branch string) (int, bool) {
	rest, ok := strings.CutPrefix(branch, "ben/")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// validateSubject decides whether the pull request a milestone points at is
// really this issue's work, and is the single gate every routing decision sits
// behind.
//
// Identity is resolved twice from sources BEN writes at different moments and
// with different credentials — the branch the workspace created (SPEC §6.2)
// and the closing keyword the agent typed into the PR body — and both must
// equal the issue that carries the marker. One source alone is forgeable by
// whoever controls the other: an agent can write any closing keyword, and a
// pull request can be opened from any branch. Requiring agreement means an
// attacker needs the workspace *and* the publish credential, which is the same
// pair SPEC §6.7 already treats as the trust boundary.
func validateSubject(cfg Config, t Trigger, pr *PullRequest) error {
	if pr == nil {
		return fmt.Errorf("the milestone's pull request %s could not be read", t.PRURL)
	}
	owner, repo, number, err := ParsePRURL(t.PRURL)
	if err != nil {
		return fmt.Errorf("the milestone's link is not usable: %w", err)
	}
	if !eqFold(owner, cfg.Owner) || !eqFold(repo, cfg.Repo) {
		return fmt.Errorf("the milestone links %s/%s, which is not %s/%s", owner, repo, cfg.Owner, cfg.Repo)
	}
	if number != pr.Number {
		return fmt.Errorf("the milestone links pull request #%d but #%d was read", number, pr.Number)
	}
	apiOwner, apiRepo, apiNumber, err := ParsePRURL(pr.URL)
	if err != nil {
		return fmt.Errorf("pull request #%d reports no canonical web URL: %w", pr.Number, err)
	}
	if !eqFold(apiOwner, cfg.Owner) || !eqFold(apiRepo, cfg.Repo) || apiNumber != pr.Number {
		return fmt.Errorf("pull request #%d reports URL %q outside %s/%s", pr.Number, pr.URL, cfg.Owner, cfg.Repo)
	}
	if strings.TrimSpace(t.PRURL) != strings.TrimSpace(pr.URL) {
		return fmt.Errorf("the milestone's link %q does not exactly match the forge's canonical %q", t.PRURL, pr.URL)
	}
	if pr.Closed || pr.Merged {
		return fmt.Errorf("pull request #%d is not open", pr.Number)
	}
	if pr.Branch != cfg.Branch() {
		return fmt.Errorf("pull request #%d is from %q, not the canonical %q", pr.Number, pr.Branch, cfg.Branch())
	}
	if n, ok := branchIssue(pr.Branch); !ok || n != cfg.Issue {
		return fmt.Errorf("branch %q does not resolve to issue #%d", pr.Branch, cfg.Issue)
	}
	if !isFullSHA(pr.Head) {
		return fmt.Errorf("pull request #%d reports head %q, which is not a commit sha", pr.Number, pr.Head)
	}
	if strings.TrimSpace(pr.Base) == "" {
		return fmt.Errorf("pull request #%d reports no base ref", pr.Number)
	}
	if !isFullSHA(pr.BaseSHA) {
		return fmt.Errorf("pull request #%d reports base %q, which is not a commit sha", pr.Number, pr.BaseSHA)
	}
	issues, foreign := closingIssues(cfg, pr.Body)
	switch {
	case foreign && len(issues) > 0:
		return fmt.Errorf("pull request #%d also closes an issue in another repository; one branch may close only #%d", pr.Number, cfg.Issue)
	case foreign:
		return fmt.Errorf("pull request #%d closes an issue in another repository, not #%d", pr.Number, cfg.Issue)
	case len(issues) == 0:
		return fmt.Errorf("pull request #%d names no issue to close, so it cannot be confirmed as #%d's work", pr.Number, cfg.Issue)
	case len(issues) > 1:
		return fmt.Errorf("pull request #%d closes %d issues; one branch, one issue, one pull request", pr.Number, len(issues))
	case issues[0] != cfg.Issue:
		return fmt.Errorf("pull request #%d closes issue #%d, not #%d", pr.Number, issues[0], cfg.Issue)
	}
	return nil
}
