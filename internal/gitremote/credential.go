package gitremote

import (
	"net/url"
	"strings"
)

// CredentialHelper is the `credential.helper` BEN installs on a Git invocation
// that contacts the configured remote, and — with CredentialConfig clearing the
// inherited list first — the only credential source that invocation has.
//
// **It answers only for the protocol and host of the remote BEN was configured
// with.** Git writes the request it is about to authenticate to the helper's
// stdin, as `key=value` lines terminated by a blank line; this reads `protocol`
// and `host` out of that request and compares them against the scope
// CredentialEnv derived from the configured remote. Every other request —
// including one that names no scope at all — is answered with silence, which is
// Git's "this helper has no credential" and the correct refusal.
//
// Reading the request at all is the whole of #230. A helper that branches on
// the operation and nothing else hands BEN's tracker/publish credential to
// whatever host Git ends up asking about, and that is not the configured one
// after an HTTP redirect (a GitHub Enterprise deployment is the realistic case)
// or a `url.<base>.insteadOf` rewrite in the base repository a run can write.
// Git's own host scoping — a `[credential "https://host"]` section — is
// structurally unavailable here: the helper is installed as `-c` precisely so
// that no credential route is ever written into a config file a run can read or
// edit (SPEC §10.2), so the scope has to travel with the invocation.
//
// A refusal prints nothing, on stdout or on stderr. Not an omission: Git relays
// helper stderr to its own, which BEN captures into error text and logs, so a
// diagnostic about a credential request is one edit away from being a
// diagnostic containing the credential.
//
// One line and POSIX sh. Git strips the `!`, appends ` <operation>` to the
// string and hands the result to `sh -c`, which is why `$1` is the operation and
// why the trailing `f` is the call; measured under dash and bash. The `get` path
// reads to the request's terminator before deciding rather than stopping at the
// first `host=` it sees, so the answer is over the whole request — and a later
// line cannot revise a check already made.
const CredentialHelper = `!f() { [ "$1" = get ] || exit 0; p= ; h= ; ` +
	`while IFS= read -r l; do case "$l" in "") break ;; ` +
	`protocol=*) p=${l#protocol=} ;; host=*) h=${l#host=} ;; esac; done; ` +
	`[ -n "$BEN_REMOTE_PROTOCOL" ] && [ "$p" = "$BEN_REMOTE_PROTOCOL" ] && ` +
	`[ -n "$BEN_REMOTE_HOST" ] && [ "$h" = "$BEN_REMOTE_HOST" ] || exit 0; ` +
	`printf 'username=%s\npassword=%s\n' "$BEN_REMOTE_USERNAME" "$BEN_REMOTE_PASSWORD"; }; f`

// The credential environment variable names. CredentialHelper reads them from a
// shell string no compiler checks, so these are what CredentialEnv and the tests
// name, and TestCredentialHelperReadsEveryVariableCredentialEnvSets is what holds
// the shell half to them — a rename on one side has to fail somewhere.
const (
	// EnvProtocol and EnvHost are the scope the helper answers for.
	EnvProtocol = "BEN_REMOTE_PROTOCOL"
	EnvHost     = "BEN_REMOTE_HOST"
	// EnvUsername and EnvPassword carry the credential itself.
	EnvUsername = "BEN_REMOTE_USERNAME"
	EnvPassword = "BEN_REMOTE_PASSWORD"
)

// CredentialConfig returns the `-c` arguments that make CredentialHelper the
// invocation's only credential source. The inherited helper list is cleared
// first, so what answers is deterministic rather than whatever the daemon host
// happens to have configured globally.
//
// A fresh slice every call: callers append their own argv to it.
func CredentialConfig() []string {
	return []string{
		"-c", "credential.helper=",
		"-c", "credential.helper=" + CredentialHelper,
	}
}

// CredentialEnv is the child environment CredentialHelper reads: the scope
// derived from remoteURL, then the credential itself. The environment and not
// argv, so the secret is never visible in `ps`, and not Git config, so it is
// never written down at all (SPEC §10.2).
//
// A remote CredentialScope cannot scope — a filesystem path, an scp-like
// address — yields an empty scope, and the helper answers nothing for an empty
// scope. That is not a hole: Git consults a credential helper only for the URL
// transports it authenticates, and for those the scope is always derivable. It
// is also the safe direction, because the two ways to be wrong are not
// symmetric — an under-derived scope costs a refused fetch that says so, and an
// over-derived one is the credential handed to a host BEN never configured.
func CredentialEnv(remoteURL, username, password string) []string {
	protocol, host := CredentialScope(remoteURL)
	return []string{
		EnvProtocol + "=" + protocol,
		EnvHost + "=" + host,
		EnvUsername + "=" + username,
		EnvPassword + "=" + password,
	}
}

// CredentialScope reports the `protocol` and `host` Git names in the credential
// request it writes when it contacts remoteURL. Both are empty when remoteURL
// carries no URL authority for Git to take them from.
//
// Git's reading of the URL rather than net/url's, because Git is the thing that
// asks: the scheme is the text before the first `://` **verbatim**, where
// net/url would lowercase it; the authority runs to the first `/`; userinfo up
// to its first `@` is dropped; the port stays, because two forges can serve
// different repositories on one host; and what remains is percent-decoded, with
// an undecodable escape refused outright the way Git refuses such a URL rather
// than guessed at.
//
// Both are returned together and neither is normalized. The comparison this
// feeds is byte equality against a request Git derived from this same string,
// so any divergence that remains is a scope Git's request cannot match — a
// refusal, never a wider grant.
func CredentialScope(remoteURL string) (protocol, host string) {
	scheme, rest, ok := strings.Cut(remoteURL, "://")
	if !ok || scheme == "" {
		// An scp-like address, a filesystem path, or a transport helper (refused
		// by IsTransportHelper before either Git driver acts). Git authenticates
		// none of them through a credential helper.
		return "", ""
	}
	authority := rest
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	if at := strings.IndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	decoded, err := url.PathUnescape(authority)
	if err != nil || decoded == "" {
		return "", ""
	}
	return scheme, decoded
}

// IsCleartextTransport reports whether Git would send a credential for
// remoteURL over a transport that carries it in the clear.
//
// `http` and `ftp` are the two, and they are the two because they are exactly
// the schemes that are both credential-helper transports for Git and
// unencrypted. This is deliberately not "anything that is not https": ssh
// authenticates outside the credential protocol, and for a filesystem path or a
// `file://` URL Git asks no helper at all, so refusing those would refuse
// configurations that expose nothing — including the local remotes BEN's own
// suites drive real Git against.
//
// Case-folded, while CredentialScope keeps the scheme verbatim, and the
// asymmetry is intended. Git matches its transports case-sensitively, so
// `HTTP://…` is a remote Git cannot fetch from at all; folding here only widens
// a refusal, whereas folding there would widen a grant.
func IsCleartextTransport(remoteURL string) bool {
	scheme, _, ok := strings.Cut(remoteURL, "://")
	if !ok {
		return false
	}
	switch strings.ToLower(scheme) {
	case "http", "ftp":
		return true
	default:
		return false
	}
}
