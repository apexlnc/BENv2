package bench

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

// The comparison. Attempt facts come from the append-only outcome log (#60):
// those records carry the adapter, model, verdict, usage and span, written when
// an attempt ended and never reconstructed from a WORKFLOW.md that has since
// been edited (state.Attempt). The predeclared matrix, dispatch join, immutable
// checked head and per-check results come from the session manifest.
//
// Three rules shape every number below, and each has a failure it exists to
// prevent:
//
//  1. **Only what a run claims.** An attempt is counted only if a manifest run
//     names its (state dir, issue). Everything else in the log is dogfood work
//     that happened to share a host, and pooling it would compare queues.
//  2. **Only matched cases.** A cell's rate is computed over the cases *every*
//     declared cell ran. Cases differ in difficulty far more than adapters differ
//     in ability, so an unmatched denominator measures which cases each adapter
//     happened to be given.
//  3. **Every denominator, and every drop.** Rates are reported beside their
//     counts, and the cells, attempts, runs and cases left out are named
//     (CellsWithoutRuns, UnjoinedAttempts, RunsWithoutAttempts, Coverage). A
//     benchmark that silently narrows its own input reads as coverage it does
//     not have.

// The refusals that need a cohort and a manifest together.
var (
	// ErrCohortMismatch is a session recorded against a different cohort version.
	ErrCohortMismatch = errors.New("bench: the manifest was recorded against another cohort version")
	// ErrUnknownCase is a run naming a case the cohort does not define.
	ErrUnknownCase = errors.New("bench: unknown case")
	// ErrCaseDefinitionMismatch is a run recorded against a different source
	// repository or case content under the same version and ID. The per-case
	// fingerprint prevents a current repository, title, task, base, or check from
	// relabelling or reinterpreting an older session.
	ErrCaseDefinitionMismatch = errors.New("bench: the run was recorded against another case definition")
	// ErrBaseMismatch is a run whose recorded base is not the case's pinned base:
	// the cell worked a different tree, so its result is not a result for this
	// case.
	ErrBaseMismatch = errors.New("bench: the run's base is not the case's pinned base")
	// ErrCellMismatch is a manifest run whose attempt records name a different
	// adapter or model than the run claims.
	//
	// A refusal rather than a relabelling, and it fails the whole report rather
	// than the row: the manifest is the join, so a wrong join means the grouping
	// cannot be trusted anywhere. The record is the evidence and the manifest is
	// the claim (SPEC §3.5) — fix the manifest to say what ran.
	ErrCellMismatch = errors.New("bench: the attempt records name a different cell than the manifest")
	// ErrCheckMismatch is check evidence that cannot be joined to the cohort or
	// to a published attempt: an unknown/missing check, or results recorded for a
	// run BEN did not publish.
	ErrCheckMismatch = errors.New("bench: check evidence does not describe the published cohort case")
)

// Report is one session's matched-case comparison.
type Report struct {
	Cohort     string `json:"cohort"`
	SourceRepo string `json:"source_repo"`
	Session    string `json:"session"`
	// CohortCases is the cohort's size — the denominator MatchedCases is a
	// fraction of.
	CohortCases  int      `json:"cohort_cases"`
	MatchedCases []string `json:"matched_cases"`
	// ExpectedCells is the matrix declared before dispatch. CellsWithoutRuns
	// makes a wholly omitted adapter/model visible even though it has no RunRef.
	ExpectedCells    []Cell `json:"expected_cells"`
	CellsWithoutRuns []Cell `json:"cells_without_runs,omitempty"`
	// Cells are the observed cells: those with at least one joined attempt, in
	// stable order. Matching still ranges over every cell the manifest declares;
	// a wholly unbacked cell is a denominator hole, not a reason to narrow it.
	Cells []CellResult `json:"cells"`
	// Coverage names every cohort case and which cells produced attempts for it,
	// so an excluded case is visible rather than merely absent.
	Coverage []CaseCoverage `json:"coverage"`
	// AttemptsRead is every record in the logs handed to Compare; AttemptsJoined
	// the subset a run claimed. Their difference is UnjoinedAttempts, stated so a
	// log holding unrelated dogfood work says so instead of looking like a
	// benchmark twice the size.
	AttemptsRead     int `json:"attempts_read"`
	AttemptsJoined   int `json:"attempts_joined"`
	UnjoinedAttempts int `json:"unjoined_attempts"`
	// RunsWithoutAttempts are runs the manifest declares that no record backs:
	// dispatched and still running, never dispatched, or a mistyped state
	// directory. Reported, because each one is a hole in a denominator.
	RunsWithoutAttempts []RunRef `json:"runs_without_attempts"`
}

// CellResult is one adapter/model pair's numbers over the matched cases.
type CellResult struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
	Label string `json:"label"`
	// Cases is the matched-case denominator — the same for every cell, by
	// construction — and PublishedCases how many of them this cell published.
	Cases          int `json:"cases"`
	PublishedCases int `json:"published_cases"`
	// PassedCases published and passed every cohort check at one recorded
	// immutable head. CheckFailedCases and UncheckedPublishedCases distinguish a
	// known bad result from a publish verdict nobody has checked yet.
	PassedCases             int `json:"passed_cases"`
	CheckFailedCases        int `json:"check_failed_cases"`
	UncheckedPublishedCases int `json:"unchecked_published_cases"`
	// Runs is how many isolated canary repositories/issues this cell used. It
	// equals Cases for a complete matrix: Manifest rejects a repeated cell/case
	// rather than selecting the best of several trials.
	Runs          int `json:"runs"`
	PublishedRuns int `json:"published_runs"`
	PassedRuns    int `json:"passed_runs"`
	// These three partition PublishedRuns.
	CheckFailedRuns        int `json:"check_failed_runs"`
	UncheckedPublishedRuns int `json:"unchecked_published_runs"`
	// Attempts is every §9.6 attempt those runs produced; Ran the subset that
	// started a process (state.Attempt.Ran).
	Attempts int `json:"attempts"`
	Ran      int `json:"ran"`
	// AttemptsToPublish is the attempts spent on the cases this cell published —
	// the cost in retries of the work that landed.
	AttemptsToPublish int `json:"attempts_to_publish"`
	// P50 and P95 are attempt wall-clock, nearest rank, over every attempt
	// including those that never started a process (state.Summary.P50).
	P50 time.Duration `json:"p50"`
	P95 time.Duration `json:"p95"`

	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	// UnpricedAttempts is attempts that ran and reported no cost. codex-exec is
	// an adapter that quotes none (core.Usage), so a cost column compared across
	// adapters is meaningless without this number beside it: the cheaper-looking
	// cell may simply be the one that does not say.
	UnpricedAttempts int `json:"unpriced_attempts"`

	Failures []state.Count `json:"failures"`
	Verdicts []state.Count `json:"verdicts"`
	// CheckFailures counts failed cohort check IDs over published runs.
	CheckFailures []state.Count `json:"check_failures"`
	// CaseResults is the per-case detail behind the row, in matched-case order —
	// what makes a pair of rows readable as pairs rather than as two averages.
	CaseResults []CaseResult `json:"case_results"`
}

// PublishRate is published matched cases over matched cases, or 0 with no cases.
func (c CellResult) PublishRate() float64 {
	if c.Cases == 0 {
		return 0
	}
	return float64(c.PublishedCases) / float64(c.Cases)
}

// PassRate is cases that both published and passed their checks over the common
// matched-case denominator.
func (c CellResult) PassRate() float64 {
	if c.Cases == 0 {
		return 0
	}
	return float64(c.PassedCases) / float64(c.Cases)
}

// CheckStatus is the check half of one case outcome.
type CheckStatus string

const (
	ChecksNotPublished CheckStatus = "not-published"
	ChecksUnchecked    CheckStatus = "unchecked"
	ChecksFailed       CheckStatus = "failed"
	ChecksPassed       CheckStatus = "passed"
)

// CaseResult is one cell's result on one case.
type CaseResult struct {
	Case      string      `json:"case"`
	Published bool        `json:"published"`
	Passed    bool        `json:"passed"`
	Checks    CheckStatus `json:"checks"`
	Attempts  int         `json:"attempts"`
	// Duration is the total wall clock the case's attempts spent in this cell.
	Duration time.Duration `json:"duration"`
	// Verdict is the last §9.7 verdict recorded for the case in this cell, and
	// FailureReason the last §7.3 failure. Both are empty when nothing reached
	// one.
	Verdict       string `json:"verdict,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
}

// CaseCoverage is one cohort case and which cells produced attempts for it.
type CaseCoverage struct {
	Case    string   `json:"case"`
	Tier    Tier     `json:"tier"`
	Matched bool     `json:"matched"`
	Cells   []string `json:"cells"`
}

// RunRef names a manifest run in a report.
type RunRef struct {
	Case  string `json:"case"`
	Cell  string `json:"cell"`
	Repo  string `json:"repo"`
	Issue string `json:"issue"`
}

// Compare joins a session's runs to the cohort's cases and groups the attempt
// records by adapter and model.
//
// logs is the attempt-outcome log of each state directory the manifest names,
// keyed by that directory. Reading them is the caller's — this function performs
// no I/O, so the whole join and every number below is testable against records
// written by hand.
func Compare(c *Cohort, m *Manifest, logs map[string][]state.Attempt) (Report, error) {
	if err := m.validate(); err != nil {
		return Report{}, err
	}
	if c.Version != m.Cohort {
		return Report{}, fmt.Errorf("%w: session %s ran cohort %s, this cohort is %s",
			ErrCohortMismatch, m.Session, m.Cohort, c.Version)
	}

	type joinKey struct{ dir, issue string }
	joins := map[joinKey]int{} // -> index into m.Runs
	checkEvidence := make([]runCheckEvidence, len(m.Runs))
	for i, r := range m.Runs {
		cs, ok := c.Case(r.Case)
		if !ok {
			return Report{}, fmt.Errorf("%w: run %d names case %q, which cohort %s does not define",
				ErrUnknownCase, i+1, r.Case, c.Version)
		}
		currentDefinition := c.computedDefinitionSHA256(cs)
		if currentDefinition != cs.DefinitionSHA256 {
			return Report{}, fmt.Errorf("%w: cohort %s case %s computes %s, pins %s",
				ErrCaseDefinitionDrift, c.Version, cs.ID, currentDefinition, cs.DefinitionSHA256)
		}
		if r.CaseDefinitionSHA256 != currentDefinition {
			return Report{}, fmt.Errorf("%w: run %d case %s records %s, cohort %s defines %s",
				ErrCaseDefinitionMismatch, i+1, cs.ID, r.CaseDefinitionSHA256,
				c.Version, currentDefinition)
		}
		if r.Base != cs.BaseCommit {
			return Report{}, fmt.Errorf("%w: case %s pins %s, run %d on %s#%s recorded %s",
				ErrBaseMismatch, cs.ID, cs.BaseCommit, i+1, r.Repo, r.Issue, r.Base)
		}
		evidence, err := validateCheckEvidence(r, cs)
		if err != nil {
			return Report{}, err
		}
		checkEvidence[i] = evidence
		joins[joinKey{filepath.Clean(r.StateDir), r.Issue}] = i
	}

	rep := Report{
		Cohort:        c.Version,
		SourceRepo:    strings.ToLower(c.SourceRepo),
		Session:       m.Session,
		CohortCases:   len(c.Cases),
		ExpectedCells: m.Cells(),
	}
	// cells[cell][case] accumulates; runSeen[i] records which declared runs the
	// log actually backs.
	cells := map[Cell]map[string]*bucket{}
	runSeen := make([]bool, len(m.Runs))
	publishedRun := make([]bool, len(m.Runs))
	runs := map[Cell]map[string]int{} // distinct canary issues per (cell, case)
	runsByCell := map[Cell]int{}
	for _, r := range m.Runs {
		runsByCell[r.Cell()]++
	}
	for _, cell := range rep.ExpectedCells {
		if runsByCell[cell] == 0 {
			rep.CellsWithoutRuns = append(rep.CellsWithoutRuns, cell)
		}
	}

	dirs := make([]string, 0, len(logs))
	for dir := range logs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return Report{}, fmt.Errorf("%w: attempt log key %q", ErrNonCanonicalStateDir, dir)
		}
		attempts := logs[dir]
		for _, a := range attempts {
			rep.AttemptsRead++
			i, ok := joins[joinKey{dir, a.Issue}]
			if !ok {
				rep.UnjoinedAttempts++
				continue
			}
			r := m.Runs[i]
			if a.Agent != r.Agent || a.Model != r.Model {
				return Report{}, fmt.Errorf("%w: %s#%s (case %s) is recorded as %s, its attempt %d ran %s",
					ErrCellMismatch, r.Repo, r.Issue, r.Case, r.Cell().Label(), a.Attempt,
					Cell{Agent: a.Agent, Model: a.Model}.Label())
			}
			rep.AttemptsJoined++
			runSeen[i] = true

			cell := r.Cell()
			byCase, ok := cells[cell]
			if !ok {
				byCase = map[string]*bucket{}
				cells[cell] = byCase
				runs[cell] = map[string]int{}
			}
			b, ok := byCase[r.Case]
			if !ok {
				b = newBucket()
				byCase[r.Case] = b
			}
			if !b.runsSeen[i] {
				b.runsSeen[i] = true
				runs[cell][r.Case]++
			}
			b.add(a, i)
			if a.Published() {
				publishedRun[i] = true
			}
		}
	}

	for i, evidence := range checkEvidence {
		if evidence.recorded && !publishedRun[i] {
			r := m.Runs[i]
			return Report{}, fmt.Errorf("%w: %s#%s (case %s) records checks at %s, but its attempt log has no published verdict",
				ErrCheckMismatch, r.Repo, r.Issue, r.Case, r.CheckedCommit)
		}
	}

	for i, seen := range runSeen {
		if seen {
			continue
		}
		r := m.Runs[i]
		rep.RunsWithoutAttempts = append(rep.RunsWithoutAttempts, RunRef{
			Case: r.Case, Cell: r.Cell().Label(), Repo: r.Repo, Issue: r.Issue,
		})
	}

	order := make([]Cell, 0, len(cells))
	for cell := range cells {
		order = append(order, cell)
	}
	sortCells(order)

	// Matched: a case every declared cell produced attempts for. `order` contains
	// only observed cells for the rows, but matching from it would make a wholly
	// unbacked cell disappear and turn an incomplete matrix into a comparison.
	// Iterated in cohort order so the report's case order is the cohort's.
	declared := m.Cells()
	for _, cs := range c.Cases {
		cov := CaseCoverage{Case: cs.ID, Tier: cs.Tier}
		matched := len(declared) > 0
		for _, cell := range declared {
			if _, ok := cells[cell][cs.ID]; ok {
				cov.Cells = append(cov.Cells, cell.Label())
				continue
			}
			matched = false
		}
		cov.Matched = matched
		rep.Coverage = append(rep.Coverage, cov)
		if matched {
			rep.MatchedCases = append(rep.MatchedCases, cs.ID)
		}
	}

	for _, cell := range order {
		res := CellResult{Agent: cell.Agent, Model: cell.Model, Label: cell.Label()}
		failures, verdicts, checkFailures := map[string]int{}, map[string]int{}, map[string]int{}
		var durations []time.Duration
		for _, id := range rep.MatchedCases {
			b := cells[cell][id]
			res.Cases++
			res.Runs += runs[cell][id]
			res.Attempts += b.attempts
			res.Ran += b.ran
			res.InputTokens += b.in
			res.OutputTokens += b.out
			res.CostUSD += b.cost
			res.UnpricedAttempts += b.unpriced
			durations = append(durations, b.durations...)
			for name, n := range b.failures {
				failures[name] += n
			}
			for name, n := range b.verdicts {
				verdicts[name] += n
			}
			if b.published {
				res.PublishedCases++
				res.PublishedRuns += b.publishedRuns
				res.AttemptsToPublish += b.attempts
			}
			checkSummary := summarizeChecks(b, checkEvidence)
			res.PassedRuns += checkSummary.passedRuns
			res.CheckFailedRuns += checkSummary.failedRuns
			res.UncheckedPublishedRuns += checkSummary.uncheckedRuns
			for name, n := range checkSummary.failures {
				checkFailures[name] += n
			}
			switch checkSummary.status {
			case ChecksPassed:
				res.PassedCases++
			case ChecksFailed:
				res.CheckFailedCases++
			case ChecksUnchecked:
				res.UncheckedPublishedCases++
			}
			res.CaseResults = append(res.CaseResults, CaseResult{
				Case: id, Published: b.published, Passed: checkSummary.status == ChecksPassed,
				Checks: checkSummary.status, Attempts: b.attempts, Duration: b.duration,
				Verdict: b.lastVerdict.value, FailureReason: b.lastFailure.value,
			})
		}
		res.P50 = state.Percentile(durations, 50)
		res.P95 = state.Percentile(durations, 95)
		res.Failures, res.Verdicts = state.Counts(failures), state.Counts(verdicts)
		res.CheckFailures = state.Counts(checkFailures)
		rep.Cells = append(rep.Cells, res)
	}
	return rep, nil
}

// runCheckEvidence is the cohort-validated check record for one manifest run.
// It is joined to the published attempt separately: a complete-looking check
// list for a run BEN never published is a contradiction, not a passing result.
type runCheckEvidence struct {
	recorded bool
	passed   bool
	failures []string
}

func validateCheckEvidence(r Run, cs Case) (runCheckEvidence, error) {
	if len(r.CheckResults) == 0 {
		return runCheckEvidence{}, nil
	}
	expected := make(map[string]bool, len(cs.Checks))
	for _, check := range cs.Checks {
		expected[check.ID] = true
	}
	seen := make(map[string]bool, len(r.CheckResults))
	evidence := runCheckEvidence{recorded: true, passed: true}
	for _, result := range r.CheckResults {
		if !expected[result.ID] {
			return runCheckEvidence{}, fmt.Errorf("%w: %s#%s (case %s) records unknown check %q",
				ErrCheckMismatch, r.Repo, r.Issue, r.Case, result.ID)
		}
		seen[result.ID] = true
		if !result.Passed {
			evidence.passed = false
			evidence.failures = append(evidence.failures, result.ID)
		}
	}
	for _, check := range cs.Checks {
		if !seen[check.ID] {
			return runCheckEvidence{}, fmt.Errorf("%w: %s#%s (case %s) has no result for check %q",
				ErrCheckMismatch, r.Repo, r.Issue, r.Case, check.ID)
		}
	}
	return evidence, nil
}

type caseCheckSummary struct {
	status                                CheckStatus
	passedRuns, failedRuns, uncheckedRuns int
	failures                              map[string]int
}

func summarizeChecks(b *bucket, evidence []runCheckEvidence) caseCheckSummary {
	s := caseCheckSummary{status: ChecksNotPublished, failures: map[string]int{}}
	if !b.published {
		return s
	}
	// Manifest permits exactly one run for this cell/case. publishedBy is still a
	// map because a bucket sees attempt records and must count a run only once.
	for runIndex := range b.publishedBy {
		e := evidence[runIndex]
		switch {
		case !e.recorded:
			s.uncheckedRuns++
			s.status = ChecksUnchecked
		case e.passed:
			s.passedRuns++
			s.status = ChecksPassed
		default:
			s.failedRuns++
			s.status = ChecksFailed
			for _, id := range e.failures {
				s.failures[id]++
			}
		}
		return s
	}
	// b.add sets published and publishedBy together, so a valid published bucket
	// always returns from the loop.
	return s
}

// bucket is one (cell, case) tally.
type bucket struct {
	attempts int
	ran      int
	unpriced int
	in, out  int64
	cost     float64
	duration time.Duration
	// durations is per attempt, for the percentiles; duration is their sum, for
	// the per-case column.
	durations []time.Duration
	published bool
	// publishedRuns counts the run that published. It is at most one because a
	// manifest refuses repeated cell/case runs.
	publishedRuns int
	// runsSeen and publishedBy are keyed by the manifest's run index, not by the
	// issue identifier: an identifier is unique within one tracker scope, so two
	// canary repositories can each carry an issue 11, and keying on "11" would
	// collapse two runs of one case into one.
	runsSeen    map[int]bool
	publishedBy map[int]bool
	failures    map[string]int
	verdicts    map[string]int
	lastVerdict lastValue
	lastFailure lastValue
}

func newBucket() *bucket {
	return &bucket{
		runsSeen:    map[int]bool{},
		publishedBy: map[int]bool{},
		failures:    map[string]int{},
		verdicts:    map[string]int{},
	}
}

func (b *bucket) add(a state.Attempt, runIndex int) {
	b.attempts++
	b.in += a.InputTokens
	b.out += a.OutputTokens
	b.cost += a.CostUSD
	b.durations = append(b.durations, a.Duration())
	b.duration += a.Duration()
	if a.Ran {
		b.ran++
		if a.CostUSD == 0 {
			// Only of the attempts that ran, on state.Summary's terms: an attempt
			// that never launched reported no usage at all, and counting it as
			// unpriced would invent an adapter limitation.
			b.unpriced++
		}
	}
	if a.FailureReason != "" {
		b.failures[string(a.FailureReason)]++
		b.lastFailure.consider(string(a.FailureReason), a, runIndex)
	}
	if a.Verdict != "" {
		b.verdicts[a.Verdict]++
		b.lastVerdict.consider(a.Verdict, a, runIndex)
	}
	if a.Published() {
		b.published = true
		if !b.publishedBy[runIndex] {
			b.publishedBy[runIndex] = true
			b.publishedRuns++
		}
	}
}

// lastValue chooses "last" across append logs that have no shared record
// order. EndedAt is the cross-log clock; manifest order and attempt number make
// equal timestamps deterministic. The logs themselves are also visited in
// sorted path order so floating-point totals do not depend on map iteration.
type lastValue struct {
	value   string
	ended   time.Time
	run     int
	attempt int
	set     bool
}

func (v *lastValue) consider(value string, a state.Attempt, runIndex int) {
	if v.set {
		switch {
		case a.EndedAt.Before(v.ended):
			return
		case a.EndedAt.Equal(v.ended) && runIndex < v.run:
			return
		case a.EndedAt.Equal(v.ended) && runIndex == v.run && a.Attempt < v.attempt:
			return
		}
	}
	v.value, v.ended, v.run, v.attempt, v.set = value, a.EndedAt, runIndex, a.Attempt, true
}

// sortCells orders cells by adapter then model, so a report over an unchanged
// session is byte-identical however the maps behind it iterated.
func sortCells(cells []Cell) {
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Agent != cells[j].Agent {
			return cells[i].Agent < cells[j].Agent
		}
		return cells[i].Model < cells[j].Model
	})
}
