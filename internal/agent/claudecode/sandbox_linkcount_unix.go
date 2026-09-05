//go:build darwin || linux

package claudecode

import (
	"os"
	"syscall"
)

// fileHasOneLink closes the inode alias left outside a path-based sandbox.
// srt supports this posture on darwin and linux, whose Stat_t both expose the
// kernel link count.
func fileHasOneLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
