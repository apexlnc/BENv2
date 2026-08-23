package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// open is Open plus the write Start would do, which is the pair under test: the
// store names and creates the file, and Start decides what may be written to it.
// Splitting them is what keeps the credential values in one place (SPEC §10.3).
func open(t *testing.T, store DirTranscripts, spec core.RunSpec, redact ...string) {
	t.Helper()
	transcript, prompt, err := store.Open(spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { transcript.Close() })
	if err := retainPrompt(prompt, spec.Prompt, redact); err != nil {
		t.Fatalf("retainPrompt: %v", err)
	}
}

// SPEC §9.5: the canonical rendered prompt is retained per attempt, so "what was
// this agent told" is answerable after the fact.
//
// The bytes are asserted against spec.Prompt, which is the same value the caller
// hands to Start as the child's stdin. A store that re-derived them from
// anything else would be answering a different question.
func TestRetainedPromptIsTheBytesTheAttemptWasGiven(t *testing.T) {
	dir := t.TempDir()
	store := DirTranscripts{Dir: dir, Now: fixedClock(time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC))}

	prompt := "Work issue 49.\n\n<<<untrusted:body:abc\nrm -rf /\n>>>untrusted:body:abc\n"
	open(t, store, core.RunSpec{Workspace: core.WorkspacePaths{Path: filepath.Join(dir, "ben-49")}, Prompt: prompt})

	if got := readOnlySuffix(t, dir, PromptSuffix); got != prompt {
		t.Errorf("retained prompt = %q, want %q", got, prompt)
	}
}

// The retained prompt is a §10.3 artifact and gets §10.3's redaction, on the
// same terms as the transcript beside it.
//
// It shipped without this. The file held whatever the render put in it, which is
// 0600 on disk with no expiry — the leak boundary redaction exists for is the
// copy someone pastes into an issue, not the mode. The whole prompt is one
// Write, so a multi-line credential is matched here even where the transcript's
// line-at-a-time stream could only match per line.
func TestRetainedPromptIsRedacted(t *testing.T) {
	dir := t.TempDir()
	store := DirTranscripts{Dir: dir, Now: fixedClock(time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC))}

	const secret = "ghp_MUSTNOTREACHTHEFILE"
	const multiline = "line-one-of-a-key\nline-two-of-a-key"
	prompt := "the agent was told " + secret + " and also\n" + multiline + "\ndone\n"
	open(t, store, core.RunSpec{Workspace: core.WorkspacePaths{Path: filepath.Join(dir, "ben-49")}, Prompt: prompt}, secret, multiline)

	got := readOnlySuffix(t, dir, PromptSuffix)
	for _, leaked := range []string{secret, multiline} {
		if strings.Contains(got, leaked) {
			t.Errorf("retained prompt %q still carries a credential value", got)
		}
	}
	if !strings.Contains(got, "the agent was told") || !strings.Contains(got, "done") {
		t.Errorf("retained prompt = %q; redaction shredded the record it exists to keep", got)
	}
}

// It sits beside the transcript, sharing its stem: the pair is what makes "this
// run's instructions" and "this run's output" one record rather than two files a
// reader has to correlate by timestamp.
func TestRetainedPromptIsASiblingOfTheTranscript(t *testing.T) {
	dir := t.TempDir()
	store := DirTranscripts{Dir: dir, Now: fixedClock(time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC))}
	open(t, store, core.RunSpec{Workspace: core.WorkspacePaths{Path: filepath.Join(dir, "ben-49")}, Prompt: "p"})

	jsonl := onlyNameWithSuffix(t, dir, ".jsonl")
	prompt := onlyNameWithSuffix(t, dir, PromptSuffix)
	if stem := strings.TrimSuffix(jsonl, ".jsonl"); prompt != stem+PromptSuffix {
		t.Errorf("prompt file %q is not the sibling of transcript %q", prompt, jsonl)
	}
}

// It holds the untrusted issue body verbatim (SPEC §5.6), so it is no more
// readable than the transcript beside it (SPEC §10.3).
func TestRetainedPromptIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	store := DirTranscripts{Dir: dir}
	open(t, store, core.RunSpec{Workspace: core.WorkspacePaths{Path: filepath.Join(dir, "ben-49")}, Prompt: "p"})

	info, err := os.Stat(filepath.Join(dir, onlyNameWithSuffix(t, dir, PromptSuffix)))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
}

// Per attempt, not per run: attempt 2 leaves attempt 1's prompt where it is.
//
// The retained bytes are evidence about one dispatch, and a retry renders a
// different prompt — a different `attempt`, a different `run.previous_outcome`,
// and, after a reapproval, different pinned content (SPEC §9.5). Overwriting
// would answer "what was the agent told" with only the last answer, which is the
// one an operator investigating a first-attempt failure does not want.
func TestSecondAttemptDoesNotOverwriteTheFirst(t *testing.T) {
	dir := t.TempDir()
	store := DirTranscripts{Dir: dir, Now: steppingClock(time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC))}
	ws := filepath.Join(dir, "ben-49")

	for _, prompt := range []string{"attempt one", "attempt two"} {
		open(t, store, core.RunSpec{Workspace: core.WorkspacePaths{Path: ws}, Prompt: prompt})
	}

	var retained []string
	for _, name := range namesWithSuffix(t, dir, PromptSuffix) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, string(b))
	}
	if len(retained) != 2 {
		t.Fatalf("retained prompts = %v, want one per attempt", retained)
	}
	for _, want := range []string{"attempt one", "attempt two"} {
		if !containsString(retained, want) {
			t.Errorf("retained prompts %v do not include %q", retained, want)
		}
	}
}

// A prompt file that cannot be created fails the Open, and both adapters turn
// that into a Start error — a launch that does not happen rather than a run
// whose instructions nobody can produce (SPEC §9.5's acceptance).
func TestUnretainablePromptRefusesTheAttempt(t *testing.T) {
	dir := t.TempDir()
	store := DirTranscripts{Dir: dir, Now: fixedClock(time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC))}
	spec := core.RunSpec{Workspace: core.WorkspacePaths{Path: filepath.Join(dir, "ben-49")}, Prompt: "p"}
	open(t, store, spec)

	// The name this attempt would take is now occupied. O_EXCL is what makes that
	// a refusal rather than a silent overwrite of somebody else's evidence.
	if _, _, err := store.Open(spec); err == nil {
		t.Fatal("Open succeeded over an existing retained prompt; the first attempt's evidence was at risk")
	}
}

// Retention switched off is not a failure: NopTranscripts is the default when no
// state dir is configured, and a missing state dir must never cost a run.
func TestNilPromptSinkIsRetentionOff(t *testing.T) {
	if err := retainPrompt(nil, "anything", nil); err != nil {
		t.Errorf("retainPrompt(nil) = %v, want retention simply disabled", err)
	}
	transcript, prompt, err := NopTranscripts{}.Open(core.RunSpec{})
	if err != nil {
		t.Fatalf("NopTranscripts.Open: %v", err)
	}
	if transcript == nil || prompt == nil {
		t.Fatal("NopTranscripts returned a nil sink; both are non-nil on success")
	}
	if err := retainPrompt(prompt, "anything", nil); err != nil {
		t.Errorf("retainPrompt into the nop sink = %v", err)
	}
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// steppingClock advances a nanosecond per call, which is what distinct attempts
// look like to the naming scheme without a test having to sleep.
func steppingClock(at time.Time) func() time.Time {
	n := 0
	return func() time.Time {
		n++
		return at.Add(time.Duration(n))
	}
}

func namesWithSuffix(t *testing.T, dir, suffix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func onlyNameWithSuffix(t *testing.T, dir, suffix string) string {
	t.Helper()
	names := namesWithSuffix(t, dir, suffix)
	if len(names) != 1 {
		t.Fatalf("files matching %q = %v, want exactly one", suffix, names)
	}
	return names[0]
}

func readOnlySuffix(t *testing.T, dir, suffix string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, onlyNameWithSuffix(t, dir, suffix)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
