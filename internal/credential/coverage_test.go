package credential_test

import (
	"slices"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
)

// Every registered `credential_sources` kind runs credtest.Contract — including
// its cross-kind rule that a remote endpoint must be https (#245).
//
// This is the independent boundary that assertion is anchored at. The suite is
// derived from each case's own block, so a kind that runs it cannot omit the TLS
// check; this pins the other half, that the set of kinds running it is the
// registry's closed set and does not shrink when a kind is added. A new kind
// registered without a conformance case fails here rather than shipping with no
// contract at all.
//
// It lives in credential_test because `internal/registry` imports
// `internal/credential`: package credential cannot import the registry back.
func TestTheConformanceSuiteCoversTheClosedSourceSet(t *testing.T) {
	got, want := credential.ConformanceKinds(), registry.SourceNames()
	if !slices.Equal(got, want) {
		t.Errorf("kinds running credtest.Contract = %v, want the registered set %v", got, want)
	}
}
