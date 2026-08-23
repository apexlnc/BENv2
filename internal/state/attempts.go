package state

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// attempts.jsonl — one record per attempt, appended when the attempt ends (#60).
// The durability machinery is jsonl.go's, the same as the §9.11 log's.
//
// **Why a second log rather than more fields on the first.** A §9.11 entry is an
// *edge*, and an attempt is not: an attempt's cost, its duration and its verdict
// are known only once it is over, and the edge that ends it — to `backoff`, to
// `done`, to `needs-review` — carries none of them. Spreading an attempt across
// the six edges it produces would make every question in this file a join, and
// would put a numeric field on a tuple SPEC §9.11 fixes.
//
// **What it is not.** Not authority, and not evidence for any decision: nothing
// reads it back into the loop. §9.10 keeps the tracker and git as the source of
// truth, and a daemon started against an empty state dir behaves identically to
// one started against a full one. This is the readout the §9.6 retry knobs and
// the §5.2.7 limits are tuned from, and the record internal/bench's adapter
// comparison groups by (#62) — which is why Agent and Model are on it rather
// than being reconstructible from a WORKFLOW.md that has since been edited.
// That comparison takes its attempt facts only from this file; its declared
// matrix, dispatch join and post-publish check evidence live in the benchmark
// session manifest. The reader is a command the daemon does not link
// (cmd/benchreport).

// attemptNoun names this log in the errors jsonl.go raises about it.
const attemptNoun = "attempt outcome"

// Attempt is one line of the log: everything about one dispatch that is known
// when it ends and cannot be recovered afterwards.
type Attempt struct {
	Issue string `json:"issue"`
	// Attempt is the §9.6 attempt number, 1-based. It is not a count of records
	// for this issue in this file: a fresh claim whose branch already carries
	// work starts at 2 (§9.6's attempt floor), and a human re-queue restores the
	// budget without resetting the counter.
	Attempt int `json:"attempt"`
	// Turns is continuation sessions consumed at the point the attempt ended.
	Turns int `json:"turns"`
	// RunID is the §10.3 correlation handle: the same value on this attempt's
	// log lines, its §9.11 edges and its transcript. It is what joins this
	// record to all three.
	RunID string `json:"run_id,omitempty"`

	// Agent and Model name what ran (#62). Recorded per attempt rather than
	// looked up, because the workflow that dispatched it can be edited between
	// the attempt and the question — §5.4 makes that an ordinary event, not an
	// unusual one.
	//
	// **No `omitempty` on either, deliberately.** An empty Model is a *statement*:
	// the block named no model, so the harness's own default applied and BEN
	// cannot know its name (core.AgentDescriptor). Omitting the key would make
	// that statement indistinguishable from a record written before the field
	// existed, and would drop the ordinary default-model configuration out of the
	// one comparison these two fields are here for. It is present and empty, and
	// every renderer says what empty means.
	Agent string `json:"agent"`
	Model string `json:"model"`

	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	// Ran reports that a process was actually started for this attempt.
	//
	// It separates the numbers a *run* produces from the numbers a *dispatch*
	// produces, and only the first kind may be conditioned on it. Usage is the
	// first kind: an attempt with no process reported none, so counting it among
	// the ones that quoted no price would invent an adapter limitation.
	//
	// Duration is emphatically **not**. StartedAt is the entry to `preparing`, so
	// an attempt that died in Prepare has a real dispatch-to-outcome span — and
	// the slow ones are the interesting ones, a clone that timed out or an
	// after_create hook that hung. Excluding them was this field's first use and
	// it was wrong: it deleted exactly the worktree and hook failures p95 exists
	// to surface.
	Ran bool `json:"ran"`

	// FailureReason is the §7.3 verdict this attempt ended on. Empty means the
	// attempt did not fail — the agent claimed done and Verdict says what the
	// evidence made of that (SPEC §7.4, §9.7).
	FailureReason core.FailureReason `json:"failure_reason,omitempty"`
	// Verdict is the §9.7 publish-evidence verdict, as the orchestrator spells
	// it: published, incomplete, contradicted, or unknown. Empty when
	// verification never ran, which is every failed attempt.
	//
	// A string for the reason State is one in runs.json: the enum belongs to the
	// authority loop, and a file format that imports the loop makes the loop
	// unable to ever import the file format.
	Verdict string `json:"verdict,omitempty"`

	// The §7.2 usage this attempt reported, accumulated over its usage events
	// and reset per attempt — unlike the §9.9 cost cap, which is cumulative per
	// issue.
	//
	// Zero cost with non-zero tokens is the honest shape of an adapter that
	// reports no price (core.Usage: "0 when the adapter cannot report cost", and
	// codex-exec is one). Summary counts those attempts rather than summing a
	// price nobody quoted.
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// Duration is the attempt's wall clock: dispatch to outcome, so it includes the
// worktree preparation and the §9.7 evidence check either side of the run. That
// is the span a queue's throughput is made of, and the one an operator waits
// through.
func (a Attempt) Duration() time.Duration {
	if a.StartedAt.IsZero() || a.EndedAt.Before(a.StartedAt) {
		return 0
	}
	return a.EndedAt.Sub(a.StartedAt)
}

// Published reports the one verdict that means the attempt produced reviewable
// work (SPEC §9.7).
func (a Attempt) Published() bool { return a.Verdict == VerdictPublished }

// VerdictPublished is orchestrator.VerdictPublished's spelling, stated here
// because this package must not import the loop (see failedState). The writing
// side pins the two together in a test, where a rename would otherwise pass
// silently.
const VerdictPublished = "published"

// AttemptWriter appends to the log. One per daemon; see appendLog.
type AttemptWriter struct{ *appendLog }

// AppendAttempts opens the log for appending, creating it if needed, after
// repairing an incomplete trailing record (see openAppendLog).
func (d Dir) AppendAttempts() (*AttemptWriter, error) {
	if err := d.Prepare(); err != nil {
		return nil, err
	}
	log, err := openAppendLog(d.AttemptsPath(), attemptNoun)
	if err != nil {
		return nil, err
	}
	return &AttemptWriter{appendLog: log}, nil
}

// Append writes one record durably, and either the record is in the file
// afterwards or nothing of it is (see appendLog.appendRecord).
func (w *AttemptWriter) Append(a Attempt) error {
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("state: encoding an attempt outcome: %w", err)
	}
	return w.appendRecord(body, a.Issue)
}

// AttemptReader reads the log, on TransitionReader's terms: no handle held
// between calls.
type AttemptReader struct{ path string }

// ReadAttempts names the log for reading.
func (d Dir) ReadAttempts() AttemptReader { return AttemptReader{path: d.AttemptsPath()} }

// Tail returns the last n records, oldest first, and how many the log holds.
// n <= 0 returns everything. See TransitionReader.Tail for why both come back
// from one pass.
func (r AttemptReader) Tail(n int) ([]Attempt, int, error) {
	var (
		ring  []Attempt
		total int
	)
	err := walkLog(r.path, attemptNoun, func(a Attempt) {
		total++
		ring = append(ring, a)
		if n > 0 && len(ring) > n {
			ring = ring[1:]
		}
	})
	if err != nil {
		return nil, 0, err
	}
	return ring, total, nil
}

// Summary is the aggregate `ben status` renders (#60): the readout the §9.6
// retry policy and the §5.2.7 limits are knobs for.
//
// Over the whole log, not a window. The log is append-only and v1 never rotates
// it, so this is every attempt the daemon has ever recorded here — which is what
// makes "what fraction land on attempt 1" a real fraction rather than a fraction
// of the last screenful.
type Summary struct {
	// Attempts is every record; Ran is the subset that started a process.
	Attempts int `json:"attempts"`
	Ran      int `json:"ran"`
	// Issues is how many distinct issues those attempts belong to.
	Issues int `json:"issues"`
	// First and Last bound the window the numbers below describe.
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`

	// PublishedIssues is issues with at least one `published` attempt, and
	// FirstAttemptPublished the subset that published on attempt 1.
	//
	// Counted per *issue* rather than per attempt because the question is about
	// tickets: an issue that published on attempt 3 also has two failures in the
	// log, and an attempt-level ratio would report that as 33% rather than as one
	// ticket that took three goes.
	PublishedIssues       int     `json:"published_issues"`
	FirstAttemptPublished int     `json:"first_attempt_published"`
	AttemptsToPublish     int     `json:"attempts_to_publish"`
	CostOfPublishedIssues float64 `json:"cost_of_published_issues"`
	// UnpricedAttempts is how many attempts that ran reported tokens but no
	// cost. Stated rather than folded in: the sums above understate by exactly
	// these attempts' unquoted price, and a total that hid them would read as
	// though those runs were free.
	UnpricedAttempts int `json:"unpriced_attempts"`

	// P50 and P95 are attempt wall-clock by nearest rank, over **every** attempt
	// rather than only those that started a process.
	//
	// Dispatch to outcome is the span an operator waits through, and a Prepare
	// that hung for six minutes before failing cost exactly that. Conditioning
	// these on Ran hid the slow worktree and hook failures — the ones a p95 is
	// read to find (see Attempt.Ran).
	P50 time.Duration `json:"p50"`
	P95 time.Duration `json:"p95"`

	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`

	// Failures counts the §7.3 reasons, commonest first. Verdicts does the same
	// for the §9.7 verdicts of the attempts that reached one.
	Failures []Count `json:"failures"`
	Verdicts []Count `json:"verdicts"`
	// Agents breaks the same numbers down by what ran, which is the whole
	// purpose of carrying Agent and Model on the record (#62).
	Agents []AgentCount `json:"agents"`
}

// Count is one name and how often it occurred.
type Count struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// AgentCount is one (kind, model) pair's share of the log. Model is empty for
// the pair whose workflow named none — a row of its own, not an absence, on
// Attempt.Model's terms.
type AgentCount struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
	// Attempts is every attempt this pair ran; Published is how many of them
	// reached the §9.7 published verdict.
	Attempts  int           `json:"attempts"`
	Published int           `json:"published"`
	CostUSD   float64       `json:"cost_usd"`
	P50       time.Duration `json:"p50"`
}

// Summarize reads the whole log and aggregates it in one streaming pass.
//
// Memory is O(issues) plus one int64 per attempt that ran, not O(file): the
// per-issue tallies are what "cost of a completed ticket" needs, and the
// durations are what a percentile needs. Both are far smaller than the records
// they are drawn from, which is the point — `ben status` is a command an
// operator reaches for when a host is already unwell.
//
// An absent log is ErrNoState, as everywhere else here. The caller decides what
// that means; for `ben status` it is "no attempt has ended on this host yet",
// which is an answer.
func (r AttemptReader) Summarize() (Summary, error) {
	var (
		s         Summary
		durations []time.Duration
		failures  = map[string]int{}
		verdicts  = map[string]int{}
		agents    = map[core.AgentDescriptor]*agentTally{}
		issues    = map[string]*issueTally{}
		order     []string
	)
	err := walkLog(r.path, attemptNoun, func(a Attempt) {
		s.Attempts++
		if s.First.IsZero() || a.EndedAt.Before(s.First) {
			s.First = a.EndedAt
		}
		if a.EndedAt.After(s.Last) {
			s.Last = a.EndedAt
		}
		s.InputTokens += a.InputTokens
		s.OutputTokens += a.OutputTokens
		s.CostUSD += a.CostUSD

		if a.FailureReason != "" {
			failures[string(a.FailureReason)]++
		}
		if a.Verdict != "" {
			verdicts[a.Verdict]++
		}

		who := core.AgentDescriptor{Kind: a.Agent, Model: a.Model}
		tally, ok := agents[who]
		if !ok {
			tally = &agentTally{}
			agents[who] = tally
		}
		tally.attempts++
		tally.cost += a.CostUSD
		if a.Published() {
			tally.published++
		}

		issue, ok := issues[a.Issue]
		if !ok {
			issue = &issueTally{}
			issues[a.Issue] = issue
			order = append(order, a.Issue)
		}
		issue.attempts++
		issue.cost += a.CostUSD
		if a.Published() && !issue.published {
			issue.published = true
			issue.publishedAt = a.Attempt
		}

		// Every attempt's span, whether or not a process started: see
		// Summary.P50.
		durations = append(durations, a.Duration())
		tally.durations = append(tally.durations, a.Duration())

		if !a.Ran {
			return
		}
		s.Ran++
		if a.CostUSD == 0 {
			// Only of the attempts that ran. One that never launched reported no
			// usage at all, and counting it here would read as an adapter that
			// quotes no price.
			s.UnpricedAttempts++
		}
	})
	if err != nil {
		return Summary{}, err
	}

	s.Issues = len(issues)
	for _, id := range order {
		t := issues[id]
		if !t.published {
			continue
		}
		s.PublishedIssues++
		s.AttemptsToPublish += t.attempts
		s.CostOfPublishedIssues += t.cost
		if t.publishedAt <= 1 {
			s.FirstAttemptPublished++
		}
	}
	s.P50, s.P95 = Percentile(durations, 50), Percentile(durations, 95)
	s.Failures, s.Verdicts = Counts(failures), Counts(verdicts)
	s.Agents = agentCounts(agents)
	return s, nil
}

// issueTally is one issue's running total across the log.
type issueTally struct {
	attempts  int
	cost      float64
	published bool
	// publishedAt is the attempt number the first `published` verdict landed on.
	publishedAt int
}

type agentTally struct {
	attempts  int
	published int
	cost      float64
	durations []time.Duration
}

// Percentile is the nearest-rank value at p, over an unsorted slice it sorts in
// place. Nearest rank rather than interpolation: the answer is then always a
// duration some attempt actually took, which is what an operator reading "p95
// 21m" assumes it is.
//
// Exported for #62's adapter comparison, which reads this same log. One
// definition, deliberately: two readouts of one file whose percentiles are
// computed differently disagree about the same attempts, and the disagreement
// looks like a difference between adapters.
func Percentile(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	rank := (p*len(d) + 99) / 100 // ceil(p/100 * n)
	if rank < 1 {
		rank = 1
	}
	if rank > len(d) {
		rank = len(d)
	}
	return d[rank-1]
}

// Counts orders a histogram commonest first, ties by name, so a rendering is
// stable across runs over an unchanged log. Exported alongside Percentile, and
// for the same reason.
func Counts(m map[string]int) []Count {
	if len(m) == 0 {
		return nil
	}
	out := make([]Count, 0, len(m))
	for name, n := range m {
		out = append(out, Count{Name: name, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func agentCounts(m map[core.AgentDescriptor]*agentTally) []AgentCount {
	if len(m) == 0 {
		return nil
	}
	out := make([]AgentCount, 0, len(m))
	for who, t := range m {
		out = append(out, AgentCount{
			Agent: who.Kind, Model: who.Model,
			Attempts: t.attempts, Published: t.published, CostUSD: t.cost,
			P50: Percentile(t.durations, 50),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Attempts != out[j].Attempts {
			return out[i].Attempts > out[j].Attempts
		}
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].Model < out[j].Model
	})
	return out
}
