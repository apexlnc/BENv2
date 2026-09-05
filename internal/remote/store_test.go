package remote_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// The durable encoding and the file store behind it.
//
// The encoding is what a *later binary* reads, so the round trip is asserted over
// a fully-populated record rather than a minimal one: a field that silently
// stopped surviving is an address a restart gets wrong, and every field here is
// either an address or the answer to "what was this agent told".
func TestRecordSurvivesTheRoundTrip(t *testing.T) {
	want := remote.Record{
		Identity:         testIdentity(),
		RunID:            "run-42-1",
		RequestDigest:    "sha256:request",
		Dispatched:       true,
		BackendRunID:     "backend-xyz",
		Cursor:           17,
		DecoderTail:      []byte("{\"partial\""),
		Terminal:         true,
		TemplateRevision: "tmpl-3",
		PromptDigest:     remote.PromptDigest("hello"),
		Provider:         "claude-code",
		Model:            "claude-opus-5",
		Transcript:       "transcripts/run-42-1.jsonl",
	}
	body, err := remote.EncodeRecord(want)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	got, err := remote.DecodeRecord(body)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	want.Version = remote.RecordVersion
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip lost something:\n got %+v\nwant %+v", got, want)
	}
}

// A record from a future version is refused rather than best-effort decoded.
//
// The opposite call from state.ReadRuns, deliberately: that renders forensics for
// a human and should degrade rather than refuse, while this is an address, and
// attaching to a half-understood one dispatches twice.
func TestAFutureRecordVersionIsRefused(t *testing.T) {
	future := remote.Record{Version: remote.RecordVersion + 1, Identity: testIdentity(), RunID: "run-1"}
	body, err := remote.EncodeRecord(future)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	if _, err := remote.DecodeRecord(body); err == nil {
		t.Error("a record from a newer binary decoded without complaint")
	}
}

// The file store: a round trip through the filesystem, an absence that is a named
// fact, and a delete that tolerates being repeated.
func TestDirStoreRoundTripsAndReportsAbsence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "remote")
	store := remote.NewDirStore(root)
	claim := testClaim()

	// Every record lives under the root it was given, and nothing is written
	// before one is saved: the daemon points this at the §10.3 state directory,
	// and a store that created it at construction would make a `config effective`
	// on a read-only host fail for a reason that is not about the config.
	if store.Root() != root {
		t.Errorf("Root() = %q, want %q", store.Root(), root)
	}
	if got := store.Path(claim); filepath.Dir(got) != root {
		t.Errorf("Path(%s) = %q, which is not under the root %q", claim, got, root)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the root exists before anything was saved (%v)", err)
	}

	if _, err := store.Load(claim); !errors.Is(err, remote.ErrNoRecord) {
		t.Fatalf("Load from an empty store = %v, want %v", err, remote.ErrNoRecord)
	}
	// A disposal that never happened must not fail: a crash between deleting the
	// backend workspace and deleting the record leaves exactly this.
	if err := store.Delete(claim); err != nil {
		t.Errorf("Delete of an absent record = %v, want nil", err)
	}

	rec := remote.Record{Identity: testIdentity(), RunID: "run-1", Dispatched: true, Cursor: 5}
	if err := store.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RunID != rec.RunID || got.Cursor != rec.Cursor || !got.Dispatched {
		t.Errorf("Load = %+v, want the record that was saved", got)
	}

	if err := store.Delete(claim); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(claim); !errors.Is(err, remote.ErrNoRecord) {
		t.Errorf("Load after Delete = %v, want %v", err, remote.ErrNoRecord)
	}
}

// Two claim cycles never share a file, however their identifiers are spelled.
//
// The sanitized part of a filename collides by construction — a repository name
// carries a slash and an issue identifier is a tracker's to choose — so the
// digest is what actually distinguishes them. Without it, `a/b#1@1` and `a_b#1@1`
// would be one record, and the second claim would attach to the first one's run.
func TestDirStoreDoesNotCollideOnSanitizedNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "remote")
	store := remote.NewDirStore(root)

	claims := []remote.Claim{
		{Repository: "acme/widgets", Issue: "1", Epoch: 1},
		{Repository: "acme_widgets", Issue: "1", Epoch: 1},
		{Repository: "acme/widgets", Issue: "1", Epoch: 2},
		{Repository: "acme/widgets", Issue: "1-1", Epoch: 1},
		{Repository: "acme/widgets", Issue: "11", Epoch: 1},
	}
	seen := map[string]remote.Claim{}
	for _, c := range claims {
		path := store.Path(c)
		if other, dup := seen[path]; dup {
			t.Fatalf("%s and %s share the file %s", c, other, path)
		}
		seen[path] = c
		id := testIdentity()
		id.Claim = c
		if err := store.Save(remote.Record{Identity: id, RunID: remote.RunID("run-" + c.String())}); err != nil {
			t.Fatalf("Save(%s): %v", c, err)
		}
	}
	for _, c := range claims {
		got, err := store.Load(c)
		if err != nil {
			t.Fatalf("Load(%s): %v", c, err)
		}
		if want := remote.RunID("run-" + c.String()); got.RunID != want {
			t.Errorf("Load(%s).RunID = %q, want %q", c, got.RunID, want)
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(claims) {
		t.Errorf("the store holds %d files for %d claims", len(entries), len(claims))
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("unexpected file %q in the store", e.Name())
		}
	}
}

// A file whose contents name a different claim than the one asked for is refused,
// not answered. It can only happen through a collision the digest was meant to
// prevent, and answering with somebody else's address is the failure this whole
// package is arranged around.
func TestDirStoreRefusesARecordForAnotherClaim(t *testing.T) {
	root := filepath.Join(t.TempDir(), "remote")
	store := remote.NewDirStore(root)
	asked := testClaim()

	other := testIdentity()
	other.Claim.Epoch = asked.Epoch + 1
	body, err := remote.EncodeRecord(remote.Record{Identity: other, RunID: "run-other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(asked), body, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Load(asked); !errors.Is(err, remote.ErrClaimMismatch) {
		t.Errorf("Load = %v, want %v", err, remote.ErrClaimMismatch)
	}
}

// The claim's durable spelling is stable and complete: every field appears, so
// two cycles that differ in any one of them differ here.
func TestClaimStringIsTheDurableSpelling(t *testing.T) {
	c := remote.Claim{Repository: "acme/widgets", Issue: "42", Epoch: 7}
	if got, want := c.String(), "acme/widgets#42@7"; got != want {
		t.Errorf("Claim.String() = %q, want %q", got, want)
	}
	for _, tc := range []struct {
		name  string
		claim remote.Claim
		valid bool
	}{
		{"complete", c, true},
		{"no repository", remote.Claim{Issue: "42", Epoch: 7}, false},
		{"no issue", remote.Claim{Repository: "acme/widgets", Epoch: 7}, false},
		{"no epoch", remote.Claim{Repository: "acme/widgets", Issue: "42"}, false},
		{"a pre-epoch pin authorizes nothing", remote.Claim{Repository: "acme/widgets", Issue: "42", Epoch: 0}, false},
		{"a negative epoch", remote.Claim{Repository: "acme/widgets", Issue: "42", Epoch: -1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.claim.Valid(); got != tc.valid {
				t.Errorf("Valid() = %v, want %v", got, tc.valid)
			}
		})
	}
}
