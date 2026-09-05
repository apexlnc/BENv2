package localdomain

import "fmt"

type nestedMountResult uint8

const (
	nestedMountUnknown nestedMountResult = iota
	nestedMountDenied
	nestedMountContained
	nestedMountExposed
)

type capabilityReport struct {
	UnifiedV2         bool
	NSDelegate        bool
	WritableDelegate  bool
	CgroupKill        bool
	Openat2           bool
	Statx             bool
	Clone3Placement   bool
	CgroupUnshare     bool
	UserPIDMountNS    bool
	MountCover        bool
	PidfdOpen         bool
	PidfdSignal       bool
	MigrationRejected bool
	Cleanup           bool
	NestedCgroupMount nestedMountResult
	NestedProcMount   nestedMountResult
}

func validateCapabilityReport(report capabilityReport) error {
	required := []struct {
		name string
		ok   bool
	}{
		{"unified cgroup v2", report.UnifiedV2},
		{"nsdelegate", report.NSDelegate},
		{"writable delegation", report.WritableDelegate},
		{"cgroup.kill", report.CgroupKill},
		{"openat2", report.Openat2},
		{"statx", report.Statx},
		{"clone3 atomic placement", report.Clone3Placement},
		{"post-placement cgroup unshare", report.CgroupUnshare},
		{"user/PID/mount namespaces", report.UserPIDMountNS},
		{"locked-mount cover", report.MountCover},
		{"pidfd_open", report.PidfdOpen},
		{"pidfd_send_signal", report.PidfdSignal},
		{"parent/sibling migration rejection", report.MigrationRejected},
		{"bounded cleanup", report.Cleanup},
	}
	for _, capability := range required {
		if !capability.ok {
			return fmt.Errorf("%w: %s", ErrUnavailable, capability.name)
		}
	}
	for name, result := range map[string]nestedMountResult{
		"provider-created cgroup2 mount": report.NestedCgroupMount,
		"provider-created proc mount":    report.NestedProcMount,
	} {
		if result != nestedMountDenied && result != nestedMountContained {
			return fmt.Errorf("%w: %s is not denied or contained", ErrUnavailable, name)
		}
	}
	return nil
}
