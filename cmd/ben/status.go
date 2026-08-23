package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

// `ben status` — the §10.3 state files, rendered (SPEC §11). Read-only, and it
// works while the daemon runs, which is the constraint the file format was built
// around rather than something this command arranges.
//
// It reads *files*, never the daemon. There is no IPC to a running BEN and there
// is not meant to be: §10.3 says GitHub is the dashboard and these are white-box
// state files beside it. So `ben status` works on a daemon that has wedged, on a
// daemon that has died, and on a host whose WORKFLOW.md no longer parses — which
// between them are most of the occasions anyone runs it.

// transitionTail is how much of the §9.11 log the text rendering shows. The
// count it was taken from is printed alongside it, so the cap is stated rather
// than left to look like the whole log; --json carries the same window.
const transitionTail = 10

// liveness is what a run-record file says about the daemon that wrote it.
//
// Three values because there are three situations and an operator does something
// different in each. They are also the reason this command reports rather than
// probes: asking the OS whether the recorded pid is alive would answer confidently
// and wrongly after a reboot has reissued it, which is the mistake core.RunEvidence
// exists to avoid (SPEC §9.10).
type liveness string

const (
	// livenessRunning — the heartbeat is current.
	livenessRunning liveness = "running"
	// livenessStopped — the daemon wrote a final record on its way out.
	livenessStopped liveness = "stopped"
	// livenessStale — no final record and no heartbeat. Killed, wedged, or its
	// host went down; nothing in the file can tell those apart, and claiming one
	// of them would be inventing a fact.
	livenessStale liveness = "stale"
)

// statusReport is `ben status --json`: a stable contract, whose fields may gain
// members and may not change or lose them.
//
// **Its own types, not the state dir's.** Re-using `state.Run` here was shorter
// and was wrong twice over. It published `continuation` — an opaque resume token
// whose meaning belongs to whichever adapter minted it — into an interface BEN
// then has to keep; and it made every field the *file* ever gains a field this
// contract ships, so the on-disk format could no longer change without changing
// a published API. A presentation type breaks both: what the daemon needs to
// write down and what an operator may be handed are different questions, and
// this is where the second one is answered.
//
// Nothing here is redacted at render time. The values that must not be published
// are absent from the type, so there is no path by which a future field reaches
// stdout because somebody forgot a rule (#47's lesson: do not enumerate leak
// routes).
type statusReport struct {
	Workflow string `json:"workflow"`
	StateDir string `json:"state_dir"`
	// Status is the liveness verdict — the one thing here that cannot be read
	// off a file and has to be computed against the clock at the moment of
	// reading.
	Status      liveness         `json:"status"`
	Daemon      daemonView       `json:"daemon"`
	Runs        []runView        `json:"runs"`
	Transitions []transitionView `json:"transitions"`
	// TransitionsTotal is how many entries the log holds, against the window in
	// Transitions. Present so a consumer can see that it is looking at a tail.
	TransitionsTotal int `json:"transitions_total"`
	// Attempts aggregates every attempt this state dir has recorded (#60), or is
	// absent when none has been. Unlike the two above it is not a window: the
	// whole log is the point, since a fraction of a screenful is not a fraction.
	//
	// state.Summary is used here as-is rather than restated, unlike runView. The
	// argument for a presentation type was that the file holds values an operator
	// may not be handed — the continuation token — and there is no such value in
	// this one: every field is a count, a sum or a duration, and the one string,
	// the model, was already put through `config effective`'s redactor before it
	// reached the file (config.AgentDescriptor).
	Attempts *state.Summary `json:"attempts,omitempty"`
}

type daemonView struct {
	ID          string    `json:"id"`
	Workflow    string    `json:"workflow"`
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"started_at"`
	WrittenAt   time.Time `json:"written_at"`
	HeartbeatMS int       `json:"heartbeat_ms"`
	Draining    bool      `json:"draining"`
	HeldClaims  int       `json:"held_claims"`
	Stopped     bool      `json:"stopped"`
}

type runView struct {
	Issue string `json:"issue"`
	// RunID is the publishable correlation handle: BEN mints it, it identifies
	// an attempt and nothing else, and it is already in every log line and in
	// BEN_RUN_ID. It is what `session_id` is deliberately not.
	RunID         string             `json:"run_id,omitempty"`
	State         string             `json:"state"`
	Attempt       int                `json:"attempt"`
	Turns         int                `json:"turns"`
	FailureReason core.FailureReason `json:"failure_reason,omitempty"`
	Branch        string             `json:"branch,omitempty"`
	UpdatedAt     time.Time          `json:"updated_at"`
	NextTimerAt   *time.Time         `json:"next_timer_at,omitempty"`
	NextTimer     string             `json:"next_timer,omitempty"`

	// Resuming reports *that* the next attempt carries a continuation token,
	// never what it is.
	//
	// The token and the session id are one string in both current adapters, and
	// §10.3 does put the session id on every log line — but a log goes to
	// journald under the supervisor's access control, and this goes to whatever
	// an operator pastes into an issue. They are not the same surface, and the
	// asymmetry is the point rather than an inconsistency: BEN cannot know what
	// a given provider's session identifier grants, and a stable API is a
	// promise to keep publishing it. `ben run`'s own state dir keeps the value,
	// at 0600, for the operator who needs it.
	Resuming bool `json:"resuming"`
}

type transitionView struct {
	TS            time.Time          `json:"ts"`
	Issue         string             `json:"issue"`
	From          string             `json:"from"`
	To            string             `json:"to"`
	Actor         string             `json:"actor"`
	Reason        string             `json:"reason"`
	RunID         string             `json:"run_id,omitempty"`
	FailureReason core.FailureReason `json:"failure_reason,omitempty"`
}

// runStatus is `ben status`. Exit codes follow the CLI's: 0 when the state was
// read and reported, 1 when it could not be read, 2 for a usage error.
//
// Reporting "no daemon has run here" is a *success*: it is an answer, arrived at
// from the absence of a file that a daemon creates before it does anything else.
// Only a file that is present and unreadable — a torn write, a foreign file, a
// permission fault — is a failure, because that is the case where the command
// genuinely does not know.
func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit the stable JSON contract instead of text")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2 // the FlagSet already reported the flag error to stderr
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(stderr, "ben: status takes at most one path argument; got %d: %s\n", fs.NArg(), strings.Join(fs.Args(), " "))
		for _, a := range fs.Args()[1:] {
			if strings.HasPrefix(a, "-") {
				fmt.Fprint(stderr, "(flags must come before the path: `ben status --json [path]`)\n")
				break
			}
		}
		return 2
	}
	path := "WORKFLOW.md"
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}

	// KeyFor reads no file: the key is a function of the path alone. That is what
	// lets this command work against a daemon whose WORKFLOW.md is currently
	// broken, which is the state an operator is most likely to be inspecting.
	key, err := config.KeyFor(path)
	if err != nil {
		fmt.Fprintf(stderr, "ben: %v\n", err)
		return 1
	}
	dir := state.For(key)

	report, err := readStatus(dir, key)
	switch {
	case errors.Is(err, state.ErrNoState):
		if *asJSON {
			// An answer, so it is emitted as one. A consumer scripting against
			// this must not have to parse stderr to learn that nothing is here.
			return emitJSON(stdout, stderr, statusReport{Workflow: key, StateDir: dir.Root(), Status: livenessStale})
		}
		fmt.Fprintf(stdout, "No BEN state for %s.\n", path)
		fmt.Fprintf(stdout, "Nothing has been written to %s, so no daemon has run this workflow on this host.\n", dir.Root())
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "ben: %v\n", err)
		return 1
	}

	if *asJSON {
		return emitJSON(stdout, stderr, report)
	}
	writeStatusText(stdout, path, report)
	return 0
}

func emitJSON(stdout, stderr io.Writer, r statusReport) int {
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "ben: rendering JSON: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(out))
	return 0
}

// readStatus reads both files and computes what only the reader can.
func readStatus(dir state.Dir, key string) (statusReport, error) {
	runs, err := dir.ReadRuns()
	if err != nil {
		return statusReport{}, err
	}

	r := statusReport{
		Workflow: key,
		StateDir: dir.Root(),
		Status:   livenessOf(runs.Daemon, time.Now()),
		Daemon: daemonView{
			ID: runs.Daemon.ID, Workflow: runs.Daemon.Workflow, PID: runs.Daemon.PID,
			StartedAt: runs.Daemon.StartedAt, WrittenAt: runs.Daemon.WrittenAt,
			HeartbeatMS: runs.Daemon.HeartbeatMS, Draining: runs.Daemon.Draining,
			HeldClaims: runs.Daemon.HeldClaims, Stopped: runs.Daemon.Stopped,
		},
		Runs: make([]runView, 0, len(runs.Records)),
	}
	for _, run := range runs.Records {
		r.Runs = append(r.Runs, runView{
			Issue: run.Issue, RunID: run.RunID, State: run.State,
			Attempt: run.Attempt, Turns: run.Turns, FailureReason: run.FailureReason,
			Branch: run.Branch, UpdatedAt: run.UpdatedAt,
			NextTimerAt: run.NextTimerAt, NextTimer: run.NextTimer,
			// The token itself stays in the file. See runView.Resuming.
			Resuming: run.Resuming(),
		})
	}

	// The log is created before the daemon does anything else, so a run-record
	// file with no log beside it means something removed one of the two. Report
	// it as an error rather than as an empty log: "no transitions" and "the
	// transitions are gone" are different, and only one of them is news.
	//
	// %v rather than %w, deliberately. A missing log reports ErrNoState, and
	// wrapping it would carry that sentinel up to a caller whose whole reading of
	// it is "no daemon has ever run here" — which is the conclusion this branch
	// exists to deny. The run records in hand are the evidence against it.
	//
	// Bounded, not Tail(0). The log is append-only and v1 never rotates it, so
	// asking for everything in order to count it makes `ben status` allocate a
	// slice the size of the daemon's whole history — on the one command an
	// operator reaches for when a host is already unwell. Tail reports the count
	// from the same streaming pass.
	window, total, err := dir.ReadTransitions().Tail(transitionTail)
	if err != nil {
		return statusReport{}, fmt.Errorf("the run records in %s are readable but the transition log beside them is not: %v", dir.Root(), err)
	}
	r.TransitionsTotal = total
	r.Transitions = make([]transitionView, 0, len(window))
	for _, t := range window {
		r.Transitions = append(r.Transitions, transitionView{
			TS: t.TS, Issue: t.Issue, From: t.From, To: t.To, Actor: t.Actor,
			Reason: t.Reason, RunID: t.RunID, FailureReason: t.FailureReason,
		})
	}

	// An absent attempt log is an answer here, not the error a missing
	// transition log is (#60). The transition log is created before the daemon
	// does anything else, so its absence beside run records means something
	// removed it; the attempt log a daemon writes to is created at the same
	// moment, but this command also reads state dirs written by a daemon that
	// predates the file entirely, and refusing to render a status because a
	// rolling upgrade has not reached this host yet is the wrong trade for
	// telemetry. The cost is that a *removed* attempt log reads as an empty one,
	// which is the direction that costs a number rather than a whole report.
	summary, err := dir.ReadAttempts().Summarize()
	switch {
	case errors.Is(err, state.ErrNoState):
	case err != nil:
		return statusReport{}, fmt.Errorf("the run records in %s are readable but the attempt-outcome log beside them is not: %v", dir.Root(), err)
	default:
		r.Attempts = &summary
	}
	return r, nil
}

// livenessOf reads the daemon's own account of itself against the clock.
func livenessOf(d state.Daemon, now time.Time) liveness {
	switch {
	case d.Stopped:
		return livenessStopped
	case d.Stale(now, staleGrace(d)):
		return livenessStale
	default:
		return livenessRunning
	}
}

// staleGrace is how long past a missed heartbeat this command waits before
// calling a daemon gone.
//
// Derived from the interval the *file* declares rather than from this binary's
// own constant: a `ben status` older or newer than the daemon it is reading must
// not measure it against a cadence it never promised. Three intervals absorbs a
// busy host — crying "dead" at one missed beat is how a status surface teaches
// people to ignore it — and the floor keeps a short interval from making the
// verdict hair-trigger.
func staleGrace(d state.Daemon) time.Duration {
	interval := time.Duration(d.HeartbeatMS) * time.Millisecond
	if g := 3 * interval; g > 5*time.Second {
		return g
	}
	return 5 * time.Second
}

func writeStatusText(w io.Writer, path string, r statusReport) {
	// Two columns, not three: one of the values here is a filesystem path, and a
	// third column would be padded out past it on every line.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "workflow\t%s\n", r.Workflow)
	fmt.Fprintf(tw, "config\t%s\n", path)
	fmt.Fprintf(tw, "state dir\t%s\n", r.StateDir)
	fmt.Fprintf(tw, "daemon\t%s\n", r.Daemon.ID)
	fmt.Fprintf(tw, "status\t%s\n", describeLiveness(r))
	if !r.Daemon.StartedAt.IsZero() {
		fmt.Fprintf(tw, "started\t%s\n", r.Daemon.StartedAt.UTC().Format(time.RFC3339))
	}
	if r.Daemon.Draining {
		// §9.8: dispatch has stopped and in-flight runs are being waited on.
		// From outside, a draining daemon and an idle one look identical.
		fmt.Fprint(tw, "\tdraining — dispatch stopped, waiting for in-flight runs to confirm termination\n")
	}
	if r.Daemon.HeldClaims > 0 {
		fmt.Fprintf(tw, "held claims\t%d — published, awaiting each issue's close (SPEC §9.8)\n", r.Daemon.HeldClaims)
	}
	tw.Flush() //nolint:errcheck // writing to the caller's stdout

	fmt.Fprintf(w, "\nRUNS (%d)\n", len(r.Runs))
	if len(r.Runs) == 0 {
		fmt.Fprintln(w, "  none")
	} else {
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ISSUE\tSTATE\tATTEMPT\tTURNS\tBRANCH\tNEXT\tLAST FAILURE\tRUN")
		now := time.Now()
		for _, run := range r.Runs {
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
				run.Issue, run.State, run.Attempt, run.Turns,
				orDash(run.Branch), describeTimer(run, now),
				orDash(string(run.FailureReason)), orDash(run.RunID))
		}
		tw.Flush() //nolint:errcheck // as above
	}

	fmt.Fprintf(w, "\nTRANSITIONS (%s)\n", describeTail(len(r.Transitions), r.TransitionsTotal))
	if len(r.Transitions) == 0 {
		fmt.Fprintln(w, "  none")
	} else {
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  TIME\tISSUE\tFROM\tTO\tREASON")
		for _, t := range r.Transitions {
			reason := t.Reason
			if t.FailureReason != "" {
				// The §7.3 verdict first: it is the closed taxonomy retry policy is
				// decided from, and the trigger text beside it is prose.
				reason = string(t.FailureReason) + " — " + reason
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
				t.TS.UTC().Format(time.RFC3339), t.Issue, t.From, t.To, reason)
		}
		tw.Flush() //nolint:errcheck // as above
	}

	writeAttemptsText(w, r.Attempts)
}

// writeAttemptsText renders the attempt aggregate (#60): the four questions a
// second week of dogfooding asks, and nothing else.
//
// Each line states its own denominator. A rate whose denominator is elsewhere on
// the screen is a rate somebody will quote without it — "40% first-attempt" over
// five tickets is not a finding — and the log is small for a long time before it
// is large.
func writeAttemptsText(w io.Writer, s *state.Summary) {
	if s == nil {
		fmt.Fprint(w, "\nATTEMPTS (none)\n  no attempt has finished on this host yet\n")
		return
	}
	fmt.Fprintf(w, "\nATTEMPTS (all %d, over %s)\n", s.Attempts, describeSpan(s.First, s.Last))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintf(tw, "  issues\t%d, %d of them published\n", s.Issues, s.PublishedIssues)
	if s.PublishedIssues > 0 {
		fmt.Fprintf(tw, "  first attempt\t%d of %d published issues landed on attempt 1 (%.1f attempts each on average)\n",
			s.FirstAttemptPublished, s.PublishedIssues,
			float64(s.AttemptsToPublish)/float64(s.PublishedIssues))
		fmt.Fprintf(tw, "  cost to publish\t%s per published issue, %s in total\n",
			usd(s.CostOfPublishedIssues/float64(s.PublishedIssues)), usd(s.CostOfPublishedIssues))
	}
	if s.Attempts > 0 {
		// Every attempt, including the ones that never launched: a Prepare that
		// hung for six minutes before failing cost exactly that, and it is the
		// kind of span a p95 is read to find (state.Summary.P50).
		fmt.Fprintf(tw, "  duration\tp50 %s · p95 %s, dispatch to outcome over all %s\n",
			s.P50.Round(time.Second), s.P95.Round(time.Second), plural(s.Attempts, "attempt"))
		if s.Ran < s.Attempts {
			fmt.Fprintf(tw, "\t%d of them never started a process\n", s.Attempts-s.Ran)
		}
	}
	fmt.Fprintf(tw, "  usage\t%s in · %s out · %s\n",
		count(s.InputTokens), count(s.OutputTokens), usd(s.CostUSD))
	if s.UnpricedAttempts > 0 {
		// Never folded into the total above. codex-exec reports no price at all
		// (core.Usage: "0 when the adapter cannot report cost"), so a sum that
		// stayed silent about these would read as though those runs were free.
		fmt.Fprintf(tw, "\t%d of them reported no cost, so the total is a lower bound\n", s.UnpricedAttempts)
	}
	if len(s.Failures) > 0 {
		fmt.Fprintf(tw, "  failures\t%s\n", joinCounts(s.Failures))
	}
	if len(s.Verdicts) > 0 {
		fmt.Fprintf(tw, "  verdicts\t%s\n", joinCounts(s.Verdicts))
	}
	tw.Flush() //nolint:errcheck // writing to the caller's stdout

	if len(s.Agents) == 0 {
		return
	}
	fmt.Fprintln(w, "\n  BY AGENT")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  AGENT\tMODEL\tATTEMPTS\tPUBLISHED\tP50\tCOST")
	for _, a := range s.Agents {
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%s\t%s\n",
			orDash(a.Agent), modelName(a.Model), a.Attempts, a.Published,
			a.P50.Round(time.Second), usd(a.CostUSD))
	}
	tw.Flush() //nolint:errcheck // as above
}

// describeSpan is the window the aggregate covers, in the terms an operator
// needs to judge whether the numbers mean anything yet.
func describeSpan(first, last time.Time) string {
	if first.IsZero() || last.IsZero() || !last.After(first) {
		return "a single moment"
	}
	return last.Sub(first).Round(time.Minute).String() +
		", ending " + last.UTC().Format(time.RFC3339)
}

func joinCounts(cs []state.Count) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s %d", c.Name, c.Count))
	}
	return strings.Join(parts, " · ")
}

// usd prints cents, because the numbers this reports are single-digit dollars
// for a long while and a rounded-to-the-dollar cost reads as zero.
func usd(v float64) string { return fmt.Sprintf("$%.2f", v) }

// modelName renders the model column. An empty model is the ordinary
// configuration that names none, so it gets the word `config effective` already
// uses for a value the adapter supplies (config.SourceAdapter) rather than a
// dash — a dash reads as "not recorded", and this row is a real cohort of runs
// whose model BEN cannot name (see state.Attempt.Model).
func modelName(model string) string {
	if model == "" {
		return "(adapter default)"
	}
	return model
}

// plural counts a noun. The aggregate is read most often when the log is small,
// which is exactly when "the 1 attempts" is on screen.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// count abbreviates token totals, which reach millions and are never read to
// the digit.
func count(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return strconv.FormatInt(n, 10)
	}
}

func describeLiveness(r statusReport) string {
	age := time.Since(r.Daemon.WrittenAt).Round(time.Second)
	switch r.Status {
	case livenessStopped:
		return fmt.Sprintf("stopped — it exited %s ago, at %s",
			age, r.Daemon.WrittenAt.UTC().Format(time.RFC3339))
	case livenessStale:
		// Deliberately three possibilities and no guess between them. Nothing in
		// the file distinguishes them, and naming one would be inventing it.
		return fmt.Sprintf("NOT RUNNING — pid %d last wrote %s ago (killed, wedged, or the host went down)",
			r.Daemon.PID, age)
	default:
		return fmt.Sprintf("running — pid %d, last heartbeat %s ago", r.Daemon.PID, age)
	}
}

// describeTimer renders the §9.6 wake-up a record is waiting on. It is the
// field that separates *stuck* from *waiting*, which is the question a run
// sitting in backoff actually raises.
func describeTimer(run runView, now time.Time) string {
	if run.NextTimerAt == nil {
		return "—"
	}
	kind := run.NextTimer
	if kind == "" {
		kind = "timer"
	}
	d := run.NextTimerAt.Sub(now).Round(time.Second)
	if d <= 0 {
		// Due, and the daemon has not acted on it yet — which on a live daemon
		// is a moment and on a stale one is the whole story.
		return kind + " due"
	}
	return kind + " in " + d.String()
}

func describeTail(shown, total int) string {
	if shown >= total {
		return "all " + strconv.Itoa(total)
	}
	return "last " + strconv.Itoa(shown) + " of " + strconv.Itoa(total)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
