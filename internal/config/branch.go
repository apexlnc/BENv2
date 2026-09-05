package config

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// validateBaseBranch applies the exact public grammar of workspace.base_branch.
// The final predicate is git-check-ref-format(1)'s ref grammar over
// refs/heads/<value>, implemented without a subprocess so structural workflow
// validation stays offline and credential-free.
func validateBaseBranch(branch string) error {
	switch {
	case !utf8.ValidString(branch):
		return fmt.Errorf("must be valid UTF-8")
	case len(branch) == 0 || len(branch) > 255:
		return fmt.Errorf("must contain 1 to 255 bytes")
	case strings.HasPrefix(branch, "-"):
		return fmt.Errorf("must not start with '-'")
	case strings.HasPrefix(branch, "refs/"):
		return fmt.Errorf("must not start with 'refs/'")
	case strings.HasPrefix(branch, "origin/"):
		return fmt.Errorf("must not start with 'origin/'")
	case branch == "ben" || strings.HasPrefix(branch, "ben/"):
		return fmt.Errorf("must not use BEN's reserved 'ben' branch namespace")
	case !validGitRef("refs/heads/" + branch):
		return fmt.Errorf("refs/heads/%s is not a valid Git ref", branch)
	default:
		return nil
	}
}

func validGitRef(ref string) bool {
	if ref == "" || ref == "@" || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "//") || strings.Contains(ref, "..") ||
		strings.Contains(ref, "@{") || strings.ContainsRune(ref, '\\') {
		return false
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	for _, r := range ref {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[", r) {
			return false
		}
	}
	return true
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
