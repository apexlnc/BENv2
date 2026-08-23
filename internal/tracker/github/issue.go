package github

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v90/github"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// statePrefix is the reserved label namespace BEN projects its state into
// (SPEC §9.3). Nothing else may write these labels.
const statePrefix = "ben:"

// stateLabelName renders a §9.3 projection. core.StateLabelNone has no name:
// "no state label" is the projection.
func stateLabelName(l core.StateLabel) (string, error) {
	switch l {
	case core.StateLabelNone:
		return "", nil
	case core.StateLabelClaimed, core.StateLabelRunning, core.StateLabelNeedsReview, core.StateLabelFailed:
		return statePrefix + string(l), nil
	default:
		return "", ErrUnknownStateLabel
	}
}

func isStateLabel(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), statePrefix)
}

// normalize maps a GitHub issue onto the tracker-agnostic model (SPEC §8.3).
// Blockers are filled in separately: they cost a second request, so the
// adapter only pays for them where the answer can change a verdict.
func normalize(iss *gh.Issue) core.Issue {
	out := core.Issue{
		Identifier: strconv.Itoa(iss.GetNumber()),
		Title:      iss.GetTitle(),
		Body:       iss.GetBody(),
		State:      iss.GetState(),
		URL:        iss.GetHTMLURL(),
		CreatedAt:  iss.GetCreatedAt().Time,
		UpdatedAt:  iss.GetUpdatedAt().Time,
		Revision:   projectRevision(iss).token(),
	}
	for _, l := range iss.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	for _, a := range iss.Assignees {
		out.Assignees = append(out.Assignees, a.GetLogin())
	}
	return out
}

// revisionProjection is the whole of SPEC §8.3's **revision projection**,
// reified so that the exclusion is structural rather than a matter of
// discipline. token cannot reach a field that is not here, so the projection
// grows only by changing this type — a visible edit the compiler forces through
// projectRevision, not a quiet extra term in a hash. A negative test per
// unwanted field could never be exhaustive over a provider payload; a closed
// type is (see TestRevisionProjectionTypeIsClosed).
//
// Each element earns its place. State catches a close that stands. StateReason
// catches a close a reopen has undone: GitHub sets it to "reopened", so the pair
// moves the token even though the timestamp cannot — timestamps are
// second-granularity (§8.4), so a close and a reopen inside one second leave it
// unmoved, and a token keyed on it alone would never send the sweep to the change
// log, losing exactly the reopen the two ClaimEvent state kinds were added for.
// UpdatedAt catches a *repeated* reopen, narrowing the blind spot from any second
// cycle to one landing inside a single second.
//
// Nothing else belongs, and "one more field" is not a free choice: a title, a
// body, a label, or a comment count cannot mean the issue went terminal, so each
// would only buy a change-log read per edit — in the one place per-issue reads
// were ruled out (§9.8). The sweep's other rules never consult this token; they
// read state, labels, and presence from the same response, which is what makes
// the exclusion safe rather than merely cheap.
type revisionProjection struct {
	State       string
	StateReason string
	UpdatedAt   time.Time
}

// projectRevision is the only function that sees the provider payload and the
// projection at once. Keeping that seam in one place is what leaves token with
// nothing to widen into.
func projectRevision(iss *gh.Issue) revisionProjection {
	return revisionProjection{
		State:       iss.GetState(),
		StateReason: iss.GetStateReason(),
		UpdatedAt:   iss.GetUpdatedAt().Time,
	}
}

// token renders the projection as SPEC §8.3's opaque change token. FNV-1a: this
// detects change, it defends nothing. The timestamp is hashed at whatever
// precision the tracker gave it, so the §8.3 residual is bounded by the API's
// granularity rather than by a truncation of ours.
func (p revisionProjection) token() string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%s\x00%s", p.State, p.StateReason, p.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return strconv.FormatUint(h.Sum64(), 16)
}

func normalizeBlockers(blocking []*gh.Issue) []core.Blocker {
	out := make([]core.Blocker, 0, len(blocking))
	for _, b := range blocking {
		state := b.GetState()
		out = append(out, core.Blocker{
			Identifier: strconv.Itoa(b.GetNumber()),
			State:      state,
			Open:       strings.EqualFold(state, "open"),
		})
	}
	return out
}

// dispatchable is the §8.3 eligibility verdict, computed from the normalized
// issue alone so it can be read and tested as one rule.
func dispatchable(issue core.Issue, requiredLabels, activeStates []string) bool {
	return eligibleIgnoringBlockers(issue, requiredLabels, activeStates) && !hasOpenBlocker(issue)
}

// eligibleIgnoringBlockers is everything §8.3 can decide from the list
// response, before spending a request on dependencies.
func eligibleIgnoringBlockers(issue core.Issue, requiredLabels, activeStates []string) bool {
	for _, want := range requiredLabels {
		if !containsFold(issue.Labels, want) {
			return false
		}
	}
	if !containsFold(activeStates, issue.State) {
		return false
	}
	// Any assignee blocks dispatch. Assigned-to-other is a human calling dibs
	// (SPEC §8.4); assigned-to-self is our own retained claim on a published
	// issue awaiting review (SPEC §9.10 step 3). Neither is ours to start.
	if len(issue.Assignees) > 0 {
		return false
	}
	// A state label means some daemon, present or past, already holds a
	// verdict on this issue (SPEC §8.3, BUILD.md decision 8).
	for _, l := range issue.Labels {
		if isStateLabel(l) {
			return false
		}
	}
	return true
}

func hasOpenBlocker(issue core.Issue) bool {
	for _, b := range issue.Blockers {
		if b.Open {
			return true
		}
	}
	return false
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}
