package bench

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

// The comparison's three rules, each asserted as a number that would be wrong
// without it: only attempts a run claims are counted, only cases every cell ran
// are compared, and every drop is named. The fourth property here is a refusal —
// a manifest whose claim about which adapter ran is contradicted by the records
// fails the whole report rather than mislabelling a row.

var t0 = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

const (
	baseOne   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseTwo   = "cccccccccccccccccccccccccccccccccccccccc"
	baseThree = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	dirClaude = "/s/claude"
	dirCodex  = "/s/codex"
)

// threeCases is the fixture cohort: three cases, so a report can have a matched
// pair and an unmatched third.
func threeCases(t *testing.T) *Cohort {
	t.Helper()
	c := Cohort{Version: "v1", SourceRepo: "acme/ben"}
	tasks := map[string]string{}
	for _, spec := range []struct {
		id, base, good string
		tier           Tier
	}{
		{"case-one", baseOne, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", TierEasy},
		{"case-two", baseTwo, "dddddddddddddddddddddddddddddddddddddddd", TierMedium},
		{"case-three", baseThree, "ffffffffffffffffffffffffffffffffffffffff", TierHard},
	} {
		text := "task text for " + spec.id + "\n"
		file := spec.id + ".task.md"
		tasks[file] = text
		c.Cases = append(c.Cases, Case{
			ID: spec.id, Title: spec.id, Tier: spec.tier,
			TaskFile: file, TaskSHA256: digest(text),
			BaseCommit: spec.base,
			KnownGood:  Outcome{Commit: spec.good, Summary: "what solved it"},
			Checks:     []Check{{ID: "probe", Run: "true", Why: "because", FailsAtBase: true}},
		})
	}
	for i := range c.Cases {
		c.Cases[i].DefinitionSHA256 = c.computedDefinitionSHA256(c.Cases[i])
	}
	loaded, err := Load(files(t, c, tasks))
	if err != nil {
		t.Fatalf("fixture cohort: %v", err)
	}
	return loaded
}

// run is a manifest row, spelled compactly for the fixtures below.
func run(caseID, agent, model, issue, base, dir string) Run {
	return Run{
		Case: caseID, Agent: agent, Model: model,
		Repo: "acme/canary-" + issue, Issue: issue, Base: base, StateDir: dir,
	}
}

func comparisonManifest(c *Cohort, runs ...Run) *Manifest {
	for i := range runs {
		cs, ok := c.Case(runs[i].Case)
		if ok {
			runs[i].CaseDefinitionSHA256 = cs.DefinitionSHA256
		} else {
			// Well-formed but deliberately unjoinable, so Compare reaches the
			// independent unknown-case refusal rather than schema validation.
			runs[i].CaseDefinitionSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
		}
	}
	return &Manifest{
		Cohort:  "v1",
		Session: "s1",
		ExpectedCells: []Cell{
			{Agent: "claude-code", Model: "opus"},
			{Agent: "codex-exec"},
		},
		Runs: runs,
	}
}

type mod func(*state.Attempt)

func published() mod       { return func(a *state.Attempt) { a.Verdict = state.VerdictPublished } }
func verdict(v string) mod { return func(a *state.Attempt) { a.Verdict = v } }
func failed(r core.FailureReason) mod {
	return func(a *state.Attempt) { a.FailureReason = r }
}
func usage(in, out int64, cost float64) mod {
	return func(a *state.Attempt) { a.InputTokens, a.OutputTokens, a.CostUSD = in, out, cost }
}
func neverRan() mod { return func(a *state.Attempt) { a.Ran = false } }

func att(issue, agent, model string, n int, span time.Duration, mods ...mod) state.Attempt {
	a := state.Attempt{
		Issue: issue, Agent: agent, Model: model, Attempt: n, Ran: true,
		StartedAt: t0, EndedAt: t0.Add(span),
	}
	for _, m := range mods {
		m(&a)
	}
	return a
}

// The whole readout, over a session shaped like a real one: two cells, two cases
// each, a third case only one cell ran, one attempt belonging to unrelated
// dogfood work in the same log, and one declared run nothing backs.
func TestCompareReportsMatchedCasesWithDenominators(t *testing.T) {
	c := threeCases(t)
	m := comparisonManifest(c,
		run("case-one", "claude-code", "opus", "11", baseOne, dirClaude),
		run("case-two", "claude-code", "opus", "12", baseTwo, dirClaude),
		run("case-three", "claude-code", "opus", "13", baseThree, dirClaude),
		run("case-one", "codex-exec", "", "21", baseOne, dirCodex),
		run("case-two", "codex-exec", "", "22", baseTwo, dirCodex),
		// Declared and never dispatched: it must not shrink a denominator
		// silently.
		run("case-three", "codex-exec", "", "23", baseThree, dirCodex),
	)
	logs := map[string][]state.Attempt{
		dirClaude: {
			att("11", "claude-code", "opus", 1, 10*time.Minute, failed(core.FailureTimeout), usage(100, 10, 1)),
			att("11", "claude-code", "opus", 2, 20*time.Minute, published(), usage(200, 20, 2)),
			att("12", "claude-code", "opus", 1, 30*time.Minute, published(), usage(300, 30, 3)),
			att("13", "claude-code", "opus", 1, 40*time.Minute, published(), usage(400, 40, 4)),
			// A dogfood issue that shares the host. Not a benchmark run, so not a
			// benchmark number.
			att("999", "claude-code", "opus", 1, 5*time.Minute, published(), usage(1, 1, 99)),
		},
		dirCodex: {
			att("21", "codex-exec", "", 1, 50*time.Minute, published(), usage(500, 50, 0)),
			att("22", "codex-exec", "", 1, 60*time.Minute, verdict("incomplete"), usage(600, 60, 0)),
		},
	}

	rep, err := Compare(c, m, logs)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	if rep.Cohort != "v1" || rep.Session != "s1" || rep.SourceRepo != "acme/ben" {
		t.Errorf("report provenance = %+v", rep)
	}
	if rep.CohortCases != 3 {
		t.Errorf("CohortCases = %d, want 3 — the denominator matched cases is a fraction of", rep.CohortCases)
	}
	if got := rep.MatchedCases; len(got) != 2 || got[0] != "case-one" || got[1] != "case-two" {
		t.Errorf("MatchedCases = %v, want the two both cells ran, in cohort order", got)
	}
	if rep.AttemptsRead != 7 || rep.AttemptsJoined != 6 || rep.UnjoinedAttempts != 1 {
		t.Errorf("read/joined/unjoined = %d/%d/%d, want 7/6/1 — the dogfood attempt is read, "+
			"reported, and not counted", rep.AttemptsRead, rep.AttemptsJoined, rep.UnjoinedAttempts)
	}
	if len(rep.RunsWithoutAttempts) != 1 || rep.RunsWithoutAttempts[0].Issue != "23" {
		t.Errorf("RunsWithoutAttempts = %+v, want the one declared run no record backs",
			rep.RunsWithoutAttempts)
	}

	// Coverage names all three cases; the third is excluded and says by whom.
	if len(rep.Coverage) != 3 {
		t.Fatalf("Coverage covers %d cases, want every cohort case", len(rep.Coverage))
	}
	third := rep.Coverage[2]
	if third.Case != "case-three" || third.Matched || len(third.Cells) != 1 ||
		third.Cells[0] != "claude-code (opus)" || third.Tier != TierHard {
		t.Errorf("coverage of case-three = %+v, want unmatched and attributed to the one cell that ran it", third)
	}

	if len(rep.Cells) != 2 {
		t.Fatalf("Cells = %d, want two", len(rep.Cells))
	}
	claude, codex := rep.Cells[0], rep.Cells[1]
	if claude.Agent != "claude-code" || codex.Agent != "codex-exec" {
		t.Fatalf("cells are ordered %s then %s", claude.Label, codex.Label)
	}

	// The matched-case rule at work: case-three published for claude-code and is
	// counted nowhere, so neither the rate nor the cost includes it.
	if claude.Cases != 2 || claude.PublishedCases != 2 || claude.PublishRate() != 1 {
		t.Errorf("claude cases/published/rate = %d/%d/%.2f, want 2/2/1.00",
			claude.Cases, claude.PublishedCases, claude.PublishRate())
	}
	if claude.Runs != 2 || claude.PublishedRuns != 2 {
		t.Errorf("claude runs/published runs = %d/%d, want 2/2", claude.Runs, claude.PublishedRuns)
	}
	if claude.Attempts != 3 || claude.Ran != 3 || claude.AttemptsToPublish != 3 {
		t.Errorf("claude attempts/ran/to-publish = %d/%d/%d, want 3/3/3",
			claude.Attempts, claude.Ran, claude.AttemptsToPublish)
	}
	if claude.InputTokens != 600 || claude.OutputTokens != 60 || claude.CostUSD != 6 {
		t.Errorf("claude usage = %d/%d/$%.2f, want 600/60/$6.00 — case-three's 400/40/$4 excluded, "+
			"and the dogfood attempt's $99 nowhere near it",
			claude.InputTokens, claude.OutputTokens, claude.CostUSD)
	}
	if claude.P50 != 20*time.Minute || claude.P95 != 30*time.Minute {
		t.Errorf("claude p50/p95 = %s/%s, want 20m/30m", claude.P50, claude.P95)
	}
	if claude.UnpricedAttempts != 0 {
		t.Errorf("claude unpriced = %d, want 0", claude.UnpricedAttempts)
	}
	if len(claude.Failures) != 1 || claude.Failures[0].Name != string(core.FailureTimeout) {
		t.Errorf("claude failures = %+v, want one timeout", claude.Failures)
	}
	if len(claude.Verdicts) != 1 || claude.Verdicts[0].Name != state.VerdictPublished ||
		claude.Verdicts[0].Count != 2 {
		t.Errorf("claude verdicts = %+v, want published twice", claude.Verdicts)
	}
	if len(claude.CaseResults) != 2 || claude.CaseResults[0].Case != "case-one" ||
		claude.CaseResults[0].Attempts != 2 || claude.CaseResults[0].Duration != 30*time.Minute ||
		!claude.CaseResults[0].Published {
		t.Errorf("claude case results = %+v, want the per-case detail behind the row", claude.CaseResults)
	}

	if codex.Cases != 2 || codex.PublishedCases != 1 || codex.PublishRate() != 0.5 {
		t.Errorf("codex cases/published/rate = %d/%d/%.2f, want 2/1/0.50",
			codex.Cases, codex.PublishedCases, codex.PublishRate())
	}
	// An adapter that quotes no price reports $0 with real tokens. The cost
	// column alone would read as the cheaper adapter (core.Usage).
	if codex.CostUSD != 0 || codex.UnpricedAttempts != 2 {
		t.Errorf("codex cost/unpriced = $%.2f/%d, want $0.00/2 — a zero cost that says so",
			codex.CostUSD, codex.UnpricedAttempts)
	}
	if len(codex.Verdicts) != 2 {
		t.Errorf("codex verdicts = %+v, want published and incomplete", codex.Verdicts)
	}
	if codex.CaseResults[1].Verdict != "incomplete" {
		t.Errorf("codex case-two verdict = %q, want the §9.7 verdict it ended on",
			codex.CaseResults[1].Verdict)
	}
}

func TestChecksDecidePassSeparatelyFromPublish(t *testing.T) {
	c := threeCases(t)
	const checked = "9999999999999999999999999999999999999999"
	claudeOne := run("case-one", "claude-code", "opus", "11", baseOne, dirClaude)
	claudeOne.CheckedCommit = checked
	claudeOne.CheckResults = []CheckResult{{ID: "probe", Passed: true}}
	claudeTwo := run("case-two", "claude-code", "opus", "12", baseTwo, dirClaude)
	codexOne := run("case-one", "codex-exec", "", "21", baseOne, dirCodex)
	codexOne.CheckedCommit = checked
	codexOne.CheckResults = []CheckResult{{ID: "probe", Passed: false}}
	codexTwo := run("case-two", "codex-exec", "", "22", baseTwo, dirCodex)
	m := comparisonManifest(c, claudeOne, claudeTwo, codexOne, codexTwo)
	logs := map[string][]state.Attempt{
		dirClaude: {
			att("11", "claude-code", "opus", 1, time.Minute, published()),
			att("12", "claude-code", "opus", 1, time.Minute, published()),
		},
		dirCodex: {
			att("21", "codex-exec", "", 1, time.Minute, published()),
			att("22", "codex-exec", "", 1, time.Minute, verdict("incomplete")),
		},
	}

	rep, err := Compare(c, m, logs)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	claude, codex := rep.Cells[0], rep.Cells[1]
	if claude.PublishedCases != 2 || claude.PassedCases != 1 ||
		claude.UncheckedPublishedCases != 1 || claude.CheckFailedCases != 0 {
		t.Errorf("claude publish/pass/check status = %d/%d/%d/%d, want 2/1/1/0",
			claude.PublishedCases, claude.PassedCases,
			claude.UncheckedPublishedCases, claude.CheckFailedCases)
	}
	if codex.PublishedCases != 1 || codex.PassedCases != 0 || codex.CheckFailedCases != 1 {
		t.Errorf("codex publish/pass/check-fail = %d/%d/%d, want 1/0/1",
			codex.PublishedCases, codex.PassedCases, codex.CheckFailedCases)
	}
	if len(codex.CheckFailures) != 1 || codex.CheckFailures[0].Name != "probe" || codex.CheckFailures[0].Count != 1 {
		t.Errorf("codex check failures = %+v, want probe 1", codex.CheckFailures)
	}
	if got := claude.CaseResults[0].Checks; got != ChecksPassed || !claude.CaseResults[0].Passed {
		t.Errorf("checked published case = %+v, want passed", claude.CaseResults[0])
	}
	if got := claude.CaseResults[1].Checks; got != ChecksUnchecked || claude.CaseResults[1].Passed {
		t.Errorf("unchecked published case = %+v, want unchecked and not passed", claude.CaseResults[1])
	}
	if got := codex.CaseResults[0].Checks; got != ChecksFailed || codex.CaseResults[0].Passed {
		t.Errorf("failed-check case = %+v, want failed and not passed", codex.CaseResults[0])
	}
	if got := codex.CaseResults[1].Checks; got != ChecksNotPublished {
		t.Errorf("unpublished case checks = %q, want %q", got, ChecksNotPublished)
	}
}

func TestCompareRefusesCheckEvidenceItCannotJoin(t *testing.T) {
	const checked = "9999999999999999999999999999999999999999"
	for _, tc := range []struct {
		name   string
		mutate func(*Cohort, *Run, *state.Attempt)
	}{
		{
			name: "an unknown check",
			mutate: func(_ *Cohort, r *Run, _ *state.Attempt) {
				r.CheckResults[0].ID = "not-in-the-cohort"
			},
		},
		{
			name: "a missing cohort check",
			mutate: func(c *Cohort, _ *Run, _ *state.Attempt) {
				c.Cases[0].Checks = append(c.Cases[0].Checks,
					Check{ID: "second-probe", Run: "true", Why: "the other condition", FailsAtBase: true})
				c.Cases[0].DefinitionSHA256 = c.computedDefinitionSHA256(c.Cases[0])
			},
		},
		{
			name: "results for a run BEN did not publish",
			mutate: func(_ *Cohort, _ *Run, a *state.Attempt) {
				a.Verdict = "incomplete"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := threeCases(t)
			r := run("case-one", "claude-code", "opus", "11", baseOne, dirClaude)
			r.CheckedCommit = checked
			r.CheckResults = []CheckResult{{ID: "probe", Passed: true}}
			a := att("11", "claude-code", "opus", 1, time.Minute, published())
			tc.mutate(c, &r, &a)
			m := comparisonManifest(c, r)
			if _, err := Compare(c, m, map[string][]state.Attempt{dirClaude: {a}}); !errors.Is(err, ErrCheckMismatch) {
				t.Fatalf("Compare = %v, want ErrCheckMismatch", err)
			}
		})
	}
}

func TestCompareRefusesAJoinItCannotTrust(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest func(*Manifest)
		logs     map[string][]state.Attempt
		want     error
	}{
		{
			name:     "a session recorded against another cohort version",
			manifest: func(m *Manifest) { m.Cohort = "v2" },
			want:     ErrCohortMismatch,
		},
		{
			name:     "a run naming a case the cohort does not define",
			manifest: func(m *Manifest) { m.Runs[0].Case = "case-four" },
			want:     ErrUnknownCase,
		},
		{
			// The pinned base is the fixed input. A run from another tree is not a
			// run of this case, whatever it published.
			name:     "a run worked from another base",
			manifest: func(m *Manifest) { m.Runs[0].Base = baseTwo },
			want:     ErrBaseMismatch,
		},
		{
			// The record is the evidence, the manifest is the claim (SPEC §3.5).
			name: "records that name a different adapter than the manifest",
			logs: map[string][]state.Attempt{
				dirClaude: {att("11", "codex-exec", "", 1, time.Minute, published())},
			},
			want: ErrCellMismatch,
		},
		{
			name: "records that name a different model than the manifest",
			logs: map[string][]state.Attempt{
				dirClaude: {att("11", "claude-code", "sonnet", 1, time.Minute, published())},
			},
			want: ErrCellMismatch,
		},
		{
			// The default-model cell is a cell: a record with no model where one
			// was claimed is the same mislabelling in the other direction.
			name: "records with no model where the manifest claims one",
			logs: map[string][]state.Attempt{
				dirClaude: {att("11", "claude-code", "", 1, time.Minute, published())},
			},
			want: ErrCellMismatch,
		},
		{
			name: "an attempt log supplied under an aliased state directory",
			logs: map[string][]state.Attempt{
				dirClaude + "/.": nil,
			},
			want: ErrNonCanonicalStateDir,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := threeCases(t)
			m := comparisonManifest(c, run("case-one", "claude-code", "opus", "11", baseOne, dirClaude))
			if tc.manifest != nil {
				tc.manifest(m)
			}
			logs := tc.logs
			if logs == nil {
				logs = map[string][]state.Attempt{}
			}
			if _, err := Compare(c, m, logs); !errors.Is(err, tc.want) {
				t.Fatalf("Compare = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCompareRefusesAChangedCaseDefinitionUnderTheSameVersionAndID(t *testing.T) {
	c := threeCases(t)
	m := comparisonManifest(c, run("case-one", "claude-code", "opus", "11", baseOne, dirClaude))
	original := m.Runs[0].CaseDefinitionSHA256

	c.Cases[0].Title = "A different task under the retained case ID"
	c.Cases[0].DefinitionSHA256 = c.computedDefinitionSHA256(c.Cases[0])
	if c.Cases[0].DefinitionSHA256 == original {
		t.Fatal("the changed case retained its definition fingerprint")
	}
	if _, err := Compare(c, m, nil); !errors.Is(err, ErrCaseDefinitionMismatch) {
		t.Fatalf("Compare = %v, want ErrCaseDefinitionMismatch", err)
	}
}

func TestCompareRefusesAChangedSourceRepositoryUnderTheSameVersionAndID(t *testing.T) {
	c := threeCases(t)
	m := comparisonManifest(c, run("case-one", "claude-code", "opus", "11", baseOne, dirClaude))
	original := m.Runs[0].CaseDefinitionSHA256

	c.SourceRepo = "other/ben"
	c.Cases[0].DefinitionSHA256 = c.computedDefinitionSHA256(c.Cases[0])
	if c.Cases[0].DefinitionSHA256 == original {
		t.Fatal("the changed source repository retained its definition fingerprint")
	}
	if _, err := Compare(c, m, nil); !errors.Is(err, ErrCaseDefinitionMismatch) {
		t.Fatalf("Compare = %v, want ErrCaseDefinitionMismatch", err)
	}
}

func TestCompareReportsTheCanonicalSourceRepositoryIdentity(t *testing.T) {
	c := threeCases(t)
	m := comparisonManifest(c, run("case-one", "claude-code", "opus", "11", baseOne, dirClaude))
	c.SourceRepo = "ACME/BEN"

	rep, err := Compare(c, m, nil)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if rep.SourceRepo != "acme/ben" {
		t.Fatalf("SourceRepo = %q, want canonical GitHub identity acme/ben", rep.SourceRepo)
	}
}

// An attempt that never started a process is a real span an operator waited
// through — a clone that hung, a hook that never returned — so it counts toward
// the percentiles and not toward the unpriced tally (state.Attempt.Ran).
func TestAnAttemptThatNeverRanCountsTowardTimeAndNotTowardPrice(t *testing.T) {
	c := threeCases(t)
	m := comparisonManifest(c,
		run("case-one", "claude-code", "opus", "11", baseOne, dirClaude),
		run("case-one", "codex-exec", "", "21", baseOne, dirCodex),
	)
	logs := map[string][]state.Attempt{dirClaude: {
		att("11", "claude-code", "opus", 1, 90*time.Minute, neverRan(), failed(core.FailureLaunchError)),
		att("11", "claude-code", "opus", 2, 10*time.Minute, published(), usage(10, 1, 0.5)),
	}, dirCodex: {
		att("21", "codex-exec", "", 1, time.Minute, published()),
	}}

	rep, err := Compare(c, m, logs)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	cell := rep.Cells[0]
	if cell.Attempts != 2 || cell.Ran != 1 {
		t.Errorf("attempts/ran = %d/%d, want 2/1", cell.Attempts, cell.Ran)
	}
	if cell.P95 != 90*time.Minute {
		t.Errorf("p95 = %s, want the 90m dispatch that never launched — excluding it would delete "+
			"exactly the worktree and hook failures a p95 is read to find", cell.P95)
	}
	if cell.UnpricedAttempts != 0 {
		t.Errorf("unpriced = %d, want 0: the attempt that never ran reported no usage at all, so "+
			"counting it would invent an adapter that quotes no price", cell.UnpricedAttempts)
	}
}

// A repeated case in one cell is not an extra chance to win. v1 declares no
// balanced trial count, so Compare refuses the manifest instead of selecting the
// best of several runs for whichever cell happened to be repeated.
func TestCompareRefusesARepeatedCellCase(t *testing.T) {
	c := threeCases(t)
	first := run("case-one", "claude-code", "opus", "11", baseOne, dirClaude)
	second := run("case-one", "claude-code", "opus", "14", baseOne, "/s/claude-repeat")
	second.Repo = "acme/canary-two"
	peer := run("case-one", "codex-exec", "", "21", baseOne, dirCodex)
	m := comparisonManifest(c, first, second, peer)
	if _, err := Compare(c, m, nil); !errors.Is(err, ErrDuplicateRun) {
		t.Fatalf("Compare = %v, want ErrDuplicateRun", err)
	}
}

// A manifest cell with no attempt records is still part of the experiment. If
// matching considered only observed cells, the other cell's case would become
// a matched one-cell result and the missing comparison would look complete.
func TestAWhollyUnbackedCellCannotShrinkTheMatchedDenominator(t *testing.T) {
	c := threeCases(t)
	m := comparisonManifest(c,
		run("case-one", "claude-code", "opus", "11", baseOne, dirClaude),
		run("case-one", "codex-exec", "", "21", baseOne, dirCodex),
	)
	logs := map[string][]state.Attempt{
		dirClaude: {att("11", "claude-code", "opus", 1, time.Minute, published())},
	}

	rep, err := Compare(c, m, logs)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(rep.MatchedCases) != 0 {
		t.Errorf("MatchedCases = %v, want none while the declared codex cell is wholly unbacked", rep.MatchedCases)
	}
	if len(rep.Cells) != 1 || rep.Cells[0].Cases != 0 {
		t.Errorf("observed cells = %+v, want the claude row over zero matched cases", rep.Cells)
	}
	if len(rep.RunsWithoutAttempts) != 1 || rep.RunsWithoutAttempts[0].Cell != "codex-exec (default model)" {
		t.Errorf("RunsWithoutAttempts = %+v, want the missing codex cell named", rep.RunsWithoutAttempts)
	}
	if rep.Coverage[0].Matched {
		t.Errorf("case-one coverage = %+v, want unmatched", rep.Coverage[0])
	}
}

// With no cell holding records at all, there is nothing to match and the report
// says so rather than reporting a rate over an empty set.
func TestASessionWithNoRecordsMatchesNothing(t *testing.T) {
	c := threeCases(t)
	m := comparisonManifest(c, run("case-one", "claude-code", "opus", "11", baseOne, dirClaude))
	rep, err := Compare(c, m, map[string][]state.Attempt{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(rep.Cells) != 0 || len(rep.MatchedCases) != 0 {
		t.Errorf("cells/matched = %d/%v, want neither", len(rep.Cells), rep.MatchedCases)
	}
	if len(rep.RunsWithoutAttempts) != 1 {
		t.Errorf("RunsWithoutAttempts = %+v, want the declared run", rep.RunsWithoutAttempts)
	}
	if len(rep.CellsWithoutRuns) != 1 || rep.CellsWithoutRuns[0].Agent != "codex-exec" {
		t.Errorf("CellsWithoutRuns = %+v, want the wholly omitted codex cell", rep.CellsWithoutRuns)
	}
	for _, cov := range rep.Coverage {
		if cov.Matched {
			t.Errorf("case %s reported matched with no records anywhere", cov.Case)
		}
	}
}

// Two reports over one session are byte-identical. The join walks maps, so
// without the ordering this would be a comparison that changes between runs of
// the same command.
func TestCompareIsDeterministic(t *testing.T) {
	c := threeCases(t)
	m := comparisonManifest(c,
		run("case-one", "claude-code", "opus", "11", baseOne, dirClaude),
		run("case-two", "claude-code", "opus", "12", baseTwo, dirClaude),
		run("case-one", "codex-exec", "", "21", baseOne, dirCodex),
		run("case-two", "codex-exec", "", "22", baseTwo, dirCodex),
	)
	logs := map[string][]state.Attempt{
		dirClaude: {
			att("11", "claude-code", "opus", 1, time.Minute, published(), failed("")),
			att("12", "claude-code", "opus", 1, 2*time.Minute, failed(core.FailureTimeout)),
		},
		dirCodex: {
			att("21", "codex-exec", "", 1, 3*time.Minute, failed(core.FailureLaunchError)),
			att("22", "codex-exec", "", 1, 4*time.Minute, published()),
		},
	}

	var first string
	for i := range 8 {
		rep, err := Compare(c, m, logs)
		if err != nil {
			t.Fatalf("Compare: %v", err)
		}
		body, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = string(body)
			continue
		}
		if string(body) != first {
			t.Fatalf("report %d differs from the first:\n%s\n%s", i, first, body)
		}
	}
}
