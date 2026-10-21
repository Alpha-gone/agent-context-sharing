package diagnostic

import "golang.org/x/sys/unix"

func checkFilesystem(fd int) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return fileFailure("filesystem_unavailable", err)
	}
	return checkFilesystemStat(stat)
}

func checkFilesystemStat(stat unix.Statfs_t) error {
	if stat.Flags&unix.ST_RDONLY != 0 {
		return fileFailure("storage_read_only", nil)
	}
	switch uint64(stat.Type) {
	case unix.TMPFS_MAGIC, unix.RAMFS_MAGIC, unix.PROC_SUPER_MAGIC, unix.SYSFS_MAGIC, unix.DEVPTS_SUPER_MAGIC, unix.CGROUP_SUPER_MAGIC, unix.CGROUP2_SUPER_MAGIC, unix.OVERLAYFS_SUPER_MAGIC:
		return fileFailure("volatile_storage", nil)
	}
	return nil
}
