//go:build !darwin && !linux

package claudecode

import "os"

// The srt posture is implemented and verified only on darwin and linux. Keep
// the package buildable on the remaining targets.
func fileHasOneLink(os.FileInfo) bool { return true }
