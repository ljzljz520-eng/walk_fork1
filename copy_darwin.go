//go:build darwin

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	ufNoUnlink      = 0x00000010
	darwinFileFlags = unix.UF_NODUMP |
		unix.UF_IMMUTABLE |
		unix.UF_APPEND |
		unix.UF_OPAQUE |
		ufNoUnlink |
		unix.UF_HIDDEN |
		unix.SF_ARCHIVED |
		unix.SF_IMMUTABLE |
		unix.SF_APPEND |
		unix.SF_NOUNLINK
)

func getFileFlags(fd int) (uint64, bool) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, false
	}
	return uint64(st.Flags) & uint64(darwinFileFlags), true
}

func setFileFlags(fd int, flags uint64) error {
	return ignoreOperationNotPermitted(unix.Fchflags(fd, int(flags)))
}

func copySymlinkMetadata(srcParent int, srcName string, dstParent int, dstName string) error {
	srcFD, err := unix.Openat(srcParent, srcName,
		unix.O_RDONLY|unix.O_SYMLINK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(srcFD)

	dstFD, err := unix.Openat(dstParent, dstName,
		unix.O_RDONLY|unix.O_SYMLINK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dstFD)

	if err := copyExtendedAttributes(srcFD, dstFD); err != nil {
		return err
	}
	if flags, ok := getFileFlags(srcFD); ok && flags != 0 {
		return setFileFlags(dstFD, flags)
	}
	return nil
}

func copySpecial(_ int, name string, _ fileMetadata) error {
	return fmt.Errorf("cannot copy special file %s: %w", name, unix.EOPNOTSUPP)
}

func noExtendedAttributeError() error {
	return unix.ENOATTR
}
