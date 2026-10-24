package diagnostic

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestOBS02FilesystemClassification(t *testing.T) {
	for _, name := range []string{"tmpfs", "mfs", "devfs", "fdesc", "procfs"} {
		var stat unix.Statfs_t
		copy(stat.Fstypename[:], name)
		if checkFilesystemStat(stat) == nil {
			t.Fatal("가상·메모리 파일 시스템 수락")
		}
	}
	if checkFilesystemStat(unix.Statfs_t{Flags: unix.MNT_RDONLY}) == nil {
		t.Fatal("읽기 전용 수락")
	}
	var stat unix.Statfs_t
	copy(stat.Fstypename[:], "apfs")
	if err := checkFilesystemStat(stat); err != nil {
		t.Fatal(err)
	}
}
