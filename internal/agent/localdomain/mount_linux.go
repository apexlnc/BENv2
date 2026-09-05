//go:build linux

package localdomain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const canonicalCgroup = "/sys/fs/cgroup"

type mountAliasReport struct {
	ID          uint64 `json:"id"`
	Target      string `json:"target"`
	Filesystem  string `json:"filesystem"`
	Root        string `json:"root"`
	Magic       int64  `json:"magic"`
	Directory   bool   `json:"directory"`
	CoveredBy   uint64 `json:"covered_by"`
	Propagating bool   `json:"propagating"`
}

type mountSetupReport struct {
	Aliases       []mountAliasReport `json:"aliases"`
	ProcMountID   uint64             `json:"proc_mount_id"`
	CgroupMountID uint64             `json:"cgroup_mount_id"`
}

type coverTarget struct {
	target    string
	directory bool
	mountID   uint64
}

func setupMountNamespace() (report mountSetupReport, retErr error) {
	records, err := currentMountInfo()
	if err != nil {
		return report, err
	}
	for _, record := range records {
		if record.Filesystem != "proc" && record.Filesystem != "cgroup" && record.Filesystem != "cgroup2" {
			continue
		}
		info, err := os.Lstat(record.Target)
		if err != nil {
			return report, fmt.Errorf("resolve inherited %s mount %q: %w", record.Filesystem, record.Target, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return report, fmt.Errorf("inherited mount target %q has uncoverable type %v", record.Target, info.Mode())
		}
		magic, err := filesystemMagicAt(record.Target)
		if err != nil {
			return report, fmt.Errorf("identify inherited %s mount %q: %w", record.Filesystem, record.Target, err)
		}
		wantMagic, ok := filesystemMagic(record.Filesystem)
		if !ok || magic != wantMagic {
			return report, fmt.Errorf("inherited %s mount %q has magic %#x, want %#x", record.Filesystem, record.Target, magic, wantMagic)
		}
		report.Aliases = append(report.Aliases, mountAliasReport{
			ID: record.ID, Target: record.Target, Filesystem: record.Filesystem, Root: record.Root,
			Magic: magic, Directory: info.IsDir(), Propagating: hasMountPropagation(record),
		})
	}
	if len(report.Aliases) == 0 {
		return report, fmt.Errorf("no inherited proc/cgroup mounts found")
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return report, fmt.Errorf("make mount propagation private: %w", err)
	}
	private, err := currentMountInfo()
	if err != nil {
		return report, err
	}
	for _, record := range private {
		if hasMountPropagation(record) {
			return report, fmt.Errorf("mount %q retains propagation linkage", record.Target)
		}
	}

	targets, err := minimalCoverTargets(report.Aliases)
	if err != nil {
		return report, err
	}
	scratch, err := os.MkdirTemp("", ".ben-localdomain-cover-")
	if err != nil {
		return report, err
	}
	defer func() {
		_ = os.Remove(filepath.Join(scratch, "empty-file"))
		_ = os.Remove(filepath.Join(scratch, "empty-dir"))
		_ = os.Remove(filepath.Join(scratch, "cgroup2"))
		_ = os.Remove(scratch)
	}()
	if err := unix.Mount("tmpfs", scratch, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0700,size=4096"); err != nil {
		return report, fmt.Errorf("mount cover scratch: %w", err)
	}
	defer func() {
		if err := unix.Unmount(scratch, 0); retErr == nil && err != nil {
			retErr = fmt.Errorf("unmount cover scratch: %w", err)
		}
	}()
	emptyDir := filepath.Join(scratch, "empty-dir")
	emptyFile := filepath.Join(scratch, "empty-file")
	if err := os.Mkdir(emptyDir, 0o500); err != nil {
		return report, err
	}
	file, err := os.OpenFile(emptyFile, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o400)
	if err != nil {
		return report, err
	}
	file.Close()
	for i := range targets {
		source := emptyFile
		if targets[i].directory {
			source = emptyDir
		}
		if err := unix.Mount(source, targets[i].target, "", unix.MS_BIND, ""); err != nil {
			return report, fmt.Errorf("cover %q: %w", targets[i].target, err)
		}
		flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
		if err := unix.Mount("", targets[i].target, "", flags, ""); err != nil {
			return report, fmt.Errorf("make cover %q read-only: %w", targets[i].target, err)
		}
		targets[i].mountID, err = mountIDAt(unix.AT_FDCWD, targets[i].target, 0)
		if err != nil {
			return report, fmt.Errorf("identify cover %q: %w", targets[i].target, err)
		}
	}
	if err := mountCanonical("proc", "/proc", "proc"); err != nil {
		return report, err
	}
	if err := mountCanonicalStaged("none", filepath.Join(scratch, "cgroup2"), canonicalCgroup, "cgroup2"); err != nil {
		return report, err
	}
	report.ProcMountID, err = mountIDAt(unix.AT_FDCWD, "/proc", 0)
	if err != nil {
		return report, err
	}
	report.CgroupMountID, err = mountIDAt(unix.AT_FDCWD, canonicalCgroup, 0)
	if err != nil {
		return report, err
	}
	for i := range report.Aliases {
		report.Aliases[i].CoveredBy = expectedCoverID(report.Aliases[i].Target, targets, report)
	}
	if err := verifyMountCover(report); err != nil {
		return report, err
	}
	return report, nil
}

func filesystemMagicAt(name string) (int64, error) {
	fd, err := unix.Open(name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Type), nil
}

func filesystemMagic(filesystem string) (int64, bool) {
	switch filesystem {
	case "proc":
		return unix.PROC_SUPER_MAGIC, true
	case "cgroup":
		return unix.CGROUP_SUPER_MAGIC, true
	case "cgroup2":
		return unix.CGROUP2_SUPER_MAGIC, true
	default:
		return 0, false
	}
}

func mountCanonical(source, target, filesystem string) error {
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("canonical %s target %q is not a directory", filesystem, target)
	}
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	// nsdelegate is a system-wide cgroup-v2 option which setup already
	// validated. Linux ignores attempts to set it from this non-initial cgroup
	// namespace, so the leaf-rooted mount asks for no superblock mutation.
	if err := unix.Mount(source, target, filesystem, flags, ""); err != nil {
		return fmt.Errorf("mount private %s at %q: %w", filesystem, target, err)
	}
	return nil
}

func mountCanonicalStaged(source, staging, target, filesystem string) (retErr error) {
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("canonical %s target %q is not a directory", filesystem, target)
	}
	if err := os.Mkdir(staging, 0o500); err != nil {
		return fmt.Errorf("create private %s staging mount: %w", filesystem, err)
	}
	flags := uintptr(unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	// The inherited cgroup mount is locked to this less-privileged mount
	// namespace. Create the new cgroup-namespace view on supervisor-owned
	// scratch, then stack a bind of that view at the canonical path.
	if err := unix.Mount(source, staging, filesystem, flags, ""); err != nil {
		return fmt.Errorf("mount private %s staging view: %w", filesystem, err)
	}
	defer func() {
		if err := unix.Unmount(staging, 0); retErr == nil && err != nil {
			retErr = fmt.Errorf("unmount private %s staging view: %w", filesystem, err)
		}
	}()
	if err := unix.Mount(staging, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("stack private %s at %q: %w", filesystem, target, err)
	}
	if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|flags, ""); err != nil {
		return fmt.Errorf("make private %s bind read-only: %w", filesystem, err)
	}
	return nil
}

func minimalCoverTargets(aliases []mountAliasReport) ([]coverTarget, error) {
	var candidates []coverTarget
	for _, alias := range aliases {
		if beneathPath("/proc", alias.Target) || beneathPath(canonicalCgroup, alias.Target) {
			continue
		}
		if alias.Target == "/" {
			return nil, fmt.Errorf("sensitive filesystem mounted at root cannot be covered")
		}
		candidates = append(candidates, coverTarget{target: alias.Target, directory: alias.Directory})
	}
	sort.Slice(candidates, func(i, j int) bool {
		leftDepth := strings.Count(candidates[i].target, "/")
		rightDepth := strings.Count(candidates[j].target, "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return candidates[i].target < candidates[j].target
	})
	var result []coverTarget
	for _, candidate := range candidates {
		covered := false
		for _, existing := range result {
			if existing.directory && beneathPath(existing.target, candidate.target) {
				covered = true
				break
			}
			if existing.target == candidate.target {
				if existing.directory != candidate.directory {
					return nil, fmt.Errorf("mount target %q changes file type", candidate.target)
				}
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, candidate)
		}
	}
	return result, nil
}

func expectedCoverID(target string, covers []coverTarget, report mountSetupReport) uint64 {
	if beneathPath("/proc", target) {
		return report.ProcMountID
	}
	if beneathPath(canonicalCgroup, target) {
		return report.CgroupMountID
	}
	for _, cover := range covers {
		if cover.target == target || cover.directory && beneathPath(cover.target, target) {
			return cover.mountID
		}
	}
	return 0
}

func verifyMountCover(report mountSetupReport) error {
	for _, alias := range report.Aliases {
		id, err := mountIDAt(unix.AT_FDCWD, alias.Target, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) && alias.CoveredBy != 0 {
				continue
			}
			return fmt.Errorf("resolve covered alias %q: %w", alias.Target, err)
		}
		if id == alias.ID {
			return fmt.Errorf("inherited %s mount %d remains reachable at %q", alias.Filesystem, alias.ID, alias.Target)
		}
		if alias.CoveredBy == 0 || id != alias.CoveredBy {
			return fmt.Errorf("alias %q resolves to mount %d, want cover %d", alias.Target, id, alias.CoveredBy)
		}
	}
	procFD, err := unix.Open("/proc", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	if err := requireFilesystem(procFD, unix.PROC_SUPER_MAGIC); err != nil {
		unix.Close(procFD)
		return err
	}
	unix.Close(procFD)
	cgroupFD, err := unix.Open(canonicalCgroup, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	if err := requireFilesystem(cgroupFD, unix.CGROUP2_SUPER_MAGIC); err != nil {
		unix.Close(cgroupFD)
		return err
	}
	unified, err := readAt(cgroupFD, "cgroup.procs")
	_ = unified
	unix.Close(cgroupFD)
	if err != nil && !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EROFS) {
		return fmt.Errorf("read private cgroup2 view: %w", err)
	}
	cgroupData, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	if path, err := parseUnifiedCgroup(string(cgroupData)); err != nil || path != "/" {
		return fmt.Errorf("private cgroup namespace root = %q: %v", path, err)
	}
	records, err := currentMountInfo()
	if err != nil {
		return err
	}
	byID := make(map[uint64]mountRecord, len(records))
	for _, record := range records {
		byID[record.ID] = record
		if hasMountPropagation(record) {
			return fmt.Errorf("mount %q regained propagation linkage", record.Target)
		}
		if record.Filesystem != "proc" && record.Filesystem != "cgroup" && record.Filesystem != "cgroup2" {
			continue
		}
		resolved, resolveErr := mountIDAt(unix.AT_FDCWD, record.Target, 0)
		if errors.Is(resolveErr, unix.ENOENT) {
			continue
		}
		if resolveErr != nil {
			return fmt.Errorf("re-resolve sensitive mount %q: %w", record.Target, resolveErr)
		}
		if resolved == record.ID && !((record.Target == "/proc" && record.ID == report.ProcMountID) ||
			(record.Target == canonicalCgroup && record.ID == report.CgroupMountID)) {
			return fmt.Errorf("reachable noncanonical %s mount %d remains at %q", record.Filesystem, record.ID, record.Target)
		}
	}
	coverIDs := map[uint64]bool{report.ProcMountID: true, report.CgroupMountID: true}
	for _, alias := range report.Aliases {
		coverIDs[alias.CoveredBy] = true
	}
	for id := range coverIDs {
		record, found := byID[id]
		if !found {
			return fmt.Errorf("cover mount %d is absent from mountinfo", id)
		}
		for _, option := range []string{"nosuid", "nodev", "noexec"} {
			if !hasMountOption(record, option) {
				return fmt.Errorf("cover mount %d at %q lacks %s", id, record.Target, option)
			}
		}
		access := "ro"
		if id == report.ProcMountID {
			// A writable private procfs is required for an unprivileged
			// provider to create a nested user namespace and install its
			// own UID/GID maps. It exposes only the attempt PID namespace.
			access = "rw"
		}
		if !hasMountOption(record, access) {
			return fmt.Errorf("cover mount %d at %q lacks %s", id, record.Target, access)
		}
	}
	for _, want := range []struct {
		target     string
		filesystem string
		id         uint64
	}{{"/proc", "proc", report.ProcMountID}, {canonicalCgroup, "cgroup2", report.CgroupMountID}} {
		matched := false
		for _, record := range records {
			if record.ID == want.id && record.Target == want.target && record.Root == "/" && record.Filesystem == want.filesystem {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("canonical %s mount %d at %s not found", want.filesystem, want.id, want.target)
		}
	}
	if !hasMountOption(byID[report.CgroupMountID], "nsdelegate") {
		return fmt.Errorf("private cgroup2 mount lacks nsdelegate")
	}
	return nil
}

func currentMountInfo() ([]mountRecord, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return parseMountInfo(file)
}

func beneathPath(root, path string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}
