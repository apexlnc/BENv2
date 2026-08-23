package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// One benchmark session's declared matrix and record of what it dispatched.
//
// The cohort says what the tasks are; this says which canary issue carried which
// task under which adapter, and where the attempt-outcome records for it landed.
// It exists because nothing else can answer that: an attempt record names an
// issue by its tracker identifier (state.Attempt.Issue), and no issue number
// carries the case it was created from.
//
// **The matrix is declared first and runs are written as the session runs, not
// afterwards.** Dispatch fields are observed facts — including Run.Base, which
// is what the canary's default branch actually pointed at when the workspace was
// cloned from it, and the case-definition fingerprint copied at that moment.
// Check evidence is appended after a published run at the exact head it names.
// Compare refuses a session whose definition or recorded base is not the case's
// current pin, so "the inputs were held constant" is a checked claim rather than
// a remembered one.

// Manifest is one session's declared cells and observed runs.
type Manifest struct {
	// Cohort is the version the session ran, and Compare refuses to read it
	// against a different one.
	Cohort string `json:"cohort"`
	// Session is the operator's label for this sitting. It is carried into the
	// report so two printed comparisons cannot be confused for each other.
	Session string `json:"session"`
	Notes   string `json:"notes,omitempty"`
	// ExpectedCells is the matrix declared before anything is dispatched. It is
	// separate from Runs so omitting every run for one adapter/model cannot make
	// that cell disappear from the denominator.
	ExpectedCells []Cell `json:"cells"`
	Runs          []Run  `json:"runs"`
}

// Run is one execution in the matrix: one case, under one adapter and model, in
// one isolated canary repository and issue.
type Run struct {
	Case string `json:"case"`
	// CaseDefinitionSHA256 is copied from the cohort case at dispatch. Compare
	// requires it to equal the source repository and definition it is asked to
	// interpret, so keeping a cohort version and case ID cannot rewrite history.
	CaseDefinitionSHA256 string `json:"case_definition_sha256"`
	// Agent is the `agent.kind` that ran it, and Model the model that kind's
	// block selected. Model is empty when the block named none — an answer, not a
	// gap (core.AgentDescriptor), and the value the attempt record carries for
	// the ordinary default-model configuration.
	Agent string `json:"agent"`
	Model string `json:"model"`
	// Repo is the canary this cell ran against, `owner/name` — a mirror of the
	// cohort's source repository whose default branch was pinned at Base. GitHub
	// repository identity is case-insensitive; validation folds this value for
	// queue and issue uniqueness while retaining the observed spelling.
	Repo string `json:"repo"`
	// Issue is the canary issue's tracker identifier, as
	// state.Attempt.Issue spells it (for GitHub, the number as a string).
	Issue string `json:"issue"`
	// Base is the commit the canary's default branch carried at dispatch. See the
	// note above: this is the observation, and the cohort holds the requirement.
	Base string `json:"base"`
	// StateDir is the §10.3 state directory whose attempts.jsonl holds this run's
	// records — `ben status --json <workflow> | jq -r .state_dir`. It must be an
	// absolute, lexically canonical path so one log has one join identity.
	//
	// Part of the join and not decoration: an issue identifier is unique within a
	// tracker scope, so (StateDir, Issue) is what identifies a run's records, and
	// a session that pointed two workflows at one directory could not be read at
	// all.
	StateDir string `json:"state_dir"`
	// CheckedCommit is the immutable published head on which CheckResults were
	// run. Both fields are absent until a published run has been checked; once
	// either is present the other is required.
	CheckedCommit string        `json:"checked_commit,omitempty"`
	CheckResults  []CheckResult `json:"check_results,omitempty"`
}

// CheckResult is one cohort check observed at Run.CheckedCommit. False is a
// result, not a missing value; absence is represented by no CheckResults at all.
type CheckResult struct {
	ID     string `json:"id"`
	Passed bool   `json:"passed"`
}

// Cell is the pair a comparison groups by.
type Cell struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
}

// Cell is this run's cell.
func (r Run) Cell() Cell { return Cell{Agent: r.Agent, Model: r.Model} }

// Label renders a cell for a human. An empty model is named rather than left
// blank: it is the ordinary configuration, and a blank column reads as missing
// data (state.Attempt.Model).
func (c Cell) Label() string {
	if c.Model == "" {
		return c.Agent + " (default model)"
	}
	return c.Agent + " (" + c.Model + ")"
}

// The refusals a session record can earn.
var (
	// ErrManifestSchema is a manifest file that is not one.
	ErrManifestSchema = errors.New("bench: the manifest file is not a session manifest")
	// ErrSharedIssue is two runs on one canary issue.
	//
	// This is the experiment's central prohibition, so it is a load refusal
	// rather than a note in a procedure: two cells on one issue means two agents
	// pushing one branch, two pull requests, and a claim only one of them can
	// hold (SPEC §9.3, §10.1). A manifest recording it is either a mistake to fix
	// or a session whose numbers are about a race.
	ErrSharedIssue = errors.New("bench: two runs share one canary issue")
	// ErrSharedQueue is two runs in one canary repository. BEN is a long-lived
	// queue worker, so the daemon intended for one run can claim another queued
	// issue in the repository. One repository per run is the isolation boundary.
	ErrSharedQueue = errors.New("bench: two runs share one canary repository queue")
	// ErrAmbiguousJoin is two runs whose (state dir, issue) pair is the same, so
	// an attempt record in that directory cannot be attributed to one of them.
	ErrAmbiguousJoin = errors.New("bench: two runs cannot be told apart in one state directory")
	// ErrNonCanonicalStateDir is a relative or lexically aliased state directory.
	// Join identity must have one spelling or the same attempts can be read and
	// attributed twice through paths such as /x/state and /x/state/.
	ErrNonCanonicalStateDir = errors.New("bench: state directory is not an absolute canonical path")
	// ErrDuplicateCell is a matrix that names one adapter/model pair twice.
	ErrDuplicateCell = errors.New("bench: duplicate adapter/model cell")
	// ErrUndeclaredCell is a run whose adapter/model pair is absent from the
	// matrix declared before dispatch.
	ErrUndeclaredCell = errors.New("bench: run belongs to an undeclared adapter/model cell")
	// ErrDuplicateRun is more than one run for a case in one declared cell. The
	// v1 comparison has no predeclared trial count or balancing rule, so accepting
	// repeats would let whichever cell was rerun more often win a case by "any
	// pass" selection.
	ErrDuplicateRun = errors.New("bench: duplicate case run for adapter/model cell")
)

// ParseManifest decodes and self-checks a session manifest: the checks that need
// nothing but the manifest itself. The ones that need a cohort — is this case
// real, was its base held — are Compare's, where the two meet.
func ParseManifest(body []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrManifestSchema, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("more than one JSON value")
		}
		return nil, fmt.Errorf("%w: trailing content: %w", ErrManifestSchema, err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadManifest reads a session manifest from disk.
func LoadManifest(path string) (*Manifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrManifestSchema, path, err)
	}
	return ParseManifest(body)
}

// Cells are the cells the manifest declared before dispatch, in stable order.
func (m *Manifest) Cells() []Cell {
	out := append([]Cell(nil), m.ExpectedCells...)
	sortCells(out)
	return out
}

func (m *Manifest) validate() error {
	if strings.TrimSpace(m.Cohort) == "" {
		return fmt.Errorf("%w: cohort — the version this session ran", ErrMissingField)
	}
	if strings.TrimSpace(m.Session) == "" {
		return fmt.Errorf("%w: session — a label for this sitting", ErrMissingField)
	}
	if len(m.ExpectedCells) < 2 {
		return fmt.Errorf("%w: session %s declares %d cells; a comparison needs at least two",
			ErrMissingField, m.Session, len(m.ExpectedCells))
	}

	declared := map[Cell]bool{}
	for i, cell := range m.ExpectedCells {
		if strings.TrimSpace(cell.Agent) == "" {
			return fmt.Errorf("%w: cell %d: agent", ErrMissingField, i+1)
		}
		if declared[cell] {
			return fmt.Errorf("%w: %s", ErrDuplicateCell, cell.Label())
		}
		declared[cell] = true
	}

	type issueKey struct{ repo, issue string }
	type joinKey struct{ dir, issue string }
	type runKey struct {
		cell   Cell
		caseID string
	}
	issues := map[issueKey]Run{}
	repositories := map[string]Run{}
	joins := map[joinKey]Run{}
	runs := map[runKey]Run{}
	for i, r := range m.Runs {
		if err := r.validate(i); err != nil {
			return err
		}
		if !declared[r.Cell()] {
			return fmt.Errorf("%w: run %d (case %s) names %s",
				ErrUndeclaredCell, i+1, r.Case, r.Cell().Label())
		}
		rk := runKey{cell: r.Cell(), caseID: r.Case}
		if first, ok := runs[rk]; ok {
			return fmt.Errorf("%w: case %s has %s runs on %s#%s and %s#%s",
				ErrDuplicateRun, r.Case, r.Cell().Label(),
				first.Repo, first.Issue, r.Repo, r.Issue)
		}
		runs[rk] = r
		// GitHub owner and repository names are case-insensitive. The spelling is
		// retained as observed, but queue and issue identity use GitHub's identity.
		repoKey := strings.ToLower(r.Repo)
		ik := issueKey{repoKey, r.Issue}
		if first, ok := issues[ik]; ok {
			return fmt.Errorf("%w: %s#%s carries both %s (case %s) and %s (case %s)",
				ErrSharedIssue, r.Repo, r.Issue, first.Cell().Label(), first.Case, r.Cell().Label(), r.Case)
		}
		issues[ik] = r
		if first, ok := repositories[repoKey]; ok {
			return fmt.Errorf("%w: %s and %s carry #%s (%s, case %s) and #%s (%s, case %s)",
				ErrSharedQueue, first.Repo, r.Repo,
				first.Issue, first.Cell().Label(), first.Case,
				r.Issue, r.Cell().Label(), r.Case)
		}
		repositories[repoKey] = r
		jk := joinKey{filepath.Clean(r.StateDir), r.Issue}
		if first, ok := joins[jk]; ok {
			return fmt.Errorf("%w: issue %s in %s is claimed by %s (%s) and %s (%s)",
				ErrAmbiguousJoin, r.Issue, r.StateDir,
				first.Repo, first.Cell().Label(), r.Repo, r.Cell().Label())
		}
		joins[jk] = r
	}
	return nil
}

func (r Run) validate(i int) error {
	where := fmt.Sprintf("run %d", i+1)
	if r.Case != "" {
		where = fmt.Sprintf("run %d (case %s)", i+1, r.Case)
	}
	for _, f := range []struct {
		name, value string
	}{
		// Model is deliberately absent: empty is a value here.
		{"case", r.Case},
		{"case_definition_sha256", r.CaseDefinitionSHA256},
		{"agent", r.Agent},
		{"issue", r.Issue},
		{"state_dir", r.StateDir},
	} {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("%w: %s: %s", ErrMissingField, where, f.name)
		}
	}
	if !repoName.MatchString(r.Repo) {
		return fmt.Errorf("%w: %s: repo %q is not owner/name", ErrMissingField, where, r.Repo)
	}
	if !sha256Hex.MatchString(r.CaseDefinitionSHA256) {
		return fmt.Errorf("%w: %s: case_definition_sha256 %q is not a lowercase sha-256 hex digest",
			ErrMissingField, where, r.CaseDefinitionSHA256)
	}
	cleanStateDir := filepath.Clean(r.StateDir)
	if !filepath.IsAbs(r.StateDir) || cleanStateDir != r.StateDir {
		return fmt.Errorf("%w: %s: state_dir %q (canonical spelling %q)",
			ErrNonCanonicalStateDir, where, r.StateDir, cleanStateDir)
	}
	if strings.ContainsAny(r.Issue, " \t\n") {
		return fmt.Errorf("%w: %s: issue %q is not a tracker identifier", ErrMissingField, where, r.Issue)
	}
	if !hexRevision.MatchString(r.Base) {
		return fmt.Errorf("%w: %s: base %q — record what the canary's default branch pointed at, "+
			"not the branch that pointed at it", ErrMutableRevision, where, r.Base)
	}
	if r.CheckedCommit == "" && len(r.CheckResults) > 0 {
		return fmt.Errorf("%w: %s: checked_commit", ErrMissingField, where)
	}
	if r.CheckedCommit != "" {
		if !hexRevision.MatchString(r.CheckedCommit) {
			return fmt.Errorf("%w: %s: checked_commit %q", ErrMutableRevision, where, r.CheckedCommit)
		}
		if len(r.CheckResults) == 0 {
			return fmt.Errorf("%w: %s: check_results", ErrMissingField, where)
		}
	}
	seenChecks := map[string]bool{}
	for _, result := range r.CheckResults {
		if !idPattern.MatchString(result.ID) {
			return fmt.Errorf("%w: %s: check result id %q must match %s",
				ErrMissingField, where, result.ID, idPattern)
		}
		if seenChecks[result.ID] {
			return fmt.Errorf("%w: %s records check %q twice", ErrDuplicateID, where, result.ID)
		}
		seenChecks[result.ID] = true
	}
	return nil
}
