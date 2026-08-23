package workspace

import (
	"fmt"
	"hash/fnv"
	"io"
	"regexp"
	"strings"
)

var keyUnsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Key derives the workspace key from an issue identifier (SPEC §6.3,
// invariant 3): sanitize to [A-Za-z0-9._-]; if sanitization changed the
// identifier — or left something unusable as a directory name or as a
// component of the ben/<key> branch ref — append the FNV-1a hash of the
// original, 64 bits as 16 hex characters, meeting the ≥64-bit
// collision-resistance floor.
func Key(identifier string) string {
	s := keyUnsafeRe.ReplaceAllString(identifier, "_")
	if s == identifier && refComponentSafe(s) {
		return s
	}
	h := fnv.New64a()
	io.WriteString(h, identifier)
	suffix := fmt.Sprintf("%016x", h.Sum64())
	// Within the sanitized charset every git-check-ref-format(1) hazard
	// involves a dot (leading ".", "..", trailing ".", ".lock"), so
	// flattening dots makes the prefix safe; the suffix keeps distinct
	// originals distinct.
	prefix := strings.ReplaceAll(s, ".", "_")
	if prefix == "" {
		prefix = "issue"
	}
	return prefix + "-" + suffix
}

// refComponentSafe reports whether s is usable both as a directory name and
// as the final component of refs/heads/ben/<s> (git-check-ref-format(1)).
// The sanitized charset already excludes every other hazard git names.
func refComponentSafe(s string) bool {
	switch {
	case s == "":
		return false
	case strings.HasPrefix(s, "."), strings.HasSuffix(s, "."):
		return false
	case strings.Contains(s, ".."), strings.HasSuffix(s, ".lock"):
		return false
	}
	return true
}
