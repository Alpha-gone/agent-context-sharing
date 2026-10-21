package diagnostic

import (
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

func checkFilesystem(fd int) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return fileFailure("filesystem_unavailable", err)
	}
	return checkFilesystemStat(stat)
}

func checkFilesystemStat(stat unix.Statfs_t) error {
	if stat.Flags&unix.MNT_RDONLY != 0 {
		return fileFailure("storage_read_only", nil)
	}
	name := strings.TrimRight(string(stat.Fstypename[:]), "\x00")
	if slices.Contains([]string{"tmpfs", "mfs", "devfs", "fdesc", "procfs"}, name) {
		return fileFailure("volatile_storage", nil)
	}
	return nil
}
