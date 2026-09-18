//go:build linux

package main

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

func createRegularDestination(_ int, dstParent int, dstName string, mode uint32, _ uint64) (int, bool, error) {
	fd, err := unix.Openat(dstParent, dstName,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil && errors.Is(err, unix.ELOOP) {
		err = unix.EEXIST
	}
	return fd, false, err
}

func getFileFlags(fd int) (uint64, bool) {
	flags, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		return 0, false
	}
	return uint64(flags), true
}

func setFileFlags(fd int, flags uint64) error {
	return ignoreOperationNotPermitted(
		unix.IoctlSetInt(fd, unix.FS_IOC_SETFLAGS, int(flags)),
	)
}

func copyRegularData(srcFD, dstFD int, size int64) error {
	return copySparseFile(srcFD, dstFD, size)
}

func copyACLAndExtendedAttributes(srcFD, dstFD int) error {
	return copyExtendedAttributes(srcFD, dstFD)
}

func copySymlinkMetadata(_ int, _ string, _ int, _ string) error {
	// Linux requires privileged operations or /proc/self/fd tricks to modify a
	// symbolic link's own extended attributes. The link target, ownership and
	// timestamps are preserved; this does not follow the link.
	return nil
}

func copySpecial(dstParent int, dstName string, meta fileMetadata) error {
	switch meta.mode & unix.S_IFMT {
	case unix.S_IFIFO:
		if err := unix.Mkfifoat(dstParent, dstName, meta.mode&0o7777); err != nil {
			return err
		}
	case unix.S_IFCHR, unix.S_IFBLK, unix.S_IFSOCK:
		if err := unix.Mknodat(dstParent, dstName, meta.mode, meta.rdev); err != nil {
			return err
		}
	default:
		return fmt.Errorf("cannot copy special file %s: %w", dstName, unix.EOPNOTSUPP)
	}

	if err := ignoreOperationNotPermitted(unix.Fchownat(
		dstParent, dstName, int(meta.uid), int(meta.gid), unix.AT_SYMLINK_NOFOLLOW,
	)); err != nil {
		return err
	}
	if err := unix.Fchmodat(dstParent, dstName, meta.mode&0o7777, 0); err != nil {
		return err
	}
	return setPlatformTimestamps(dstParent, -1, dstName, meta.atim, meta.mtim, false)
}

func setPlatformTimestamps(parentFD, _ int, name string, atim, mtim unix.Timespec, _ bool) error {
	times := [2]unix.Timespec{atim, mtim}
	path, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_UTIMENSAT,
		uintptr(parentFD),
		uintptr(unsafe.Pointer(path)),
		uintptr(unsafe.Pointer(&times[0])),
		uintptr(unix.AT_SYMLINK_NOFOLLOW),
		0,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func noExtendedAttributeError() error {
	return unix.ENODATA
}
