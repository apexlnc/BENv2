package harness

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Raw transcript retention (SPEC §7.2, §10.3): the normalized event stream is
// lossy by design, so the harness's own output is kept verbatim for forensics.
//
// The sink is an interface rather than a path in RunSpec so assembly can choose
// retention policy without putting filesystem policy in core. DirTranscripts
// names files from the attempt facts visible at the adapter boundary; another
// store can choose a different name without changing RunSpec.
type TranscriptStore interface {
	// Open returns the two sinks one attempt writes: the raw harness stream, and
	// the canonical rendered prompt retained beside it (SPEC §9.5). Both are
	// non-nil on success. The adapter closes the transcript when the process
	// ends; Start closes the prompt sink as soon as it has written to it.
	//
	// Two sinks rather than a store that writes the prompt itself, because the
	// redaction is Start's (SPEC §10.3): it holds the credential values, and a
	// store handed them would be a second place that has to remember to use
	// them. A store that opens a file is not a store that decides what may be
	// written to it.
	Open(spec core.RunSpec) (transcript, prompt io.WriteCloser, err error)
}

// PromptSuffix is what the retained prompt file is named beside its transcript
// (SPEC §9.5). Exported so a reader — `ben status`, a forensic script, #52's
// redaction pass — can find the pair without restating the convention.
const PromptSuffix = ".prompt.txt"

// DirTranscripts writes one `.jsonl` per attempt into a directory, and beside it
// the exact prompt bytes that attempt was given.
type DirTranscripts struct {
	Dir string
	// Now defaults to time.Now; injectable so a test can assert the name.
	Now func() time.Time
}

// Open creates both of an attempt's files.
//
// The prompt first, so a stem collision refuses before a transcript exists for
// an attempt that will not launch — and 0600 with O_EXCL on both, which is what
// makes "one file per attempt" a property of the filesystem rather than of the
// naming scheme being unique enough.
func (d DirTranscripts) Open(spec core.RunSpec) (io.WriteCloser, io.WriteCloser, error) {
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("transcript dir: %w", err)
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	// Workspace base + start instant: unique per attempt without inventing a
	// run-id contract, and sorts chronologically per issue.
	stem := fmt.Sprintf("%s-%s",
		sanitizeName(filepath.Base(spec.Workspace.Path)),
		now().UTC().Format("20060102T150405.000000000"))

	prompt, err := os.OpenFile(filepath.Join(d.Dir, stem+PromptSuffix), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("prompt file: %w", err)
	}
	transcript, err := os.OpenFile(filepath.Join(d.Dir, stem+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		prompt.Close()
		return nil, nil, fmt.Errorf("transcript file: %w", err)
	}
	return transcript, prompt, nil
}

// retainPrompt writes one attempt's canonical rendered prompt, redacted
// (SPEC §9.5, §10.3).
//
// The bytes are Launch.Prompt, which is the same value that becomes the child's
// stdin — not a second render. A re-render would be a different question ("what
// would this attempt be told now") wearing the answer's name.
//
// Redacted through the same writer the transcript uses, and the whole prompt is
// one Write, so a multi-line credential matches here even where the transcript's
// line-at-a-time stream could only match per line (see needles).
//
// A nil sink is retention switched off, not a failure: NopTranscripts is the
// default when no state dir is configured, and a missing state dir must never
// cost a run.
func retainPrompt(sink io.WriteCloser, prompt string, redact []string) error {
	if sink == nil {
		return nil
	}
	w := redactTranscript(sink, redact)
	if _, err := io.WriteString(w, prompt); err != nil {
		w.Close()
		return fmt.Errorf("writing the retained prompt: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing the retained prompt: %w", err)
	}
	return nil
}

// sanitizeName keeps a transcript filename to characters that cannot surprise
// a shell, a filesystem, or a log reader.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}

// NopTranscripts discards both sinks — the default when no store is configured,
// so a missing state dir never costs a run.
type NopTranscripts struct{}

func (NopTranscripts) Open(core.RunSpec) (io.WriteCloser, io.WriteCloser, error) {
	return nopWriteCloser{}, nopWriteCloser{}, nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
