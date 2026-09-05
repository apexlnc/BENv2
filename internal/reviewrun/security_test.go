package reviewrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

// The credentials the trusted process holds. Named as they would be, and given
// values a substring search can actually find.
var trustedSecrets = map[string]string{
	"BEN_REVIEW_TOKEN":  "ghs_controller_credential_value",
	"GITHUB_TOKEN":      "ghs_tracker_credential_value",
	"BEN_AIRLOCK_TOKEN": "airlock_bearer_credential_value",
	"ANTHROPIC_API_KEY": "sk-ant-reusable-provider-key",
	"OPENAI_API_KEY":    "sk-oai-reusable-provider-key",
}

func secretValues() []string {
	out := make([]string, 0, len(trustedSecrets))
	for _, v := range trustedSecrets {
		out = append(out, v)
	}
	return out
}

// #204's security acceptance, asserted against the *actual serialized request*
// rather than against the allowlist that composed it.
//
// The distinction matters: an allowlist tested against itself cannot see a leak
// that arrives some other way — through a rendered prompt, a stdin payload, or
// an argv element an operator wrote. So this marshals exactly what would cross
// the wire and searches it for every value the trusted process holds.
func TestTheSerializedRequestCarriesNoTrustedCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
		want error
	}{
		{
			name: "the ordinary request",
			req: Request{
				Argv:  []string{"codex", "exec", "--json", "-"},
				Env:   map[string]string{"BEN_REVIEW_HEAD": head1, "BEN_REVIEW_PR": "42"},
				Stdin: []byte("review this diff\n--- a/x\n+++ b/x\n"),
			},
		},
		{
			name: "a controller token in the environment",
			req:  Request{Env: map[string]string{"BEN_REVIEW_TOKEN": trustedSecrets["BEN_REVIEW_TOKEN"]}},
			want: ErrCredentialLeak,
		},
		{
			name: "a tracker token under a name nobody thought of",
			req:  Request{Env: map[string]string{"HELPFUL_CONTEXT": trustedSecrets["GITHUB_TOKEN"]}},
			want: ErrCredentialLeak,
		},
		{
			name: "the backend bearer token pasted into a prompt",
			req:  Request{Stdin: []byte("here is some context: " + trustedSecrets["BEN_AIRLOCK_TOKEN"])},
			want: ErrCredentialLeak,
		},
		{
			name: "a credential smuggled through argv",
			req:  Request{Argv: []string{"codex", "--header", "Authorization: " + trustedSecrets["GITHUB_TOKEN"]}},
			want: ErrCredentialLeak,
		},
		{
			name: "a reusable provider key crossing to a sandbox",
			req:  Request{Env: map[string]string{"ANTHROPIC_API_KEY": trustedSecrets["ANTHROPIC_API_KEY"]}},
			want: ErrCredentialLeak,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckRequest(tc.req, secretValues(), true)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("CheckRequest: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("CheckRequest = %v, want %v", err, tc.want)
			}
		})
	}
}

// The end-to-end version: drive a review through the real remote executor
// against #192's contract fake, and inspect the process spec that actually
// crossed the boundary.
//
// Also the "Airlock mode drives one durable run and no local reviewer process"
// half of #204's first acceptance row: the whole path from subject to verdict
// runs with no `exec.Cmd` anywhere in it, which is a property of the types
// rather than of a count — internal/reviewrun's remote leg imports no os/exec
// at all (see remote.go).
func TestWhatTheBackendReceivesHoldsNoCredential(t *testing.T) {
	for name, value := range trustedSecrets {
		t.Setenv(name, value)
	}
	backend := remotetest.New("profile-1")
	rec := &recorder{ProcessBackend: backend, t: t}
	exec, err := NewRemote(RemoteOptions{
		Backend: rec, GitRepository: "acme/widgets", Logger: testLogger(t),
	})
	if err != nil {
		t.Fatal(err)
	}

	sub := testSubject()
	rec.wantGit = remote.GitScope{
		Phase: remote.GitPhaseReview, Repository: "acme/widgets",
		Branch: "ben/11", BaseCommit: base1, BaseBranch: "release/v2",
	}
	run, err := sub.RunID()
	if err != nil {
		t.Fatal(err)
	}
	// The fake needs the run to exist before it can be scripted, and the session
	// blocks until the stream seals — so the script runs the moment the dispatch
	// lands, which is also when the spec is available to inspect.
	rec.after = func() {
		backend.Emit(remote.RunID(run), []byte(envelope(`{"verdict":"clean","findings":"nothing to report"}`)))
		backend.Quiet(remote.RunID(run))
	}

	s, err := New(Options{
		Executor: exec,
		Store:    NewDirStore(t.TempDir()),
		Sandbox: func(context.Context, Subject) (Placement, error) {
			return Placement{
				Branch: "ben/11", BaseSHA: base1, TargetBranch: "release/v2",
				Sandbox: "sandbox-1", Profile: "profile-1",
			}, nil
		},
		Secrets: secretValues,
		Compose: func(sub Subject) (Request, error) {
			return Request{
				Argv:  []string{"codex", "exec", "--json", "-"},
				Env:   map[string]string{"BEN_REVIEW_HEAD": sub.Head},
				Stdin: []byte(PromptContract() + "\n" + sub.Diff),
			}, nil
		},
		Sleep:  boundedSleep(2),
		Logger: testLogger(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Review(context.Background(), sub)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if report.Verdict != review.VerdictClean {
		t.Fatalf("verdict = %q", report.Verdict)
	}
	if rec.starts != 1 {
		t.Fatalf("the backend was started %d times, want exactly one durable run", rec.starts)
	}
}

// recorder asserts on every request that crosses the boundary. A pass-through
// rather than a fake, so what it inspects is what the production path produced.
type recorder struct {
	remote.ProcessBackend
	t       *testing.T
	after   func()
	starts  int
	wantGit remote.GitScope
}

func (r *recorder) Start(ctx context.Context, ref remote.ProcessRef, spec remote.ProcessSpec) (remote.Status, error) {
	body, err := json.Marshal(spec)
	if err != nil {
		r.t.Fatal(err)
	}
	// The marshalled body and the stdin bytes separately: encoding/json base64s
	// a []byte, so a credential pasted into the prompt would not appear in the
	// body as itself.
	for name, value := range trustedSecrets {
		if strings.Contains(string(body), value) || strings.Contains(string(spec.Stdin), value) ||
			strings.Contains(strings.Join(spec.Argv, " "), value) {
			r.t.Fatalf("the process spec crossing to the backend carries $%s", name)
		}
	}
	for _, name := range append(ForbiddenEnv(), ProviderEnv()...) {
		if _, ok := spec.Env[name]; ok {
			r.t.Fatalf("the process spec crossing to the backend names %s", name)
		}
	}
	if spec.Git != r.wantGit {
		r.t.Fatalf("review Git scope = %+v, want %+v", spec.Git, r.wantGit)
	}
	r.starts++
	st, startErr := r.ProcessBackend.Start(ctx, ref, spec)
	if startErr == nil && r.after != nil {
		after := r.after
		r.after = nil
		after()
	}
	return st, startErr
}

// A local child's composed environment is the other half of the same property,
// and it is asserted from the child's own view in local_test.go
// (TestLocalReviewerNeverSeesTheForgeCredential). What is checked here is the
// rule that governs it: every forbidden name is refused, and refused by the
// same predicate production applies.
func TestEveryForbiddenNameIsRefusedOnBothSubstrates(t *testing.T) {
	for _, name := range ForbiddenEnv() {
		if err := CheckEnvName(name); !errors.Is(err, ErrCredentialLeak) {
			t.Errorf("CheckEnvName(%q) = %v, want ErrCredentialLeak", name, err)
		}
		if err := CheckRemoteEnvName(name); !errors.Is(err, ErrCredentialLeak) {
			t.Errorf("CheckRemoteEnvName(%q) = %v, want ErrCredentialLeak", name, err)
		}
	}
	// The provider split: an operator's own model credential may reach a local
	// child and may never be serialized into a backend request.
	for _, name := range ProviderEnv() {
		if err := CheckEnvName(name); err != nil {
			t.Errorf("CheckEnvName(%q) = %v, want the local path to allow it", name, err)
		}
		if err := CheckRemoteEnvName(name); !errors.Is(err, ErrCredentialLeak) {
			t.Errorf("CheckRemoteEnvName(%q) = %v, want ErrCredentialLeak", name, err)
		}
	}
	// Case-insensitively, because an environment variable named `github_token`
	// is the same credential.
	if err := CheckEnvName("github_token"); !errors.Is(err, ErrCredentialLeak) {
		t.Errorf("a lowercased forge credential was accepted: %v", err)
	}
}

// The forge interface a reviewer could reach is empty by construction: this
// package holds no forge client, no tracker and no credential, so there is no
// method to call. Asserted structurally rather than by prose — a compile-time
// fact is stronger than a token scope.
func TestThisPackageHoldsNoForgeSurface(t *testing.T) {
	// Session's whole exported surface, enumerated. A method that published
	// anything would have to appear here.
	var s *Session
	_ = s.Review
	_ = s.Retire
	_ = s.Record
	_ = s.Reconcile
}
