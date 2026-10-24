package diagnostic

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOBS02FilesystemClassification(t *testing.T) {
	for _, kind := range []int64{unix.TMPFS_MAGIC, unix.RAMFS_MAGIC, unix.PROC_SUPER_MAGIC, unix.SYSFS_MAGIC, unix.DEVPTS_SUPER_MAGIC, unix.CGROUP_SUPER_MAGIC, unix.CGROUP2_SUPER_MAGIC, unix.OVERLAYFS_SUPER_MAGIC} {
		if checkFilesystemStat(unix.Statfs_t{Type: kind}) == nil {
			t.Fatal("가상·메모리 파일 시스템 수락")
		}
	}
	if checkFilesystemStat(unix.Statfs_t{Type: unix.EXT4_SUPER_MAGIC, Flags: unix.ST_RDONLY}) == nil {
		t.Fatal("읽기 전용 수락")
	}
	if err := checkFilesystemStat(unix.Statfs_t{Type: unix.EXT4_SUPER_MAGIC}); err != nil {
		t.Fatal(err)
	}
}

func TestOBS02VolatileMount(t *testing.T) {
	root := os.Getenv("ACS_OBS02_VOLATILE_ROOT")
	if root == "" {
		t.Skip("실제 tmpfs 주입 환경에서 실행한다")
	}
	source, _ := NewSource("volatile")
	if files, err := PrepareFiles(t.Context(), filepath.Join(root, "logs"), source); err == nil {
		files.Close()
		t.Fatal("실제 tmpfs 수락")
	} else if failure, ok := errors.AsType[*FileError](err); !ok || failure.Code != "volatile_storage" {
		t.Fatal("tmpfs 검사에 도달하지 못했다", err)
	}
}

func TestOBS02ReadOnlyMount(t *testing.T) {
	root := os.Getenv("ACS_OBS02_READONLY_ROOT")
	if root == "" {
		t.Skip("실제 읽기 전용 마운트 주입 환경에서 실행한다")
	}
	source, _ := NewSource("readonly")
	if files, err := PrepareFiles(t.Context(), root, source); err == nil {
		files.Close()
		t.Fatal("실제 읽기 전용 마운트 수락")
	} else if failure, ok := errors.AsType[*FileError](err); !ok || failure.Code != "storage_read_only" {
		t.Fatal("읽기 전용 검사에 도달하지 못했다", err)
	}
}
