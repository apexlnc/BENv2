//go:build !linux

package localdomain

// Run is the portable result shape. Unsupported platforms never construct one.
type Run struct {
	Evidence     Evidence
	Handle       *Handle
	ProviderDone <-chan ProviderExit
}
