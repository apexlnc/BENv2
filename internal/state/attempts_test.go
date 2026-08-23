package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// attempts.jsonl (#60): the record round-trips, and the aggregate answers the
// four questions the ticket names without inventing any of them.

func attempt(issue string, n int, at time.Time, took time.Duration) Attempt {
	return Attempt{
		Issue: issue, Attempt: n,
		Agent: "claude-code", Model: "opus",
		StartedAt: at, EndedAt: at.Add(took), Ran: true,
	}
}

func TestAttemptsRoundTrip(t *testing.T) {
	d := At(t.TempDir())
	w, err := d.AppendAttempts()
	if err != nil {
		t.Fatalf("AppendAttempts: %v", err)
	}
	at := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	want := []Attempt{
		{
			Issue: "#7", Attempt: 2, Turns: 1, RunID: "7-abc-1.0",
			Agent: "codex-exec", Model: "gpt-5",
			StartedAt: at, EndedAt: at.Add(4 * time.Minute), Ran: true,
			FailureReason: core.FailureStalled,
			InputTokens:   1200, OutputTokens: 90,
		},
		{
			Issue: "#7", Attempt: 3, RunID: "7-abc-2.0",
			Agent: "codex-exec", Model: "gpt-5",
			StartedAt: at.Add(5 * time.Minute), EndedAt: at.Add(9 * time.Minute), Ran: true,
			Verdict: VerdictPublished,
			CostUSD: 0.75,
		},
	}
	for _, a := range want {
		if err := w.Append(a); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, total, err := d.ReadAttempts().Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if total != len(want) {
		t.Fatalf("total = %d, want %d", total, len(want))
	}
	for i := range want {
		if !got[i].EndedAt.Equal(want[i].EndedAt) {
			t.Errorf("record %d ended_at = %s, want %s", i, got[i].EndedAt, want[i].EndedAt)
		}
		got[i].StartedAt, got[i].EndedAt = want[i].StartedAt, want[i].EndedAt
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A workflow that names no model is the ordinary one, and its runs are a cohort
// rather than an absence. The field is written empty rather than omitted, so a
// consumer can tell "the block named none" from "this record predates the
// field" — and so the default-model runs do not vanish from the comparison.
func TestADefaultModelRecordIsWrittenEmptyRatherThanOmitted(t *testing.T) {
	d := At(t.TempDir())
	w, err := d.AppendAttempts()
	if err != nil {
		t.Fatalf("AppendAttempts: %v", err)
	}
	at := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	if err := w.Append(Attempt{
		Issue: "#1", Attempt: 1, Agent: "codex-exec",
		StartedAt: at, EndedAt: at.Add(time.Minute), Ran: true,
		Verdict: VerdictPublished,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(d.AttemptsPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var line map[string]any
	if err := json.Unmarshal(raw[:len(raw)-1], &line); err != nil {
		t.Fatalf("the record does not parse: %v", err)
	}
	for _, key := range []string{"agent", "model"} {
		if _, ok := line[key]; !ok {
			t.Errorf("the record omits %q; a default-model run is then indistinguishable from a record written before the field existed:\n%s", key, raw)
		}
	}

	// And it aggregates as its own row rather than being dropped.
	s, err := d.ReadAttempts().Summarize()
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(s.Agents) != 1 || s.Agents[0].Agent != "codex-exec" || s.Agents[0].Model != "" {
		t.Errorf("agents = %+v, want one codex-exec row with no model named", s.Agents)
	}
	if s.Agents[0].Published != 1 {
		t.Errorf("the default-model row published %d, want 1", s.Agents[0].Published)
	}
}

func TestAbsentAttemptLogIsErrNoState(t *testing.T) {
	d := At(t.TempDir())
	if _, err := d.ReadAttempts().Summarize(); !errors.Is(err, ErrNoState) {
		t.Fatalf("Summarize on an absent log = %v, want ErrNoState", err)
	}
	if _, _, err := d.ReadAttempts().Tail(5); !errors.Is(err, ErrNoState) {
		t.Fatalf("Tail on an absent log = %v, want ErrNoState", err)
	}
}

// The two logs share one writer (jsonl.go), and this is the assertion that the
// sharing is real rather than a comment: a crash-torn tail in *this* file is
// repaired before the next append, or one crash costs the whole log.
func TestATornAttemptLogIsRepairedBeforeTheNextAppend(t *testing.T) {
	d := At(t.TempDir())
	w, err := d.AppendAttempts()
	if err != nil {
		t.Fatalf("AppendAttempts: %v", err)
	}
	at := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	if err := w.Append(attempt("#1", 1, at, time.Minute)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A daemon killed mid-append leaves a fragment with no newline.
	f, err := os.OpenFile(d.AttemptsPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"issue":"#2","attem`); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close() //nolint:errcheck // the write is what mattered

	w2, err := d.AppendAttempts()
	if err != nil {
		t.Fatalf("reopening after a torn write: %v", err)
	}
	if w2.Repaired == 0 {
		t.Error("Repaired = 0; the fragment was not discarded, so the next append glues onto it")
	}
	if err := w2.Append(attempt("#3", 1, at, time.Minute)); err != nil {
		t.Fatalf("Append after repair: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, _, err := d.ReadAttempts().Tail(0)
	if err != nil {
		t.Fatalf("Tail after repair: %v", err)
	}
	if len(got) != 2 || got[0].Issue != "#1" || got[1].Issue != "#3" {
		t.Errorf("log holds %+v, want the complete record and the one written after the repair", got)
	}
}

func TestSummarizeAnswersTheTicketsQuestions(t *testing.T) {
	d := At(t.TempDir())
	w, err := d.AppendAttempts()
	if err != nil {
		t.Fatalf("AppendAttempts: %v", err)
	}
	at := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)

	// #1 publishes first time. #2 crashes once, then publishes. #3 never gets a
	// process at all — a clone and then an after_create hook that each hung for
	// a long time before failing. #4 is a codex run that reports tokens but no
	// price.
	//
	// The spans are chosen so the two candidate denominators *disagree*. All six
	// are 2·4·6·10·20·30 minutes (p50 6m, p95 30m); the four that ran are
	// 2·4·6·20 (p50 4m, p95 20m). An earlier fixture had them coincide at 4m/20m,
	// so gating the percentiles on Ran passed it — which is what a slow prep
	// failure disappearing from p95 looks like from inside the suite.
	records := []Attempt{
		must(attempt("#1", 1, at, 2*time.Minute), func(a *Attempt) { a.Verdict = VerdictPublished; a.CostUSD = 1 }),
		must(attempt("#2", 1, at.Add(time.Hour), 6*time.Minute), func(a *Attempt) {
			a.FailureReason = core.FailureCrashed
			a.CostUSD = 0.5
		}),
		must(attempt("#2", 2, at.Add(2*time.Hour), 4*time.Minute), func(a *Attempt) { a.Verdict = VerdictPublished; a.CostUSD = 2 }),
		{
			Issue: "#3", Attempt: 1, Agent: "claude-code", Model: "opus",
			StartedAt: at, EndedAt: at.Add(10 * time.Minute), FailureReason: core.FailureLaunchError,
		},
		{
			Issue: "#3", Attempt: 2, Agent: "claude-code", Model: "opus",
			StartedAt: at, EndedAt: at.Add(30 * time.Minute), FailureReason: core.FailureLaunchError,
		},
		must(attempt("#4", 1, at.Add(3*time.Hour), 20*time.Minute), func(a *Attempt) {
			a.Agent, a.Model = "codex-exec", "gpt-5"
			// Spelled literally: this package names only the verdict it reasons
			// about (see VerdictPublished), and the rest are strings it carries.
			a.Verdict = "contradicted"
			a.InputTokens, a.OutputTokens = 8000, 400
		}),
	}
	for _, a := range records {
		if err := w.Append(a); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err := d.ReadAttempts().Summarize()
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if s.Attempts != 6 || s.Ran != 4 || s.Issues != 4 {
		t.Errorf("attempts/ran/issues = %d/%d/%d, want 6/4/4", s.Attempts, s.Ran, s.Issues)
	}
	// "What fraction of tickets land on attempt 1" — per issue, not per attempt.
	if s.PublishedIssues != 2 || s.FirstAttemptPublished != 1 {
		t.Errorf("published = %d issues, %d on attempt 1; want 2 and 1",
			s.PublishedIssues, s.FirstAttemptPublished)
	}
	// "What a completed ticket costs" — every attempt on the issues that
	// published, including the crash that preceded one of them.
	if s.AttemptsToPublish != 3 {
		t.Errorf("attempts to publish = %d, want 3 (one for #1, two for #2)", s.AttemptsToPublish)
	}
	if s.CostOfPublishedIssues != 3.5 {
		t.Errorf("cost of published issues = %v, want 3.5 including #2's failed attempt", s.CostOfPublishedIssues)
	}
	// "p50/p95 attempt duration" — dispatch to outcome over *every* attempt.
	// Over the four that ran it would be 4m/20m, so this fails if the
	// percentiles are gated on Ran and #3's two hung prepares drop out.
	if s.P50 != 6*time.Minute || s.P95 != 30*time.Minute {
		t.Errorf("p50/p95 = %s/%s, want 6m/30m over all six attempts (4m/20m is the four that ran)", s.P50, s.P95)
	}
	// "Which failure reason dominates."
	if len(s.Failures) != 2 || s.Failures[0] != (Count{Name: "launch_error", Count: 2}) {
		t.Errorf("failures = %+v, want launch_error first", s.Failures)
	}
	if s.UnpricedAttempts != 1 {
		t.Errorf("unpriced = %d, want 1 — the codex run reported tokens and no price", s.UnpricedAttempts)
	}
	if s.CostUSD != 3.5 || s.InputTokens != 8000 || s.OutputTokens != 400 {
		t.Errorf("totals = $%v / %d in / %d out", s.CostUSD, s.InputTokens, s.OutputTokens)
	}

	// The breakdown #62 needs.
	if len(s.Agents) != 2 {
		t.Fatalf("agents = %+v, want one row per (kind, model)", s.Agents)
	}
	if s.Agents[0].Agent != "claude-code" || s.Agents[0].Attempts != 5 || s.Agents[0].Published != 2 {
		t.Errorf("claude-code row = %+v, want 5 attempts and 2 published", s.Agents[0])
	}
	// Its own spans: 2·4·6·10·30 over all five, 2·4·6 over the three that ran —
	// 6m against 4m, so this row is gated on Ran independently of the one above.
	if s.Agents[0].P50 != 6*time.Minute {
		t.Errorf("claude-code p50 = %s, want 6m over all five of its attempts (4m is the three that ran)", s.Agents[0].P50)
	}
	if s.Agents[1].Agent != "codex-exec" || s.Agents[1].CostUSD != 0 {
		t.Errorf("codex-exec row = %+v, want it separate and unpriced", s.Agents[1])
	}
}

// An empty log is a summary of nothing rather than a division by zero.
func TestSummarizeAnEmptyLog(t *testing.T) {
	d := At(t.TempDir())
	w, err := d.AppendAttempts()
	if err != nil {
		t.Fatalf("AppendAttempts: %v", err)
	}
	w.Close() //nolint:errcheck // creating the file is the point

	s, err := d.ReadAttempts().Summarize()
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if s.Attempts != 0 || s.P50 != 0 || len(s.Failures) != 0 || len(s.Agents) != 0 {
		t.Errorf("summary of an empty log = %+v, want zeroes", s)
	}
}

// Nearest rank, so every percentile is a duration some attempt actually took.
func TestPercentileIsNearestRank(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       []time.Duration
		p50, p95 time.Duration
	}{
		{"empty", nil, 0, 0},
		{"one", []time.Duration{7 * time.Second}, 7 * time.Second, 7 * time.Second},
		{"ten", durations(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 5 * time.Second, 10 * time.Second},
		{"unsorted", durations(9, 1, 5), 5 * time.Second, 9 * time.Second},
		{"twenty", durations(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20), 10 * time.Second, 19 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Percentile(append([]time.Duration(nil), tc.in...), 50); got != tc.p50 {
				t.Errorf("p50 = %s, want %s", got, tc.p50)
			}
			if got := Percentile(append([]time.Duration(nil), tc.in...), 95); got != tc.p95 {
				t.Errorf("p95 = %s, want %s", got, tc.p95)
			}
		})
	}
}

// A hole in the log is loud rather than quietly skipped: an aggregate computed
// over a file it could only partly read is a number with no denominator.
func TestACorruptAttemptRecordIsLoud(t *testing.T) {
	d := At(t.TempDir())
	if err := d.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	body := "{\"issue\":\"#1\"}\nnot json at all\n{\"issue\":\"#2\"}\n"
	if err := os.WriteFile(d.AttemptsPath(), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := d.ReadAttempts().Summarize()
	if err == nil {
		t.Fatal("Summarize accepted a log with a hole in it")
	}
	if !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "attempt outcome") {
		t.Errorf("error = %v, want it to name the line and the record kind", err)
	}
}

// The state dir keeps its two logs apart: a reader of one must not be handed the
// other's records, which would parse into mostly-zero values rather than fail.
func TestTheTwoLogsAreSeparateFiles(t *testing.T) {
	d := At(t.TempDir())
	if filepath.Base(d.AttemptsPath()) == filepath.Base(d.TransitionsPath()) {
		t.Fatal("the attempt log and the transition log resolve to one path")
	}
	w, err := d.AppendAttempts()
	if err != nil {
		t.Fatalf("AppendAttempts: %v", err)
	}
	defer w.Close() //nolint:errcheck // the test is about the other file
	if _, err := os.Stat(d.TransitionsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("opening the attempt log created %s too", d.TransitionsPath())
	}
}

func durations(secs ...int) []time.Duration {
	out := make([]time.Duration, 0, len(secs))
	for _, s := range secs {
		out = append(out, time.Duration(s)*time.Second)
	}
	return out
}

func must(a Attempt, fn func(*Attempt)) Attempt {
	fn(&a)
	return a
}
