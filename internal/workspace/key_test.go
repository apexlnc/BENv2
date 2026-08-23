package workspace

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"testing"
)

func fnv16(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return fmt.Sprintf("%016x", h.Sum64())
}

func TestKey(t *testing.T) {
	parallel(t)
	tests := []struct {
		name       string
		identifier string
		want       string
	}{
		{"github issue number", "42", "42"},
		{"already safe", "ABC-1.2_x", "ABC-1.2_x"},
		{"inner dots stay", "v1.0", "v1.0"},
		{"slash sanitized with suffix", "a/b", "a_b-" + fnv16("a/b")},
		{"hash sanitized with suffix", "a#b", "a_b-" + fnv16("a#b")},
		{"spaces", "issue 7", "issue_7-" + fnv16("issue 7")},
		{"empty", "", "issue-" + fnv16("")},
		{"dot", ".", "_-" + fnv16(".")},
		{"dotdot", "..", "__-" + fnv16("..")},
		{"leading dot", ".x", "_x-" + fnv16(".x")},
		{"trailing dot", "x.", "x_-" + fnv16("x.")},
		{"lock suffix", "x.lock", "x_lock-" + fnv16("x.lock")},
		{"double dot", "..x", "__x-" + fnv16("..x")},
		{"double dot inside", "a..b", "a__b-" + fnv16("a..b")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Key(tt.identifier); got != tt.want {
				t.Errorf("Key(%q) = %q, want %q", tt.identifier, got, tt.want)
			}
		})
	}
}

// Identifiers that sanitize to the same string must still get distinct keys —
// the ≥64-bit suffix is the collision resistance (SPEC §6.3, invariant 3).
func TestKeyCollisionResistance(t *testing.T) {
	parallel(t)
	a, b := Key("a/b"), Key("a#b")
	if a == b {
		t.Fatalf("Key(a/b) == Key(a#b) == %q; suffix must disambiguate", a)
	}
	suffix := a[strings.LastIndex(a, "-")+1:]
	if len(suffix) != 16 {
		t.Errorf("suffix %q is %d hex chars, want 16 (≥64 bits)", suffix, len(suffix))
	}
}

var keyCharsetRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

var hostileIdentifiers = []string{
	"42", "", ".", "..", ".x", "x.", "..x", "x.lock", "a..b", "v1.0",
	"-x", "a/b", "ödipus", "x y z", "🎻", "a\\b", "a@{b", "-lead",
}

func TestKeyCharset(t *testing.T) {
	parallel(t)
	for _, id := range hostileIdentifiers {
		got := Key(id)
		if !keyCharsetRe.MatchString(got) {
			t.Errorf("Key(%q) = %q violates the [A-Za-z0-9._-] charset", id, got)
		}
		if got == "." || got == ".." {
			t.Errorf("Key(%q) = %q is not usable as a directory name", id, got)
		}
	}
}

// Every key must form a valid branch ref — git-check-ref-format(1) is the
// authority, so ask git itself.
func TestKeyFormsValidGitRef(t *testing.T) {
	parallel(t)
	dir := t.TempDir()
	for _, id := range hostileIdentifiers {
		ref := "refs/heads/ben/" + Key(id)
		runGit(t, dir, "check-ref-format", ref)
	}
}
