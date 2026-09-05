package remote_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

func TestProcessRequestDigestPreservesLegacyWireAndVersionsGitScope(t *testing.T) {
	identity := digestTestIdentity()
	spec := remote.ProcessSpec{
		Identity: identity,
		Argv:     []string{"agent", "--json"},
		Env:      map[string]string{"B": "2", "A": "1"},
		Stdin:    []byte("hi"),
		Limits: core.RunLimits{
			StallTimeout: time.Second, AttemptTimeout: 2 * time.Second,
			MaxTurns: 3, MaxCostUSD: 4.5,
		},
	}
	legacy := `{"Identity":{"Claim":{"Repository":"repo","Issue":"42","Epoch":7},"Branch":"ben/42","BaseSHA":"base","SandboxID":"sbx","ProfileRevision":"profile"},"Argv":["agent","--json"],"Env":{"A":"1","B":"2"},"Stdin":"aGk=","Limits":{"StallTimeout":1000000000,"AttemptTimeout":2000000000,"MaxTurns":3,"MaxCostUSD":4.5}}`
	assertProcessDigest(t, spec, legacy)

	spec.Git = remote.GitScope{
		Phase: remote.GitPhaseCoding, Repository: "repo", Branch: "ben/42",
		BaseCommit: "base", BaseBranch: "main",
	}
	versioned := `{"Version":2,"Identity":{"Claim":{"Repository":"repo","Issue":"42","Epoch":7},"Branch":"ben/42","BaseSHA":"base","SandboxID":"sbx","ProfileRevision":"profile"},"Argv":["agent","--json"],"Env":{"A":"1","B":"2"},"Stdin":"aGk=","Limits":{"StallTimeout":1000000000,"AttemptTimeout":2000000000,"MaxTurns":3,"MaxCostUSD":4.5},"Git":{"Phase":"coding","Repository":"repo","Branch":"ben/42","BaseCommit":"base","BaseBranch":"main","CheckoutCommit":"","Operation":""}}`
	assertProcessDigest(t, spec, versioned)
}

func TestHookRequestDigestPreservesLegacyWireAndVersionsDirectCommands(t *testing.T) {
	identity := digestTestIdentity()
	spec := remote.HookSpec{
		Identity: identity, Phase: remote.HookBeforeRun, Attempt: 2,
		Script: "echo ok", Timeout: 3 * time.Second,
	}
	legacy := `{"Identity":{"Claim":{"Repository":"repo","Issue":"42","Epoch":7},"Branch":"ben/42","BaseSHA":"base","SandboxID":"sbx","ProfileRevision":"profile"},"Phase":"before_run","Attempt":2,"Script":"echo ok","Timeout":3000000000}`
	assertHookDigest(t, spec, legacy)

	spec.Phase = remote.HookGitPrepare
	spec.Script = ""
	spec.Argv = []string{"airlock-git", "prepare"}
	spec.Git = remote.GitScope{
		Phase: remote.GitPhasePrepare, Repository: "repo", Branch: "ben/42",
		BaseCommit: "base", BaseBranch: "main", CheckoutCommit: "checkout",
	}
	versioned := `{"Version":2,"Identity":{"Claim":{"Repository":"repo","Issue":"42","Epoch":7},"Branch":"ben/42","BaseSHA":"base","SandboxID":"sbx","ProfileRevision":"profile"},"Phase":"ben_git_prepare","Attempt":2,"Script":"","Argv":["airlock-git","prepare"],"Git":{"Phase":"prepare","Repository":"repo","Branch":"ben/42","BaseCommit":"base","BaseBranch":"main","CheckoutCommit":"checkout","Operation":""},"Timeout":3000000000}`
	assertHookDigest(t, spec, versioned)
}

func digestTestIdentity() remote.Identity {
	return remote.Identity{
		Claim:  remote.Claim{Repository: "repo", Issue: "42", Epoch: 7},
		Branch: "ben/42", BaseSHA: "base", SandboxID: "sbx", ProfileRevision: "profile",
	}
}

func assertProcessDigest(t *testing.T, spec remote.ProcessSpec, preimage string) {
	t.Helper()
	got, err := remote.ProcessRequestDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := digestPreimage(preimage); got != want {
		t.Fatalf("ProcessRequestDigest = %s, want %s for frozen preimage %s", got, want, preimage)
	}
}

func assertHookDigest(t *testing.T, spec remote.HookSpec, preimage string) {
	t.Helper()
	got, err := remote.HookRequestDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := digestPreimage(preimage); got != want {
		t.Fatalf("HookRequestDigest = %s, want %s for frozen preimage %s", got, want, preimage)
	}
}

func digestPreimage(preimage string) string {
	sum := sha256.Sum256([]byte(preimage))
	return "sha256:" + hex.EncodeToString(sum[:])
}
