package credential_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
)

// `static.value` is exactly one `$VAR` reference — not a literal, not an
// interpolation (SPEC §5.5, amendment 3; mutation 20).
//
// A literal would put a credential in a repo-owned file and in every
// `config effective` rendering of it; an interpolation would make one token the
// concatenation of several secrets, none of which has an identity §10.2's split
// check could compare.
func TestStaticValueMustBeExactlyOneReference(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		ok          bool
	}{
		{"one reference", "$GH_TOKEN", true},
		{"a literal credential", "ghp-actually-a-secret", false},
		{"an interpolation", "$PREFIX-$GH_TOKEN", false},
		{"a reference with a suffix", "$GH_TOKEN-extra", false},
		{"a reference with surrounding whitespace", " $GH_TOKEN ", false},
		{"empty", "", false},
		{"lowercase, which $VAR syntax does not accept", "$gh_token", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (credential.StaticKind{}).Describe(map[string]any{"kind": "static", "value": tt.value})
			switch {
			case tt.ok && err != nil:
				t.Errorf("Describe(%q) = %v, want ok", tt.value, err)
			case !tt.ok && !errors.Is(err, credential.ErrSourceValueNotReference):
				t.Errorf("Describe(%q) = %v, want ErrSourceValueNotReference", tt.value, err)
			}
		})
	}
	// The refusal never echoes the value: the one thing an operator writes here
	// by mistake is the credential itself, and a loader that echoed it would put
	// a live token in the CI log of the run that refused it.
	t.Run("the refusal echoes nothing", func(t *testing.T) {
		_, err := (credential.StaticKind{}).Describe(map[string]any{"kind": "static", "value": "ghp-live-token"})
		if err == nil || strings.Contains(err.Error(), "ghp-live-token") {
			t.Errorf("refusal = %v, want one that names no value", err)
		}
	})
}

// A `static` source is **explicitly unbounded**: it states no deadline, so every
// TTL gate is skipped for it and every configuration that loads today stays
// valid (mutations 2, 3).
func TestStaticIsExplicitlyUnbounded(t *testing.T) {
	t.Setenv("BEN_TEST_UNBOUNDED", "ghp-value")
	d, err := (credential.StaticKind{}).Describe(map[string]any{"kind": "static", "value": "$BEN_TEST_UNBOUNDED"})
	if err != nil {
		t.Fatal(err)
	}
	if d.MinFreshTTL != 0 || d.Bounded() {
		t.Fatalf("descriptor = %+v, want MinFreshTTL zero — explicitly unbounded", d)
	}
	src := credential.NewEnv("BEN_TEST_UNBOUNDED")
	tok, err := src.FetchFresh(context.Background(), core.PurposePublish)
	if err != nil {
		t.Fatal(err)
	}
	if !tok.UsableUntil.IsZero() {
		t.Errorf("UsableUntil = %s, want the zero value", tok.UsableUntil)
	}
}

// An unset variable is a **permanent** fetch failure naming the variable, and
// never a load one: §5.5 defers the read so a workflow file loads on a host that
// holds no secret.
func TestStaticRefusesAnUnsetVariablePermanently(t *testing.T) {
	t.Setenv("BEN_TEST_UNSET", "")
	src := credential.NewEnv("BEN_TEST_UNSET")
	_, err := src.Fetch(context.Background(), core.PurposeTracker)
	class, ok := core.CredentialFailure(err)
	if !ok || class != core.CredentialPermanent {
		t.Fatalf("Fetch = %v (class %v, credential %v), want a permanent credential failure", err, class, ok)
	}
	if !strings.Contains(err.Error(), "BEN_TEST_UNSET") {
		t.Errorf("error = %v, want it to name the variable", err)
	}
}

// An env-backed source reads the variable per fetch and holds no cache: the
// value is one an operator can change under a running process, and this kind has
// no deadline for a cache to expire against.
func TestStaticReadsTheVariablePerFetch(t *testing.T) {
	t.Setenv("BEN_TEST_ROTATES", "before")
	src := credential.NewEnv("BEN_TEST_ROTATES")
	ctx := context.Background()
	for _, surface := range []func() (core.Token, error){
		func() (core.Token, error) { return src.Fetch(ctx, core.PurposeTracker) },
		func() (core.Token, error) { return src.FetchFresh(ctx, core.PurposeTracker) },
	} {
		t.Setenv("BEN_TEST_ROTATES", "before")
		if tok, err := surface(); err != nil || tok.Value != "before" {
			t.Fatalf("fetch = (%+v, %v), want the current value", tok, err)
		}
		t.Setenv("BEN_TEST_ROTATES", "after")
		if tok, err := surface(); err != nil || tok.Value != "after" {
			t.Errorf("fetch after rotation = (%+v, %v), want the rotated value", tok, err)
		}
	}
}

// An implicit source over a value the loader already resolved carries the
// **full SHA-256** of that value in its binding key (SPEC §5.4, amendment 2;
// mutations 18, 23).
//
// This is the case a name-free binding would otherwise drop: `Config.Tracker`
// DeepEqual catches a rotated literal today because the secret sits in the
// compared map, and a key carrying only `site:tracker.provider.token` would stop
// rebuilding on one — a regression introduced by the fix.
func TestALiteralsBindingKeyCarriesItsFullDigest(t *testing.T) {
	const site = "site:tracker.provider.token"
	before := credential.LiteralDescriptor(site, "ghp-before")
	after := credential.LiteralDescriptor(site, "ghp-after")

	if before.Authority != site || after.Authority != site {
		t.Fatalf("authorities = %q and %q, want the config site unchanged by a rotation", before.Authority, after.Authority)
	}
	if before.BindingKey == after.BindingKey {
		t.Fatal("a rotated literal left the binding key unchanged; the reload would not rebuild")
	}
	sum := sha256.Sum256([]byte("ghp-before"))
	want := site + "#" + hex.EncodeToString(sum[:])
	if before.BindingKey != want {
		t.Errorf("BindingKey = %q, want %q — the full digest, since a truncated one makes a "+
			"collision suppress a required rebuild", before.BindingKey, want)
	}
	// Non-secret, which is the point of a digest over the value itself: the key
	// is compared and logged, and carrying the credential across that boundary
	// would defeat the whole compilation.
	if strings.Contains(before.BindingKey, "ghp-before") {
		t.Error("the binding key carries the credential")
	}
}

// An env-backed source's binding key carries **no** digest, and that is not an
// omission: the value is read at every fetch, so a rotation is picked up without
// rebuilding anything and there is nothing about it for a reload to compare.
func TestAnEnvSourcesBindingKeyIsJustTheVariable(t *testing.T) {
	d := credential.EnvDescriptor("GH_TOKEN")
	if d.Authority != "env:GH_TOKEN" || d.BindingKey != "env:GH_TOKEN" {
		t.Errorf("descriptor = %+v, want both keys to be the namespaced variable", d)
	}
}
