package bench

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A session manifest is the join, so its refusals are about the join being
// readable at all — and ErrSharedQueue is about the experiment's isolation
// boundary rather than about parsing.

func validManifest() Manifest {
	return Manifest{
		Cohort:        "v1",
		Session:       "2026-08-19-a",
		ExpectedCells: []Cell{{Agent: "claude-code", Model: "claude-opus-4"}, {Agent: "codex-exec"}},
		Runs: []Run{{
			Case:                 "case-one",
			CaseDefinitionSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Agent:                "claude-code",
			Model:                "claude-opus-4",
			Repo:                 "acme/canary-one",
			Issue:                "11",
			Base:                 "1111111111111111111111111111111111111111",
			StateDir:             "/var/lib/ben-bench/one/claude",
		}, {
			Case:                 "case-one",
			CaseDefinitionSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Agent:                "codex-exec",
			Model:                "",
			Repo:                 "acme/canary-two",
			Issue:                "12",
			Base:                 "1111111111111111111111111111111111111111",
			StateDir:             "/var/lib/ben-bench/one/codex",
		}},
	}
}

func parse(t *testing.T, m Manifest) (*Manifest, error) {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshalling the fixture: %v", err)
	}
	return ParseManifest(body)
}

func TestAValidManifestParses(t *testing.T) {
	m, err := parse(t, validManifest())
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	// An empty model is a value, not a gap: it is the ordinary configuration, and
	// it must survive to the report as its own cell (core.AgentDescriptor).
	cells := m.Cells()
	if len(cells) != 2 {
		t.Fatalf("Cells = %v, want two", cells)
	}
	if cells[0].Agent != "claude-code" || cells[1].Agent != "codex-exec" {
		t.Errorf("Cells = %v, want them ordered by adapter", cells)
	}
	if got := cells[1].Label(); got != "codex-exec (default model)" {
		t.Errorf("label of the model-less cell = %q, want it named rather than blank", got)
	}
	if got := cells[0].Label(); got != "claude-code (claude-opus-4)" {
		t.Errorf("label = %q", got)
	}
}

func TestParseManifestRefusesASessionItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Manifest)
		want   error
	}{
		{
			name:   "no cohort version",
			mutate: func(m *Manifest) { m.Cohort = "" },
			want:   ErrMissingField,
		},
		{
			name:   "no session label",
			mutate: func(m *Manifest) { m.Session = " " },
			want:   ErrMissingField,
		},
		{
			name:   "no declared cells",
			mutate: func(m *Manifest) { m.ExpectedCells = nil },
			want:   ErrMissingField,
		},
		{
			name:   "only one declared cell",
			mutate: func(m *Manifest) { m.ExpectedCells = m.ExpectedCells[:1] },
			want:   ErrMissingField,
		},
		{
			name:   "a declared cell with no agent",
			mutate: func(m *Manifest) { m.ExpectedCells[0].Agent = " " },
			want:   ErrMissingField,
		},
		{
			name: "a declared cell twice",
			mutate: func(m *Manifest) {
				m.ExpectedCells[1] = m.ExpectedCells[0]
			},
			want: ErrDuplicateCell,
		},
		{
			name: "a run for a cell the matrix did not declare",
			mutate: func(m *Manifest) {
				m.Runs[1].Agent = "another-agent"
			},
			want: ErrUndeclaredCell,
		},
		{
			name: "a case repeated in one cell",
			mutate: func(m *Manifest) {
				repeat := m.Runs[0]
				repeat.Repo = "acme/canary-repeat"
				repeat.Issue = "13"
				repeat.StateDir = "/var/lib/ben-bench/one/claude-repeat"
				m.Runs = append(m.Runs, repeat)
			},
			want: ErrDuplicateRun,
		},
		{
			name:   "a run with no case",
			mutate: func(m *Manifest) { m.Runs[0].Case = "" },
			want:   ErrMissingField,
		},
		{
			name:   "a run with no case definition fingerprint",
			mutate: func(m *Manifest) { m.Runs[0].CaseDefinitionSHA256 = "" },
			want:   ErrMissingField,
		},
		{
			name:   "a malformed case definition fingerprint",
			mutate: func(m *Manifest) { m.Runs[0].CaseDefinitionSHA256 = "deadbeef" },
			want:   ErrMissingField,
		},
		{
			name:   "a run with no agent",
			mutate: func(m *Manifest) { m.Runs[0].Agent = "" },
			want:   ErrMissingField,
		},
		{
			name:   "a run with no issue",
			mutate: func(m *Manifest) { m.Runs[0].Issue = "" },
			want:   ErrMissingField,
		},
		{
			name:   "a run with no state directory",
			mutate: func(m *Manifest) { m.Runs[0].StateDir = "" },
			want:   ErrMissingField,
		},
		{
			name:   "a relative state directory",
			mutate: func(m *Manifest) { m.Runs[0].StateDir = "state/claude" },
			want:   ErrNonCanonicalStateDir,
		},
		{
			name: "an aliased state directory",
			mutate: func(m *Manifest) {
				m.Runs[1].Issue = m.Runs[0].Issue
				m.Runs[1].StateDir = m.Runs[0].StateDir + "/."
			},
			want: ErrNonCanonicalStateDir,
		},
		{
			name:   "a run whose repo is not owner/name",
			mutate: func(m *Manifest) { m.Runs[0].Repo = "canary-one" },
			want:   ErrMissingField,
		},
		{
			// The recorded base is an observation of what the canary pointed at.
			// A branch name is the claim the observation exists to replace.
			name:   "a base recorded as a branch",
			mutate: func(m *Manifest) { m.Runs[0].Base = "main" },
			want:   ErrMutableRevision,
		},
		{
			name:   "a base recorded abbreviated",
			mutate: func(m *Manifest) { m.Runs[0].Base = "1111111" },
			want:   ErrMutableRevision,
		},
		{
			name: "check results without a checked commit",
			mutate: func(m *Manifest) {
				m.Runs[0].CheckResults = []CheckResult{{ID: "repo-check", Passed: true}}
			},
			want: ErrMissingField,
		},
		{
			name: "a checked commit without results",
			mutate: func(m *Manifest) {
				m.Runs[0].CheckedCommit = "2222222222222222222222222222222222222222"
			},
			want: ErrMissingField,
		},
		{
			name: "a mutable checked commit",
			mutate: func(m *Manifest) {
				m.Runs[0].CheckedCommit = "HEAD"
				m.Runs[0].CheckResults = []CheckResult{{ID: "repo-check", Passed: true}}
			},
			want: ErrMutableRevision,
		},
		{
			name: "a duplicate check result",
			mutate: func(m *Manifest) {
				m.Runs[0].CheckedCommit = "2222222222222222222222222222222222222222"
				m.Runs[0].CheckResults = []CheckResult{{ID: "repo-check"}, {ID: "repo-check"}}
			},
			want: ErrDuplicateID,
		},
		{
			// BEN watches the repository queue, not one issue. Two different issue
			// numbers still race when the first run's daemon remains alive.
			name:   "two runs sharing one canary queue",
			mutate: func(m *Manifest) { m.Runs[1].Repo = m.Runs[0].Repo },
			want:   ErrSharedQueue,
		},
		{
			// GitHub repository identity is case-insensitive. Different spellings do
			// not create independent queues.
			name:   "two runs sharing one canary queue through casing variants",
			mutate: func(m *Manifest) { m.Runs[1].Repo = "ACME/CANARY-ONE" },
			want:   ErrSharedQueue,
		},
		{
			// Two agents on one issue is the experiment the approved decision
			// rules out: one branch, two pushes, two pull requests and a claim
			// only one principal can hold.
			name: "two cells on one canary issue",
			mutate: func(m *Manifest) {
				m.Runs[1].Repo = m.Runs[0].Repo
				m.Runs[1].Issue = m.Runs[0].Issue
			},
			want: ErrSharedIssue,
		},
		{
			name: "two cells sharing one issue through repository casing variants",
			mutate: func(m *Manifest) {
				m.Runs[1].Repo = "ACME/CANARY-ONE"
				m.Runs[1].Issue = m.Runs[0].Issue
			},
			want: ErrSharedIssue,
		},
		{
			// Same issue number, different canaries, one state directory: the
			// attempt records in that directory cannot be attributed.
			name: "two runs indistinguishable in one state directory",
			mutate: func(m *Manifest) {
				m.Runs[1].Issue = m.Runs[0].Issue
				m.Runs[1].StateDir = m.Runs[0].StateDir
			},
			want: ErrAmbiguousJoin,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)
			if _, err := parse(t, m); !errors.Is(err, tc.want) {
				t.Fatalf("ParseManifest = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestADeclaredMatrixMayBeRecordedBeforeItsRuns(t *testing.T) {
	m := validManifest()
	m.Runs = nil
	parsed, err := parse(t, m)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if got := parsed.Cells(); len(got) != 2 {
		t.Fatalf("Cells = %v, want the two cells declared before dispatch", got)
	}
}

func TestParseManifestRefusesAFileThatIsNotAManifest(t *testing.T) {
	valid, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not JSON", "cohort: v1\n"},
		{"a misspelled field", `{"cohort":"v1","session":"s","run":[]}`},
		{"a misspelled field inside a run", `{"cohort":"v1","session":"s","runs":[{"case":"a","agents":"x"}]}`},
		{"junk after a valid manifest", string(valid) + "\nnot-json"},
		{"a second value after a valid manifest", string(valid) + "\n{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(tc.body)); !errors.Is(err, ErrManifestSchema) {
				t.Fatalf("ParseManifest = %v, want ErrManifestSchema", err)
			}
		})
	}
}

func TestLoadManifestReadsAFile(t *testing.T) {
	body, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(path); err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if _, err := LoadManifest(filepath.Join(t.TempDir(), "absent.json")); !errors.Is(err, ErrManifestSchema) {
		t.Errorf("LoadManifest of a missing file = %v, want ErrManifestSchema", err)
	}
}
