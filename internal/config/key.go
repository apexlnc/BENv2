package config

import (
	"fmt"
	"hash/fnv"
	"path/filepath"
	"regexp"
)

var keyUnsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// sanitizeKey applies the workspace-key charset rule (SPEC §6.3): only
// [A-Za-z0-9._-] survives; everything else becomes "_".
func sanitizeKey(s string) string {
	return keyUnsafeRe.ReplaceAllString(s, "_")
}

// KeyFor derives the workflow key for a WORKFLOW.md path without loading it
// (SPEC §5.1). It is what names the state directory of §10.3, and `ben status`
// needs that directory for a daemon whose WORKFLOW.md is currently **broken** —
// which is the state an operator is most likely to be inspecting. A key that
// could only be obtained by a successful Load would make the status surface
// unavailable exactly when it is wanted.
//
// The key is a function of the path and nothing else, so this reads no file. It
// cannot disagree with what Load computes because Load calls it: two
// derivations of one name is a daemon writing to one state directory while
// `ben status` reads another.
//
// The error is from resolving path to an absolute one, which is what the key
// hashes — a relative path would give two names to one workflow.
func KeyFor(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", path, err)
	}
	return workflowKey(abs), nil
}

// workflowKey derives the stable workflow identity (SPEC §5.1): the sanitized
// basename of the directory containing WORKFLOW.md, a hyphen, and the first
// 8 hex characters of the FNV-1a hash of the file's absolute path. It names
// the data and state directories and appears in daemon identity strings.
func workflowKey(absPath string) string {
	h := fnv.New64a()
	h.Write([]byte(absPath))
	digest := fmt.Sprintf("%016x", h.Sum64())
	return sanitizeKey(filepath.Base(filepath.Dir(absPath))) + "-" + digest[:8]
}
