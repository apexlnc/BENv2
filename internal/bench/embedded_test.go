package bench

import (
	"io/fs"
	"strings"
	"testing"
)

// The checked-in cohort, held to the same rules as any other — and to two more
// that only apply to a cohort somebody has to maintain.
//
// Per AGENTS.md, a test driven by the declaration it checks proves the declared
// entries behave. So the file-claim check below is driven by the *directory*: it
// fails when a task file arrives that no case names, or when a case is deleted
// and its text left behind, neither of which a walk over c.Cases can see.

func TestTheCheckedInCohortLoads(t *testing.T) {
	c, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	if c.Version != EmbeddedVersion {
		t.Errorf("cohort version = %q, but it is embedded from cohort/%s; the directory and the "+
			"declaration name the same version or a session cannot say which it ran",
			c.Version, EmbeddedVersion)
	}
	if c.SourceRepo != "srhg-ai-7cef3f93/ben" {
		t.Errorf("source_repo = %q, want BEN's own repository — the corpus these base commits are from",
			c.SourceRepo)
	}
	if len(c.Cases) < 3 {
		t.Errorf("the cohort has %d cases; a publish rate over fewer than three is a number with no "+
			"spread behind it", len(c.Cases))
	}
	for _, cs := range c.Cases {
		task, err := c.Task(cs.ID)
		if err != nil {
			t.Errorf("case %s: %v", cs.ID, err)
			continue
		}
		if strings.TrimSpace(task) == "" {
			t.Errorf("case %s: task text is blank", cs.ID)
		}
	}
}

// The cohort spans the tiers.
//
// Not style: difficulty dominates publish rate far more than the adapter does,
// so a cohort of three easy cases reports both adapters at 100% and measures
// nothing. This is the one property of the *set* rather than of a case, which is
// why it is asserted here and not in validate.
func TestTheCheckedInCohortSpansTheTiers(t *testing.T) {
	c, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	seen := map[Tier]int{}
	for _, cs := range c.Cases {
		seen[cs.Tier]++
	}
	for _, tier := range []Tier{TierEasy, TierMedium, TierHard} {
		if seen[tier] == 0 {
			t.Errorf("no %s case in the cohort (have %v); a cohort without a spread reports every "+
				"adapter as equally able", tier, seen)
		}
	}
}

// Every file in the cohort directory is either the manifest or a task file
// exactly one case claims.
//
// Driven by the directory: an orphaned task file is text nothing dispatches, and
// a case naming a file that is not there is a case nothing can dispatch. The
// second half Load already refuses; this is the half it structurally cannot see.
func TestEveryFileInTheCohortDirectoryIsClaimed(t *testing.T) {
	c, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	claimed := map[string]string{}
	for _, cs := range c.Cases {
		claimed[cs.TaskFile] = cs.ID
	}

	entries, err := fs.ReadDir(cohortFS, "cohort/"+EmbeddedVersion)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("cohort/%s/%s is a directory; a cohort is a manifest and flat task files",
				EmbeddedVersion, e.Name())
			continue
		}
		seen++
		if e.Name() == cohortFile {
			continue
		}
		if _, ok := claimed[e.Name()]; !ok {
			t.Errorf("cohort/%s/%s is claimed by no case — text nothing dispatches, or a case that "+
				"was removed without it", EmbeddedVersion, e.Name())
		}
	}
	// The walk is the test; a wrong directory would report the all-clear having
	// read nothing.
	if seen < len(c.Cases)+1 {
		t.Errorf("read %d files from cohort/%s for %d cases; the walk is looking in the wrong place",
			seen, EmbeddedVersion, len(c.Cases))
	}
}
