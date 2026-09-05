package remote

import (
	"crypto/sha256"
	"encoding/hex"
)

// PromptDigest is the durable answer to "what was this agent told" that
// Record.PromptDigest holds (SPEC §9.5).
//
// The full hex digest, not a short one. This is the value an operator compares
// against a retained prompt to prove the two are the same bytes, and a truncated
// digest turns a proof into a likelihood for no saving worth having in a file
// that is already kilobytes.
func PromptDigest(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// shortDigest disambiguates a sanitized filename (DirStore.Path). Short is
// correct here for the reason the full digest is correct above: nobody compares
// it, it only has to not collide, and it shares its filename with a readable
// prefix that says which claim it is.
func shortDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}
