// Package gitremote owns the syntactic safety checks applied to repository
// remotes before any BEN component passes them to Git or records an identity.
package gitremote

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Named refusals are deliberately constant: callers may put these errors in
// logs, while the rejected value may itself contain a credential.
var (
	ErrRemoteEmpty           = errors.New("gitremote: remote URL is empty")
	ErrRemoteCredentials     = errors.New("gitremote: remote URL embeds credentials")
	ErrTransportHelperRemote = errors.New("gitremote: Git transport-helper remotes are not supported")
)

// RepositoryIdentity reduces a credential-free Git remote to the comparison
// key BEN records in durable facts.
//
// The readable suffix is not a URL to fetch from: it omits transport details
// and a conventional SSH username while keeping the host's port. Those omitted
// details can still distinguish two repositories, though, as can a literal
// path ending in .git from one without it. The fingerprint therefore covers the
// exact configured remote and makes the durable comparison key collision-
// resistant without recording a reconstructible fetch URL.
func RepositoryIdentity(remoteURL string) (string, error) {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return "", ErrRemoteEmpty
	}
	if IsTransportHelper(raw) {
		return "", ErrTransportHelperRemote
	}
	if EmbedsCredential(raw) {
		return "", ErrRemoteCredentials
	}
	var display string
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		path := strings.TrimSuffix(strings.TrimPrefix(u.EscapedPath(), "/"), ".git")
		display = u.Host + "/" + path
	} else {
		// scp-like or a path. Everything before the last `@` of the authority is
		// dropped: `git@host:owner/repo` keeps `host:owner/repo`.
		display = raw
		if slash := strings.IndexByte(display, '/'); slash < 0 || strings.IndexByte(display, '@') < slash {
			if at := strings.LastIndexByte(display, '@'); at >= 0 {
				display = display[at+1:]
			}
		}
		display = strings.TrimSuffix(display, ".git")
	}

	fingerprint := sha256.Sum256([]byte(remoteURL))
	return fmt.Sprintf("sha256:%x/%s", fingerprint, display), nil
}

// IsTransportHelper recognizes Git's explicit <helper>::<address> syntax.
// The helper owns the address grammar, so callers must refuse the whole form
// rather than guess whether its opaque address contains a credential.
func IsTransportHelper(remoteURL string) bool {
	for i := 0; i < len(remoteURL); i++ {
		if isTransportHelperNameByte(remoteURL[i], i == 0) {
			continue
		}
		return remoteURL[i] == ':' && i+1 < len(remoteURL) && remoteURL[i+1] == ':'
	}
	return false
}

func isTransportHelperNameByte(c byte, first bool) bool {
	alphanumeric := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	return alphanumeric || !first && (c == '+' || c == '-' || c == '.')
}

// EmbedsCredential reports whether a remote URL carries a credential in its
// userinfo (SPEC §10.2; #52). A password component is refused for every URL
// scheme. HTTP(S) refuses even bare userinfo because it is live authentication;
// SSH accepts a conventional public username such as git@host.
func EmbedsCredential(remoteURL string) bool {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return unparsedAuthorityHasCredential(remoteURL)
	}
	if u.User == nil {
		return false
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		return true
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// unparsedAuthorityHasCredential carries EmbedsCredential's rules across URLs
// net/url cannot parse. The value still reaches Git, so unreadable must not mean
// unchecked. It is confined to scheme:// authorities to avoid misreading an
// scp-like path containing colons and at signs as URL userinfo.
func unparsedAuthorityHasCredential(remoteURL string) bool {
	scheme, rest, ok := strings.Cut(remoteURL, "://")
	if !ok {
		return false
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return false
	}
	if strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https") {
		return true
	}
	return strings.Contains(rest[:at], ":")
}
