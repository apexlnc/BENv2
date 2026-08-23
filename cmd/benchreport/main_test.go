package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/bench"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

// The command is the boundary an operator reads, so what is asserted here is the
// rendering: every rate beside its denominator, the unpriced column present, and
// the dropped attempts and unbacked runs named. The arithmetic behind it is
// internal/bench's own tests — this is the independent end of it (AGENTS.md,
// "Conventions").

// session writes a benchmark session against the embedded cohort: two cells over
// its first two cases, a third case in one cell only, one unrelated dogfood
// attempt in the same log, and one declared run nothing backs.
func session(t *testing.T) (manifestPath string) {
	t.Helper()
	cohort, err := bench.Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	if len(cohort.Cases) < 3 {
		t.Fatalf("the embedded cohort has %d cases; this fixture needs three", len(cohort.Cases))
	}
	one, two, three := cohort.Cases[0], cohort.Cases[1], cohort.Cases[2]

	root := t.TempDir()
	claudeDir, codexDir := filepath.Join(root, "claude"), filepath.Join(root, "codex")

	t0 := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	write := func(dir string, attempts ...state.Attempt) {
		t.Helper()
		w, err := state.At(dir).AppendAttempts()
		if err != nil {
			t.Fatalf("AppendAttempts: %v", err)
		}
		defer w.Close() //nolint:errcheck // the append errors are what matter
		for _, a := range attempts {
			if err := w.Append(a); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
	}
	attempt := func(issue, agent, model string, n int, span time.Duration, verdict string) state.Attempt {
		return state.Attempt{
			Issue: issue, Attempt: n, Agent: agent, Model: model, Ran: true,
			StartedAt: t0, EndedAt: t0.Add(span), Verdict: verdict,
			InputTokens: 1000, OutputTokens: 100, CostUSD: 1.25,
		}
	}
	unpriced := func(issue, agent, model string, n int, span time.Duration, verdict string) state.Attempt {
		a := attempt(issue, agent, model, n, span, verdict)
		a.CostUSD = 0
		return a
	}

	write(claudeDir,
		attempt("11", "claude-code", "opus", 1, 10*time.Minute, "incomplete"),
		attempt("11", "claude-code", "opus", 2, 20*time.Minute, state.VerdictPublished),
		attempt("12", "claude-code", "opus", 1, 30*time.Minute, state.VerdictPublished),
		attempt("13", "claude-code", "opus", 1, 40*time.Minute, state.VerdictPublished),
		// A dogfood issue that shares the host: read, reported, not counted.
		attempt("900", "claude-code", "opus", 1, time.Minute, state.VerdictPublished),
	)
	write(codexDir,
		unpriced("21", "codex-exec", "", 1, 50*time.Minute, state.VerdictPublished),
		unpriced("22", "codex-exec", "", 1, 60*time.Minute, "incomplete"),
	)
	checkResults := func(cs bench.Case, pass bool) []bench.CheckResult {
		results := make([]bench.CheckResult, 0, len(cs.Checks))
		for i, check := range cs.Checks {
			results = append(results, bench.CheckResult{ID: check.ID, Passed: pass || i > 0})
		}
		return results
	}
	const checked = "9999999999999999999999999999999999999999"

	m := bench.Manifest{
		Cohort:  cohort.Version,
		Session: "test-session",
		ExpectedCells: []bench.Cell{
			{Agent: "claude-code", Model: "opus"},
			{Agent: "codex-exec"},
		},
		Runs: []bench.Run{
			{Case: one.ID, Agent: "claude-code", Model: "opus", Repo: "acme/claude-one", Issue: "11",
				CaseDefinitionSHA256: one.DefinitionSHA256, Base: one.BaseCommit, StateDir: claudeDir,
				CheckedCommit: checked, CheckResults: checkResults(one, true)},
			{Case: two.ID, Agent: "claude-code", Model: "opus", Repo: "acme/claude-two", Issue: "12",
				CaseDefinitionSHA256: two.DefinitionSHA256, Base: two.BaseCommit, StateDir: claudeDir,
				CheckedCommit: checked, CheckResults: checkResults(two, true)},
			{Case: three.ID, Agent: "claude-code", Model: "opus", Repo: "acme/claude-three", Issue: "13",
				CaseDefinitionSHA256: three.DefinitionSHA256, Base: three.BaseCommit, StateDir: claudeDir},
			{Case: one.ID, Agent: "codex-exec", Repo: "acme/codex-one", Issue: "21",
				CaseDefinitionSHA256: one.DefinitionSHA256, Base: one.BaseCommit, StateDir: codexDir,
				CheckedCommit: checked, CheckResults: checkResults(one, false)},
			{Case: two.ID, Agent: "codex-exec", Repo: "acme/codex-two", Issue: "22",
				CaseDefinitionSHA256: two.DefinitionSHA256, Base: two.BaseCommit, StateDir: codexDir},
			{Case: three.ID, Agent: "codex-exec", Repo: "acme/codex-three", Issue: "23",
				CaseDefinitionSHA256: three.DefinitionSHA256, Base: three.BaseCommit, StateDir: codexDir},
		}}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath = filepath.Join(root, "session.json")
	if err := os.WriteFile(manifestPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return manifestPath
}

func TestTheTableCarriesEveryDenominator(t *testing.T) {
	path := session(t)
	var out, errs strings.Builder
	if code := run([]string{path}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errs.String())
	}
	text := out.String()

	cohort, err := bench.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		phrase string
		why    string
	}{
		{"cohort " + cohort.Version, "which cohort the numbers are about"},
		{"session test-session", "which sitting, so two printed reports cannot be confused"},
		{"declared cells (2)", "the matrix declared before dispatch, including cells with no runs"},
		{"matched cases: 2 of 3", "the denominator, beside the cohort's size"},
		{cohort.Cases[2].ID, "the excluded case is named, not merely absent"},
		{"excluded: not run by every declared cell", "and why it was excluded"},
		{"published", "the rate's column"},
		{"2 (100%)", "the count beside the rate, claude-code's two of two"},
		{"1 (50%)", "and codex-exec's one of two"},
		{"passed", "the combined publish-and-check rate"},
		{"0 (0%)", "the published codex case whose cohort check failed"},
		{"check failures", "the failed check IDs behind the combined result"},
		{"unpriced", "the column without which a $0.00 cost reads as cheaper"},
		{"claude-code (opus)", "the cell with a model"},
		{"codex-exec (default model)", "and the cell whose block named none — named, not blank"},
		{"attempts read 7, joined 6", "what was read against what was counted"},
		{"to no run in this session", "the dogfood attempt, stated"},
		{"runs with no attempt record (1)", "the declared run nothing backs"},
		{"per matched case", "the side-by-side that makes the pairing visible"},
		{"incomplete", "the §9.7 verdict a lost case ended on"},
	} {
		if !strings.Contains(text, want.phrase) {
			t.Errorf("the table does not carry %q — %s\n\n%s", want.phrase, want.why, text)
		}
	}

	// The excluded case's numbers are nowhere in the comparison: 40m is its
	// duration, and it must not appear in either cell's row.
	if strings.Contains(text, "40m0s") {
		t.Errorf("the table shows 40m0s, which belongs to the unmatched case:\n\n%s", text)
	}
}

func TestJSONIsTheSameReport(t *testing.T) {
	path := session(t)
	var out, errs strings.Builder
	if code := run([]string{"--json", path}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errs.String())
	}
	var rep bench.Report
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatalf("decoding --json: %v\n%s", err, out.String())
	}
	if len(rep.MatchedCases) != 2 || rep.CohortCases != 3 {
		t.Errorf("matched %v of %d, want two of three", rep.MatchedCases, rep.CohortCases)
	}
	if rep.UnjoinedAttempts != 1 || len(rep.RunsWithoutAttempts) != 1 {
		t.Errorf("unjoined %d, unbacked runs %d, want one of each",
			rep.UnjoinedAttempts, len(rep.RunsWithoutAttempts))
	}
	if len(rep.ExpectedCells) != 2 {
		t.Errorf("expected cells = %v, want the two cells declared before dispatch", rep.ExpectedCells)
	}
	if len(rep.Cells) != 2 {
		t.Fatalf("cells = %d, want two", len(rep.Cells))
	}
	if rep.Cells[1].UnpricedAttempts != 2 || rep.Cells[1].CostUSD != 0 {
		t.Errorf("the adapter that quotes no price = %+v, want $0 with both attempts counted unpriced",
			rep.Cells[1])
	}
}

// A state directory no daemon has written is a note and an unbacked run, not a
// refusal: a session is read while its last cell is still working.
func TestAMissingAttemptLogIsReportedRatherThanFatal(t *testing.T) {
	cohort, err := bench.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	m := bench.Manifest{Cohort: cohort.Version, Session: "empty",
		ExpectedCells: []bench.Cell{{Agent: "claude-code"}, {Agent: "codex-exec"}}, Runs: []bench.Run{{
			Case: cohort.Cases[0].ID, Agent: "claude-code", Repo: "acme/canary", Issue: "1",
			CaseDefinitionSHA256: cohort.Cases[0].DefinitionSHA256,
			Base:                 cohort.Cases[0].BaseCommit,
			StateDir:             filepath.Join(root, "never-written"),
		}}}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errs strings.Builder
	if code := run([]string{path}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errs.String())
	}
	if !strings.Contains(errs.String(), "holds no attempt log yet") {
		t.Errorf("stderr = %q, want the note about the unwritten directory", errs.String())
	}
	if !strings.Contains(out.String(), "nothing to compare") {
		t.Errorf("stdout = %q, want it to say there is nothing to compare", out.String())
	}
	if !strings.Contains(out.String(), "declared cells with no run (1)") ||
		!strings.Contains(out.String(), "codex-exec (default model)") {
		t.Errorf("stdout = %q, want the wholly omitted declared cell named", out.String())
	}
	// The note goes to stderr so `--json | jq` still parses.
	if strings.Contains(out.String(), "holds no attempt log") {
		t.Error("the note reached stdout, where a JSON consumer would choke on it")
	}
}

func TestArgumentHandling(t *testing.T) {
	path := session(t)
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no manifest", nil, 2},
		{"two manifests", []string{path, path}, 2},
		{"an unknown flag", []string{"--depth", "2", path}, 2},
		{"help", []string{"-h"}, 0},
		{"a manifest that is not there", []string{filepath.Join(t.TempDir(), "absent.json")}, 1},
		{"a cohort directory that is not there", []string{"--cohort", filepath.Join(t.TempDir(), "nope"), path}, 1},
		{"the manifest read as a cohort", []string{"--cohort", filepath.Dir(path), path}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errs strings.Builder
			if code := run(tc.args, &out, &errs); code != tc.want {
				t.Errorf("exit %d, want %d (stderr: %s)", code, tc.want, errs.String())
			}
		})
	}
}
