//go:build darwin

package harness

import "golang.org/x/sys/unix"

// bootIdentity returns this boot's identity from kern.bootsessionuuid, a UUID
// the kernel generates once per boot.
//
// Deliberately *not* kern.boottime. XNU rewrites boottime whenever calendar time
// is reset (osfmk/kern/clock.c), so a clock step — an NTP correction, a manual
// set, a VM resuming from suspend — changes it mid-boot. A marker written before
// the step would then disagree with the identity read after it, and a
// disagreement is read as proof that the marker belongs to an earlier boot: the
// one verdict §9.10 lets free a workspace. That turns a live run into a
// reattachable one, which is the failure the precondition exists to prevent.
//
// An unreadable value yields "", which callers must treat as *no* identity
// rather than as a matching one — see bootID.
func bootIdentity() string {
	uuid, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return ""
	}
	return uuid
}
