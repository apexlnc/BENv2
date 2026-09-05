package gitremote

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCredentialScope(t *testing.T) {
	tests := []struct {
		name, remote, protocol, host string
	}{
		{"https", "https://github.test/acme/repo.git", "https", "github.test"},
		{"port is part of the host git asks about", "https://github.test:8443/acme/repo.git", "https", "github.test:8443"},
		{"no path", "https://github.test", "https", "github.test"},
		{"trailing slash only", "https://github.test/", "https", "github.test"},
		{"query and fragment stay out of the authority", "https://github.test/x?a=b#c", "https", "github.test"},
		{"query without a path stays out of the authority", "https://github.test?a=b", "https", "github.test"},
		{"fragment without a path stays out of the authority", "https://github.test#where", "https", "github.test"},
		{"http keeps its own protocol", "http://github.test/acme/repo.git", "http", "github.test"},
		{"ssh URL", "ssh://git@github.test/acme/repo.git", "ssh", "github.test"},
		{"ipv6 literal keeps its brackets", "https://[2001:db8::1]:8443/acme/repo.git", "https", "[2001:db8::1]:8443"},
		// Git takes the scheme verbatim; net/url would fold it, and a scope that
		// did would claim to cover a request git spells differently.
		{"scheme is not folded", "HTTPS://github.test/acme/repo.git", "HTTPS", "github.test"},
		// Git drops userinfo up to the first `@` of the authority. Both driving
		// packages refuse these before construction (EmbedsCredential); the scope
		// still has to be right, because a wrong one is a wrong grant.
		{"userinfo dropped", "https://user@github.test/acme/repo.git", "https", "github.test"},
		{"userinfo with password dropped", "https://user:pw@github.test/acme/repo.git", "https", "github.test"},
		{"first at sign wins, as in git", "https://a@b@github.test/x", "https", "b@github.test"},
		{"percent-escapes decoded, as git decodes them", "https://git%68ub.test/acme/repo.git", "https", "github.test"},

		// No URL authority: git authenticates none of these through a helper, so
		// there is no scope and the helper answers nothing.
		{"scp-like", "git@github.test:acme/repo.git", "", ""},
		{"local path", "/srv/git/acme/repo.git", "", ""},
		{"relative path", "../repo.git", "", ""},
		{"empty", "", "", ""},
		{"scheme only", "://github.test/x", "", ""},
		{"empty authority", "https:///acme/repo.git", "", ""},
		// Git refuses a URL whose escapes will not decode rather than guessing at
		// it; so does this, and an empty scope is a refusal.
		{"undecodable escape", "https://git%zzub.test/x", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			protocol, host := CredentialScope(tt.remote)
			if protocol != tt.protocol || host != tt.host {
				t.Errorf("CredentialScope(%q) = (%q, %q), want (%q, %q)",
					tt.remote, protocol, host, tt.protocol, tt.host)
			}
		})
	}
}

func TestIsCleartextTransport(t *testing.T) {
	tests := []struct {
		remote string
		want   bool
	}{
		{"http://github.test/acme/repo.git", true},
		{"HTTP://github.test/acme/repo.git", true},
		{"ftp://github.test/acme/repo.git", true},
		{"https://github.test/acme/repo.git", false},
		{"ftps://github.test/acme/repo.git", false},
		{"ssh://git@github.test/acme/repo.git", false},
		{"git://github.test/acme/repo.git", false},
		{"file:///srv/git/repo.git", false},
		{"git@github.test:acme/repo.git", false},
		{"/srv/git/acme/repo.git", false},
		{"", false},
		// Not a prefix match: the scheme is what precedes the separator.
		{"https://http.github.test/x", false},
	}
	for _, tt := range tests {
		t.Run(tt.remote, func(t *testing.T) {
			if got := IsCleartextTransport(tt.remote); got != tt.want {
				t.Errorf("IsCleartextTransport(%q) = %v, want %v", tt.remote, got, tt.want)
			}
		})
	}
}

// CredentialEnv sets exactly the four variables the shell reads, and the shell
// reads exactly the four it sets.
//
// The independent anchor for the constants (AGENTS.md, Conventions): the two
// halves are a Go slice and a shell string no compiler relates to each other, so
// a rename on one side has to fail somewhere. Written against `$NAME` as the
// shell spells the read, not against the constant alone, which the assignment
// half would satisfy on its own.
func TestCredentialHelperReadsEveryVariableCredentialEnvSets(t *testing.T) {
	env := CredentialEnv("https://github.test/acme/repo.git", "x-access-token", "sekret123")
	want := map[string]string{
		EnvProtocol: "https",
		EnvHost:     "github.test",
		EnvUsername: "x-access-token",
		EnvPassword: "sekret123",
	}
	if len(env) != len(want) {
		t.Errorf("CredentialEnv returned %d variables, want %d: %v", len(env), len(want), env)
	}
	got := map[string]string{}
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("CredentialEnv produced %q, which is not a key=value entry", kv)
		}
		got[key] = value
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("CredentialEnv set %s=%q, want %q", name, got[name], value)
		}
		if !strings.Contains(CredentialHelper, "$"+name) {
			t.Errorf("CredentialHelper never reads $%s, so CredentialEnv is setting a variable "+
				"nothing consumes", name)
		}
	}
}

// The helper is installed as the invocation's only credential source: the
// inherited list is cleared first, and the reset must come before the helper or
// it would clear it.
func TestCredentialConfigClearsInheritedHelpersFirst(t *testing.T) {
	got := CredentialConfig()
	want := []string{"-c", "credential.helper=", "-c", "credential.helper=" + CredentialHelper}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("CredentialConfig() = %q, want %q", got, want)
	}
	// A fresh slice per call: appending the caller's own argv is the documented
	// use, and a shared backing array would let two concurrent invocations
	// overwrite each other's subcommand.
	a := append(CredentialConfig(), "one")
	b := append(CredentialConfig(), "two")
	if a[len(a)-1] != "one" || b[len(b)-1] != "two" {
		t.Errorf("CredentialConfig() results share storage: %v / %v", a, b)
	}
}

// The behavioral case: real git, driving the real helper, over a request for a
// host that is not the configured one.
//
// **This is the test that fails before #230.** The previous helper branched on
// the operation alone, so it answered `username=`/`password=` for `evil.test`
// exactly as it did for the configured host.
//
// Two helpers rather than one, and that is what makes "refused" observable
// without a prompt: ours is asked first, and a sentinel behind it answers
// whatever ours declined. So the credential git ends up with names the helper
// that produced it, git exits 0 either way, and neither outcome is confused with
// git failing for a reason of its own.
func TestCredentialHelperAnswersOnlyItsConfiguredScope(t *testing.T) {
	git := requireGit(t)
	const (
		secret   = "sekret123"
		username = "x-access-token"
		// The sentinel's answer is what git reports when our helper declines.
		fallbackUser = "sentinel-user"
		fallbackPass = "sentinel-pass"
	)
	// Configured for exactly one place: https, github.test, port 8443.
	const remote = "https://github.test:8443/acme/repo.git"
	sentinel := `!f() { if [ "$1" = get ]; then printf 'username=` + fallbackUser +
		`\npassword=` + fallbackPass + `\n'; fi; }; f`

	tests := []struct {
		name    string
		request string
		ours    bool
	}{
		{"the configured protocol and host", "protocol=https\nhost=github.test:8443\npath=acme/repo.git\n", true},
		{"an unread attribute first", "path=acme/repo.git\nprotocol=https\nhost=github.test:8443\n", true},
		{"a different host", "protocol=https\nhost=evil.test\npath=acme/repo.git\n", false},
		{"a host the configured one is a suffix of", "protocol=https\nhost=evil.github.test:8443\n", false},
		{"a host that extends the configured one", "protocol=https\nhost=github.test:8443.evil.test\n", false},
		{"the configured host on a different port", "protocol=https\nhost=github.test:9443\n", false},
		{"the configured host with no port", "protocol=https\nhost=github.test\n", false},
		{"the configured host over cleartext", "protocol=http\nhost=github.test:8443\n", false},
		// A request carrying no protocol or no host is not driven here: git
		// refuses to work with such a credential and dies before any helper is
		// consulted. Those shapes are driven at the shell below.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(git,
				"-c", "credential.helper=",
				"-c", "credential.helper="+CredentialHelper,
				"-c", "credential.helper="+sentinel,
				"credential", "fill")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), CredentialEnv(remote, username, secret)...)
			cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
			cmd.Stdin = strings.NewReader(tt.request + "\n")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("git credential fill: %v\nstdout: %s\nstderr: %s", err, &stdout, &stderr)
			}
			// git relays helper stderr to its own, so anything written there
			// reaches BEN's error text and logs — where a message about a
			// credential request is one edit away from carrying the credential.
			if stderr.Len() != 0 {
				t.Errorf("the helper wrote to stderr: %s", &stderr)
			}

			answered := stdout.String()
			if tt.ours {
				if !strings.Contains(answered, "username="+username) ||
					!strings.Contains(answered, "password="+secret) {
					t.Errorf("git filled %q for the configured remote; the helper did not answer its own scope",
						answered)
				}
				return
			}
			// The refusal, from both directions: the sentinel behind us answered,
			// and no part of BEN's credential reached git.
			if !strings.Contains(answered, "username="+fallbackUser) {
				t.Errorf("git filled %q, want the sentinel's answer — our helper did not decline", answered)
			}
			for _, leak := range []string{secret, username} {
				if strings.Contains(answered, leak) {
					t.Errorf("the helper handed %q to a request outside its scope: %s", leak, answered)
				}
			}
		})
	}
}

// The two request shapes git will not produce: no protocol, and no host. Git
// dies on such a credential before it consults any helper, so they are driven at
// the shell instead — exactly as git drives it, by stripping the leading `!`,
// appending the operation and handing the result to `sh -c`.
//
// Worth driving anyway, because the same guard covers the case a *caller* can
// produce: CredentialEnv over a remote with no URL authority sets an empty
// scope, and an empty scope must answer nothing rather than match an empty
// request field.
func TestCredentialHelperRefusesWithoutABothSidedScope(t *testing.T) {
	const secret = "sekret123"
	tests := []struct {
		name, remote, request string
	}{
		{"no scope, empty request", "/srv/git/repo.git", ""},
		{"no scope, a request naming a host", "/srv/git/repo.git", "protocol=https\nhost=github.test\n"},
		{"no scope, scp-like remote", "git@github.test:acme/repo.git", "protocol=ssh\nhost=github.test\n"},
		{"a scope, but the request names no host", "https://github.test/x", "protocol=https\n"},
		{"a scope, but the request names no protocol", "https://github.test/x", "host=github.test\n"},
		{"a scope, but the request names neither", "https://github.test/x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr := runHelper(t, tt.remote, secret, "get", tt.request+"\n")
			if stdout != "" || stderr != "" {
				t.Errorf("the helper answered a request outside its scope: stdout %q, stderr %q",
					stdout, stderr)
			}
		})
	}
}

// The request parse itself: the helper reads only the request, tolerates
// attributes it has no interest in, and stops at the blank line terminating it.
//
// At the shell rather than through `git credential fill` because the interesting
// attributes are version-dependent — `wwwauth[]` and `capability[]` are recent
// additions, and a git that does not know one warns about it on the very stderr
// this suite requires to be empty. What the helper owes them does not depend on
// which git is in front of it.
func TestCredentialHelperReadsOnlyTheRequest(t *testing.T) {
	const (
		remote = "https://github.test:8443/acme/repo.git"
		secret = "sekret123"
	)
	tests := []struct {
		name    string
		request string
		answers bool
	}{
		{
			"attributes it does not read",
			"capability[]=authtype\nprotocol=https\nwwwauth[]=Basic realm=\"x\"\n" +
				"host=github.test:8443\npath=acme/repo.git\n\n",
			true,
		},
		{
			// A value containing `=` is one attribute, not two.
			"a value containing the separator",
			"protocol=https\nhost=github.test:8443\npath=a=b\n\n",
			true,
		},
		{
			// git sends one request per invocation and the blank line ends it.
			// Reading past it would let a later `host=` revise a scope decision
			// the request had already settled.
			"a second request behind the terminator",
			"protocol=https\nhost=evil.test\n\nprotocol=https\nhost=github.test:8443\n\n",
			false,
		},
		{
			"the configured scope, then trailing data",
			"protocol=https\nhost=github.test:8443\n\nhost=evil.test\n",
			true,
		},
		{
			// None of this is syntax to the helper: it reads with `read -r` and
			// compares quoted, so the request is data on every path.
			"a value that would otherwise be shell syntax",
			"protocol=https\nhost=$(id);`id`;*\n\n",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr := runHelper(t, remote, secret, "get", tt.request)
			if stderr != "" {
				t.Errorf("the helper wrote to stderr: %q", stderr)
			}
			if !tt.answers {
				if stdout != "" {
					t.Errorf("the helper answered a request outside its scope: %q", stdout)
				}
				return
			}
			want := "username=x-access-token\npassword=" + secret + "\n"
			if stdout != want {
				t.Errorf("the helper answered %q, want %q", stdout, want)
			}
		})
	}
}

// The operations that are not `get`. A helper must not answer them, and must not
// fail either: git treats a non-zero exit as a broken helper and says so.
func TestCredentialHelperIsSilentForStoreAndErase(t *testing.T) {
	git := requireGit(t)
	const remote = "https://github.test/acme/repo.git"
	for _, op := range []string{"approve", "reject"} {
		t.Run(op, func(t *testing.T) {
			cmd := exec.Command(git,
				"-c", "credential.helper=",
				"-c", "credential.helper="+CredentialHelper,
				"credential", op)
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), CredentialEnv(remote, "x-access-token", "sekret123")...)
			cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
			cmd.Stdin = strings.NewReader("protocol=https\nhost=github.test\nusername=u\npassword=p\n\n")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("git credential %s: %v\nstderr: %s", op, err, &stderr)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("git credential %s produced stdout %q / stderr %q, want silence",
					op, stdout.String(), stderr.String())
			}
		})
	}
}

// runHelper drives the helper the way git drives it: the leading `!` stripped,
// the operation appended, the whole thing handed to `sh -c`, the request on
// stdin. A non-zero exit is a failure rather than a refusal — git reports a
// helper that exits non-zero as broken, so silence at exit 0 is the only refusal
// this helper is allowed to make.
func runHelper(t *testing.T, remote, secret, op, request string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", strings.TrimPrefix(CredentialHelper, "!")+" "+op)
	cmd.Env = append(os.Environ(), CredentialEnv(remote, "x-access-token", secret)...)
	cmd.Stdin = strings.NewReader(request)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("the helper exited non-zero: %v\nstdout: %s\nstderr: %s", err, &out, &errOut)
	}
	return out.String(), errOut.String()
}

func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
	return path
}
