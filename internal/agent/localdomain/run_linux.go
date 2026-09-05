//go:build linux

package localdomain

// Run separates direct-provider completion from execution-domain quiet.
type Run struct {
	Evidence     Evidence
	Handle       *Handle
	ProviderDone <-chan ProviderExit
	termAck      <-chan struct{}
	started      <-chan struct{}
	mounts       mountSetupReport
}
