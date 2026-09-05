package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// OctoSTSKindName is the `kind` this file answers to.
const OctoSTSKindName = "octo_sts"

// OctoMinFreshTTL is the lifetime this kind stands behind for a freshly minted
// credential, and the operand every TTL gate is computed against.
//
// Fifty minutes, not sixty. GitHub documents a one-hour expiry for the
// credential an exchange produces, which is not a guarantee of one hour
// *remaining* once the exchange has happened — the clock started at the issuer.
// The ten minutes are the difference between a documented figure and a contract,
// and they are what makes the resulting attempt maximum forty-five minutes
// (50m − CredentialTTLMargin).
const OctoMinFreshTTL = 50 * time.Minute

// octoDeadlineHandoff is the small difference between the concrete deadline
// on a returned token and the minimum this kind advertises. Without it, a
// consumer checking the exact 50-minute floor after FetchFresh returns observes
// 49:59:59.999... and rejects the documented 45-minute attempt maximum. One
// second changes no load-time limit and leaves essentially the full ten-minute
// cushion beneath the issuer's documented one-hour expiry.
const octoDeadlineHandoff = time.Second

// octoExchangePath is the STS exchange endpoint, appended to the configured URL.
const octoExchangePath = "/sts/exchange"

// The only transport an issuer URL may name, and its default port — dropped so
// two spellings of one endpoint reduce to one identity.
const (
	httpsScheme      = "https"
	httpsDefaultPort = "443"
)

// octoHTTPTimeout bounds one exchange. An issuer that hangs would otherwise
// hold a poll tick, a workspace fetch or an agent launch open indefinitely.
const octoHTTPTimeout = 30 * time.Second

// OctoSTSKind exchanges a workload identity for a short-lived GitHub credential
// (SPEC §10.2).
//
// Its exchange scope belongs to the **source definition**: every exchange sends
// the validated configured `scope` and `identity`, no consumer supplies either,
// and nothing derives a scope from a clone URL, an API URL or a
// `core.Repository`. Scope participates in both Authority and BindingKey because
// it selects the trust-policy namespace; `oidc_token_path` participates only in
// BindingKey, because one projected service-account token federating two
// trust-policy identities is the intended deployment.
type OctoSTSKind struct{}

var _ core.SourceKind = OctoSTSKind{}

// octoFields is the schema, in the order refusals list it.
var octoFields = []string{"url", "scope", "identity", "oidc_token_path"}

// Describe validates, canonicalizes and reduces a block to its descriptor.
//
// PURE: it parses a URL and trims three strings. It reads no file — an
// `oidc_token_path` naming nothing at all still describes — and contacts no
// issuer, which is what lets a workload-identity configuration load-validate in
// CI (SPEC §5.7, amendment 4).
//
// **This is a new rule, not an existing one reused.** `tracker.provider.api_url`
// refuses non-endpoint *components* at validation and canonicalizes separately,
// for a different purpose, in `requestControlKey` — and it strips no trailing
// slash and constrains no scheme. The two are deliberately not the same
// mechanism, so this schema states its own rule rather than citing one that does
// something else.
func (OctoSTSKind) Describe(block map[string]any) (core.SourceDescriptor, error) {
	s, err := parseOcto(block)
	if err != nil {
		return core.SourceDescriptor{}, err
	}
	return s.descriptor, nil
}

// New builds the runtime instance. The descriptor argument is deliberately
// unused: New must build only what Describe would accept, so it parses through
// the same path rather than trusting a value a caller assembled — the same rule
// TrackerKind.New and RunnerKind.New follow (SPEC §5.7).
func (OctoSTSKind) New(_ core.SourceDescriptor, block map[string]any) (core.CredentialSource, error) {
	s, err := parseOcto(block)
	if err != nil {
		return nil, err
	}
	s.client = &http.Client{
		Timeout: octoHTTPTimeout,
		// The configured URL is the one HTTPS endpoint Describe approved. The
		// default client follows redirects and preserves Authorization across a
		// same-host scheme downgrade, which would put the projected token on a
		// plaintext request. Surface the 3xx to the existing status classifier
		// instead; no Location is another spelling of this source's authority.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	s.now = time.Now
	return s, nil
}

// parseOcto is the schema, applied once for both entry points.
func parseOcto(block map[string]any) (*octoSource, error) {
	if err := checkKeys(block, OctoSTSKindName, octoFields...); err != nil {
		return nil, err
	}
	values := make(map[string]string, len(octoFields))
	for _, f := range octoFields {
		v, err := requireLiteral(block, OctoSTSKindName, f)
		if err != nil {
			return nil, err
		}
		values[f] = v
	}
	canonical, err := canonicalIssuerURL(values["url"])
	if err != nil {
		return nil, err
	}
	// Escape both the separator and the escape character before composing the
	// tuple. The fields are otherwise preserved as the schema promises, while
	// distinct definitions such as ("org#ben", "tracker") and
	// ("org", "ben#tracker") cannot collapse to one credential identity.
	authority := strings.Join([]string{
		"octo:" + escapeDescriptorField(canonical),
		escapeDescriptorField(values["scope"]),
		escapeDescriptorField(values["identity"]),
	}, "#")
	return &octoSource{
		descriptor: core.SourceDescriptor{
			Kind:         OctoSTSKindName,
			Authority:    authority,
			PrincipalKey: authority,
			// The complete definition: authority plus the one field authority
			// deliberately ignores. Narrower and an `oidc_token_path` edit is
			// missed; wider and a rename rebuilds.
			BindingKey:  authority + "#" + escapeDescriptorField(values["oidc_token_path"]),
			MinFreshTTL: OctoMinFreshTTL,
		},
		exchangeURL: canonical + octoExchangePath,
		scope:       values["scope"],
		identity:    values["identity"],
		oidcPath:    values["oidc_token_path"],
	}, nil
}

// escapeDescriptorField makes the `#`-separated descriptor tuple injective
// without changing the readable spelling of ordinary URLs, scopes, identities
// or paths. Backslash is escaped first in the conceptual encoding so `\#` in a
// literal cannot collide with the escape sequence for `#`.
func escapeDescriptorField(value string) string {
	return strings.NewReplacer(`\`, `\\`, "#", `\#`).Replace(value)
}

// canonicalIssuerURL reduces a written URL to the endpoint it addresses, so two
// spellings of one issuer produce one identity.
//
// The scheme MUST be https: the exchange this URL addresses carries the
// projected OIDC token as a bearer credential, so a plaintext issuer is refused
// here rather than warned about (ErrSourceURLScheme). It is a *load-time*
// refusal in a pure Describe — a scheme check reads nothing and reaches nothing,
// which is exactly the shape this path admits (SPEC §5.7, amendment 4).
//
// Host is lowercased, the default port is dropped, a trailing slash is stripped
// and the path is otherwise preserved — a path-mounted issuer is a real
// deployment. Userinfo, a query and a fragment are refused rather than stripped,
// so an operator who wrote one learns it meant nothing; userinfo is additionally
// the one component that could carry a credential into a field printed in full.
func canonicalIssuerURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse's own text embeds the URL, so it is deliberately not wrapped.
		return "", fmt.Errorf("%w: it could not be parsed", ErrSourceURL)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case httpsScheme:
	case "http":
		// The scheme is named but the URL is not echoed: the refusal is pasted
		// into CI logs, and this is the one field that can carry userinfo.
		return "", fmt.Errorf("%w: http would send the projected OIDC bearer token in the clear",
			ErrSourceURLScheme)
	default:
		return "", fmt.Errorf("%w: %q is not https", ErrSourceURLScheme, u.Scheme)
	}
	switch {
	case u.User != nil:
		return "", fmt.Errorf("%w: it carries userinfo", ErrSourceURL)
	case u.RawQuery != "" || u.ForceQuery:
		return "", fmt.Errorf("%w: it carries a query", ErrSourceURL)
	case u.Fragment != "" || u.RawFragment != "":
		return "", fmt.Errorf("%w: it carries a fragment", ErrSourceURL)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("%w: it names no host", ErrSourceURL)
	}
	port := u.Port()
	if port == httpsDefaultPort {
		port = ""
	}
	authority := host
	switch {
	case port != "":
		authority = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		// An IPv6 literal, unbracketed by Hostname().
		authority = "[" + host + "]"
	}
	return scheme + "://" + authority + strings.TrimSuffix(u.EscapedPath(), "/"), nil
}

// octoSource performs the exchange.
//
// The OIDC token is read from disk on **every** exchange rather than at
// construction: the projection is rotated under a running process, and a path
// read once would pin the token that happened to be there at startup.
type octoSource struct {
	descriptor  core.SourceDescriptor
	exchangeURL string
	scope       string
	identity    string
	oidcPath    string
	client      *http.Client
	now         func() time.Time

	mu       sync.Mutex
	cache    map[core.Purpose]core.Token
	inflight map[core.Purpose]*octoFetch
}

// octoFetch is one shared cache fill. Waiters receive the leader's exact
// answer, while FetchFresh remains outside this mechanism and always exchanges.
type octoFetch struct {
	done  chan struct{}
	token core.Token
	err   error
}

func (s *octoSource) Descriptor() core.SourceDescriptor { return s.descriptor }

// Fetch serves from this source's own cache while the cached token still has
// more than the fixed margin of life left, and exchanges otherwise.
//
// Partitioned by purpose, which is the whole of what a purpose does here: it
// never selects an identity, so two purposes served by one instance receive
// credentials of the same authority and differ only in when they were minted.
func (s *octoSource) Fetch(ctx context.Context, p core.Purpose) (core.Token, error) {
	s.mu.Lock()
	tok, ok := s.cache[p]
	if ok && s.now().Before(tok.UsableUntil.Add(-core.CredentialTTLMargin)) {
		s.mu.Unlock()
		return tok, nil
	}
	if fetch := s.inflight[p]; fetch != nil {
		s.mu.Unlock()
		select {
		case <-fetch.done:
			return fetch.token, fetch.err
		case <-ctx.Done():
			return core.Token{}, transient(s.descriptor.Authority, ctx.Err())
		}
	}
	if s.inflight == nil {
		s.inflight = map[core.Purpose]*octoFetch{}
	}
	fetch := &octoFetch{done: make(chan struct{})}
	s.inflight[p] = fetch
	s.mu.Unlock()

	tok, err := s.FetchFresh(ctx, p)
	s.mu.Lock()
	if err == nil {
		if s.cache == nil {
			s.cache = map[core.Purpose]core.Token{}
		}
		s.cache[p] = tok
	}
	fetch.token, fetch.err = tok, err
	delete(s.inflight, p)
	close(fetch.done)
	s.mu.Unlock()
	return tok, err
}

// FetchFresh exchanges every time, and never consults or is served by the cache.
func (s *octoSource) FetchFresh(ctx context.Context, _ core.Purpose) (core.Token, error) {
	oidc, err := os.ReadFile(s.oidcPath)
	if err != nil {
		// Permanent. An unreadable projection is a deployment that did not mount
		// what it said it would; retrying spends the budget to read the same
		// absent file. The path is named because it is non-secret and is what an
		// operator has to fix; the token it would have held never existed here.
		return core.Token{}, permanent(s.descriptor.Authority,
			fmt.Errorf("reading the OIDC token at %s: %w", s.oidcPath, err))
	}
	if strings.TrimSpace(string(oidc)) == "" {
		return core.Token{}, permanent(s.descriptor.Authority,
			fmt.Errorf("the OIDC token at %s is empty", s.oidcPath))
	}

	// The scope and identity on the wire are the configured literals and nothing
	// else. No consumer supplies either, and nothing derives one from a
	// repository: the token's one-repository ceiling is the trust policy's
	// `repositories:` list, which is the issuer's to enforce (SPEC §11).
	q := url.Values{"scope": {s.scope}, "identity": {s.identity}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.exchangeURL+"?"+q.Encode(), nil)
	if err != nil {
		return core.Token{}, permanent(s.descriptor.Authority, fmt.Errorf("building the exchange request: %w", err))
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(oidc)))
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Our own deadline or a shutdown, not the issuer's verdict.
			return core.Token{}, transient(s.descriptor.Authority, ctxErr)
		}
		return core.Token{}, transient(s.descriptor.Authority, fmt.Errorf("reaching the issuer: %w", err))
	}
	defer resp.Body.Close()
	// The status is the issuer's verdict and does not depend on an error body's
	// shape or completeness. Classify it before reading that body, or a
	// truncated 401 becomes transient transport weather instead of a permanent
	// trust-policy refusal.
	if err := classifyExchangeStatus(s.descriptor.Authority, resp.StatusCode); err != nil {
		return core.Token{}, err
	}
	// Bounded: the body is an issuer-controlled stream, and the answer is one
	// small JSON object.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return core.Token{}, transient(s.descriptor.Authority, fmt.Errorf("reading the exchange response: %w", err))
	}

	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// A 200 whose body is not the documented shape is the issuer breaking its
		// contract, not weather. The body is never echoed: it is the response
		// that was supposed to carry a credential.
		return core.Token{}, permanent(s.descriptor.Authority,
			errors.New("the exchange response was not the documented JSON object"))
	}
	token := strings.TrimSpace(payload.Token)
	if token == "" {
		token = strings.TrimSpace(payload.AccessToken)
	}
	if token == "" {
		return core.Token{}, permanent(s.descriptor.Authority, core.ErrCredentialEmpty)
	}
	// A contract, not an observed expiry: the exchange returns none. The small
	// handoff allowance lets a caller observe the declared floor after this
	// function returns; the descriptor remains the conservative load-time bound.
	return core.Token{Value: token, UsableUntil: s.now().Add(OctoMinFreshTTL + octoDeadlineHandoff)}, nil
}

// classifyExchangeStatus maps an issuer's status onto the class that routes.
//
// The two directions do not cost the same, which is why the default is not
// transient: a permanent error classified transient burns the retry budget and
// then parks anyway, while a transient error classified permanent parks a run
// that would have succeeded on the next tick. Statuses that are neither
// obviously configuration nor obviously weather are left **unknown**, which
// parks — the same posture as the zero value, and for the same reason.
func classifyExchangeStatus(authority string, status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return permanent(authority, fmt.Errorf(
			"the issuer refused the exchange (%d): the trust policy does not admit this identity", status))
	case status == http.StatusNotFound:
		return permanent(authority, fmt.Errorf(
			"the issuer does not know this scope and identity (%d)", status))
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return transient(authority, fmt.Errorf("the issuer answered %d", status))
	default:
		return &core.CredentialError{
			Class:     core.CredentialUnknown,
			Authority: authority,
			Err:       fmt.Errorf("the issuer answered %d", status),
		}
	}
}
