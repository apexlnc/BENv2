// Command benchreport is #62's documented comparison: it reads a benchmark
// session's manifest, joins its runs to the checked-in cohort, and prints the
// matched-case publish-and-check readout grouped by adapter and model.
// docs/BENCH.md is the procedure that produces the manifest; this is the query
// at the end of it.
//
// **A command of its own, not a `ben` subcommand.** `cmd/ben` is one package, so
// a benchmark reader living in it would be linked into the daemon and reachable
// from dispatch — and "no runtime decision depends on benchmark telemetry" is a
// property worth making structural rather than promising. It is the only importer
// of internal/bench, and internal/arch/bench_test.go fails if the daemon ever
// becomes another (#62).
//
// It reads files and nothing else: no tracker, no network, no agent. Safe to run
// while a benchmark session is still going, on the same terms as `ben status`.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/bench"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

const usage = `benchreport — compare runner adapters over a fixed benchmark cohort (#62)

Usage:
  benchreport [--json] [--cohort dir] <session-manifest.json>

  --cohort dir   read the cohort from a directory instead of the one compiled in
  --json         emit the report as JSON instead of a table

The manifest declares the adapter/model matrix before dispatch, then records each
run's isolated canary, case-definition fingerprint, base and state directory and,
after publish, the immutable head and per-check results. See docs/BENCH.md.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run carries argument handling and exit codes so the tests can exercise them
// without exec'ing a built binary, on cmd/ben's terms. Exit codes: 0 success,
// 1 operational failure, 2 usage error.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("benchreport", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	cohortDir := fs.String("cohort", "", "read the cohort from this directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2 // the FlagSet already reported the flag error
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "benchreport: one session manifest, got %d\n\n%s", fs.NArg(), usage)
		return 2
	}

	cohort, err := loadCohort(*cohortDir)
	if err != nil {
		fmt.Fprintf(stderr, "benchreport: %v\n", err)
		return 1
	}
	manifest, err := bench.LoadManifest(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "benchreport: %v\n", err)
		return 1
	}

	logs, err := readLogs(manifest, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "benchreport: %v\n", err)
		return 1
	}
	report, err := bench.Compare(cohort, manifest, logs)
	if err != nil {
		fmt.Fprintf(stderr, "benchreport: %v\n", err)
		return 1
	}

	if *asJSON {
		body, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "benchreport: rendering JSON: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(body))
		return 0
	}
	writeReport(stdout, report)
	return 0
}

func loadCohort(dir string) (*bench.Cohort, error) {
	if dir == "" {
		return bench.Embedded()
	}
	return bench.LoadDir(dir)
}

// readLogs reads the attempt-outcome log of every state directory the manifest
// names, once each.
//
// A directory with no log yet is a note on stderr and an empty read, not a
// refusal: a session is often read while its last cell is still working, and
// Compare already reports the runs nothing backs (Report.RunsWithoutAttempts).
// stdout stays clean either way, so `--json | jq` is unaffected.
func readLogs(m *bench.Manifest, stderr io.Writer) (map[string][]state.Attempt, error) {
	logs := map[string][]state.Attempt{}
	for _, dir := range stateDirs(m) {
		attempts, _, err := state.At(dir).ReadAttempts().Tail(0)
		switch {
		case errors.Is(err, state.ErrNoState):
			fmt.Fprintf(stderr, "benchreport: %s holds no attempt log yet; its runs are reported as unbacked\n", dir)
			continue
		case err != nil:
			return nil, fmt.Errorf("reading %s: %w", dir, err)
		}
		logs[dir] = attempts
	}
	return logs, nil
}

func stateDirs(m *bench.Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range m.Runs {
		if !seen[r.StateDir] {
			seen[r.StateDir] = true
			out = append(out, r.StateDir)
		}
	}
	sort.Strings(out)
	return out
}

// The rendering. Every rate is printed beside the counts it came from, and the
// three coverage lines at the end are not optional detail: a benchmark table
// whose denominators and drops are elsewhere is read as though it covered
// everything.
func writeReport(w io.Writer, r bench.Report) {
	fmt.Fprintf(w, "benchmark comparison — cohort %s (%s), session %s\n\n", r.Cohort, r.SourceRepo, r.Session)
	expected := make([]string, 0, len(r.ExpectedCells))
	for _, cell := range r.ExpectedCells {
		expected = append(expected, cell.Label())
	}
	fmt.Fprintf(w, "declared cells (%d): %s\n", len(expected), strings.Join(expected, ", "))

	fmt.Fprintf(w, "matched cases: %d of %d\n", len(r.MatchedCases), r.CohortCases)
	cases := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, cov := range r.Coverage {
		ran := "no cell"
		if len(cov.Cells) > 0 {
			ran = strings.Join(cov.Cells, ", ")
		}
		note := ""
		if !cov.Matched {
			note = "— excluded: not run by every declared cell"
		}
		fmt.Fprintf(cases, "  %s\t(%s)\t%s\t%s\n", cov.Case, cov.Tier, ran, note)
	}
	cases.Flush() //nolint:errcheck // a short write to stdout is not worth a second error path
	fmt.Fprintln(w)

	if len(r.Cells) == 0 {
		fmt.Fprintln(w, "no cell in this session has an attempt record, so there is nothing to compare.")
		writeCoverage(w, r)
		writeCheckMeaning(w)
		return
	}

	cells := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(cells, "cell\tcases\tpublished\tpassed\tcheck-fail\tunchecked\truns\tattempts\tran\tp50\tp95\tinput\toutput\tcost\tunpriced")
	for _, c := range r.Cells {
		fmt.Fprintf(cells, "%s\t%d\t%d (%s)\t%d (%s)\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%d\n",
			c.Label, c.Cases, c.PublishedCases, pct(c.PublishRate()),
			c.PassedCases, pct(c.PassRate()), c.CheckFailedCases, c.UncheckedPublishedCases,
			c.Runs, c.Attempts, c.Ran,
			span(c.P50), span(c.P95), count(c.InputTokens), count(c.OutputTokens), usd(c.CostUSD),
			c.UnpricedAttempts)
	}
	cells.Flush() //nolint:errcheck // as above
	fmt.Fprintln(w)

	// Matched cases only, side by side: the pairing is the comparison, and two
	// averages hide which case each cell lost.
	if len(r.MatchedCases) > 0 {
		fmt.Fprintln(w, "per matched case — published / checks / passed / attempts / wall clock / last verdict")
		perCase := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
		header := "case"
		for _, c := range r.Cells {
			header += "\t" + c.Label
		}
		fmt.Fprintln(perCase, header)
		for _, id := range r.MatchedCases {
			row := id
			for _, c := range r.Cells {
				// By case rather than by position: the row and the cell's results
				// are both in matched order today, and a table that silently
				// reads one cell's number under another case is the worst
				// possible way for that to stop being true.
				res, ok := resultFor(c, id)
				if !ok {
					row += "\t—"
					continue
				}
				row += fmt.Sprintf("\t%s / %s / %s / %d / %s / %s",
					yesNo(res.Published), res.Checks, yesNo(res.Passed),
					res.Attempts, span(res.Duration), verdictOf(res))
			}
			fmt.Fprintln(perCase, row)
		}
		perCase.Flush() //nolint:errcheck // as above
		fmt.Fprintln(w)
	}

	for _, c := range r.Cells {
		fmt.Fprintf(w, "%s — failures: %s; verdicts: %s; check failures: %s\n",
			c.Label, joinCounts(c.Failures), joinCounts(c.Verdicts), joinCounts(c.CheckFailures))
	}
	fmt.Fprintln(w)
	writeCoverage(w, r)

	writeCheckMeaning(w)
}

func writeCheckMeaning(w io.Writer) {
	fmt.Fprintln(w, "\npassed means BEN published the run and every cohort check passed at its recorded immutable head; unchecked publishes never pass.")
}

func writeCoverage(w io.Writer, r bench.Report) {
	fmt.Fprintf(w, "attempts read %d, joined %d", r.AttemptsRead, r.AttemptsJoined)
	if n := r.UnjoinedAttempts; n == 1 {
		fmt.Fprint(w, "; 1 record belongs to no run in this session and was not counted")
	} else if n > 1 {
		fmt.Fprintf(w, "; %d records belong to no run in this session and were not counted", n)
	}
	fmt.Fprintln(w)
	if len(r.CellsWithoutRuns) > 0 {
		fmt.Fprintf(w, "declared cells with no run (%d):\n", len(r.CellsWithoutRuns))
		for _, cell := range r.CellsWithoutRuns {
			fmt.Fprintf(w, "  %s\n", cell.Label())
		}
	}
	if len(r.RunsWithoutAttempts) == 0 {
		return
	}
	fmt.Fprintf(w, "runs with no attempt record (%d):\n", len(r.RunsWithoutAttempts))
	for _, ref := range r.RunsWithoutAttempts {
		fmt.Fprintf(w, "  %s  %s  %s#%s\n", ref.Case, ref.Cell, ref.Repo, ref.Issue)
	}
}

func resultFor(c bench.CellResult, caseID string) (bench.CaseResult, bool) {
	for _, res := range c.CaseResults {
		if res.Case == caseID {
			return res, true
		}
	}
	return bench.CaseResult{}, false
}

func pct(rate float64) string { return fmt.Sprintf("%.0f%%", rate*100) }

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func verdictOf(r bench.CaseResult) string {
	switch {
	case r.Verdict != "":
		return r.Verdict
	case r.FailureReason != "":
		return r.FailureReason
	default:
		return "—"
	}
}

// span rounds to the second: an attempt is minutes long, and nanoseconds in a
// comparison table are noise.
func span(d time.Duration) string {
	if d == 0 {
		return "—"
	}
	return d.Round(time.Second).String()
}

func usd(v float64) string { return fmt.Sprintf("$%.2f", v) }

// count abbreviates a token total, on `ben status`'s terms.
func count(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func joinCounts(cs []state.Count) string {
	if len(cs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s %d", c.Name, c.Count))
	}
	return strings.Join(parts, ", ")
}
