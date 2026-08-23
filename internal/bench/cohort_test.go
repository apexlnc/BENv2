package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// The cohort's validation is the whole of "fixed" (#62): a cohort that loads is
// one whose cases are distinct, whose bases cannot move, whose task text is the
// text it was pinned as, and whose checks can tell a solution from the tree it
// started on. Each refusal below is a distinct thing to fix, so each is asserted
// by its named error rather than by message text.

const taskText = "**Spec:** §12.4\n\nDo the thing the ticket says.\n"

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// validCohort is the fixture every rejection below is one edit away from. It is
// built in Go and marshalled, so a field renamed in cohort.go breaks this file
// at compile time rather than silently unsetting a field in a JSON blob.
func validCohort() Cohort {
	c := Cohort{
		Version:    "v1",
		SourceRepo: "acme/ben",
		Cases: []Case{{
			ID:         "case-one",
			Title:      "The first case",
			Tier:       TierMedium,
			TaskFile:   "case-one.task.md",
			TaskSHA256: digest(taskText),
			BaseCommit: "1111111111111111111111111111111111111111",
			KnownGood: Outcome{
				Commit:  "2222222222222222222222222222222222222222",
				PR:      7,
				Summary: "what the historical solution did",
			},
			Checks: []Check{{
				ID:          "probe",
				Run:         "[ ! -e gone.go ]",
				Why:         "the acceptance bullet this comes from",
				FailsAtBase: true,
			}},
		}},
	}
	c.Cases[0].DefinitionSHA256 = c.computedDefinitionSHA256(c.Cases[0])
	return c
}

// files is the cohort directory: the manifest plus one file per case.
func files(t *testing.T, c Cohort, tasks map[string]string) fstest.MapFS {
	t.Helper()
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshalling the fixture: %v", err)
	}
	out := fstest.MapFS{cohortFile: &fstest.MapFile{Data: body}}
	for name, text := range tasks {
		out[name] = &fstest.MapFile{Data: []byte(text)}
	}
	return out
}

func defaultTasks() map[string]string {
	return map[string]string{"case-one.task.md": taskText}
}

func TestAValidCohortLoads(t *testing.T) {
	c, err := Load(files(t, validCohort(), defaultTasks()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.IDs(); len(got) != 1 || got[0] != "case-one" {
		t.Errorf("IDs = %v, want [case-one]", got)
	}
	cs, ok := c.Case("case-one")
	if !ok {
		t.Fatal("Case(case-one) not found")
	}
	if cs.Tier != TierMedium {
		t.Errorf("tier = %q, want %q", cs.Tier, TierMedium)
	}
	task, err := c.Task("case-one")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if task != taskText {
		t.Errorf("Task = %q, want the pinned text", task)
	}
	if _, err := c.Task("nope"); !errors.Is(err, ErrUnknownCase) {
		t.Errorf("Task(unknown) = %v, want ErrUnknownCase", err)
	}
}

func TestCaseDefinitionFingerprintNormalizesTheSourceRepository(t *testing.T) {
	c := validCohort()
	c.SourceRepo = "ACME/BEN"
	if _, err := Load(files(t, c, defaultTasks())); err != nil {
		t.Fatalf("Load after a source_repo casing change = %v, want the same GitHub identity", err)
	}
}

func TestLoadRefusesACohortItCannotCompare(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Cohort)
		tasks  func(map[string]string)
		want   error
	}{
		{
			name:   "no version",
			mutate: func(c *Cohort) { c.Version = "" },
			want:   ErrMissingField,
		},
		{
			name:   "source repo is not owner/name",
			mutate: func(c *Cohort) { c.SourceRepo = "ben" },
			want:   ErrMissingField,
		},
		{
			name:   "source repo changed under the same case definition pin",
			mutate: func(c *Cohort) { c.SourceRepo = "other/ben" },
			want:   ErrCaseDefinitionDrift,
		},
		{
			name:   "no cases",
			mutate: func(c *Cohort) { c.Cases = nil },
			want:   ErrMissingField,
		},
		{
			name: "two cases share an id",
			mutate: func(c *Cohort) {
				second := c.Cases[0]
				second.TaskFile = "case-two.task.md"
				c.Cases = append(c.Cases, second)
			},
			tasks: func(m map[string]string) { m["case-two.task.md"] = taskText },
			want:  ErrDuplicateID,
		},
		{
			name: "two cases share a task file",
			mutate: func(c *Cohort) {
				second := c.Cases[0]
				second.ID = "case-two"
				c.Cases = append(c.Cases, second)
			},
			want: ErrDuplicateID,
		},
		{
			name: "two checks share an id",
			mutate: func(c *Cohort) {
				c.Cases[0].Checks = append(c.Cases[0].Checks, c.Cases[0].Checks[0])
			},
			want: ErrDuplicateID,
		},
		{
			name:   "an id that needs quoting",
			mutate: func(c *Cohort) { c.Cases[0].ID = "Case One" },
			want:   ErrMissingField,
		},
		{
			name:   "no title",
			mutate: func(c *Cohort) { c.Cases[0].Title = " " },
			want:   ErrMissingField,
		},
		{
			name:   "a tier outside the set",
			mutate: func(c *Cohort) { c.Cases[0].Tier = "trivial" },
			want:   ErrUnknownTier,
		},
		{
			name:   "no tier at all",
			mutate: func(c *Cohort) { c.Cases[0].Tier = "" },
			want:   ErrUnknownTier,
		},
		{
			// The base is what makes two cells comparable, so every spelling that
			// could resolve to a different tree later is refused — not only the
			// obviously mutable ones.
			name:   "a base that is a branch",
			mutate: func(c *Cohort) { c.Cases[0].BaseCommit = "main" },
			want:   ErrMutableRevision,
		},
		{
			name:   "a base that is HEAD",
			mutate: func(c *Cohort) { c.Cases[0].BaseCommit = "HEAD" },
			want:   ErrMutableRevision,
		},
		{
			name:   "a base that is a tag",
			mutate: func(c *Cohort) { c.Cases[0].BaseCommit = "v1.2.3" },
			want:   ErrMutableRevision,
		},
		{
			name:   "an abbreviated base",
			mutate: func(c *Cohort) { c.Cases[0].BaseCommit = "1111111111" },
			want:   ErrMutableRevision,
		},
		{
			// One commit, two strings: a join keyed on the spelling would split
			// the case in half.
			name:   "an uppercase base",
			mutate: func(c *Cohort) { c.Cases[0].BaseCommit = strings.ToUpper("a111111111111111111111111111111111111111") },
			want:   ErrMutableRevision,
		},
		{
			name:   "a base that is not hex at all",
			mutate: func(c *Cohort) { c.Cases[0].BaseCommit = "" },
			want:   ErrMutableRevision,
		},
		{
			name:   "a known-good revision that can move",
			mutate: func(c *Cohort) { c.Cases[0].KnownGood.Commit = "release-1" },
			want:   ErrMutableRevision,
		},
		{
			name:   "a known-good commit that is the base",
			mutate: func(c *Cohort) { c.Cases[0].KnownGood.Commit = c.Cases[0].BaseCommit },
			want:   ErrInertCase,
		},
		{
			name:   "no known-good summary",
			mutate: func(c *Cohort) { c.Cases[0].KnownGood.Summary = "" },
			want:   ErrMissingField,
		},
		{
			name:   "no checks",
			mutate: func(c *Cohort) { c.Cases[0].Checks = nil },
			want:   ErrMissingField,
		},
		{
			name:   "a check with no command",
			mutate: func(c *Cohort) { c.Cases[0].Checks[0].Run = "  " },
			want:   ErrMissingField,
		},
		{
			name:   "a check that says nothing about itself",
			mutate: func(c *Cohort) { c.Cases[0].Checks[0].Why = "" },
			want:   ErrMissingField,
		},
		{
			// The inert case: everything it asks for is already true at its own
			// base, so every cell passes it without doing the work.
			name:   "no check fails at the base",
			mutate: func(c *Cohort) { c.Cases[0].Checks[0].FailsAtBase = false },
			want:   ErrInertCase,
		},
		{
			name:   "a task file outside the cohort directory",
			mutate: func(c *Cohort) { c.Cases[0].TaskFile = "../elsewhere.md" },
			want:   ErrMissingField,
		},
		{
			name:   "a task digest that is not a digest",
			mutate: func(c *Cohort) { c.Cases[0].TaskSHA256 = "deadbeef" },
			want:   ErrMissingField,
		},
		{
			name:   "a definition digest that is not a digest",
			mutate: func(c *Cohort) { c.Cases[0].DefinitionSHA256 = "deadbeef" },
			want:   ErrMissingField,
		},
		{
			name:   "a task file that is not there",
			mutate: func(c *Cohort) { c.Cases[0].TaskFile = "missing.task.md" },
			want:   ErrMissingField,
		},
		{
			name:  "an empty task file",
			tasks: func(m map[string]string) { m["case-one.task.md"] = "" },
			want:  ErrMissingField,
		},
		{
			// The pin's whole purpose: the text moved, so what a session recorded
			// last week is no longer what a session records today.
			name:  "task text that has drifted from its pin",
			tasks: func(m map[string]string) { m["case-one.task.md"] = taskText + "and one more thing\n" },
			want:  ErrTaskDrift,
		},
		{
			name:   "a title changed under the same definition pin",
			mutate: func(c *Cohort) { c.Cases[0].Title = "A reinterpreted title" },
			want:   ErrCaseDefinitionDrift,
		},
		{
			name: "task text and its task pin changed under the same definition pin",
			mutate: func(c *Cohort) {
				c.Cases[0].TaskSHA256 = digest(taskText + "and one more thing\n")
			},
			tasks: func(m map[string]string) {
				m["case-one.task.md"] = taskText + "and one more thing\n"
			},
			want: ErrCaseDefinitionDrift,
		},
		{
			name:   "a check command changed under the same definition pin",
			mutate: func(c *Cohort) { c.Cases[0].Checks[0].Run = "true" },
			want:   ErrCaseDefinitionDrift,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validCohort()
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			tasks := defaultTasks()
			if tc.tasks != nil {
				tc.tasks(tasks)
			}
			_, err := Load(files(t, c, tasks))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Load = %v, want %v", err, tc.want)
			}
		})
	}
}

// A cohort file that is not one. Raw JSON rather than a marshalled fixture,
// because these are the failures a Go struct cannot express: the misspelling
// that would otherwise be a check nobody ran.
func TestLoadRefusesAFileThatIsNotACohort(t *testing.T) {
	for _, tc := range []struct {
		name string
		fsys fs.FS
		want error
	}{
		{
			name: "no cohort file",
			fsys: fstest.MapFS{"README.md": &fstest.MapFile{Data: []byte("nothing here")}},
			want: ErrCohortSchema,
		},
		{
			name: "not JSON",
			fsys: fstest.MapFS{cohortFile: &fstest.MapFile{Data: []byte("version: v1\n")}},
			want: ErrCohortSchema,
		},
		{
			name: "a misspelled field",
			fsys: fstest.MapFS{cohortFile: &fstest.MapFile{Data: []byte(
				`{"version":"v1","source_repo":"acme/ben","case":[]}`)}},
			want: ErrCohortSchema,
		},
		{
			name: "a misspelled field inside a case",
			fsys: fstest.MapFS{cohortFile: &fstest.MapFile{Data: []byte(
				`{"version":"v1","source_repo":"acme/ben","cases":[{"id":"a","checks_":[]}]}`)}},
			want: ErrCohortSchema,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(tc.fsys); !errors.Is(err, tc.want) {
				t.Fatalf("Load = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestLoadRefusesContentAfterTheCohort(t *testing.T) {
	for _, suffix := range []string{"\nnot-json", "\n{}"} {
		fsys := files(t, validCohort(), defaultTasks())
		manifest := fsys[cohortFile]
		manifest.Data = append(manifest.Data, suffix...)
		if _, err := Load(fsys); !errors.Is(err, ErrCohortSchema) {
			t.Errorf("Load with suffix %q = %v, want ErrCohortSchema", suffix, err)
		}
	}
}

// Task re-checks the pin rather than trusting the check Load already did: a
// cohort read from a working directory can change under a process that holds it,
// and the text is what a canary issue gets dispatched with.
func TestTaskRefusesTextThatChangedAfterLoad(t *testing.T) {
	fsys := files(t, validCohort(), defaultTasks())
	c, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fsys["case-one.task.md"] = &fstest.MapFile{Data: []byte("a different ticket entirely\n")}
	if _, err := c.Task("case-one"); !errors.Is(err, ErrTaskDrift) {
		t.Fatalf("Task after an edit = %v, want ErrTaskDrift", err)
	}
}
