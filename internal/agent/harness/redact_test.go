package harness

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// sinkWriter records what actually reached the sink, per write.
type sinkWriter struct {
	writes []string
	closed bool
	err    error
}

func (s *sinkWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.writes = append(s.writes, string(p))
	return len(p), nil
}

func (s *sinkWriter) Close() error { s.closed = true; return nil }

func (s *sinkWriter) text() string { return strings.Join(s.writes, "") }

// The floor is measured per needle — raw and escaped separately — so a value
// whose literal spelling is under it can still be redacted in the spelling an
// encoder gives it. That asymmetry is the whole reason the two are separate
// needles rather than one value with two forms.
func TestNeedleLengthFloorBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    string
		written  string
		redacted bool
	}{
		{"one byte under the floor", "1234567", "x 1234567 y", false},
		{"exactly at the floor", "12345678", "x 12345678 y", true},
		{"one byte over", "123456789", "x 123456789 y", true},
		{
			// 7 bytes raw, 8 escaped: the literal stays, the escaped spelling goes.
			name:     "under the floor raw, at it escaped",
			value:    `123456"`,
			written:  `x 123456\" y`,
			redacted: true,
		},
		{
			name:     "under the floor raw, and the literal is left alone",
			value:    `123456"`,
			written:  `x 123456" y`,
			redacted: false,
		},
		{
			// 8 raw, 9 escaped: both qualify, and both must go.
			name:     "at the floor raw, over it escaped",
			value:    `1234567"`,
			written:  `x 1234567" and 1234567\" y`,
			redacted: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &sinkWriter{}
			w := redactTranscript(sink, []string{tc.value})
			if _, err := w.Write([]byte(tc.written)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			got := sink.text()
			if redacted := strings.Contains(got, redactedMarker); redacted != tc.redacted {
				t.Errorf("wrote %q, sink got %q; redacted = %t, want %t",
					tc.written, got, redacted, tc.redacted)
			}
			if tc.redacted && strings.Contains(got, tc.written) {
				t.Errorf("sink got the input unchanged: %q", got)
			}
		})
	}
}

// A value containing another must be replaced whole, whatever order the values
// arrive in — otherwise the shorter one eats its prefix and leaves the rest of
// the longer one on disk.
func TestRedactingWriterReplacesLongestFirst(t *testing.T) {
	const (
		short = "ghp-abcdefgh"
		long  = short + "-with-more-secret"
	)
	for _, values := range [][]string{{short, long}, {long, short}} {
		sink := &sinkWriter{}
		w := redactTranscript(sink, values)
		if _, err := w.Write([]byte("token=" + long + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got, want := sink.text(), "token="+redactedMarker+"\n"; got != want {
			t.Errorf("values %v: sink got %q, want %q", values, got, want)
		}
	}
}

// The transforming-writer contract: the caller's bytes were all consumed, even
// though fewer reached the sink. A short count reads as a partial write.
func TestRedactingWriterReportsTheCallerLength(t *testing.T) {
	sink := &sinkWriter{}
	w := redactTranscript(sink, []string{"ghp-abcdefgh"})
	in := []byte("token=ghp-abcdefgh\n")
	n, err := w.Write(in)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(in) {
		t.Errorf("Write = %d, want %d (the caller's length, not the sink's)", n, len(in))
	}
	if got := len(sink.text()); got >= len(in) {
		t.Errorf("sink got %d bytes; the redaction did not shorten anything", got)
	}
}

// The retained event field (SPEC §10.3, §9.6, #61): the orchestrator puts a
// bounded tail of core.Event.Text into the next attempt's prompt, so a credential
// the agent echoed must not be in it.
//
// Both halves in one test, because each is only half the claim. The translator
// sees the line the process wrote, byte for byte — that is what keeps the run's
// verdict independent of redaction — and the event that comes back out of it does
// not carry the credential.
func TestEmittedEventTextIsRedacted(t *testing.T) {
	const credential = "ghp-canary-0123456789"
	var seen []string
	handle, err := Start(context.Background(), Launch{
		Name: "event-redaction",
		Argv: []string{"/bin/sh", "-c", "echo saw " + credential},
		Env:  []string{"PATH=/usr/bin:/bin"},
		Dir:  t.TempDir(),
		Translate: func(line []byte) []core.Event {
			seen = append(seen, string(line))
			return []core.Event{
				{Type: core.EventProgress, Text: string(line)},
				{Type: core.EventSucceeded},
			}
		},
		Redact:  []string{credential},
		Timings: Timings{StopGrace: 20 * time.Millisecond, PostExitDrain: 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var texts []string
	for ev := range handle.Events() {
		texts = append(texts, ev.Text)
	}
	<-handle.Done()

	joinedInput, joinedText := strings.Join(seen, "\n"), strings.Join(texts, "\n")
	// The line reached translation at all, so the absence below proves something.
	if !strings.Contains(joinedInput, credential) {
		t.Fatalf("the translator never saw the credential, so nothing here proves anything: %q", joinedInput)
	}
	if strings.Contains(joinedText, credential) {
		t.Errorf("event text leaks the credential: %q", joinedText)
	}
	// And what is left is still an account of the line, not a row of markers.
	if !strings.Contains(joinedText, "saw ") {
		t.Errorf("event text lost the non-credential half of the line: %q", joinedText)
	}
}

// readStdout hands the pump the same slice it writes to the transcript, so a
// writer that redacted in place would rewrite the line the translator reads —
// and a verdict read off redacted text is a different verdict.
//
// Asserted here, directly on the property, because the conformance suite can no
// longer stand in for it: handle.emit now redacts core.Event.Text deliberately
// (#61), so an in-place edit and the intended redaction produce the same event
// stream. This is the assertion that tells them apart.
func TestWriteDoesNotEditTheCallersBuffer(t *testing.T) {
	const line = "token=ghp-abcdefgh and more\n"
	in := []byte(line)
	sink := &sinkWriter{}
	w := redactTranscript(sink, []string{"ghp-abcdefgh"})
	if _, err := w.Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if string(in) != line {
		t.Errorf("Write edited the caller's buffer: %q, want %q", in, line)
	}
	if strings.Contains(sink.text(), "ghp-abcdefgh") {
		t.Errorf("nothing was redacted, so the buffer's survival proves nothing:\n%s", sink.text())
	}
}

func TestRedactingWriterPropagatesSinkFailures(t *testing.T) {
	fail := errors.New("disk full")
	sink := &sinkWriter{err: fail}
	w := redactTranscript(sink, []string{"ghp-abcdefgh"})
	if _, err := w.Write([]byte("token=ghp-abcdefgh\n")); !errors.Is(err, fail) {
		t.Errorf("Write = %v, want %v", err, fail)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if !sink.closed {
		t.Error("Close did not reach the sink: the forensic record is never closed")
	}
}

// With nothing eligible, the sink is handed back untouched rather than wrapped:
// a no-op wrapper is a copy of every transcript byte for nothing.
func TestRedactTranscriptSkipsWrappingWithNothingToRedact(t *testing.T) {
	sink := &sinkWriter{}
	for _, values := range [][]string{nil, {}, {""}, {"short"}} {
		if got := redactTranscript(sink, values); got != io.WriteCloser(sink) {
			t.Errorf("values %v: got a wrapper, want the sink itself", values)
		}
	}
}

// The set is a projection of SensitiveFields, over blocks nobody has validated:
// Load never runs Structural, so this is asked of malformed input too (#47).
func TestCredentialValues(t *testing.T) {
	creds := []CredentialKey{
		{ProviderKey: "api_key", Env: "X_API_KEY"},
		{ProviderKey: "auth_token", Env: "X_AUTH_TOKEN"},
	}
	for _, tc := range []struct {
		name  string
		block map[string]any
		want  []string
	}{
		{
			name: "every credential key and every env value",
			block: map[string]any{
				"api_key":    "key-secret",
				"auth_token": "token-secret",
				"env": map[string]any{
					"GH_TOKEN": "publish-secret",
					"NOISE":    "flag-value",
				},
			},
			want: []string{"key-secret", "token-secret", "publish-secret", "flag-value"},
		},
		{
			name:  "a declared key the block does not set contributes nothing",
			block: map[string]any{"api_key": "key-secret"},
			want:  []string{"key-secret"},
		},
		{
			name: "non-string leaves are not credentials",
			block: map[string]any{
				"api_key": 42,
				"env":     map[string]any{"DEBUG": true, "PORT": 8080, "EMPTY": ""},
			},
			want: nil,
		},
		{
			name: "a subtree contributes every string inside it",
			block: map[string]any{
				"env": map[string]any{
					"NESTED": map[string]any{"inner": "inner-secret", "n": 1},
					"LIST":   []any{"list-secret", 2},
				},
			},
			want: []string{"list-secret", "inner-secret"},
		},
		{
			name:  "env is not a map",
			block: map[string]any{"env": "not-a-map", "api_key": "key-secret"},
			want:  []string{"key-secret"},
		},
		{name: "empty block", block: map[string]any{}, want: nil},
		{name: "nil block", block: nil, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CredentialValues(tc.block, creds)
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("CredentialValues = %q, want %q", got, want)
			}
		})
	}
}

// The projection is what keeps one declaration authoritative: a key dropped from
// a credential table disappears from display redaction and from the transcript
// set together, rather than from one of them.
func TestCredentialValuesTracksTheDeclaration(t *testing.T) {
	block := map[string]any{"api_key": "key-secret", "auth_token": "token-secret"}
	full := []CredentialKey{{ProviderKey: "api_key"}, {ProviderKey: "auth_token"}}
	if got := CredentialValues(block, full); len(got) != 2 {
		t.Fatalf("CredentialValues = %q, want both credentials", got)
	}
	short := []CredentialKey{{ProviderKey: "api_key"}}
	got := CredentialValues(block, short)
	if slices.Contains(got, "token-secret") {
		t.Errorf("CredentialValues = %q, reporting a value no table names", got)
	}
	if fields := SensitiveFields(block, short); len(fields) != len(got) {
		t.Errorf("SensitiveFields names %d paths but CredentialValues found %d values: "+
			"the two have stopped agreeing", len(fields), len(got))
	}
}

// A credential may contain a newline — StringMap permits one (block.go) — and
// readStdout writes one line per call, so the value's halves land in different
// writes and a needle spelled with the newline matches neither. The transcript is
// the concatenation, so it carries the credential intact.
//
// Regression: the whole-line property held and the leak happened anyway.
func TestMultilineCredentialSurvivesNeitherWrite(t *testing.T) {
	const value = "abcdefgh\nijklmnop"
	sink := &sinkWriter{}
	w := redactTranscript(sink, []string{value})
	for _, line := range strings.SplitAfter(value, "\n") {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got := sink.text()
	for _, half := range strings.Split(value, "\n") {
		if strings.Contains(got, half) {
			t.Errorf("transcript carries %q of the multiline credential: %q", half, got)
		}
	}
	if strings.Contains(got, value) {
		t.Errorf("transcript carries the whole multiline credential: %q", got)
	}
}

// Replacing one needle can synthesize another whose turn has already passed, so
// a single ordered pass is not enough: the output must be a fixed point.
func TestRedactionReachesAFixedPoint(t *testing.T) {
	// Ordered longest-first, "***suffix" is processed before "abcdefgh" creates it.
	values := []string{redactedMarker + "suffix", "abcdefgh"}
	sink := &sinkWriter{}
	w := redactTranscript(sink, values)
	if _, err := w.Write([]byte("token=abcdefghsuffix\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := sink.text()
	for _, v := range values {
		if strings.Contains(got, v) {
			t.Errorf("transcript carries %q after redaction: %q", v, got)
		}
	}
}

// The fixed-point loop terminates because a replacement always shortens, which
// requires the floor to exceed the marker. Pinned rather than commented: lower
// minNeedle below this and Write can loop forever.
func TestFloorExceedsTheMarker(t *testing.T) {
	if minNeedle <= len(redactedMarker) {
		t.Fatalf("minNeedle = %d, redactedMarker = %q: a replacement no longer shortens, "+
			"so the fixed-point loop in Write need not terminate", minNeedle, redactedMarker)
	}
}

// The scanner strips a trailing carriage return, so a CRLF value's lines reach
// the transcript without it. The trimmed spelling is the needle that matches.
func TestCRLFCredentialIsRedactedAsTheScannerPresentsIt(t *testing.T) {
	const value = "abcdefgh\r\nijklmnop"
	sink := &sinkWriter{}
	w := redactTranscript(sink, []string{value})
	// What readStdout writes: bufio.ScanLines drops the \r with the \n.
	for _, line := range []string{"abcdefgh\n", "ijklmnop\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got := sink.text()
	for _, half := range []string{"abcdefgh", "ijklmnop"} {
		if strings.Contains(got, half) {
			t.Errorf("transcript carries %q as the scanner presented it: %q", half, got)
		}
	}
}

// The one shape the stateless writer cannot cover: eligible as a value, with no
// line the harness writes that is eligible on its own. Refused at load, so the
// invariant holds rather than gaining an exception.
func TestCheckRedactable(t *testing.T) {
	sentinel := errors.New("test: invalid provider value")
	creds := []CredentialKey{{ProviderKey: "api_key", Env: "X_API_KEY"}}
	for _, tc := range []struct {
		name    string
		value   string
		refused bool
	}{
		{"single line at the floor", "abcdefgh", false},
		{"single line well past it", strings.Repeat("x", 40), false},
		{"multiline with both lines eligible", "abcdefgh\nijklmnop", false},
		{"multiline with one eligible line", "abc\nijklmnop", false},
		{
			// The reported case: 8 bytes, so the invariant covers it; lines of 3
			// and 4, so no needle can match a write.
			name: "multiline with every line under the floor", value: "abc\n1234", refused: true,
		},
		{name: "CRLF with every line under the floor", value: "abc\r\n1234", refused: true},
		{name: "CRLF with an eligible trimmed line", value: "abcdefgh\r\n123"},
		// The block's first line is eight bytes; the scanner's is seven, because
		// ScanLines takes the carriage return with the newline. Coverage has to be
		// read off the spelling the transcript will carry.
		{name: "CRLF line eligible only before the scanner trims it", value: "1234567\r\nx", refused: true},
		{name: "CRLF line still eligible after trimming", value: "12345678\r\nx"},
		// The same defect with no second line at all.
		{name: "single line eligible only before the trim", value: "1234567\r", refused: true},
		{name: "single line still eligible after trimming", value: "12345678\r"},
		{name: "three short lines", value: "abc\ndef\nghi", refused: true},
		{
			// Under the floor as a value: outside the invariant, and the documented
			// limit rather than a refusal.
			name: "under the floor entirely", value: "abc\n12", refused: false,
		},
		{name: "a PEM-shaped value is not refused", value: "-----BEGIN PRIVATE KEY-----\n" +
			strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7", 2) +
			"\n-----END PRIVATE KEY-----"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, block := range []map[string]any{
				{"api_key": tc.value},
				{"env": map[string]any{"SOME_TOKEN": tc.value}},
			} {
				err := CheckRedactable(block, creds, sentinel)
				if tc.refused != (err != nil) {
					t.Fatalf("CheckRedactable(%q) = %v, refused = %t, want %t",
						tc.value, err, err != nil, tc.refused)
				}
				if err == nil {
					continue
				}
				if !errors.Is(err, sentinel) {
					t.Errorf("refusal does not wrap the adapter's sentinel: %v", err)
				}
				// The value being refused is the secret (boundary 2's call).
				if strings.Contains(err.Error(), tc.value) {
					t.Errorf("refusal echoes the credential: %v", err)
				}
				for _, line := range strings.Split(tc.value, "\n") {
					if len(line) > 2 && strings.Contains(err.Error(), line) {
						t.Errorf("refusal echoes a line of the credential: %v", err)
					}
				}
			}
		})
	}
}

// The refusal names the field it read, so an operator can find it without the
// value being shown.
func TestCheckRedactableNamesTheField(t *testing.T) {
	sentinel := errors.New("test: invalid provider value")
	creds := []CredentialKey{{ProviderKey: "api_key", Env: "X_API_KEY"}}
	err := CheckRedactable(map[string]any{
		"env": map[string]any{"SHORT_LINES": "abc\n1234"},
	}, creds, sentinel)
	if err == nil {
		t.Fatal("CheckRedactable = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "env.SHORT_LINES") {
		t.Errorf("refusal does not name the field: %v", err)
	}
}

// The accepted half of the CRLF boundary: a value the check admits must actually
// be redacted, in the spelling the scanner hands over.
func TestCRLFEligibleAfterTrimIsRedacted(t *testing.T) {
	const value = "12345678\r\nx"
	if err := CheckRedactable(map[string]any{"api_key": value},
		[]CredentialKey{{ProviderKey: "api_key"}}, errors.New("test")); err != nil {
		t.Fatalf("CheckRedactable = %v, want the value accepted", err)
	}
	sink := &sinkWriter{}
	w := redactTranscript(sink, []string{value})
	// What readStdout writes: ScanLines drops the \r with the \n.
	for _, line := range []string{"12345678\n", "x\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := sink.text(); strings.Contains(got, "12345678") {
		t.Errorf("transcript carries the eligible line as the scanner presented it: %q", got)
	}
}

// The passthrough half of the refusal. Same predicate, different provenance: the
// shape is the host's, so it can only be asked once the environment is read.
func TestCheckRedactableEnv(t *testing.T) {
	sentinel := errors.New("test: invalid provider value")
	for _, tc := range []struct {
		name      string
		forwarded map[string]string
		refused   string // the field the refusal must name, "" for accepted
	}{
		{name: "nothing forwarded", forwarded: nil},
		{name: "a redactable token", forwarded: map[string]string{"env_passthrough.FORWARDED_PAT": "ghp-abcdefghijkl"}},
		{name: "a short value is under the floor", forwarded: map[string]string{"env_passthrough.NODE_ENV": "dev"}},
		{
			name:      "no scanner-visible line is eligible",
			forwarded: map[string]string{"env_passthrough.FORWARDED_PAT": "abc\n1234"},
			refused:   "env_passthrough.FORWARDED_PAT",
		},
		{
			name:      "eligible only before the scanner trims",
			forwarded: map[string]string{"env_passthrough.FORWARDED_PAT": "1234567\r"},
			refused:   "env_passthrough.FORWARDED_PAT",
		},
		{
			// Deterministic: sorted names, so the same block always names the same
			// field rather than whichever the map handed over first.
			name: "two bad values refuse the first by name",
			forwarded: map[string]string{
				"env_passthrough.ZZ_LAST_PAT":  "abc\n1234",
				"env_passthrough.AA_FIRST_PAT": "abc\n5678",
			},
			refused: "env_passthrough.AA_FIRST_PAT",
		},
		{
			// The publish credential reports here too, and the key is why: it is
			// the site the refusal names, so an operator is not sent to an
			// `env_passthrough` entry that does not exist (SPEC §5.2.8).
			name:      "an unredactable publish credential names its own site",
			forwarded: map[string]string{"publish.value": "abc\n1234"},
			refused:   "publish.value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckRedactableEnv(tc.forwarded, sentinel)
			if tc.refused == "" {
				if err != nil {
					t.Fatalf("CheckRedactableEnv = %v, want ok", err)
				}
				return
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("CheckRedactableEnv = %v, want %v", err, sentinel)
			}
			if !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("refusal does not name %s: %v", tc.refused, err)
			}
			for _, v := range tc.forwarded {
				if strings.Contains(err.Error(), v) {
					t.Errorf("refusal echoes a forwarded value: %v", err)
				}
			}
		})
	}
}
