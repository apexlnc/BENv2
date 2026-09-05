// Package credtest is the credential-source conformance suite: one shared,
// table-driven statement of what a `credential_sources` kind owes, which every
// registered kind runs unmodified (SPEC §5.2, §5.7, §10.2).
//
// It exists for the reason agenttest exists. "A source states a deadline and BEN
// does not use a token past it" is a claim about behaviour, and a claim two
// private test suites make separately is a claim about two implementations that
// happen to agree today. A future `github_app` inherits the proof by filling in
// a Case.
//
// What deliberately stays in a kind's own tests: the wire format of its
// exchange, the mapping from a provider's status codes onto a class, and the
// canonicalization rules of its own fields — everything whose correctness is a
// statement about one issuer rather than about the contract.
//
// The one field rule that is *not* a kind's own is TLS. Every kind here presents
// or obtains a bearer credential, so "a remote endpoint is https" is a statement
// about the contract, and it lives in testEndpointsRequireTLS (#245) rather than
// being restated once per kind and diverging.
package credtest

import (
	"context"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Case is everything the suite needs to exercise one kind.
type Case struct {
	// Name is the `kind` value, for test output.
	Name string
	// Kind is the package-level registration under test.
	Kind core.SourceKind

	// Block builds a *minimal valid* source block. A fresh map every call: the
	// suite mutates the result.
	Block func() map[string]any

	// Alternatives gives, per key of Block other than `kind`, a different but
	// still valid value.
	//
	// The suite iterates **Block's own keys** and requires an entry for each,
	// which is what anchors the binding-key check at a boundary no table here
	// controls: adding a field to a kind's schema and to its minimal block
	// without adding an alternative fails the suite, rather than silently
	// under-covering it.
	Alternatives map[string]any

	// Required names every field whose absence — or whose blank value — is a
	// refusal.
	Required []string

	// Bounded says this kind states a deadline. A bounded kind's tokens carry a
	// non-zero UsableUntil and are subject to every TTL gate; an unbounded one's
	// carry zero and are subject to none.
	Bounded bool
	// StablePrincipal says the source definition fixes one downstream service
	// principal across token rotation. A false case must leave PrincipalKey
	// empty so a principal-scoped consumer cannot mistake source identity for
	// service identity.
	StablePrincipal bool
	// NoRemoteEndpoints is the explicit exception for a source that neither
	// presents nor obtains a credential over the network. Without it, the
	// minimal block must expose at least one HTTP(S) endpoint for the shared TLS
	// contract to exercise.
	NoRemoteEndpoints bool

	// Rotate changes the credential the world holds, so the suite can prove that
	// FetchFresh sees the new one and Authority does not move.
	Rotate func(t *testing.T)
	// AdvancePastDeadline moves a bounded instance's clock past a token deadline.
	// It is a test seam, not source behaviour: the suite owns the assertion that
	// Fetch stops serving that cached token. Every bounded case must supply it.
	AdvancePastDeadline func(t *testing.T, source core.CredentialSource, deadline time.Time)

	// Secrets are values the world holds that must appear in neither Authority
	// nor BindingKey. Both keys are compared, logged and — for a source block —
	// printed in full, so a secret reaching either is a leak with a long
	// lifetime.
	Secrets []string

	// UnreachableBlock is a block that describes cleanly and whose *instance*
	// could never reach anything: a filesystem path that does not exist, a host
	// nothing listens on. It is how the suite asserts Describe's purity without
	// having to sandbox the process.
	UnreachableBlock func() map[string]any
}

// Contract runs the whole suite.
func Contract(t *testing.T, c Case) {
	t.Helper()
	t.Run(c.Name+"/describe accepts a valid block", func(t *testing.T) { testDescribeAccepts(t, c) })
	t.Run(c.Name+"/describe is pure", func(t *testing.T) { testDescribeIsPure(t, c) })
	t.Run(c.Name+"/required fields", func(t *testing.T) { testRequiredFields(t, c) })
	t.Run(c.Name+"/unknown keys refuse", func(t *testing.T) { testUnknownKey(t, c) })
	t.Run(c.Name+"/endpoints require TLS", func(t *testing.T) { testEndpointsRequireTLS(t, c) })
	t.Run(c.Name+"/binding key covers every field", func(t *testing.T) { testBindingKeyCoversEveryField(t, c) })
	t.Run(c.Name+"/keys carry no secret", func(t *testing.T) { testKeysCarryNoSecret(t, c) })
	t.Run(c.Name+"/deadline matches boundedness", func(t *testing.T) { testDeadline(t, c) })
	t.Run(c.Name+"/fetch respects deadline", func(t *testing.T) { testFetchRespectsDeadline(t, c) })
	t.Run(c.Name+"/fetch fresh is never cached", func(t *testing.T) { testFetchFreshIsNeverCached(t, c) })
	t.Run(c.Name+"/authority is stable across rotation", func(t *testing.T) { testAuthorityStableAcrossRotation(t, c) })
	t.Run(c.Name+"/new returns the full surface", func(t *testing.T) { testNewReturnsFullSurface(t, c) })
}

func describe(t *testing.T, c Case, block map[string]any) core.SourceDescriptor {
	t.Helper()
	d, err := c.Kind.Describe(block)
	if err != nil {
		t.Fatalf("Describe(%v) = %v, want ok", block, err)
	}
	return d
}

func instance(t *testing.T, c Case) core.CredentialSource {
	t.Helper()
	block := c.Block()
	src, err := c.Kind.New(describe(t, c, block), block)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return src
}

func testDescribeAccepts(t *testing.T, c Case) {
	d := describe(t, c, c.Block())
	if d.Kind != c.Name {
		t.Errorf("Kind = %q, want %q", d.Kind, c.Name)
	}
	if d.Authority == "" || d.BindingKey == "" {
		t.Fatalf("descriptor = %+v, want both keys populated", d)
	}
	// Two distinct questions, so two distinct fields — but the binding key is
	// the *complete* definition, so it can never be narrower than the identity.
	if !strings.HasPrefix(d.BindingKey, d.Authority) {
		t.Errorf("BindingKey %q does not contain Authority %q; the complete definition cannot be "+
			"narrower than the identity", d.BindingKey, d.Authority)
	}
	if c.Bounded != d.Bounded() {
		t.Errorf("MinFreshTTL = %s (bounded %v), want bounded %v", d.MinFreshTTL, d.Bounded(), c.Bounded)
	}
	if got := d.PrincipalKey != ""; got != c.StablePrincipal {
		t.Errorf("PrincipalKey populated = %v, want stable principal %v", got, c.StablePrincipal)
	}
	// A name is not part of what a source *is*. Nothing here has supplied one,
	// and there is nowhere for one to go — which is what makes a rename not a
	// rebuild, structurally rather than by a comparison remembering to skip it.
	if describe(t, c, c.Block()) != d {
		t.Error("Describe is not deterministic over one block")
	}
}

// Describe reaches neither the network nor the filesystem (SPEC §5.7,
// amendment 4). That is what lets a workload-identity configuration
// load-validate on a host holding no credential — and what keeps
// `make workflow-check` credential-free.
//
// Asserted with a block whose instance could reach nothing: a path that does not
// exist, a host nothing listens on. A Describe that touched either would fail or
// hang; one that is pure returns immediately.
func testDescribeIsPure(t *testing.T, c Case) {
	if c.UnreachableBlock == nil {
		t.Skip("no unreachable block supplied")
	}
	done := make(chan core.SourceDescriptor, 1)
	go func() {
		d, err := c.Kind.Describe(c.UnreachableBlock())
		if err != nil {
			t.Errorf("Describe over an unreachable configuration = %v, want ok", err)
		}
		done <- d
	}()
	select {
	case d := <-done:
		if d.Authority == "" {
			t.Error("Describe over an unreachable configuration produced no authority")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Describe did not return; it is reaching something")
	}
}

func testRequiredFields(t *testing.T, c Case) {
	for _, field := range c.Required {
		t.Run("missing "+field, func(t *testing.T) {
			block := c.Block()
			delete(block, field)
			if _, err := c.Kind.Describe(block); err == nil {
				t.Errorf("Describe without %q = nil, want a refusal", field)
			}
		})
		t.Run("blank "+field, func(t *testing.T) {
			block := c.Block()
			block[field] = "   "
			if _, err := c.Kind.Describe(block); err == nil {
				t.Errorf("Describe with a blank %q = nil, want a refusal", field)
			}
		})
	}
}

// Every remote endpoint a credential source names MUST be https (#245).
//
// A `credential_sources` kind exists to present or obtain a bearer credential,
// and the endpoint in its own block is where that credential goes on the wire:
// `octo_sts` sends the projected workload-identity JWT there in an
// `Authorization: Bearer` header, and `projected_oidc`'s issuer is the identity
// the same class of token is minted against. An on-path observer who captures
// one replays it for the whole of its life. Three implementations of that rule
// existed one package apart — `octo_sts` accepted `http://` while
// `projected_oidc` and `internal/airlock` refused it — which is what this
// assertion is here to prevent recurring.
//
// The independent boundary AGENTS.md asks for is that this is **derived, not
// declared**: the suite reads the kind's own minimal block, finds every field
// spelled as an HTTP(S) URL, refuses a block that is already plaintext, and
// requires the http spelling of every https field to be refused at Describe.
// There is no per-case endpoint list to forget to extend, so a future kind with
// a remote endpoint inherits the rule by running the suite — and a kind that
// later gains one inherits it without editing anything here. A source with no
// remote endpoint must say so explicitly; absence cannot silently turn this
// contract into a skipped test.
//
// A kind with a genuine need for a plaintext endpoint cannot quietly opt out; it
// has to argue the exception here, where every other kind's authors will read it.
func testEndpointsRequireTLS(t *testing.T, c Case) {
	block := c.Block()
	checked := 0
	for _, key := range sortedKeys(block) {
		u, ok := remoteHTTPURL(block[key])
		if !ok {
			continue
		}
		checked++
		t.Run(key, func(t *testing.T) {
			if strings.EqualFold(u.Scheme, "http") {
				t.Errorf("the minimal valid block names a plaintext %q; the bearer credential this "+
					"endpoint carries would be readable to anyone on the path", key)
				return
			}
			u.Scheme = "http"
			plaintext := c.Block()
			plaintext[key] = u.String()
			if _, err := c.Kind.Describe(plaintext); err == nil {
				t.Errorf("Describe accepted a plaintext %q; the bearer credential this endpoint "+
					"carries would be readable to anyone on the path, and a warning is a refusal "+
					"nobody reads", key)
			}
		})
	}
	if checked == 0 {
		if c.NoRemoteEndpoints {
			t.Skip("this source has no remote endpoint")
		}
		t.Fatal("the minimal block names no HTTP(S) endpoint; set NoRemoteEndpoints only for a source that never sends or obtains a credential over the network")
	}
	if c.NoRemoteEndpoints {
		t.Error("NoRemoteEndpoints is set, but the minimal block names a remote endpoint")
	}
}

func remoteHTTPURL(value any) (*url.URL, bool) {
	raw, ok := value.(string)
	if !ok {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, false
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return nil, false
	}
	return u, true
}

func testUnknownKey(t *testing.T, c Case) {
	block := c.Block()
	block["not_a_field"] = "x"
	if _, err := c.Kind.Describe(block); err == nil {
		t.Error("Describe accepted an unknown key; strict-at-load applies inside a source block, " +
			"and a typo that silently left a field unset is how a partial configuration degrades")
	}
}

// The binding key changes for **every** behaviour-affecting field — driven by
// mutating each field of the kind's own minimal block in turn, not only the
// fields Authority ignores (SPEC §5.4, amendment 2).
//
// Narrower than the definition and a rebuild is missed; the direction that
// matters here is the first, because a missed rebuild is silent: the daemon
// keeps exchanging against the configuration an operator has already replaced.
func testBindingKeyCoversEveryField(t *testing.T, c Case) {
	base := describe(t, c, c.Block())
	for _, key := range sortedKeys(c.Block()) {
		if key == "kind" {
			continue
		}
		alt, ok := c.Alternatives[key]
		if !ok {
			t.Errorf("no alternative value declared for %q; the suite cannot prove the binding key "+
				"covers a field it has no second value for", key)
			continue
		}
		t.Run(key, func(t *testing.T) {
			block := c.Block()
			block[key] = alt
			if got := describe(t, c, block); got.BindingKey == base.BindingKey {
				t.Errorf("editing %q left BindingKey %q unchanged; a reload would not rebuild", key, got.BindingKey)
			}
		})
	}
}

func testKeysCarryNoSecret(t *testing.T, c Case) {
	d := describe(t, c, c.Block())
	for _, secret := range c.Secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(d.Authority, secret) {
			t.Errorf("Authority %q carries a secret", d.Authority)
		}
		if strings.Contains(d.BindingKey, secret) {
			t.Errorf("BindingKey %q carries a secret", d.BindingKey)
		}
		if strings.Contains(d.PrincipalKey, secret) {
			t.Errorf("PrincipalKey %q carries a secret", d.PrincipalKey)
		}
	}
}

// A bounded kind populates UsableUntil; an unbounded one leaves it zero, which
// is "explicitly unbounded" and never "expired" or "unknown" (SPEC §10.2).
func testDeadline(t *testing.T, c Case) {
	src := instance(t, c)
	d := src.Descriptor()
	tok, err := src.FetchFresh(context.Background(), core.PurposePublish)
	if err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}
	if tok.Value == "" {
		t.Fatal("FetchFresh returned an empty credential")
	}
	if !c.Bounded {
		if !tok.UsableUntil.IsZero() {
			t.Errorf("UsableUntil = %s for an unbounded kind, want the zero value", tok.UsableUntil)
		}
		return
	}
	if tok.UsableUntil.IsZero() {
		t.Fatal("UsableUntil is zero for a bounded kind; a zero deadline means explicitly unbounded")
	}
	// The deadline leaves at least the declared floor, bar the time this test
	// took. A bounded handoff allowance above the floor is valid; the symmetric
	// minute bound still catches a wrong unit or an unrelated lifetime.
	if left := time.Until(tok.UsableUntil); left > d.MinFreshTTL+time.Minute || left < d.MinFreshTTL-time.Minute {
		t.Errorf("UsableUntil leaves %s, want about the declared MinFreshTTL %s", left, d.MinFreshTTL)
	}
}

// Fetch may cache a bounded token, but never past the deadline the source put
// on it (SPEC §10.2). Rotate makes the old and fresh answers distinguishable;
// the clock seam gets there without making the suite wait fifty minutes.
func testFetchRespectsDeadline(t *testing.T, c Case) {
	if !c.Bounded {
		return
	}
	if c.Rotate == nil || c.AdvancePastDeadline == nil {
		t.Fatal("a bounded case must supply rotation and a deadline clock seam")
	}
	src := instance(t, c)
	ctx := context.Background()
	first, err := src.Fetch(ctx, core.PurposeTracker)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if first.UsableUntil.IsZero() {
		t.Fatal("Fetch returned a bounded token with no deadline")
	}
	c.Rotate(t)
	c.AdvancePastDeadline(t, src, first.UsableUntil)
	second, err := src.Fetch(ctx, core.PurposeTracker)
	if err != nil {
		t.Fatalf("Fetch past the cached token's deadline: %v", err)
	}
	if second.Value == first.Value {
		t.Error("Fetch served the cached credential past its deadline")
	}
	if !second.UsableUntil.After(first.UsableUntil) {
		t.Errorf("refreshed deadline = %s, want one after expired deadline %s",
			second.UsableUntil, first.UsableUntil)
	}
}

// FetchFresh performs the exchange every time and is never served from a cache
// (SPEC §7.7): the publisher's whole surface is this method, because a token
// handed to an agent must cover the attempt and a cached one has already spent
// part of its life.
func testFetchFreshIsNeverCached(t *testing.T, c Case) {
	if c.Rotate == nil {
		t.Skip("no rotation supplied")
	}
	src := instance(t, c)
	ctx := context.Background()
	first, err := src.FetchFresh(ctx, core.PurposePublish)
	if err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}
	c.Rotate(t)
	second, err := src.FetchFresh(ctx, core.PurposePublish)
	if err != nil {
		t.Fatalf("FetchFresh after rotation: %v", err)
	}
	if first.Value == second.Value {
		t.Error("FetchFresh returned the pre-rotation credential; it was served from a cache")
	}
}

// Authority is credential *identity*, so it does not move when a token does
// (SPEC §8.5, amendment 11). A gate keyed by it survives every rotation; a gate
// keyed by the token abandons its backoff on each one.
func testAuthorityStableAcrossRotation(t *testing.T, c Case) {
	if c.Rotate == nil {
		t.Skip("no rotation supplied")
	}
	src := instance(t, c)
	before := src.Descriptor().Authority
	if _, err := src.FetchFresh(context.Background(), core.PurposeTracker); err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}
	c.Rotate(t)
	if _, err := src.FetchFresh(context.Background(), core.PurposeTracker); err != nil {
		t.Fatalf("FetchFresh after rotation: %v", err)
	}
	if got := src.Descriptor().Authority; got != before {
		t.Errorf("Authority moved across a rotation: %q → %q", before, got)
	}
}

// New returns the FULL surface; consumers receive narrowed views, so narrowing
// is assembly's decision and not a kind's to get wrong (SPEC §11).
func testNewReturnsFullSurface(t *testing.T, c Case) {
	src := instance(t, c)
	if _, ok := src.(core.Source); !ok {
		t.Error("the instance does not satisfy core.Source")
	}
	if _, ok := src.(core.FreshSource); !ok {
		t.Error("the instance does not satisfy core.FreshSource")
	}
	// And New refuses what Describe refuses: a New that read only some of the
	// block could build an instance from a configuration load-validation would
	// have rejected (SPEC §5.7).
	bad := c.Block()
	bad["not_a_field"] = "x"
	if _, err := c.Kind.New(core.SourceDescriptor{}, bad); err == nil {
		t.Error("New accepted a block Describe refuses")
	}
}

// sortedKeys keeps a failing suite naming the same field twice: map iteration
// order would make which field is reported vary run to run.
func sortedKeys(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }
