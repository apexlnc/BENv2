package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// Transcript redaction (SPEC §10.3, §7.2, §10.2).
//
// A transcript is retained verbatim for forensics, at 0600, with no expiry —
// and agents echo their environment: `env | sort` in a debug step, a tool error
// quoting request headers, `gh` printing a failed URL. Mode bounds who on the
// host can read the file; it says nothing about the copy someone pastes into an
// issue, which is the leak boundary 1 closed for `config effective`.
//
// The set is a union of three layers, each read where its provenance exists: the
// declared credential values and every `env` value — those two are SensitiveFields
// projected onto values, via CredentialValues — and the values no declaration
// holds, which Environ returns because it is what resolved them: the
// env_passthrough pairs, and the publish credential (SPEC §5.2.8), whose value
// exists only for the attempt it was read for. All matched
// literally. This is deliberately not a search for things that look like secrets:
// a pattern that hunts for arbitrary credentials is an untestable promise, and one
// that misses is worse than one that was never made.
//
// Not the whole child environment, and not by subtraction. `injected` mixes a
// credential with a filesystem path by design (codex-exec's CODEX_HOME), and the
// allowlist carries PATH, HOME, SHELL and the locale — replacing those spellings
// would damage the record retention exists for. "Not provably public" is not the
// same question as "secret".
//
// So the guarantee is bounded, and the bounds are these:
//
//   - A secret in an *allowlisted* variable is not redacted. core.EnvAllowlist is
//     PATH, HOME, USER, LOGNAME, SHELL, TMPDIR and the locale — operational, and
//     none of them a credential surface. A secret parked in one of those is
//     outside what this can see.
//   - A value the agent re-encodes — base64, URL-escaped, split across two
//     lines — is not the value we hold, and no exact match can see it.
//   - A value under minNeedle is knowingly left alone (see below). At or above
//     it, the guarantee is unconditional: the one shape a stateless writer could
//     not cover — no scanner-visible line eligible on its own — is refused, by
//     CheckRedactable for the block and CheckRedactableEnv for what the daemon's
//     environment forwards, rather than exempted here.
//   - Transcripts written before this existed are not rewritten.

const (
	// redactedMarker matches internal/workspace/git.go's spelling, which already
	// scrubs the tracker credential out of git output. One repo, one spelling.
	// It deliberately does not name the key: a transcript is a forensic record,
	// not a diagnostic, and with duplicate values a label would have to pick one
	// key arbitrarily.
	redactedMarker = "***"

	// minNeedle is the shortest value worth replacing. All of `env` is in the
	// credential surface (see SensitiveFields), so `env: {DEBUG: "1"}` puts "1"
	// in the set, and replacing every "1" would shred the record the redaction
	// exists to preserve. Eight bytes clears every real token shape — `ghp_`,
	// `sk-ant-`, a JWT — while dropping flag-like values.
	//
	// The floor is why the invariant is stated over *eligible* needles: a
	// credential shorter than this is knowingly left in the transcript. Under
	// eight bytes, a credential's secrecy is not what is protecting it.
	minNeedle = 8
)

// needles returns the spellings of values that must not reach a transcript,
// longest first.
//
// Two spellings per value, because the harness re-encodes one of them itself:
// finishTranscript marshals the retained stderr into a ben:stderr line, so a
// credential containing a quote, a backslash or a control byte reaches the file
// JSON-escaped and a literal scrub would walk straight past it.
//
// And a needle per *line* of a multiline value. StringMap permits a newline in a
// credential (block.go), while readStdout writes one line per call — so the two
// halves of `abcdefgh\nijklmnop` land in different writes, a needle spelled with
// the newline matches neither, and the transcript, which is the concatenation,
// carries the value intact. Lines are the unit a write can contain, so they are
// the unit a stateless writer can match. The scanner strips a trailing carriage
// return, so the trimmed spelling is a needle too.
//
// Longest first so a value containing another is replaced whole, which makes the
// result independent of map iteration order.
func needles(values []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if len(s) < minNeedle || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, v := range values {
		for _, s := range rawSpellings(v) {
			add(s)
			add(jsonEscaped(s))
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	return out
}

// rawSpellings returns every unencoded substring of v worth matching: v itself,
// and — since readStdout writes one line per call — each of its lines, both as
// the block spells them and as the scanner presents them.
//
// Wider than scannerLines on purpose. These are needles, and a needle that never
// matches costs one pass; the check below cannot reason from the same list,
// because a spelling no write will contain proves nothing about coverage.
func rawSpellings(v string) []string {
	if !strings.Contains(v, "\n") {
		return append([]string{v}, scannerLines(v)...)
	}
	lines := strings.Split(v, "\n")
	out := make([]string, 0, 2*len(lines)+1)
	out = append(out, v)
	out = append(out, lines...)
	return append(out, scannerLines(v)...)
}

// scannerLines returns the spellings a scanner-produced write can contain:
// bufio.ScanLines hands back each line with its terminator removed, including one
// trailing carriage return. So a block's `1234567\r` reaches the transcript as
// `1234567` — seven bytes, under the floor — and the block's own spelling is not
// what has to be matched.
//
// This is the only list that can prove coverage, which is why CheckRedactable
// reads it rather than rawSpellings.
func scannerLines(v string) []string {
	lines := strings.Split(v, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, strings.TrimSuffix(line, "\r"))
	}
	return out
}

// CheckRedactable refuses a provider block holding a credential the transcript
// writer cannot keep out of the record (SPEC §10.3).
//
// One shape reaches that state: a value at or above the floor — so the invariant
// covers it — none of whose scanner-visible lines is eligible on its own.
// `abc\n1234` is eight bytes and its lines are three and four, so no needle
// matches a write and the concatenated transcript carries the credential exactly.
// Refusing at load is what keeps the invariant true rather than exempted; the
// alternative, buffering across writes, would hold forensic bytes back at the
// moment §7.2 wants them on disk.
//
// Scanner-visible, not as the block spells them, and the distinction has teeth:
// `1234567\r\nx` has an eight-byte first line in the file and a seven-byte one in
// the transcript, since ScanLines takes the carriage return with the newline. The
// untrimmed spelling stays a needle — it costs nothing — but it cannot prove
// coverage of a write that will never contain it. `1234567\r`, with no newline at
// all, is the same defect without the second line.
//
// Narrow by construction: a value at or above the floor whose line the scanner
// hands over intact is its own needle, so a single-line credential and a
// multiline one with any long line — a PEM block, an SSH key — are both accepted.
//
// The refusal names the field and never the value. That is the same call boundary
// 2 made for `RemoteURL`: the thing being refused is the secret.
func CheckRedactable(block map[string]any, credentials []CredentialKey, sentinel error) error {
	var refused error
	eachCredentialValue(block, credentials, func(path []string, v string) {
		if refused == nil && !redactable(v) {
			refused = unredactable(strings.Join(path, "."), sentinel)
		}
	})
	return refused
}

// CheckRedactableEnv is the same refusal for values that are not in the block:
// the resolved env_passthrough pairs and the publish credential that Environ
// returns, each keyed by the site that put it there. They reach the child, and
// therefore a transcript, so they are held to the same standard as a declared
// credential — but their shape is the host's, not the file's, so this can only be
// asked once the daemon's environment has been read.
//
// The key is the field name in the refusal. It arrives already spelled as a config
// site rather than being prefixed here, because there is more than one source now
// and this function cannot tell them apart from a bare variable name.
func CheckRedactableEnv(undeclared map[string]string, sentinel error) error {
	for _, site := range slices.Sorted(maps.Keys(undeclared)) {
		if !redactable(undeclared[site]) {
			return unredactable(site, sentinel)
		}
	}
	return nil
}

// redactable reports whether v is one the transcript writer can keep out of the
// record: under the floor (outside the invariant, and knowingly left alone), or
// with at least one scanner-visible line long enough to match.
func redactable(v string) bool {
	if len(v) < minNeedle {
		return true
	}
	for _, line := range scannerLines(v) {
		if len(line) >= minNeedle {
			return true
		}
	}
	return false
}

// unredactable names the field and never the value.
func unredactable(field string, sentinel error) error {
	return fmt.Errorf("%w: %s is a credential with no line of %d bytes or more once the stream "+
		"scanner has read it, so it cannot be kept out of a retained transcript (SPEC §10.3) — "+
		"put the value on one line, without a trailing carriage return, or give it a line long "+
		"enough to redact. The value is not shown", sentinel, field, minNeedle)
}

// jsonEscaped returns v as it would appear inside a JSON string, or "" when that
// is the same as v — the common case, since real tokens carry no escapable byte.
func jsonEscaped(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	s := string(b[1 : len(b)-1]) // drop the quotes json.Marshal added
	if s == v {
		return ""
	}
	return s
}

// redactor replaces known credential values with the marker. One needle set and
// one algorithm, because there are now two places that must not carry a
// credential — the retained transcript, and the one normalized event field the
// orchestrator retains (see handle.emit, #61) — and a second enumeration of the
// same values is the shape #47 already paid for.
type redactor struct{ needles []string }

// newRedactor returns nil when there is nothing to replace, which every caller
// reads as "hand the bytes through untouched".
func newRedactor(values []string) *redactor {
	n := needles(values)
	if len(n) == 0 {
		return nil
	}
	return &redactor{needles: n}
}

// apply is safe on a nil receiver: no credentials, nothing to do.
//
// The passes run to a fixed point, because replacing one needle can *synthesize*
// another whose turn has already passed: with credentials `***suffix` and
// `abcdefgh`, one ordered pass turns `abcdefghsuffix` into `***suffix`, which is
// a credential the result now carries. Termination is not an assumption —
// minNeedle exceeds len(redactedMarker), so every pass that changes anything
// makes the string strictly shorter.
func (r *redactor) apply(s string) string {
	if r == nil {
		return s
	}
	for {
		before := s
		for _, n := range r.needles {
			s = strings.ReplaceAll(s, n, redactedMarker)
		}
		if s == before {
			return s
		}
	}
}

// redactingWriter replaces known credential values on their way to a transcript
// sink. Start wraps every sink in one, so a store cannot opt out and a store
// added later cannot forget (SPEC §10.3).
//
// It is stateless — no bytes retained between writes — which rests on two facts
// together, not on one:
//
//   - The harness writes whole lines: readStdout emits one scanner token plus its
//     newline per call, and finishTranscript one marshalled line. So a write
//     boundary is a line boundary, never an arbitrary offset.
//   - Every value in the set has a needle that fits inside one such line. That is
//     not free — a multiline value's own needle *does* straddle writes, and only
//     its lines match — so CheckRedactable refuses the values for which no
//     eligible line exists, at load.
//
// Drop either and a credential crosses a boundary unmatched. The first is a
// property of code that could change, so the conformance suite asserts it at the
// sink: switch the reader to a chunked copy and a test fails instead of a
// credential leaking. The second is a property of configuration, so it is a
// refusal rather than an assertion.
type redactingWriter struct {
	w io.WriteCloser
	r *redactor
}

// redactTranscript wraps w unless there is nothing to redact, in which case the
// sink is handed back untouched.
func redactTranscript(w io.WriteCloser, values []string) io.WriteCloser {
	r := newRedactor(values)
	if r == nil {
		return w
	}
	return &redactingWriter{w: w, r: r}
}

// Write reports len(p) on success: the caller's bytes were all consumed, even
// though fewer reached the sink. Returning the short count would read as a
// partial write and is the io.Writer contract's other failure mode.
//
// It never writes through p. readStdout hands the pump the same slice it writes
// here, so a writer that redacted in place would rewrite the line the translator
// reads — and a verdict read off redacted text is a different verdict. Asserted
// directly by TestWriteDoesNotEditTheCallersBuffer, because the event stream can
// no longer stand in for it: handle.emit redacts the one field a consumer
// retains, deliberately (#61).
func (r *redactingWriter) Write(p []byte) (int, error) {
	if _, err := r.w.Write([]byte(r.r.apply(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *redactingWriter) Close() error { return r.w.Close() }
