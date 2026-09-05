package credential

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// ProjectedOIDCKindName is the direct projected-workload-token source.
const ProjectedOIDCKindName = "projected_oidc"

const projectedOIDCMaxTokenBytes = 1 << 20

var (
	// ErrProjectedOIDCIssuer refuses an issuer that is not an exact HTTPS OIDC
	// identity. The source does not contact it; Airlock does discovery and
	// signature verification, but the issuer is still part of the principal
	// definition BEN persists before a keyed request.
	ErrProjectedOIDCIssuer = errors.New("credential source: projected OIDC issuer is invalid")
	// ErrProjectedOIDCClaim refuses a tenant claim name that cannot identify one
	// scalar top-level claim.
	ErrProjectedOIDCClaim = errors.New("credential source: projected OIDC tenant claim is invalid")
	// ErrProjectedOIDCPath refuses a path whose spelling is not one absolute,
	// clean filesystem address.
	ErrProjectedOIDCPath = errors.New("credential source: projected OIDC token path is invalid")
	// ErrProjectedOIDCTTL refuses a non-positive or overflowing minimum TTL.
	ErrProjectedOIDCTTL = errors.New("credential source: projected OIDC minimum TTL is invalid")
	// ErrProjectedOIDCToken is the runtime refusal of an unreadable, malformed,
	// oversized, expired, or identity-mismatched projection. Its wrapping
	// CredentialError supplies the retry class; the token and raw claims never
	// enter the error.
	ErrProjectedOIDCToken = errors.New("credential source: projected OIDC token is invalid")
)

var oidcClaimNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]*$`)

// ProjectedOIDCKind reads a directly presented projected OIDC bearer token.
// Its definition fixes the downstream (tenant_id, subject) principal and every
// fetch checks that the current projection still addresses exactly that tuple.
// Airlock remains the signature verifier and the sole authorization authority.
type ProjectedOIDCKind struct{}

var _ core.SourceKind = ProjectedOIDCKind{}

var projectedOIDCFields = []string{
	"issuer", "audience", "tenant_claim", "tenant_id", "subject", "token_path", "min_ttl_ms",
}

type projectedOIDCConfig struct {
	descriptor  core.SourceDescriptor
	issuer      string
	audience    string
	tenantClaim string
	tenantID    string
	subject     string
	tokenPath   string
	minTTL      time.Duration
}

// Describe validates and reduces the source without reading the projection or
// contacting the issuer. This purity keeps workflow validation credential-free.
func (ProjectedOIDCKind) Describe(block map[string]any) (core.SourceDescriptor, error) {
	cfg, err := parseProjectedOIDC(block)
	if err != nil {
		return core.SourceDescriptor{}, err
	}
	return cfg.descriptor, nil
}

// New constructs the runtime source by applying the same schema as Describe.
func (ProjectedOIDCKind) New(_ core.SourceDescriptor, block map[string]any) (core.CredentialSource, error) {
	cfg, err := parseProjectedOIDC(block)
	if err != nil {
		return nil, err
	}
	return &projectedOIDCSource{projectedOIDCConfig: *cfg, now: time.Now}, nil
}

func parseProjectedOIDC(block map[string]any) (*projectedOIDCConfig, error) {
	if err := checkKeys(block, ProjectedOIDCKindName, projectedOIDCFields...); err != nil {
		return nil, err
	}
	issuer, err := requireLiteral(block, ProjectedOIDCKindName, "issuer")
	if err != nil {
		return nil, err
	}
	if err := validateProjectedOIDCIssuer(issuer); err != nil {
		return nil, err
	}
	audience, err := requireLiteral(block, ProjectedOIDCKindName, "audience")
	if err != nil {
		return nil, err
	}
	tenantClaim, err := requireLiteral(block, ProjectedOIDCKindName, "tenant_claim")
	if err != nil {
		return nil, err
	}
	if !oidcClaimNameRe.MatchString(tenantClaim) {
		return nil, fmt.Errorf("%w: %s.tenant_claim must name one top-level scalar claim",
			ErrProjectedOIDCClaim, ProjectedOIDCKindName)
	}
	tenantID, err := requireLiteral(block, ProjectedOIDCKindName, "tenant_id")
	if err != nil {
		return nil, err
	}
	subject, err := requireLiteral(block, ProjectedOIDCKindName, "subject")
	if err != nil {
		return nil, err
	}
	tokenPath, err := requireLiteral(block, ProjectedOIDCKindName, "token_path")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(tokenPath) || filepath.Clean(tokenPath) != tokenPath {
		return nil, fmt.Errorf("%w: %s.token_path must be absolute and clean",
			ErrProjectedOIDCPath, ProjectedOIDCKindName)
	}
	minTTL, err := projectedOIDCMinTTL(block)
	if err != nil {
		return nil, err
	}

	authority := strings.Join([]string{
		"projected-oidc:" + escapeDescriptorField(issuer),
		escapeDescriptorField(audience),
		escapeDescriptorField(tenantID),
		escapeDescriptorField(subject),
	}, "#")
	binding := strings.Join([]string{
		authority,
		"tenant_claim=" + escapeDescriptorField(tenantClaim),
		"token_path=" + escapeDescriptorField(tokenPath),
		"min_ttl_ms=" + strconv.FormatInt(minTTL.Milliseconds(), 10),
	}, "#")
	principal := strings.Join([]string{
		"airlock-owner:" + escapeDescriptorField(tenantID),
		escapeDescriptorField(subject),
	}, "#")

	return &projectedOIDCConfig{
		descriptor: core.SourceDescriptor{
			Kind: ProjectedOIDCKindName, Authority: authority, BindingKey: binding,
			PrincipalKey: principal, MinFreshTTL: minTTL,
		},
		issuer: issuer, audience: audience, tenantClaim: tenantClaim,
		tenantID: tenantID, subject: subject, tokenPath: tokenPath, minTTL: minTTL,
	}, nil
}

func validateProjectedOIDCIssuer(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.Opaque != "" {
		return fmt.Errorf("%w: %s.issuer must be an HTTPS URL with no userinfo, query, or fragment",
			ErrProjectedOIDCIssuer, ProjectedOIDCKindName)
	}
	return nil
}

func projectedOIDCMinTTL(block map[string]any) (time.Duration, error) {
	raw, present := block["min_ttl_ms"]
	if !present || raw == nil {
		return 0, fmt.Errorf("%w: %s requires %q", ErrMissingSourceField,
			ProjectedOIDCKindName, "min_ttl_ms")
	}
	millis, ok := raw.(int)
	if !ok {
		return 0, fmt.Errorf("%w: %s.min_ttl_ms must be a positive integer",
			ErrProjectedOIDCTTL, ProjectedOIDCKindName)
	}
	if millis <= 0 || uint64(millis) > uint64(math.MaxInt64/int64(time.Millisecond)) {
		return 0, fmt.Errorf("%w: %s.min_ttl_ms must fit a positive duration",
			ErrProjectedOIDCTTL, ProjectedOIDCKindName)
	}
	return time.Duration(millis) * time.Millisecond, nil
}

type projectedOIDCSource struct {
	projectedOIDCConfig
	now func() time.Time
}

func (s *projectedOIDCSource) Descriptor() core.SourceDescriptor { return s.descriptor }

// Fetch deliberately shares FetchFresh's no-cache behavior. Kubelet rotates
// the projection in place, and the source must observe the current token before
// each independently authenticated request.
func (s *projectedOIDCSource) Fetch(ctx context.Context, _ core.Purpose) (core.Token, error) {
	return s.fetch(ctx)
}

func (s *projectedOIDCSource) FetchFresh(ctx context.Context, _ core.Purpose) (core.Token, error) {
	return s.fetch(ctx)
}

func (s *projectedOIDCSource) fetch(ctx context.Context) (core.Token, error) {
	if err := ctx.Err(); err != nil {
		return core.Token{}, transient(s.descriptor.Authority, err)
	}
	raw, err := readProjectedOIDCToken(s.tokenPath)
	if err != nil {
		return core.Token{}, permanent(s.descriptor.Authority, err)
	}
	claims, err := parseProjectedOIDCClaims(raw)
	if err != nil {
		return core.Token{}, permanent(s.descriptor.Authority, err)
	}
	if claims.issuer != s.issuer || claims.subject != s.subject ||
		claims.stringClaim(s.tenantClaim) != s.tenantID || !claims.hasAudience(s.audience) {
		return core.Token{}, permanent(s.descriptor.Authority,
			fmt.Errorf("%w: claims do not match the configured principal", ErrProjectedOIDCToken))
	}
	now := s.now()
	expires := time.Unix(claims.expires, 0)
	if expires.Sub(now) < s.minTTL {
		// A projected token near expiry is expected to be replaced in place. The
		// same read can therefore succeed on the next call without configuration
		// changing; unlike malformed identity, this is transient weather.
		return core.Token{}, transient(s.descriptor.Authority,
			fmt.Errorf("%w: projection has less than the configured minimum lifetime", ErrProjectedOIDCToken))
	}
	return core.Token{Value: raw, UsableUntil: now.Add(s.minTTL)}, nil
}

func readProjectedOIDCToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: reading %s: %v", ErrProjectedOIDCToken, path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, projectedOIDCMaxTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: reading %s: %v", ErrProjectedOIDCToken, path, err)
	}
	if len(b) > projectedOIDCMaxTokenBytes {
		return "", fmt.Errorf("%w: %s exceeds the bounded token size", ErrProjectedOIDCToken, path)
	}
	value := strings.TrimSpace(string(b))
	if value == "" {
		return "", fmt.Errorf("%w: %s is empty", ErrProjectedOIDCToken, path)
	}
	return value, nil
}

type projectedOIDCClaims struct {
	issuer   string
	subject  string
	audience []string
	expires  int64
	raw      map[string]json.RawMessage
}

func parseProjectedOIDCClaims(token string) (projectedOIDCClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return projectedOIDCClaims{}, fmt.Errorf("%w: bearer is not a three-segment JWT", ErrProjectedOIDCToken)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return projectedOIDCClaims{}, fmt.Errorf("%w: JWT payload is not base64url", ErrProjectedOIDCToken)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil || raw == nil {
		return projectedOIDCClaims{}, fmt.Errorf("%w: JWT payload is not a JSON object", ErrProjectedOIDCToken)
	}
	issuer, ok := projectedOIDCStringClaim(raw, "iss")
	if !ok {
		return projectedOIDCClaims{}, fmt.Errorf("%w: JWT carries no scalar issuer", ErrProjectedOIDCToken)
	}
	subject, ok := projectedOIDCStringClaim(raw, "sub")
	if !ok {
		return projectedOIDCClaims{}, fmt.Errorf("%w: JWT carries no scalar subject", ErrProjectedOIDCToken)
	}
	audience, ok := projectedOIDCAudience(raw["aud"])
	if !ok {
		return projectedOIDCClaims{}, fmt.Errorf("%w: JWT carries no string audience", ErrProjectedOIDCToken)
	}
	expires, ok := projectedOIDCExpiration(raw["exp"])
	if !ok {
		return projectedOIDCClaims{}, fmt.Errorf("%w: JWT carries no integer expiry", ErrProjectedOIDCToken)
	}
	return projectedOIDCClaims{
		issuer: issuer, subject: subject, audience: audience, expires: expires, raw: raw,
	}, nil
}

func (c projectedOIDCClaims) stringClaim(name string) string {
	value, _ := projectedOIDCStringClaim(c.raw, name)
	return value
}

func (c projectedOIDCClaims) hasAudience(want string) bool {
	for _, value := range c.audience {
		if value == want {
			return true
		}
	}
	return false
}

func projectedOIDCStringClaim(raw map[string]json.RawMessage, name string) (string, bool) {
	value, ok := raw[name]
	if !ok {
		return "", false
	}
	var out string
	if err := json.Unmarshal(value, &out); err != nil || strings.TrimSpace(out) != out || out == "" {
		return "", false
	}
	return out, true
}

func projectedOIDCAudience(raw json.RawMessage) ([]string, bool) {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil, false
		}
		return []string{one}, true
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil || len(many) == 0 {
		return nil, false
	}
	for _, value := range many {
		if value == "" {
			return nil, false
		}
	}
	return many, true
}

func projectedOIDCExpiration(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || raw[0] == '"' {
		return 0, false
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	return value, err == nil && value > 0
}
