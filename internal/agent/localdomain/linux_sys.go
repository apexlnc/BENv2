//go:build linux

package localdomain

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	openResolve = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS |
		unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV
	maxControlFile = 1024 * 1024
)

func openAt(dirfd int, name string, flags uint64) (int, error) {
	how := &unix.OpenHow{Flags: flags | unix.O_CLOEXEC, Resolve: openResolve}
	fd, err := unix.Openat2(dirfd, name, how)
	if err != nil {
		return -1, err
	}
	return fd, nil
}

func openAtDir(dirfd int, name string, readable bool) (*os.File, error) {
	flags := uint64(unix.O_PATH | unix.O_DIRECTORY)
	if readable {
		flags = unix.O_RDONLY | unix.O_DIRECTORY
	}
	fd, err := openAt(dirfd, name, flags)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if err := requireFilesystem(fd, unix.CGROUP2_SUPER_MAGIC); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func readAt(dirfd int, name string) ([]byte, error) {
	fd, err := openAt(dirfd, name, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxControlFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxControlFile {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, maxControlFile)
	}
	return data, nil
}

func writeAt(dirfd int, name, value string) error {
	fd, err := openAt(dirfd, name, unix.O_WRONLY)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	n, err := io.WriteString(file, value)
	if err != nil {
		return err
	}
	if n != len(value) {
		return io.ErrShortWrite
	}
	return nil
}

func objectIDFD(fd int) (ObjectID, error) {
	id, mode, err := objectAndModeFD(fd)
	if err != nil {
		return ObjectID{}, err
	}
	if mode&unix.S_IFMT != unix.S_IFDIR {
		return ObjectID{}, fmt.Errorf("object is not an identified directory")
	}
	return id, nil
}

func objectAnyIDFD(fd int) (ObjectID, error) {
	id, _, err := objectAndModeFD(fd)
	return id, err
}

func objectAndModeFD(fd int) (ObjectID, uint16, error) {
	var stat unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW,
		unix.STATX_INO|unix.STATX_TYPE, &stat); err != nil {
		return ObjectID{}, 0, err
	}
	if stat.Ino == 0 {
		return ObjectID{}, 0, fmt.Errorf("object has zero inode")
	}
	return ObjectID{DevMajor: stat.Dev_major, DevMinor: stat.Dev_minor, Inode: stat.Ino}, stat.Mode, nil
}

func objectIDAt(dirfd int, name string, flags int) (ObjectID, error) {
	var stat unix.Statx_t
	if err := unix.Statx(dirfd, name, flags, unix.STATX_INO|unix.STATX_TYPE, &stat); err != nil {
		return ObjectID{}, err
	}
	if stat.Ino == 0 {
		return ObjectID{}, fmt.Errorf("object has zero inode")
	}
	return ObjectID{DevMajor: stat.Dev_major, DevMinor: stat.Dev_minor, Inode: stat.Ino}, nil
}

func mountIDAt(dirfd int, name string, flags int) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(dirfd, name, flags, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 {
		return 0, fmt.Errorf("statx returned no mount id")
	}
	return stat.Mnt_id, nil
}

func requireFilesystem(fd int, magic int64) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return err
	}
	if int64(stat.Type) != magic {
		return fmt.Errorf("filesystem magic %#x, want %#x", stat.Type, magic)
	}
	return nil
}

func readBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	boot := strings.TrimSuffix(string(data), "\n")
	if !validBootID(boot) {
		return "", fmt.Errorf("noncanonical boot id %q", boot)
	}
	return boot, nil
}

func readPopulation(dirfd int) (bool, error) {
	data, err := readAt(dirfd, "cgroup.events")
	if err != nil {
		return false, err
	}
	found := false
	populated := false
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return false, fmt.Errorf("malformed cgroup.events line %q", line)
		}
		if fields[0] != "populated" {
			continue
		}
		if found || fields[1] != "0" && fields[1] != "1" {
			return false, fmt.Errorf("malformed populated value")
		}
		found = true
		populated = fields[1] == "1"
	}
	if !found {
		return false, fmt.Errorf("cgroup.events has no populated field")
	}
	return populated, nil
}

func pidfdExited(fd int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
		return true, nil
	}
	if fds[0].Revents != 0 {
		return false, fmt.Errorf("pidfd poll revents %#x", fds[0].Revents)
	}
	return false, nil
}

func readProcStat(procPIDFD int) (byte, uint64, error) {
	data, err := readAt(procPIDFD, "stat")
	if err != nil {
		return 0, 0, err
	}
	return parseProcStat(string(data))
}

func procNamespaceID(procPIDFD int, name string) (ObjectID, error) {
	return objectIDAt(procPIDFD, "ns/"+name, 0)
}

func openProcPID(procFD int, pid uint32) (*os.File, error) {
	fd, err := openAt(procFD, strconv.FormatUint(uint64(pid), 10), unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "proc-pid"), nil
}

func closeFD(fd *int) {
	if *fd >= 0 {
		_ = unix.Close(*fd)
		*fd = -1
	}
}

func isNotExist(err error) bool { return errors.Is(err, unix.ENOENT) }
