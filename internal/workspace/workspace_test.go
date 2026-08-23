package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// TestMain pins git to a hermetic configuration: no user/system config, a
// fixed identity for worktree commits.
//
// It is also where the #167 cohort reports on itself. The members are top-level
// tests, so there is no parent to hang a cleanup on and no point before this
// one at which the cohort's peak is final.
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.Setenv("GIT_AUTHOR_NAME", "ben-test")
	os.Setenv("GIT_AUTHOR_EMAIL", "ben@test.invalid")
	os.Setenv("GIT_COMMITTER_NAME", "ben-test")
	os.Setenv("GIT_COMMITTER_EMAIL", "ben@test.invalid")

	code := m.Run()
	for _, problem := range cohort.Problems() {
		fmt.Fprintf(os.Stderr, "workspace cohort: %s\n", problem)
		code = 1
	}
	os.Exit(code)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture is a local "origin": a bare repo the provider fetches from, plus
// the seed working repo used to push new commits to it.
type fixture struct {
	origin string
	seed   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	runGit(t, dir, "init", "--quiet", "-b", "main", seed)
	writeFile(t, filepath.Join(seed, "README.md"), "seed\n")
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "--quiet", "-m", "seed")
	origin := filepath.Join(dir, "origin.git")
	runGit(t, dir, "clone", "--quiet", "--bare", seed, origin)
	return &fixture{origin: origin, seed: seed}
}

func (f *fixture) head(t *testing.T) string {
	t.Helper()
	return runGit(t, f.origin, "rev-parse", "refs/heads/main")
}

// pushCommit adds a commit to origin's main, simulating upstream movement.
func (f *fixture) pushCommit(t *testing.T, msg string) string {
	t.Helper()
	appendFile(t, filepath.Join(f.seed, "README.md"), msg+"\n")
	runGit(t, f.seed, "commit", "--quiet", "-am", msg)
	runGit(t, f.seed, "push", "--quiet", f.origin, "main:main")
	return runGit(t, f.seed, "rev-parse", "HEAD")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newProvider(t *testing.T, f *fixture, hooks Hooks) *Provider {
	t.Helper()
	p, err := New(Options{
		Root:        t.TempDir(),
		WorkflowKey: "wf",
		// A credential source is set even though file remotes never consult the
		// credential: every test then exercises the per-invocation resolution
		// and the credential-helper argv/env plumbing.
		Repository: core.Repository{
			RemoteURL:  f.origin,
			AuthSource: fake.NewRemoteAuth("x-access-token", "hunter2"),
		},
		Hooks:  hooks,
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func issue(id string) core.Issue { return core.Issue{Identifier: id} }

const testClaimEpoch int64 = 101

// prepareForTest gives pre-claim-epoch workspace tests the production ordering:
// pending is durable before prepare, and prepare receives the expected tracker
// epoch. Tests that need a different epoch call BeginClaimBase/PrepareClaim
// directly; this helper never repairs malformed or missing reachability state.
func prepareForTest(t *testing.T, p *Provider, ctx context.Context, issue core.Issue, attempt int) (core.Workspace, error) {
	t.Helper()
	ws, _, err := prepareClaimForTest(t, p, ctx, issue, attempt)
	return ws, err
}

func prepareClaimForTest(t *testing.T, p *Provider, ctx context.Context, issue core.Issue, attempt int) (core.Workspace, core.LocalBranchFacts, error) {
	t.Helper()
	state, err := p.ClaimBase(ctx, issue)
	if err != nil {
		return core.Workspace{}, core.LocalBranchFacts{}, err
	}
	epoch := state.Epoch
	if state.State == core.ClaimBaseAbsent {
		epoch = testClaimEpoch
		if err := p.BeginClaimBase(ctx, issue, epoch); err != nil {
			return core.Workspace{}, core.LocalBranchFacts{}, err
		}
	}
	return p.PrepareClaim(ctx, issue, attempt, epoch)
}

// repo is an unauthenticated core.Repository — what a file remote needs, and
// what New's option validation is asserted over.
func repo(remoteURL string) core.Repository { return core.Repository{RemoteURL: remoteURL} }

// agentCommit simulates the agent committing work inside the workspace.
func agentCommit(t *testing.T, wsPath, name string) string {
	t.Helper()
	writeFile(t, filepath.Join(wsPath, name), name+"\n")
	runGit(t, wsPath, "add", ".")
	runGit(t, wsPath, "commit", "--quiet", "-m", "agent: "+name)
	return runGit(t, wsPath, "rev-parse", "HEAD")
}

// agentPush publishes the workspace branch the way agents do (SPEC §5.6, §6.7).
func agentPush(t *testing.T, wsPath string) {
	t.Helper()
	runGit(t, wsPath, "push", "--quiet", "origin", "HEAD")
}

func TestNewValidation(t *testing.T) {
	parallel(t)
	tests := []struct {
		name string
		opts Options
	}{
		{"missing root", Options{WorkflowKey: "wf", Repository: repo("x")}},
		{"relative root", Options{Root: "rel/path", WorkflowKey: "wf", Repository: repo("x")}},
		{"missing workflow key", Options{Root: "/tmp/x", Repository: repo("x")}},
		{"missing remote", Options{Root: "/tmp/x", WorkflowKey: "wf"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); err == nil {
				t.Error("New() accepted invalid options")
			}
		})
	}
}

// Canary tokens: RemoteURL reaches git as argv and base.git's `origin`, so the
// refusal's own message is a publication seam of its own (SPEC §10.2; #52).
const (
	canaryPassword = "pw-canary-hunter2"
	canaryHost     = "host-canary.invalid"
)

// TestNewRefusesEmbeddedCredentials covers the password rule for every scheme
// and the stricter http(s) rule, across parsed and unparseable URLs (#52).
func TestNewRefusesEmbeddedCredentials(t *testing.T) {
	parallel(t)
	// The refusal tells a core.Repository caller where the credential belongs.
	// Keep that instruction on the current API rather than the removed Auth field.
	if got := ErrRemoteCredentials.Error(); !strings.Contains(got, "Repository.AuthSource") {
		t.Fatalf("ErrRemoteCredentials = %q, want it to name Repository.AuthSource", got)
	}
	tests := []struct {
		name string
		url  string
	}{
		// A password component, whatever the scheme.
		{"ssh password", "ssh://git:" + canaryPassword + "@" + canaryHost + "/o/r.git"},
		{"ssh password percent-encoded value", "ssh://git:pw-canary%40hunter2@" + canaryHost + "/o/r.git"},
		// Presence, not emptiness: a truncated secret is likelier than intent.
		{"ssh empty password", "ssh://git:@" + canaryHost + "/o/r.git"},
		{"ssh password without username", "ssh://:" + canaryPassword + "@" + canaryHost + "/o/r.git"},
		{"ssh password with port", "ssh://git:" + canaryPassword + "@" + canaryHost + ":22/o/r.git"},
		{"git scheme password", "git://git:" + canaryPassword + "@" + canaryHost + "/o/r.git"},
		{"custom scheme password", "weird://user:" + canaryPassword + "@" + canaryHost + "/x"},
		// net/url splits userinfo at the last "@", so the password survives an
		// alias in front of it.
		{"password behind extra at sign", "ssh://user:" + canaryPassword + "@evil@" + canaryHost + "/x"},

		// http(s) refuses userinfo whole — there it is live authentication.
		{"https password", "https://x-access-token:" + canaryPassword + "@" + canaryHost + "/o/r.git"},
		{"https token as username", "https://" + canaryPassword + "@" + canaryHost + "/o/r.git"},
		{"https empty userinfo", "https://@" + canaryHost + "/o/r.git"},
		{"http password", "http://x:" + canaryPassword + "@" + canaryHost + "/o/r.git"},
		// net/url lowercases the scheme, so the rule needs no case fold.
		{"uppercase https password", "HTTPS://x:" + canaryPassword + "@" + canaryHost + "/o/r.git"},

		// Unparseable, so the raw-authority fallback decides. Unreadable must
		// not mean unchecked: these still reach git.
		{"unparseable space in password", "ssh://git:pw canary hunter2@" + canaryHost + "/x"},
		{"unparseable invalid port", "ssh://git:" + canaryPassword + "@" + canaryHost + ":notaport/x"},
		{"unparseable alias then password", "ssh://alias@user:" + canaryPassword + "@" + canaryHost + ":notaport/x"},
		// The fallback carries the http(s) whole-userinfo rule too, or the
		// stricter scheme would get the weaker check exactly when the URL is
		// too malformed to parse.
		{"unparseable https token as username", "https://" + canaryPassword + "@" + canaryHost + ":notaport/x"},
		{"unparseable https empty userinfo", "https://@" + canaryHost + ":notaport/x"},
		{"unparseable http token as username", "http://" + canaryPassword + "@" + canaryHost + ":notaport/x"},
		// net/url lowercases the scheme; the fallback has to fold for itself.
		{"unparseable uppercase https userinfo", "HTTPS://" + canaryPassword + "@" + canaryHost + ":notaport/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(Options{Root: "/tmp/x", WorkflowKey: "wf", Repository: repo(tt.url)})
			if !errors.Is(err, ErrRemoteCredentials) {
				t.Fatalf("New(%q) error = %v, want ErrRemoteCredentials", tt.url, err)
			}
			if p != nil {
				t.Error("New returned a provider alongside a refusal")
			}
			// The value being refused is the secret, so the refusal names no
			// part of it — unlike config's RenderRefusal, where provenance
			// proves the reader can already see the value (SPEC §5.8).
			for _, leak := range []string{tt.url, canaryPassword, canaryHost, "pw-canary", "@"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("refusal leaked %q into its message: %s", leak, err)
				}
			}
		})
	}
}

// TestNewRefusesTransportHelperRemotes pins #98's fail-closed decision: the
// helper owns the address grammar, so credential-bearing and credential-free
// addresses are refused without trying to tell them apart.
func TestNewRefusesTransportHelperRemotes(t *testing.T) {
	parallel(t)
	tests := []struct {
		name string
		url  string
	}{
		{"credential-bearing address", "mock::ssh://git:" + canaryPassword + "@" + canaryHost + "/x"},
		{"credential-free address", "mock::path://name:part@segment"},
		{"digit-leading helper", "1mock::token=" + canaryPassword},
		{"empty helper prefix", "::token=" + canaryPassword},
		{"full helper-name grammar", "mock-v1.2+corp::opaque-address"},
		{"empty helper-owned address", "mock::"},
		// Git selects the helper from the prefix before interpreting the
		// address. URL parsing must not turn malformed opaque text into an
		// acceptance path.
		{"non-URL address", "mock::%zz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(Options{Root: "/tmp/x", WorkflowKey: "wf", Repository: repo(tt.url)})
			if !errors.Is(err, ErrTransportHelperRemote) {
				t.Fatalf("New(%q) error = %v, want ErrTransportHelperRemote", tt.url, err)
			}
			if p != nil {
				t.Error("New returned a provider alongside a refusal")
			}
			for _, leak := range []string{tt.url, canaryPassword, canaryHost, "mock", "@"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("refusal leaked %q into its message: %s", leak, err)
				}
			}
		})
	}
}

// TestNewAcceptsCredentialFreeRemotes pins the carve-outs. Every row is a
// remote form git accepts, so a false refusal here is a broken deployment —
// the failure mode the raw-authority fallback is confined to avoid.
func TestNewAcceptsCredentialFreeRemotes(t *testing.T) {
	parallel(t)
	tests := []struct {
		name string
		url  string
	}{
		{"ssh conventional username", "ssh://git@github.com/o/r.git"},
		{"ssh username with port", "ssh://git@github.com:2222/o/r.git"},
		{"ssh username containing at sign", "ssh://a@b@github.com/x"},
		// Verified against git's ssh argv: git hands ssh `git:pass@host` for
		// this and for ssh://git:pass@host alike, so the colon is username
		// data to ssh, not a password. Only the syntax separates them, and
		// refusing this would mean refusing ssh://s3cret@host too.
		{"ssh percent-encoded colon in username", "ssh://git%3Apass@github.com/x"},

		// scp-like syntax has no userinfo production. Verified against git's
		// ssh argv: host `git`, path `pass@github.com:o/r.git`.
		{"scp-like", "git@github.com:o/r.git"},
		{"scp-like ipv6 host", "git@[::1]:o/r.git"},
		{"scp-like at sign in path", "host:path@segment/repo.git"},
		{"scheme-shaped scp-like path", "git:pass@github.com:o/r.git"},
		// Both characteristics at once: a real userinfo *and* an "@" later in
		// the path. Verified against git's ssh argv — user `git`, host `host`,
		// path `path@segment/repo.git`. An unconfined fallback reads the
		// path's trailing "@" as the userinfo delimiter, making the colon in
		// `git@host:path` look like a password.
		{"scp-like userinfo and at sign in path", "git@host:path@segment/repo.git"},
		// Double colons outside the helper-prefix position are ordinary URL or
		// path data. A raw strings.Contains check would refuse these.
		{"ssh URL with ipv6 host", "ssh://git@[::1]/o/r.git"},
		{"double colon in path", "https://github.com/o/a::b.git"},
		// Underscore is not in Git's helper-name grammar, so this is an
		// scp-like remote with a path beginning in a colon, not a helper.
		{"double colon after non-helper prefix", "mock_bad::path"},

		// The authority ends at the first "/", "?" or "#" (RFC 3986 §3.2);
		// colons past it are path, query or fragment data.
		{"unparseable query after authority", "ssh://git@host:notaport?ref=a:b@c"},
		{"unparseable fragment after authority", "ssh://git@host:notaport#a:b@c"},
		{"at sign in path", "https://github.com/o/r@v1.git"},

		{"https no userinfo", "https://github.com/o/r.git"},
		{"file url", "file:///tmp/x"},
		{"absolute path", "/srv/repos/r.git"},
		{"absolute path with at sign", "/srv/git@corp/r.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(Options{Root: "/tmp/x", WorkflowKey: "wf", Repository: repo(tt.url)}); err != nil {
				t.Errorf("New(%q) = %v, want accepted", tt.url, err)
			}
		})
	}
}

// TestNewRefusesBeforeTouchingTheTree keeps the refusal ahead of any I/O. New
// does none today, so this is a guard rather than a live risk: it fails if the
// check is ever deferred to Prepare — which clones — or placed after an
// initialization step that grows side effects.
func TestNewRefusesBeforeTouchingTheTree(t *testing.T) {
	parallel(t)
	tests := []struct {
		name   string
		remote string
		want   error
	}{
		{"embedded credentials", "ssh://git:" + canaryPassword + "@" + canaryHost + "/o/r.git", ErrRemoteCredentials},
		{"transport helper", "mock::opaque-address", ErrTransportHelperRemote},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if _, err := New(Options{Root: root, WorkflowKey: "wf", Repository: repo(tt.remote)}); !errors.Is(err, tt.want) {
				t.Fatalf("New error = %v, want %v", err, tt.want)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("refused remote left %d entries under the root", len(entries))
			}
		})
	}
}

func TestIsApplicable(t *testing.T) {
	parallel(t)
	p := newProvider(t, newFixture(t), Hooks{})
	if !p.IsApplicable(context.Background()) {
		t.Error("the v1 strategy must always be applicable (SPEC §6.1)")
	}
}

func TestPrepareEmptyIdentifier(t *testing.T) {
	parallel(t)
	p := newProvider(t, newFixture(t), Hooks{})
	if _, err := prepareForTest(t, p, context.Background(), core.Issue{}, 1); err == nil {
		t.Error("Prepare accepted an issue with no identifier")
	}
}

func TestPrepareFirstAttempt(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if ws.Key != "42" || ws.Branch != "ben/42" {
		t.Errorf("ws = %+v; want key 42, branch ben/42", ws)
	}
	if !ws.CreatedNow {
		t.Error("first Prepare must report CreatedNow")
	}
	if ws.BaseSHA != f.head(t) {
		t.Errorf("BaseSHA = %s, want origin head %s", ws.BaseSHA, f.head(t))
	}
	if got := runGit(t, ws.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "ben/42" {
		t.Errorf("workspace branch = %q, want ben/42", got)
	}
	if !samePath(filepath.Dir(ws.Path), p.issuesDir) {
		t.Errorf("workspace %s is not directly under %s (SPEC §6.2 layout)", ws.Path, p.issuesDir)
	}
	if got := runGit(t, p.baseDir, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Error("base.git is not bare")
	}

	// Reattach on the very next call: same workspace, not re-created.
	ws2, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if ws2.CreatedNow {
		t.Error("second Prepare must reattach, not re-create")
	}
	if !samePath(ws2.Path, ws.Path) || ws2.BaseSHA != ws.BaseSHA {
		t.Errorf("second Prepare = %+v, want same path and pinned base as %+v", ws2, ws)
	}
}

// B05 acceptance: retry after a failed attempt reattaches the branch; agent
// commits from the prior attempt survive. The dispose-then-recreate leg is
// the explicit regression test against `worktree add -B` (SPEC §6.2).
func TestRetryReattachesBranchAndKeepsCommits(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws1, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare 1: %v", err)
	}
	agentSHA := agentCommit(t, ws1.Path, "work.txt")

	ws2, err := prepareForTest(t, p, ctx, issue("7"), 2)
	if err != nil {
		t.Fatalf("Prepare 2: %v", err)
	}
	if ws2.CreatedNow {
		t.Error("retry must reattach the existing worktree")
	}
	if head := runGit(t, ws2.Path, "rev-parse", "HEAD"); head != agentSHA {
		t.Errorf("retry head = %s, want surviving agent commit %s", head, agentSHA)
	}

	// Dispose, then prepare again — a fresh provider stands in for a daemon
	// restart (SPEC §9.10): the branch must be reattached, never recreated.
	if err := p.Dispose(ctx, ws2, false); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	p2, err := New(Options{Root: p.root, WorkflowKey: "wf", Repository: core.Repository{RemoteURL: f.origin}, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ws3, err := prepareForTest(t, p2, ctx, issue("7"), 3)
	if err != nil {
		t.Fatalf("Prepare 3: %v", err)
	}
	if !ws3.CreatedNow {
		t.Error("post-dispose Prepare must create a fresh worktree")
	}
	if head := runGit(t, ws3.Path, "rev-parse", "HEAD"); head != agentSHA {
		t.Errorf("recreated head = %s, want %s — worktree add used -B and discarded agent commits", head, agentSHA)
	}
	if ws3.BaseSHA != ws1.BaseSHA {
		t.Errorf("BaseSHA drifted across restart: %s → %s; the claim-time pin must hold (SPEC §9.7)", ws1.BaseSHA, ws3.BaseSHA)
	}
}

// B05 acceptance: stale worktree registration / crashed-run debris is
// recovered by prune-and-retry (SPEC §6.6).
func TestStaleRegistrationRecovered(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws1, err := prepareForTest(t, p, ctx, issue("9"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	agentSHA := agentCommit(t, ws1.Path, "progress.txt")

	// Crash debris: the directory vanished but the registration remains.
	if err := os.RemoveAll(ws1.Path); err != nil {
		t.Fatal(err)
	}
	ws2, err := prepareForTest(t, p, ctx, issue("9"), 2)
	if err != nil {
		t.Fatalf("Prepare after debris: %v", err)
	}
	if !ws2.CreatedNow {
		t.Error("recovery must recreate the worktree")
	}
	if head := runGit(t, ws2.Path, "rev-parse", "HEAD"); head != agentSHA {
		t.Errorf("recovered head = %s, want %s (branch must survive debris recovery)", head, agentSHA)
	}
}

// B05 acceptance: an unrecognized state aborts loudly with the workspace
// kept — no plain-directory fallback guessing (SPEC §6.6).
func TestUnregisteredDirFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	stray := filepath.Join(p.issuesDir, Key("13"))
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stray, "evidence.txt"), "not a worktree\n")

	_, err := prepareForTest(t, p, ctx, issue("13"), 1)
	if !errors.Is(err, ErrWorkspaceState) {
		t.Fatalf("err = %v, want ErrWorkspaceState", err)
	}
	if _, statErr := os.Stat(filepath.Join(stray, "evidence.txt")); statErr != nil {
		t.Error("fail-closed path must keep the workspace for forensics")
	}
}

// A worktree left on the wrong branch (agent misbehavior) is ambiguous
// state: refuse, keep everything (SPEC §6.6).
func TestWrongBranchFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("21"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	runGit(t, ws.Path, "checkout", "--quiet", "-b", "rogue")

	_, err = prepareForTest(t, p, ctx, issue("21"), 2)
	if !errors.Is(err, ErrWorkspaceState) {
		t.Fatalf("err = %v, want ErrWorkspaceState", err)
	}
	if !dirExists(ws.Path) {
		t.Error("fail-closed path must keep the workspace")
	}
}

func TestCorruptBaseFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)

	tests := []struct {
		name  string
		setup func(t *testing.T, baseDir string)
		check func(t *testing.T, baseDir string)
	}{
		{
			name: "empty directory",
			setup: func(t *testing.T, baseDir string) {
				if err := os.MkdirAll(baseDir, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, baseDir string) {
				entries, err := os.ReadDir(baseDir)
				if err != nil || len(entries) != 0 {
					t.Error("no auto-repair: the empty dir must be untouched")
				}
			},
		},
		{
			name: "regular file",
			setup: func(t *testing.T, baseDir string) {
				if err := os.MkdirAll(filepath.Dir(baseDir), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, baseDir, "not a repo\n")
			},
			check: func(t *testing.T, baseDir string) {
				st, err := os.Stat(baseDir)
				if err != nil || st.IsDir() {
					t.Error("no auto-repair: the file must be untouched")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProvider(t, f, Hooks{})
			tt.setup(t, p.baseDir)
			_, err := prepareForTest(t, p, ctx, issue("1"), 1)
			if !errors.Is(err, ErrBaseRepoState) {
				t.Fatalf("err = %v, want ErrBaseRepoState", err)
			}
			tt.check(t, p.baseDir)
		})
	}
}

// Fetch-before-attempt: new issues base on the moved origin head, while an
// already-claimed issue keeps its pinned base (SPEC §6.2, §9.7).
func TestFetchBeforeAttempt(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws1, err := prepareForTest(t, p, ctx, issue("1"), 1)
	if err != nil {
		t.Fatalf("Prepare 1: %v", err)
	}
	sha0 := ws1.BaseSHA

	sha1 := f.pushCommit(t, "upstream moved")

	ws2, err := prepareForTest(t, p, ctx, issue("2"), 1)
	if err != nil {
		t.Fatalf("Prepare 2: %v", err)
	}
	if ws2.BaseSHA != sha1 {
		t.Errorf("new issue BaseSHA = %s, want fetched head %s", ws2.BaseSHA, sha1)
	}

	ws1b, err := prepareForTest(t, p, ctx, issue("1"), 2)
	if err != nil {
		t.Fatalf("Prepare 1 retry: %v", err)
	}
	if ws1b.BaseSHA != sha0 {
		t.Errorf("claimed issue BaseSHA = %s, want pinned %s", ws1b.BaseSHA, sha0)
	}
}

func TestDispose(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	log := filepath.Join(t.TempDir(), "hooks.log")
	p := newProvider(t, f, Hooks{
		BeforeRemove: fmt.Sprintf("echo before_remove >> %q", log),
	})

	ws, err := prepareForTest(t, p, ctx, issue("5"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// keep=true preserves everything on disk, but "before any dispose" is
	// literal (SPEC §5.2.6): the hook fires here too.
	if err := p.Dispose(ctx, ws, true); err != nil {
		t.Fatalf("Dispose(keep): %v", err)
	}
	if !dirExists(ws.Path) {
		t.Fatal("Dispose(keep=true) removed the workspace")
	}
	if out, err := os.ReadFile(log); err != nil || strings.TrimSpace(string(out)) != "before_remove" {
		t.Errorf("after Dispose(keep=true), hook log = %q, %v; want one firing", out, err)
	}

	if err := p.Dispose(ctx, ws, false); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if dirExists(ws.Path) {
		t.Error("Dispose(keep=false) left the workspace directory")
	}
	if out, _ := os.ReadFile(log); strings.Count(string(out), "before_remove") != 2 {
		t.Errorf("hook log = %q; want a second firing from the removal", out)
	}
	if list := runGit(t, p.baseDir, "worktree", "list", "--porcelain"); strings.Contains(list, "branch refs/heads/ben/5") {
		t.Error("registration survived dispose")
	}
	// The branch is the archive — never deleted (SPEC §6.4).
	runGit(t, p.baseDir, "rev-parse", "--verify", "refs/heads/ben/5")

	// Idempotent: disposing an already-gone workspace is not an error.
	if err := p.Dispose(ctx, ws, false); err != nil {
		t.Errorf("second Dispose: %v", err)
	}
}

func TestDisposePathEscape(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	outside := t.TempDir()

	tests := []struct {
		name string
		path string
	}{
		{"absolute outside", outside},
		{"traversal", filepath.Join(p.issuesDir, "..", "..", "somewhere")},
		{"issues dir itself", p.issuesDir},
		{"base repo", p.baseDir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Dispose(ctx, core.Workspace{WorkspacePaths: core.WorkspacePaths{Path: tt.path}, Key: "x"}, false)
			if !errors.Is(err, ErrPathEscape) {
				t.Errorf("Dispose(%s) err = %v, want ErrPathEscape", tt.path, err)
			}
		})
	}
	if !dirExists(outside) {
		t.Fatal("escape target was removed")
	}
}

// B05 acceptance: the hook matrix. after_create/before_run failures abort;
// after_run/before_remove failures are logged and ignored (SPEC §5.2.6).
func TestHookFailureSemantics(t *testing.T) {
	parallel(t)
	ctx := context.Background()

	t.Run("after_create aborts creation; bootstrap re-runs until it succeeds", func(t *testing.T) {
		f := newFixture(t)
		p := newProvider(t, f, Hooks{AfterCreate: "exit 7"})
		ws, err := prepareForTest(t, p, ctx, issue("1"), 1)
		if !errors.Is(err, ErrHookFailed) {
			t.Fatalf("err = %v, want ErrHookFailed", err)
		}
		if !dirExists(ws.Path) {
			t.Error("failed after_create keeps the worktree; only the ready marker is withheld")
		}
		// A fresh provider with the hook fixed (daemon restart with a
		// corrected config) must re-run the bootstrap, not silently reuse
		// the half-built workspace.
		marker := filepath.Join(t.TempDir(), "bootstrap.ran")
		p2, err := New(Options{
			Root: p.root, WorkflowKey: "wf", Repository: repo(f.origin),
			Hooks:  Hooks{AfterCreate: fmt.Sprintf("touch %q", marker)},
			Logger: quietLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ws2, err := prepareForTest(t, p2, ctx, issue("1"), 1)
		if err != nil {
			t.Fatalf("Prepare after fixing hook: %v", err)
		}
		if !ws2.CreatedNow {
			t.Error("completing the bootstrap must report CreatedNow")
		}
		if _, err := os.Stat(marker); err != nil {
			t.Error("after_create did not re-run on the incomplete workspace")
		}
		// Once complete, the bootstrap stays complete.
		ws3, err := prepareForTest(t, p2, ctx, issue("1"), 2)
		if err != nil {
			t.Fatalf("Prepare 2: %v", err)
		}
		if ws3.CreatedNow {
			t.Error("completed bootstrap must not re-run")
		}
	})

	t.Run("before_run aborts the attempt but keeps the workspace", func(t *testing.T) {
		f := newFixture(t)
		p := newProvider(t, f, Hooks{BeforeRun: "exit 7"})
		ws, err := prepareForTest(t, p, ctx, issue("1"), 1)
		if !errors.Is(err, ErrHookFailed) {
			t.Fatalf("err = %v, want ErrHookFailed", err)
		}
		if !dirExists(ws.Path) {
			t.Error("failed before_run must keep the workspace")
		}
	})

	t.Run("after_run failure is ignored", func(t *testing.T) {
		f := newFixture(t)
		p := newProvider(t, f, Hooks{AfterRun: "exit 7"})
		ws, err := prepareForTest(t, p, ctx, issue("1"), 1)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		p.AfterRun(ctx, ws) // must not panic or disturb the workspace
		if !dirExists(ws.Path) {
			t.Error("AfterRun disturbed the workspace")
		}
	})

	t.Run("before_remove failure is ignored and removal proceeds", func(t *testing.T) {
		f := newFixture(t)
		p := newProvider(t, f, Hooks{BeforeRemove: "exit 7"})
		ws, err := prepareForTest(t, p, ctx, issue("1"), 1)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if err := p.Dispose(ctx, ws, false); err != nil {
			t.Fatalf("Dispose: %v", err)
		}
		if dirExists(ws.Path) {
			t.Error("failed before_remove must not block removal")
		}
	})
}

// B05 acceptance: all four hooks fire at the specified points, with
// cwd = workspace (SPEC §5.2.6, §6.5).
func TestHooksFireInOrderWithWorkspaceCwd(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	log := filepath.Join(t.TempDir(), "hooks.log")
	mark := func(name string) string {
		return fmt.Sprintf("echo %s \"$(pwd)\" >> %q", name, log)
	}
	p := newProvider(t, f, Hooks{
		AfterCreate:  mark("after_create"),
		BeforeRun:    mark("before_run"),
		AfterRun:     mark("after_run"),
		BeforeRemove: mark("before_remove"),
	})

	ws, err := prepareForTest(t, p, ctx, issue("3"), 1)
	if err != nil {
		t.Fatalf("Prepare 1: %v", err)
	}
	if _, err := prepareForTest(t, p, ctx, issue("3"), 2); err != nil {
		t.Fatalf("Prepare 2: %v", err)
	}
	p.AfterRun(ctx, ws)
	if err := p.Dispose(ctx, ws, false); err != nil {
		t.Fatalf("Dispose: %v", err)
	}

	out, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	wantOrder := []string{"after_create", "before_run", "before_run", "after_run", "before_remove"}
	if len(lines) != len(wantOrder) {
		t.Fatalf("hook log = %q, want %d firings %v", lines, len(wantOrder), wantOrder)
	}
	wsReal := normalizePath(ws.Path)
	for i, line := range lines {
		name, cwd, _ := strings.Cut(line, " ")
		if name != wantOrder[i] {
			t.Errorf("firing %d = %s, want %s", i, name, wantOrder[i])
		}
		if normalizePath(cwd) != wsReal {
			t.Errorf("%s ran with cwd %q, want workspace %q", name, cwd, ws.Path)
		}
	}
}

func TestHookTimeout(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{BeforeRun: "sleep 30", Timeout: 200 * time.Millisecond})

	start := time.Now()
	_, err := prepareForTest(t, p, ctx, issue("1"), 1)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrHookFailed) {
		t.Fatalf("err = %v, want ErrHookFailed", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout mention", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout took %s; the hook bound is not being enforced", elapsed)
	}
}

// #48: a hook must not inherit the daemon's environment. The credential that
// reaches BEN by environment (SPEC §10.2) is exactly what a hook would carry
// away, and the agent the hook bootstraps for is already held to the same
// allowlist (SPEC §6.5, §7.6).
func TestHookEnvironmentIsComposedNotInherited(t *testing.T) {
	ctx := context.Background()
	const secret = "ghp_daemon_only_credential"
	t.Setenv("BEN_TEST_TRACKER_PAT", secret)
	t.Setenv("LANG", "C") // allowlisted, so it must still arrive

	f := newFixture(t)
	dump := filepath.Join(t.TempDir(), "env.txt")
	p := newProvider(t, f, Hooks{AfterCreate: fmt.Sprintf("env > %q", dump)})
	if _, err := prepareForTest(t, p, ctx, issue("1"), 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if name, _, ok := strings.Cut(line, "="); ok {
			names[name] = true
		}
	}
	if names["BEN_TEST_TRACKER_PAT"] {
		t.Error("hook inherited a daemon-only variable")
	}
	// Under any name at all: absence of the key is not absence of the value.
	if strings.Contains(string(raw), secret) {
		t.Errorf("hook environment carries the daemon's secret:\n%s", raw)
	}
	// The remote credential is injected per git invocation (§10.2); a hook is
	// not a git invocation.
	if strings.Contains(string(raw), "hunter2") {
		t.Errorf("hook environment carries the remote credential:\n%s", raw)
	}
	for _, name := range []string{"PATH", "HOME", "LANG"} {
		if !names[name] {
			t.Errorf("hook environment is missing allowlisted %s; hooks still need to find their tools:\n%s", name, raw)
		}
	}
}

// The composition itself, without the shell's own additions (PWD, SHLVL) in
// the way: nothing outside the allowlist crosses, and the slice is never nil —
// os/exec reads a nil Env as "inherit", which is the whole defect (#48).
func TestHookEnvIsExactlyTheAllowlist(t *testing.T) {
	t.Setenv("BEN_TEST_DAEMON_ONLY", "x")
	env := hookEnv()
	if env == nil {
		t.Fatal("hookEnv returned nil; os/exec would inherit the daemon environment")
	}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(core.EnvAllowlist, name) {
			t.Errorf("hookEnv carries %q, which is not in core.EnvAllowlist", name)
		}
	}
}

// #48: `sh -lc` sources /etc/profile and ~/.profile, which makes a hook's
// behavior a function of the operator's dotfiles. §5.2.6 allows stricter.
func TestHookShellIsNotLogin(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".profile"), "export BEN_TEST_PROFILE_MARKER=sourced\n")
	t.Setenv("HOME", home)
	const read = `printf %s "${BEN_TEST_PROFILE_MARKER-}"`

	// Control first: the assertion below only has teeth where this platform's
	// `sh -lc` really does source ~/.profile. Where it doesn't, the test proves
	// nothing and says so instead of passing quietly.
	ctrl := exec.Command("sh", "-lc", read)
	ctrl.Env = hookEnv()
	out, err := ctrl.Output()
	if err != nil || string(out) != "sourced" {
		t.Skipf("`sh -lc` does not source ~/.profile here (got %q, err %v) — the non-login assertion would be vacuous", out, err)
	}

	f := newFixture(t)
	marker := filepath.Join(t.TempDir(), "marker.txt")
	p := newProvider(t, f, Hooks{AfterCreate: fmt.Sprintf("%s > %q", read, marker)})
	if _, err := prepareForTest(t, p, ctx, issue("1"), 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("hook read %q from ~/.profile; hooks must run under a non-login shell", got)
	}
}

func TestSweep(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	log := filepath.Join(t.TempDir(), "hooks.log")
	p := newProvider(t, f, Hooks{
		BeforeRemove: fmt.Sprintf("echo before_remove >> %q", log),
	})

	ws1, err := prepareForTest(t, p, ctx, issue("1"), 1)
	if err != nil {
		t.Fatal(err)
	}
	ws2, err := prepareForTest(t, p, ctx, issue("2"), 1)
	if err != nil {
		t.Fatal(err)
	}

	var consulted []string
	err = p.Sweep(ctx, func(key string) bool {
		consulted = append(consulted, key)
		return key == "1"
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if dirExists(ws1.Path) {
		t.Error("terminal workspace survived the sweep")
	}
	if !dirExists(ws2.Path) {
		t.Error("live workspace was swept")
	}
	if len(consulted) != 2 {
		t.Errorf("terminal predicate consulted for %v, want both workspace keys", consulted)
	}
	// The sweep's dispose fires before_remove too (SPEC §5.2.6).
	if out, _ := os.ReadFile(log); strings.TrimSpace(string(out)) != "before_remove" {
		t.Errorf("before_remove log = %q, want one firing from the sweep", out)
	}
}

func TestSweepWithoutWorkspaces(t *testing.T) {
	parallel(t)
	p := newProvider(t, newFixture(t), Hooks{})
	if err := p.Sweep(context.Background(), func(string) bool { return true }); err != nil {
		t.Errorf("Sweep on a fresh root: %v", err)
	}
}

// Per-issue locks: concurrent Prepare calls for one issue must yield exactly
// one live workspace and no races (SPEC §6.4, §6.6; run under -race).
func TestConcurrentPrepareSingleWorkspace(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	const n = 6
	var wg sync.WaitGroup
	results := make([]core.Workspace, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = prepareForTest(t, p, ctx, issue("77"), 1)
		}(i)
	}
	wg.Wait()

	created := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("Prepare %d: %v", i, errs[i])
		}
		if results[i].CreatedNow {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d goroutines created the workspace, want exactly 1", created)
	}
	list := runGit(t, p.baseDir, "worktree", "list", "--porcelain")
	if got := strings.Count(list, "branch refs/heads/ben/77"); got != 1 {
		t.Errorf("found %d registrations for ben/77, want 1:\n%s", got, list)
	}
}

// The tracker credential must never surface in errors (SPEC §10.2;
// mandatory redaction §5.8).
func TestRemoteCredentialRedaction(t *testing.T) {
	parallel(t)
	auth := fake.NewRemoteAuth("x-access-token", "sekret123")
	p, err := New(Options{
		Root:        t.TempDir(),
		WorkflowKey: "wf",
		// Port 1 refuses instantly; the fetch fails without a network.
		Repository: core.Repository{RemoteURL: "https://127.0.0.1:1/none.git", AuthSource: auth},
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = prepareForTest(t, p, context.Background(), issue("1"), 1)
	if err == nil {
		t.Fatal("Prepare against a dead remote succeeded")
	}
	if strings.Contains(err.Error(), "sekret123") {
		t.Errorf("credential leaked into error: %v", err)
	}
}

// Redaction follows the credential in use, not one captured at construction
// (SPEC §6.2, amendment 6; #156 decision 2 seam 3).
//
// The old value and the new one are both live in this test, which is the whole
// point: a provider holding the value it was built with would scrub the *stale*
// token while the live one flowed through git's stderr into error text and logs.
// That is a security fix, not a tidy-up, so it is asserted in both directions.
func TestRedactionFollowsRotation(t *testing.T) {
	parallel(t)
	const before, after = "sekret-before-rotation", "sekret-after-rotation"
	auth := fake.NewRemoteAuth("x-access-token", before)
	p, err := New(Options{
		Root:        t.TempDir(),
		WorkflowKey: "wf",
		Repository:  core.Repository{RemoteURL: "https://127.0.0.1:1/none.git", AuthSource: auth},
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareForTest(t, p, context.Background(), issue("1"), 1); err == nil {
		t.Fatal("Prepare against a dead remote succeeded")
	}
	calls := auth.Calls()
	if calls == 0 {
		t.Fatal("no credential was obtained; the fetch cannot have been authenticated")
	}

	auth.Rotate(after)
	_, err = prepareForTest(t, p, context.Background(), issue("2"), 1)
	if err == nil {
		t.Fatal("Prepare against a dead remote succeeded")
	}
	if auth.Calls() <= calls {
		t.Error("the second preparation reused a credential rather than obtaining one")
	}
	// Neither value may appear. The rotated one is the one a construction-time
	// capture would have missed.
	for _, secret := range []string{before, after} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("credential %q leaked into error: %v", secret, err)
		}
	}
}

// A credential source that refuses stops the invocation *before git runs*
// (SPEC §10.2). Asserted as an absence on the exec seam: an unauthenticated
// fetch against a public remote would succeed, which is the fallthrough this
// exists to prevent, and against a private one it would fail as a git error
// nobody could classify.
func TestAFailedCredentialNeverInvokesGit(t *testing.T) {
	parallel(t)
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})
	// A real base clone first, so the only thing standing between this
	// preparation and a working fetch is the credential.
	if _, err := prepareForTest(t, p, context.Background(), issue("1"), 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	sentinel := &core.CredentialError{
		Class: core.CredentialPermanent, Authority: "octo:https://octo.example#org#ben-tracker",
		Err: errors.New("the trust policy does not admit this identity"),
	}
	p.authSource = &refusingAuth{err: sentinel}
	// A ref that does not exist locally, so serving it requires the remote.
	_, err := p.remoteGit(context.Background(), p.baseDir, "ls-remote", "--", p.remoteURL, "HEAD")
	if err == nil {
		t.Fatal("a remote git invocation succeeded with no credential")
	}
	if class, ok := core.CredentialFailure(err); !ok || class != core.CredentialPermanent {
		t.Errorf("class = (%v, %v), want the source's permanent classification to survive", class, ok)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the source's own refusal", err)
	}
}

// refusingAuth counts what it was asked and answers nothing, so the assertion
// above is "git was never reached" rather than "git failed".
type refusingAuth struct{ err error }

func (a *refusingAuth) Auth(context.Context) (core.RemoteAuth, error) {
	return core.RemoteAuth{}, a.err
}

// The credential helper delivers the secret to git through the environment;
// `git credential fill` exercises the exact helper + env plumbing remoteGit
// uses, without a network.
func TestAuthCredentialHelper(t *testing.T) {
	parallel(t)
	cmd := exec.Command("git",
		"-c", "credential.helper=",
		"-c", "credential.helper="+gitCredentialHelper,
		"credential", "fill")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"BEN_REMOTE_USERNAME=x-access-token",
		"BEN_REMOTE_PASSWORD=sekret123",
		"GIT_TERMINAL_PROMPT=0",
	)
	cmd.Stdin = strings.NewReader("protocol=https\nhost=example.invalid\npath=o/r.git\n\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git credential fill: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "username=x-access-token") || !strings.Contains(got, "password=sekret123") {
		t.Errorf("credential fill = %q; helper did not supply the env credential", got)
	}
	// The secret must not be part of any argument the helper mechanism uses.
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "sekret123") {
			t.Errorf("credential appears in argv: %q", arg)
		}
	}
}

// #150: the canonical publish names its destination and performs no local
// upstream bookkeeping. The lock file is the canary: reintroducing -u still
// lands the remote ref and exits 0, but reports that it could not lock the
// shared config. The successful path must neither attempt that write nor
// change the config's bytes.
func TestCanonicalPublishDoesNotWriteSharedConfig(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := runGit(t, ws.Path, "remote", "get-url", "origin"); got != f.origin {
		t.Errorf("origin = %q, want %q (and no credentials)", got, f.origin)
	}
	config := filepath.Join(p.baseDir, "config")
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	const canary = "held to prove the canonical push takes no config lock\n"
	lock := config + ".lock"
	if err := os.WriteFile(lock, []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}

	sha := agentCommit(t, ws.Path, "work.txt")
	cmd := exec.Command("git", "push", "origin", "HEAD")
	cmd.Dir = ws.Path
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("canonical publish: %v\n%s", err, out)
	}
	if strings.Contains(strings.ToLower(string(out)), "error:") {
		t.Errorf("canonical publish reported a config error despite succeeding:\n%s", out)
	}
	if got := runGit(t, f.origin, "rev-parse", "refs/heads/ben/42"); got != sha {
		t.Errorf("origin ben/42 = %s, want pushed %s", got, sha)
	}
	after, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("shared config changed during canonical publish\n--- before ---\n%s\n--- after ---\n%s",
			before, after)
	}
	if got, err := os.ReadFile(lock); err != nil || string(got) != canary {
		t.Errorf("config lock canary = %q, %v; publish attempted to replace it", got, err)
	}
	branchConfig := exec.Command("git", "config", "--local", "--get-regexp", `^branch\.`)
	branchConfig.Dir = ws.Path
	branchOut, branchErr := branchConfig.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(branchErr, &exitErr) || exitErr.ExitCode() != 1 || len(branchOut) != 0 {
		t.Errorf("branch config after publish = %q, err %v; want no branch.* keys", branchOut, branchErr)
	}
}

// An existing base whose origin no longer matches the workflow's remote is
// unexpected state: fail closed, no auto-repair (SPEC §6.2).
func TestBaseOriginMismatchFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	if _, err := prepareForTest(t, p, ctx, issue("1"), 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	runGit(t, p.baseDir, "remote", "set-url", "origin", "https://example.invalid/other.git")
	_, err := prepareForTest(t, p, ctx, issue("1"), 2)
	if !errors.Is(err, ErrBaseRepoState) {
		t.Fatalf("err = %v, want ErrBaseRepoState", err)
	}
}

// Safety invariant 2 covers both links (SPEC §6.3): if issues/ itself is a
// symlink escaping the root, nothing may be created or removed through it.
func TestIssuesDirSymlinkEscapeFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})
	outside := t.TempDir()

	if err := os.MkdirAll(filepath.Dir(p.issuesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, p.issuesDir); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareForTest(t, p, ctx, issue("1"), 1); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Prepare err = %v, want ErrPathEscape", err)
	}
	inside := filepath.Join(p.issuesDir, "1")
	if err := p.Dispose(ctx, core.Workspace{WorkspacePaths: core.WorkspacePaths{Path: inside}, Key: "1"}, false); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Dispose err = %v, want ErrPathEscape", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Error("the escape target was touched")
	}
}

// A branch without its base pin cannot recover the claim-time SHA — guessing
// via merge-base is wrong after merges/rebases, so it fails closed.
func TestMissingBasePinFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ref := claimBaseRef("7", ws.ClaimEpoch)
	runGit(t, p.baseDir, "update-ref", "-d", ref)
	_, err = prepareForTest(t, p, ctx, issue("7"), 2)
	if !errors.Is(err, ErrClaimBaseState) {
		t.Fatalf("err = %v, want ErrClaimBaseState", err)
	}
}

// #16 gap 1: a corrupt ref must never be classified as absent — absence
// enters recreate paths, corruption must fail closed.
func TestRevParseClassification(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Present: the pin resolves.
	ref := claimBaseRef("7", ws.ClaimEpoch)
	sha, ok, err := p.revParse(ctx, ref)
	if err != nil || !ok || sha != ws.BaseSHA {
		t.Errorf("revParse(present) = %q, %v, %v; want %q, true, nil", sha, ok, err, ws.BaseSHA)
	}
	// Absent: silent exit 1 is git's "no such ref" verdict — not an error.
	if _, ok, err := p.revParse(ctx, claimBaseRef("nope", 1)); ok || err != nil {
		t.Errorf("revParse(absent) = _, %v, %v; want false, nil", ok, err)
	}
	// Broken: corruption is an error, never absence.
	pinFile := filepath.Join(p.baseDir, filepath.FromSlash(ref))
	writeFile(t, pinFile, "garbage-not-a-sha\n")
	if _, ok, err := p.revParse(ctx, ref); ok || !errors.Is(err, ErrBaseRepoState) {
		t.Errorf("revParse(broken) = _, %v, %v; want false, ErrBaseRepoState", ok, err)
	}

	// End to end, a corrupt pin refuses the attempt (here git's own fetch
	// connectivity check trips first — also fail-closed) and nothing
	// auto-repairs the damaged ref.
	if _, err := prepareForTest(t, p, ctx, issue("7"), 2); err == nil {
		t.Fatal("Prepare succeeded over a corrupt base pin")
	}
	if got, readErr := os.ReadFile(pinFile); readErr != nil || strings.TrimSpace(string(got)) != "garbage-not-a-sha" {
		t.Errorf("pin file = %q, %v; corruption must be left for forensics, not repaired", got, readErr)
	}
}

// Review P1: an unreadable ref store yields the same silent exit 1 as a
// missing ref — absence must only be accepted after the store proves
// readable, or Dispose would remove a worktree without clearing its marker.
func TestUnreadableRefStoreFailsClosed(t *testing.T) {
	parallel(t)
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure injection does not work as root")
	}
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("5"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	t.Run("unreadable ref directory", func(t *testing.T) {
		readyDir := filepath.Join(p.baseDir, "refs", "ben", "ready")
		if err := os.Chmod(readyDir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(readyDir, 0o755) })

		if _, ok, err := p.revParse(ctx, "refs/ben/ready/5"); ok || !errors.Is(err, ErrBaseRepoState) {
			t.Errorf("revParse = _, %v, %v; want false, ErrBaseRepoState", ok, err)
		}
		if err := p.Dispose(ctx, ws, false); err == nil {
			t.Fatal("Dispose proceeded over an unreadable bootstrap marker store")
		}
		if !dirExists(ws.Path) {
			t.Fatal("worktree removed while its ready marker was unprovably absent")
		}
	})

	t.Run("unreadable ref file", func(t *testing.T) {
		ref := claimBaseRef("5", ws.ClaimEpoch)
		pinFile := filepath.Join(p.baseDir, filepath.FromSlash(ref))
		if err := os.Chmod(pinFile, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(pinFile, 0o644) })

		if _, ok, err := p.revParse(ctx, ref); ok || !errors.Is(err, ErrBaseRepoState) {
			t.Errorf("revParse = _, %v, %v; want false, ErrBaseRepoState", ok, err)
		}
	})
}

// Review P2 (round 3): a dangling symlink in the ref store ENOENTs like a
// missing path, but its target could reappear — it must read as "cannot
// know", never as absence.
func TestDanglingSymlinkRefStoreFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("5"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	t.Run("ref directory is a dangling symlink", func(t *testing.T) {
		readyDir := filepath.Join(p.baseDir, "refs", "ben", "ready")
		if err := os.RemoveAll(readyDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), readyDir); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := p.revParse(ctx, "refs/ben/ready/5"); ok || !errors.Is(err, ErrBaseRepoState) {
			t.Errorf("revParse = _, %v, %v; want false, ErrBaseRepoState", ok, err)
		}
		if err := p.Dispose(ctx, ws, false); err == nil {
			t.Fatal("Dispose proceeded over a dangling-symlink marker store")
		}
		if !dirExists(ws.Path) {
			t.Fatal("worktree removed while its ready marker was unprovably absent")
		}
	})

	t.Run("loose ref file is a dangling symlink", func(t *testing.T) {
		ref := claimBaseRef("5", ws.ClaimEpoch)
		pinFile := filepath.Join(p.baseDir, filepath.FromSlash(ref))
		if err := os.Remove(pinFile); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), pinFile); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := p.revParse(ctx, ref); ok || !errors.Is(err, ErrBaseRepoState) {
			t.Errorf("revParse = _, %v, %v; want false, ErrBaseRepoState", ok, err)
		}
	})
}

// Review P2: revParse returns the peeled commit SHA — a ref left pointing
// at an annotated tag must not leak the tag object ID into BaseSHA.
func TestRevParsePeelsAnnotatedTag(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	if _, err := prepareForTest(t, p, ctx, issue("1"), 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	commit := runGit(t, p.baseDir, "rev-parse", "refs/heads/main")
	runGit(t, p.baseDir, "tag", "-a", "-m", "annotated", "t1", commit)
	tagObj := runGit(t, p.baseDir, "rev-parse", "refs/tags/t1")
	if tagObj == commit {
		t.Fatal("fixture bug: tag object should differ from the commit")
	}
	runGit(t, p.baseDir, "update-ref", "refs/ben/base/9", tagObj)

	sha, ok, err := p.revParse(ctx, "refs/ben/base/9")
	if err != nil || !ok {
		t.Fatalf("revParse = %q, %v, %v", sha, ok, err)
	}
	if sha != commit {
		t.Errorf("revParse returned %s, want peeled commit %s", sha, commit)
	}
}

// Review P2: the sweep prunes stale registrations even when issues/ is
// empty — otherwise they survive startup and block the next Prepare.
func TestSweepPrunesStaleRegistrationsWithEmptyIssuesDir(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("9"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Crash debris: the whole directory is gone, registration remains.
	if err := os.RemoveAll(ws.Path); err != nil {
		t.Fatal(err)
	}
	if err := p.Sweep(ctx, func(string) bool { return false }); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	list := runGit(t, p.baseDir, "worktree", "list", "--porcelain")
	if strings.Contains(list, "branch refs/heads/ben/9") {
		t.Errorf("stale registration survived the sweep:\n%s", list)
	}
}

// #16 gap 2: a bootstrap marker that cannot be cleared blocks removal — a
// stale ready ref would let a future worktree skip its bootstrap.
func TestDisposeReadyMarkerFailureBlocksRemoval(t *testing.T) {
	parallel(t)
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure injection does not work as root")
	}
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("5"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	readyDir := filepath.Join(p.baseDir, "refs", "ben", "ready")
	if err := os.Chmod(readyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(readyDir, 0o755) })

	if err := p.Dispose(ctx, ws, false); !errors.Is(err, ErrWorkspaceState) {
		t.Fatalf("Dispose err = %v, want ErrWorkspaceState", err)
	}
	if !dirExists(ws.Path) {
		t.Fatal("removal proceeded despite the uncleared bootstrap marker")
	}
	if err := os.Chmod(readyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.Dispose(ctx, ws, false); err != nil {
		t.Fatalf("Dispose after restoring permissions: %v", err)
	}
	if dirExists(ws.Path) {
		t.Error("workspace not removed after the marker became clearable")
	}
}

// #16 gap 3a: sweeping requires a validated base — a corrupt base.git must
// stop the sweep before any prune or removal.
func TestSweepCorruptBaseFailsClosed(t *testing.T) {
	parallel(t)
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	stray := filepath.Join(p.issuesDir, "1")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.baseDir, 0o755); err != nil { // empty dir, not a repo
		t.Fatal(err)
	}
	err := p.Sweep(context.Background(), func(string) bool { return true })
	if !errors.Is(err, ErrBaseRepoState) {
		t.Fatalf("err = %v, want ErrBaseRepoState", err)
	}
	if !dirExists(stray) {
		t.Error("sweep mutated state despite the invalid base")
	}
}

// #16 gap 3b: the sweep disposes only directories whose worktree
// registration on the matching ben/* branch is proven; anything else is
// reported and left in place — no directory-name guessing.
func TestSweepUnprovenDirFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws1, err := prepareForTest(t, p, ctx, issue("1"), 1)
	if err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(p.issuesDir, "9")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stray, "evidence.txt"), "not a worktree\n")

	err = p.Sweep(ctx, func(string) bool { return true })
	if !errors.Is(err, ErrWorkspaceState) {
		t.Fatalf("err = %v, want ErrWorkspaceState for the unproven directory", err)
	}
	if dirExists(ws1.Path) {
		t.Error("registered terminal workspace was not disposed")
	}
	if _, statErr := os.Stat(filepath.Join(stray, "evidence.txt")); statErr != nil {
		t.Error("unproven directory was touched")
	}
}

// #16 gap 4: hook output is bounded while the hook runs; errors still carry
// the tail, where the cause lives.
func TestHookOutputBounded(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{
		BeforeRun: "head -c 200000 /dev/zero | tr '\\0' x; echo; echo TAIL-MARKER; exit 9",
	})

	_, err := prepareForTest(t, p, ctx, issue("1"), 1)
	if !errors.Is(err, ErrHookFailed) {
		t.Fatalf("err = %v, want ErrHookFailed", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "TAIL-MARKER") {
		t.Error("error lost the output tail")
	}
	if !strings.Contains(msg, "…") {
		t.Error("truncated output must be marked")
	}
	if len(msg) > outputLimit+512 {
		t.Errorf("error carries %d bytes; hook output is not bounded", len(msg))
	}
}

func TestTailWriter(t *testing.T) {
	parallel(t)
	tests := []struct {
		name      string
		writes    []string
		want      string
		truncated bool
	}{
		{"under limit", []string{"abc"}, "abc", false},
		{"exact limit", []string{"12345678"}, "12345678", false},
		{"single oversized write", []string{"0123456789abcdef"}, "…89abcdef", true},
		{"many small writes", []string{"aaaa", "bbbb", "cccc"}, "…bbbbcccc", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &tailWriter{limit: 8}
			for _, s := range tt.writes {
				if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", s, n, err)
				}
			}
			if got := w.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			if w.truncated != tt.truncated {
				t.Errorf("truncated = %v, want %v", w.truncated, tt.truncated)
			}
		})
	}
}

// A hook timeout must kill the hook's whole process group: an orphaned child
// must not keep mutating the workspace after the hook is reported dead.
func TestHookTimeoutKillsProcessGroup(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	p := newProvider(t, f, Hooks{
		BeforeRun: fmt.Sprintf("sleep 30 & echo $! > %q; wait", pidFile),
		Timeout:   300 * time.Millisecond,
	})

	if _, err := prepareForTest(t, p, ctx, issue("1"), 1); !errors.Is(err, ErrHookFailed) {
		t.Fatalf("err = %v, want ErrHookFailed", err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("hook never wrote its child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("bad pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("orphaned hook child %d survived the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// #16 acceptance: a fresh host (empty workspace root) preparing an issue
// whose ben/* branch already exists on origin must attach that branch —
// never derive a divergent one from the default branch — and pin its base
// at the attached head (SPEC §6.2 remote-first reattach).
func TestColdRebuildReattachesRemoteBranch(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	hostA := newProvider(t, f, Hooks{})

	wsA, err := prepareForTest(t, hostA, ctx, issue("16"), 1)
	if err != nil {
		t.Fatalf("host A Prepare: %v", err)
	}
	pushed := agentCommit(t, wsA.Path, "work.txt")
	agentPush(t, wsA.Path)

	// The default branch moves after the push: a rebuilt host deriving from
	// it instead of reattaching would get a wrong base, not just a wrong head.
	f.pushCommit(t, "upstream moved")

	hostB := newProvider(t, f, Hooks{}) // fresh root: lost XDG dir / new host
	wsB, err := prepareForTest(t, hostB, ctx, issue("16"), 1)
	if err != nil {
		t.Fatalf("host B Prepare: %v", err)
	}
	if !wsB.CreatedNow {
		t.Error("cold rebuild must report CreatedNow (the bootstrap re-runs on a fresh host)")
	}
	if head := runGit(t, wsB.Path, "rev-parse", "HEAD"); head != pushed {
		t.Errorf("cold-rebuild head = %s, want origin's %s — the branch was derived, not attached", head, pushed)
	}
	if wsB.BaseSHA != pushed {
		t.Errorf("cold-rebuild BaseSHA = %s, want attached head %s (branch head at first local prepare)", wsB.BaseSHA, pushed)
	}
	if pin := runGit(t, hostB.baseDir, "rev-parse", claimBaseRef("16", wsB.ClaimEpoch)); pin != pushed {
		t.Errorf("pin = %s, want %s", pin, pushed)
	}
	if got := runGit(t, f.origin, "rev-parse", "refs/heads/ben/16"); got != pushed {
		t.Errorf("origin ben/16 = %s, want untouched %s", got, pushed)
	}
}

// #16 acceptance: two-daemon handoff (#11's reviser pattern). Daemon B has
// never seen the issue but attaches A's pushed branch with its head as the
// base — a revise run that pushes nothing must not verify against A's
// commits — and when the issue returns to A, A's checkout fast-forwards
// over B's commits instead of being reused stale.
func TestTwoDaemonHandoff(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	a := newProvider(t, f, Hooks{})
	b := newProvider(t, f, Hooks{})

	wsA, err := prepareForTest(t, a, ctx, issue("11"), 1)
	if err != nil {
		t.Fatalf("A Prepare: %v", err)
	}
	baseA := wsA.BaseSHA
	c1 := agentCommit(t, wsA.Path, "coder.txt")
	agentPush(t, wsA.Path)

	wsB, err := prepareForTest(t, b, ctx, issue("11"), 1)
	if err != nil {
		t.Fatalf("B Prepare: %v", err)
	}
	if wsB.BaseSHA != c1 {
		t.Errorf("B BaseSHA = %s, want handed-off head %s", wsB.BaseSHA, c1)
	}
	c2 := agentCommit(t, wsB.Path, "reviser.txt")
	agentPush(t, wsB.Path)

	wsA2, err := prepareForTest(t, a, ctx, issue("11"), 2)
	if err != nil {
		t.Fatalf("A re-Prepare: %v", err)
	}
	if wsA2.CreatedNow {
		t.Error("A must reattach its existing worktree")
	}
	if head := runGit(t, wsA2.Path, "rev-parse", "HEAD"); head != c2 {
		t.Errorf("A head after the round trip = %s, want fast-forwarded %s", head, c2)
	}
	if wsA2.BaseSHA != baseA {
		t.Errorf("A BaseSHA = %s, want its first-prepare pin %s", wsA2.BaseSHA, baseA)
	}
}

// The fast-forward also covers a free branch: dispose keeps the local
// branch, another daemon advances origin, and the next prepare must attach
// at the advanced head, not the stale local one.
func TestFreeBranchFastForwardsToOrigin(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	a := newProvider(t, f, Hooks{})

	wsA, err := prepareForTest(t, a, ctx, issue("8"), 1)
	if err != nil {
		t.Fatalf("A Prepare: %v", err)
	}
	agentCommit(t, wsA.Path, "first.txt")
	agentPush(t, wsA.Path)
	if err := a.Dispose(ctx, wsA, false); err != nil {
		t.Fatalf("Dispose: %v", err)
	}

	b := newProvider(t, f, Hooks{})
	wsB, err := prepareForTest(t, b, ctx, issue("8"), 1)
	if err != nil {
		t.Fatalf("B Prepare: %v", err)
	}
	c2 := agentCommit(t, wsB.Path, "second.txt")
	agentPush(t, wsB.Path)

	wsA2, err := prepareForTest(t, a, ctx, issue("8"), 2)
	if err != nil {
		t.Fatalf("A re-Prepare: %v", err)
	}
	if !wsA2.CreatedNow {
		t.Error("post-dispose Prepare must create a fresh worktree")
	}
	if head := runGit(t, wsA2.Path, "rev-parse", "HEAD"); head != c2 {
		t.Errorf("head = %s, want fast-forwarded %s", head, c2)
	}
	if local := runGit(t, a.baseDir, "rev-parse", "refs/heads/ben/8"); local != c2 {
		t.Errorf("free branch = %s, want fast-forwarded %s", local, c2)
	}
}

// #16 acceptance: true divergence — local and origin histories that have
// both moved — refuses with the named error and touches neither side.
func TestDivergedBranchFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	a := newProvider(t, f, Hooks{})

	wsA, err := prepareForTest(t, a, ctx, issue("3"), 1)
	if err != nil {
		t.Fatalf("A Prepare: %v", err)
	}
	cA := agentCommit(t, wsA.Path, "local.txt") // never pushed

	// A second daemon finds nothing on origin, derives from the default
	// branch, and pushes: origin now disagrees with A's unpushed history.
	b := newProvider(t, f, Hooks{})
	wsB, err := prepareForTest(t, b, ctx, issue("3"), 1)
	if err != nil {
		t.Fatalf("B Prepare: %v", err)
	}
	cB := agentCommit(t, wsB.Path, "remote.txt")
	agentPush(t, wsB.Path)

	_, err = prepareForTest(t, a, ctx, issue("3"), 2)
	if !errors.Is(err, ErrBranchDiverged) {
		t.Fatalf("err = %v, want ErrBranchDiverged", err)
	}
	if !dirExists(wsA.Path) {
		t.Error("fail-closed path must keep the workspace")
	}
	if local := runGit(t, a.baseDir, "rev-parse", "refs/heads/ben/3"); local != cA {
		t.Errorf("local branch = %s, want untouched %s", local, cA)
	}
	if remote := runGit(t, f.origin, "rev-parse", "refs/heads/ben/3"); remote != cB {
		t.Errorf("origin branch = %s, want untouched %s", remote, cB)
	}
}

// Local strictly ahead of origin is routine (committed, not yet pushed):
// attach as-is, move nothing on either side.
func TestLocalAheadOfOriginAttachesAsIs(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("2"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	c1 := agentCommit(t, ws.Path, "pushed.txt")
	agentPush(t, ws.Path)
	c2 := agentCommit(t, ws.Path, "unpushed.txt")

	ws2, err := prepareForTest(t, p, ctx, issue("2"), 2)
	if err != nil {
		t.Fatalf("Prepare 2: %v", err)
	}
	if ws2.CreatedNow {
		t.Error("retry must reattach")
	}
	if head := runGit(t, ws2.Path, "rev-parse", "HEAD"); head != c2 {
		t.Errorf("head = %s, want unpushed %s kept", head, c2)
	}
	if remote := runGit(t, f.origin, "rev-parse", "refs/heads/ben/2"); remote != c1 {
		t.Errorf("origin = %s, want untouched %s", remote, c1)
	}
}

// A pre-epoch pin is admitted only as the outgoing comparison fact of a newly
// begun claim. The current epoch pins the attached remote head, so handed-off
// commits cannot satisfy that epoch's "branch advanced" leg (SPEC §9.7; #11).
func TestLegacyPinIsOutgoingWhenAttachingRemoteWork(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	a := newProvider(t, f, Hooks{})

	wsA, err := prepareForTest(t, a, ctx, issue("31"), 1)
	if err != nil {
		t.Fatalf("A Prepare: %v", err)
	}
	c1 := agentCommit(t, wsA.Path, "work.txt")
	agentPush(t, wsA.Path)

	b := newProvider(t, f, Hooks{})
	if _, err := prepareForTest(t, b, ctx, issue("other"), 1); err != nil { // bootstraps B's base.git
		t.Fatalf("B bootstrap Prepare: %v", err)
	}
	// The stale pin: minted at the default head by a prepare whose branch
	// never materialized.
	legacy := runGit(t, b.baseDir, "rev-parse", "refs/heads/main")
	runGit(t, b.baseDir, "update-ref", "refs/ben/base/31", legacy)

	wsB, prior, err := prepareClaimForTest(t, b, ctx, issue("31"), 1)
	if err != nil {
		t.Fatalf("B Prepare: %v", err)
	}
	if wsB.BaseSHA != c1 {
		t.Errorf("BaseSHA = %s, want attached head %s", wsB.BaseSHA, c1)
	}
	if !prior.AdvancedPastBase(legacy) || prior.Head != c1 {
		t.Errorf("prior facts = %+v, want attached head %s past legacy base %s", prior, c1, legacy)
	}
	if pin := runGit(t, b.baseDir, "rev-parse", claimBaseRef("31", wsB.ClaimEpoch)); pin != c1 {
		t.Errorf("claim-scoped pin = %s, want %s", pin, c1)
	}
}

// Pending intent does not freeze the repository ahead of attachment. The first
// claim-aware prepare fetches and attaches, measures that head against the
// outgoing legacy pin for #94, then installs the current epoch's base.
func TestPendingFirstPreparePinsAttachedHead(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	if _, err := prepareForTest(t, p, ctx, issue("other"), 1); err != nil { // bootstraps base.git
		t.Fatalf("bootstrap Prepare: %v", err)
	}
	pinSHA := runGit(t, p.baseDir, "rev-parse", "refs/heads/main")
	runGit(t, p.baseDir, "update-ref", "refs/ben/base/9", pinSHA)
	moved := f.pushCommit(t, "upstream moved past the pin")

	ws, prior, err := prepareClaimForTest(t, p, ctx, issue("9"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if ws.BaseSHA != moved {
		t.Errorf("BaseSHA = %s, want attached head %s", ws.BaseSHA, moved)
	}
	if head := runGit(t, ws.Path, "rev-parse", "HEAD"); head != moved {
		t.Errorf("head = %s, want branch created at fetched head %s", head, moved)
	}
	if !prior.AdvancedPastBase(pinSHA) || prior.Head != moved {
		t.Errorf("prior facts = %+v, want attached head %s past outgoing %s", prior, moved, pinSHA)
	}
}
