//go:build linux

package harness

import (
	"os"
	"strings"
)

// bootIdentity returns this boot's identity from the kernel's own random
// boot_id, which is regenerated every boot and needs no privileges.
//
// An unreadable boot_id yields "", which callers must treat as *no* identity
// rather than as a matching one — see bootID.
func bootIdentity() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
