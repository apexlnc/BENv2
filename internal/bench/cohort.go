package bench

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// The cohort is data, and every rule about it is enforced here rather than in
// the procedure that reads it. A benchmark whose input is validated by a
// convention somebody remembers is a benchmark that compares two adapters
// against two different task texts one day and reports the difference as an
// adapter difference.

//go:embed cohort
var cohortFS embed.FS

// EmbeddedVersion is the version of the cohort compiled into this package. It
// is a directory name as well as a declaration: cohort/v1 holds v1, and the two
// are pinned to each other in embedded_test.go.
//
// **A version is a promise about comparability, not a release number.** Runs of
// v1 may be compared with runs of v1. Changing a case's task text, its base
// commit, or what its checks demand changes what a pass means, so it is a new
// version — the old cases stay readable, and a session recorded against them
// stays interpretable. Adding a case is the one edit that does not have to bump
// it: Compare reports over matched cases, so an older session simply matches
// fewer of them and says so.
const EmbeddedVersion = "v1"

// cohortFile is the manifest inside a cohort directory.
const cohortFile = "cohort.json"

// Cohort is one versioned, checked-in set of benchmark cases.
//
// The files live beside the manifest, so a Cohort keeps the filesystem it was
// read from: Task reads a case's text back through it, and the digest check that
// makes the text immutable needs it too.
type Cohort struct {
	Version string `json:"version"`
	// SourceRepo is the repository the base commits belong to, `owner/name`.
	// Every canary a session dispatches against is a mirror of it — that is what
	// makes a pinned commit resolve to the same tree in every cell. Definition
	// fingerprints use its lowercase GitHub identity, not its display spelling.
	SourceRepo string `json:"source_repo"`
	Notes      string `json:"notes,omitempty"`
	Cases      []Case `json:"cases"`

	files fs.FS
}

// Case is one benchmark task: what to ask, where to ask it from, what the
// known-good answer was, and how a pass is decided.
type Case struct {
	// ID is stable and is what an attempt is joined to. It never changes; a case
	// that needs different content is a different case in a later version.
	ID string `json:"id"`
	// Title is the canary issue's title, verbatim. Held here rather than in the
	// task file because the prompt renders title and body separately (SPEC §5.6)
	// and both are part of the exact task text.
	Title string `json:"title"`
	// Tier is the difficulty band this case was filed under. It is documentation
	// for whoever reads a result — a cohort of three easy cases and a cohort with
	// a spread produce very different publish rates — and it is validated only
	// against the closed set below, never computed.
	Tier Tier `json:"tier"`
	// TaskFile is the canary issue's body, verbatim, in a file of its own beside
	// the manifest.
	//
	// A file rather than a JSON string for two reasons that both matter: the
	// procedure feeds it to `gh issue create --body-file`, so no operator has to
	// re-escape or paraphrase it; and a reviewer can read it.
	TaskFile string `json:"task_file"`
	// TaskSHA256 pins the bytes of that file, lowercase hex.
	//
	// **The pin is the whole of "fixed".** Comparability across cells is a
	// property of one prompt, and comparability across *sessions* is a property
	// of one prompt over time. Nothing about editing a task file announces that
	// last week's session no longer measured the same thing, so the pin is what
	// announces it: the edit fails `make check` until somebody records the new
	// digest, which is the moment to ask whether it is a new version.
	TaskSHA256 string `json:"task_sha256"`
	// DefinitionSHA256 pins this case's complete declaration and the cohort's
	// normalized SourceRepo, with this field cleared before deterministic JSON
	// encoding. TaskSHA256 therefore binds the task bytes into the definition too.
	// A session copies this value into every run, so retaining a case ID and cohort
	// version cannot reinterpret old results after the repository, title, task pin,
	// base, provenance, or checks change.
	DefinitionSHA256 string `json:"definition_sha256"`
	// BaseCommit is the immutable commit the task is worked from — the state of
	// SourceRepo before its known-good solution landed.
	BaseCommit string `json:"base_commit"`
	// KnownGood is the historical solution, for provenance and for reading a
	// result against something real.
	KnownGood Outcome `json:"known_good"`
	// Checks decide the case, alongside the ordinary §9.7 publish verdict.
	Checks []Check `json:"checks"`
}

// Tier is the closed set of difficulty bands.
type Tier string

const (
	TierEasy   Tier = "easy"
	TierMedium Tier = "medium"
	TierHard   Tier = "hard"
)

func (t Tier) valid() bool {
	switch t {
	case TierEasy, TierMedium, TierHard:
		return true
	}
	return false
}

// Outcome is the known-good historical result: what actually solved this task,
// and what that solution did.
//
// It is provenance and it is *not* the pass criterion. Success is the publish
// verdict plus the case's Checks — never a diff against Commit. Two correct
// solutions to a real ticket do not look alike, and a benchmark that demands the
// historical patch measures imitation.
type Outcome struct {
	Commit string `json:"commit"`
	// PR is the pull request the solution merged as, 0 when there was none.
	PR      int    `json:"pr,omitempty"`
	Summary string `json:"summary"`
}

// Check is one mechanically decidable condition: run by `bash -c` in the root of
// the head the agent published, and required to exit 0. Nothing here runs it —
// the operator does, per docs/BENCH.md — so the contract is stated rather than
// enforced, and a check written for another shell is a check that fails.
//
// **Each is a necessary condition read off the ticket's acceptance list, not a
// sufficient one.** A benchmark cell is decided without a human in the loop, so
// the checks are as strong as whoever wrote them made them — a wrong solution
// that satisfies all of them counts as a pass. That is a stated limit rather
// than a hidden one, and strengthening a case's checks changes what a pass means:
// it is a new cohort version (see EmbeddedVersion).
type Check struct {
	ID  string `json:"id"`
	Run string `json:"run"`
	Why string `json:"why"`
	// FailsAtBase records that this check fails at BaseCommit — that it can tell
	// a solution from the tree the task started on.
	//
	// Declared, never detected, for the reason `deployment.mode` is (SPEC §5.2.9):
	// re-deriving it needs the case's checks run against a historical checkout,
	// which no unit test can do offline. What the validator enforces is that
	// every case has at least one — a case all of whose checks pass at its own
	// base commit measures nothing, and would report every adapter as perfect.
	// docs/BENCH.md carries the command that verifies the claim.
	FailsAtBase bool `json:"fails_at_base"`
}

// computedDefinitionSHA256 hashes the normalized source-repository identity and
// whole Case rather than a hand-maintained subset. That makes a future Case
// field part of the pin by default. These JSON shapes contain only values
// encoding/json cannot reject; panic would expose a programmer violation of
// that invariant during cohort load and tests.
func (c Cohort) computedDefinitionSHA256(cs Case) string {
	cs.DefinitionSHA256 = ""
	payload := struct {
		SourceRepo string `json:"source_repo"`
		Case       Case   `json:"case"`
	}{SourceRepo: strings.ToLower(c.SourceRepo), Case: cs}
	body, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("bench: Case is no longer JSON-encodable: %v", err))
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// The refusals. Each is a distinct thing to fix, and the tests assert on them
// rather than on message text.
var (
	// ErrCohortSchema is a cohort file that is not one: unreadable, not JSON, or
	// carrying a field this package does not define.
	ErrCohortSchema = errors.New("bench: the cohort file is not a cohort")
	// ErrMissingField is a required field left empty. A benchmark case with no
	// task text, no known-good outcome or no checks is not a case.
	ErrMissingField = errors.New("bench: a required field is missing")
	// ErrDuplicateID is two cases (or two checks in one case) sharing an ID. The
	// ID is the join key, so a duplicate would silently merge two cases'
	// attempts into one row.
	ErrDuplicateID = errors.New("bench: duplicate identifier")
	// ErrMutableRevision is a revision that is not a full-length lowercase hex
	// object ID — a branch, a tag, `HEAD`, an abbreviation, or a differently
	// cased spelling of one commit.
	ErrMutableRevision = errors.New("bench: revision is not an immutable commit ID")
	// ErrTaskDrift is a task file whose bytes do not match the pin. See
	// Case.TaskSHA256.
	ErrTaskDrift = errors.New("bench: task text does not match its pin")
	// ErrCaseDefinitionDrift is a case whose normalized source repository and
	// complete declaration no longer match DefinitionSHA256. Without this second
	// pin, changing the repository, a title, task and its digest, or check command
	// while retaining IDs reinterprets or relabels sessions.
	ErrCaseDefinitionDrift = errors.New("bench: case definition does not match its pin")
	// ErrInertCase is a case that cannot distinguish a solution from its own
	// starting tree: no check that fails at the base commit, or a known-good
	// commit that *is* the base commit.
	ErrInertCase = errors.New("bench: the case measures nothing")
	// ErrUnknownTier is a difficulty band outside the closed set.
	ErrUnknownTier = errors.New("bench: unknown tier")
)

// Embedded returns the cohort compiled into this binary, validated.
func Embedded() (*Cohort, error) {
	sub, err := fs.Sub(cohortFS, "cohort/"+EmbeddedVersion)
	if err != nil {
		return nil, fmt.Errorf("bench: %w", err)
	}
	return Load(sub)
}

// LoadDir loads a cohort from a directory on disk — a cohort being edited, or a
// cohort held somewhere other than this repository.
func LoadDir(dir string) (*Cohort, error) { return Load(os.DirFS(dir)) }

// Load reads and validates the cohort rooted at fsys.
//
// Strict, and strict at load: an unknown field, a mutable revision, a missing
// check, a drifted task file — every one of them refuses here rather than
// producing a report whose numbers are quietly about something else (AGENTS.md,
// "Conventions").
func Load(fsys fs.FS) (*Cohort, error) {
	body, err := fs.ReadFile(fsys, cohortFile)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrCohortSchema, cohortFile, err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	// The strictness yaml.KnownFields(true) buys the workflow loader (SPEC §5.3):
	// a misspelled key is a field that silently did not apply, and here that is a
	// check nobody ran or a pin nobody compared.
	dec.DisallowUnknownFields()
	var c Cohort
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrCohortSchema, cohortFile, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("more than one JSON value")
		}
		return nil, fmt.Errorf("%w: %s has trailing content: %w", ErrCohortSchema, cohortFile, err)
	}
	c.files = fsys
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Case returns the case with an ID, and whether there is one.
func (c *Cohort) Case(id string) (Case, bool) {
	for _, cs := range c.Cases {
		if cs.ID == id {
			return cs, true
		}
	}
	return Case{}, false
}

// IDs are the case identifiers in cohort order.
func (c *Cohort) IDs() []string {
	out := make([]string, 0, len(c.Cases))
	for _, cs := range c.Cases {
		out = append(out, cs.ID)
	}
	return out
}

// Task returns a case's exact task text, re-checking the pin. The check is not
// redundant with Load's: a cohort read from a directory somebody is editing can
// change under a long-running process, and the text is what gets dispatched.
func (c *Cohort) Task(id string) (string, error) {
	cs, ok := c.Case(id)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownCase, id)
	}
	body, err := c.taskBytes(cs)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (c *Cohort) taskBytes(cs Case) ([]byte, error) {
	body, err := fs.ReadFile(c.files, cs.TaskFile)
	if err != nil {
		return nil, fmt.Errorf("%w: case %q: reading %s: %w", ErrMissingField, cs.ID, cs.TaskFile, err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: case %q: %s is empty", ErrMissingField, cs.ID, cs.TaskFile)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != cs.TaskSHA256 {
		return nil, fmt.Errorf("%w: case %q: %s hashes to %s, the cohort pins %s "+
			"(record the new digest deliberately, and ask whether the edit is a new cohort version)",
			ErrTaskDrift, cs.ID, cs.TaskFile, got, cs.TaskSHA256)
	}
	return body, nil
}

// idPattern is what a stable ID may look like: lowercase, digits and hyphens. It
// names a file in a report, a directory in a procedure and a column in a table,
// so it stays free of anything that would need quoting in one of them.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// hexRevision matches a full-length lowercase object ID, SHA-1 or SHA-256.
var hexRevision = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// repoName matches `owner/name`.
var repoName = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// sha256Hex matches a lowercase sha-256 digest.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (c *Cohort) validate() error {
	if strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("%w: cohort version", ErrMissingField)
	}
	if !repoName.MatchString(c.SourceRepo) {
		return fmt.Errorf("%w: cohort %s: source_repo %q is not owner/name",
			ErrMissingField, c.Version, c.SourceRepo)
	}
	if len(c.Cases) == 0 {
		return fmt.Errorf("%w: cohort %s has no cases", ErrMissingField, c.Version)
	}

	ids := map[string]bool{}
	files := map[string]string{}
	for _, cs := range c.Cases {
		if err := c.validateCase(cs); err != nil {
			return err
		}
		if ids[cs.ID] {
			return fmt.Errorf("%w: cohort %s declares case %q twice", ErrDuplicateID, c.Version, cs.ID)
		}
		ids[cs.ID] = true
		if other, ok := files[cs.TaskFile]; ok {
			return fmt.Errorf("%w: cases %q and %q share task file %s",
				ErrDuplicateID, other, cs.ID, cs.TaskFile)
		}
		files[cs.TaskFile] = cs.ID
	}
	for _, cs := range c.Cases {
		if got := c.computedDefinitionSHA256(cs); got != cs.DefinitionSHA256 {
			return fmt.Errorf("%w: cohort %s case %q computes %s, pins %s",
				ErrCaseDefinitionDrift, c.Version, cs.ID, got, cs.DefinitionSHA256)
		}
	}
	return nil
}

func (c *Cohort) validateCase(cs Case) error {
	if !idPattern.MatchString(cs.ID) {
		return fmt.Errorf("%w: case id %q must match %s", ErrMissingField, cs.ID, idPattern)
	}
	where := fmt.Sprintf("cohort %s case %q", c.Version, cs.ID)
	if strings.TrimSpace(cs.Title) == "" {
		return fmt.Errorf("%w: %s has no title", ErrMissingField, where)
	}
	if !cs.Tier.valid() {
		return fmt.Errorf("%w: %s: tier %q is not one of %s, %s, %s",
			ErrUnknownTier, where, cs.Tier, TierEasy, TierMedium, TierHard)
	}
	// A task file is read relative to the cohort directory, and only from it: a
	// path escaping upward would make the cohort's contents depend on where it
	// was checked out.
	if cs.TaskFile == "" || strings.ContainsAny(cs.TaskFile, `/\`) {
		return fmt.Errorf("%w: %s: task_file %q must be a file name beside %s",
			ErrMissingField, where, cs.TaskFile, cohortFile)
	}
	if !sha256Hex.MatchString(cs.TaskSHA256) {
		return fmt.Errorf("%w: %s: task_sha256 %q is not a lowercase sha-256 hex digest",
			ErrMissingField, where, cs.TaskSHA256)
	}
	if !sha256Hex.MatchString(cs.DefinitionSHA256) {
		return fmt.Errorf("%w: %s: definition_sha256 %q is not a lowercase sha-256 hex digest",
			ErrMissingField, where, cs.DefinitionSHA256)
	}
	if !hexRevision.MatchString(cs.BaseCommit) {
		return fmt.Errorf("%w: %s: base_commit %q — a benchmark base must be a commit ID no "+
			"later push can move", ErrMutableRevision, where, cs.BaseCommit)
	}
	if !hexRevision.MatchString(cs.KnownGood.Commit) {
		return fmt.Errorf("%w: %s: known_good.commit %q", ErrMutableRevision, where, cs.KnownGood.Commit)
	}
	if cs.KnownGood.Commit == cs.BaseCommit {
		return fmt.Errorf("%w: %s: the known-good commit is the base commit, so the case is already solved at its base",
			ErrInertCase, where)
	}
	if strings.TrimSpace(cs.KnownGood.Summary) == "" {
		return fmt.Errorf("%w: %s: known_good.summary — what the historical solution did", ErrMissingField, where)
	}
	if cs.KnownGood.PR < 0 {
		return fmt.Errorf("%w: %s: known_good.pr %d", ErrMissingField, where, cs.KnownGood.PR)
	}
	if len(cs.Checks) == 0 {
		return fmt.Errorf("%w: %s has no checks, so nothing but the publish verdict decides it",
			ErrMissingField, where)
	}

	seen := map[string]bool{}
	discriminates := false
	for _, ck := range cs.Checks {
		if !idPattern.MatchString(ck.ID) {
			return fmt.Errorf("%w: %s: check id %q must match %s", ErrMissingField, where, ck.ID, idPattern)
		}
		if seen[ck.ID] {
			return fmt.Errorf("%w: %s declares check %q twice", ErrDuplicateID, where, ck.ID)
		}
		seen[ck.ID] = true
		if strings.TrimSpace(ck.Run) == "" {
			return fmt.Errorf("%w: %s check %q has no command", ErrMissingField, where, ck.ID)
		}
		if strings.TrimSpace(ck.Why) == "" {
			return fmt.Errorf("%w: %s check %q does not say what it is for", ErrMissingField, where, ck.ID)
		}
		discriminates = discriminates || ck.FailsAtBase
	}
	if !discriminates {
		return fmt.Errorf("%w: %s: no check is recorded as failing at %s, so every cell passes it "+
			"without doing the work", ErrInertCase, where, cs.BaseCommit[:12])
	}

	// Last, because it reads a file: a case whose declaration is wrong is worth
	// saying so about before its text is fetched.
	_, err := c.taskBytes(cs)
	return err
}
