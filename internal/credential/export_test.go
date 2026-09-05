package credential

import (
	"net/http"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// OctoKindTrustingForTest is OctoSTSKind with one test issuer's TLS trust.
//
// `octo_sts` refuses a plaintext issuer (#245), so every test that drives a real
// exchange needs an httptest **TLS** server — and an httptest server's
// certificate is trusted by nothing but the client httptest hands back. This is
// the single seam that supplies it, shared by this package's own tests and by
// the credtest case in credential_test.
//
// Describe and New are the registered kind's own, and only the transport is
// replaced: the schema, the client's timeout, the exchange and every path under
// test stay the kind's. It lives in a _test.go file, so it is not part of the
// package's API and nothing outside a test can reach it.
func OctoKindTrustingForTest(transport http.RoundTripper) core.SourceKind {
	return octoTestKind{transport: transport}
}

type octoTestKind struct{ transport http.RoundTripper }

var _ core.SourceKind = octoTestKind{}

func (octoTestKind) Describe(block map[string]any) (core.SourceDescriptor, error) {
	return OctoSTSKind{}.Describe(block)
}

func (k octoTestKind) New(descriptor core.SourceDescriptor, block map[string]any) (core.CredentialSource, error) {
	src, err := OctoSTSKind{}.New(descriptor, block)
	if err != nil {
		return nil, err
	}
	src.(*octoSource).client.Transport = k.transport
	return src, nil
}
