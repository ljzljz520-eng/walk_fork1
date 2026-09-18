//go:build darwin && !cgo

package main

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const preservesDarwinACL = false

func createRegularDestination(_ int, dstParent int, dstName string, mode uint32, _ uint64) (int, bool, error) {
	fd, err := unix.Openat(dstParent, dstName,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil && errors.Is(err, unix.ELOOP) {
		err = unix.EEXIST
	}
	return fd, false, err
}

func copyRegularData(srcFD, dstFD int, size int64) error {
	return copySparseFile(srcFD, dstFD, size)
}

func copyACLAndExtendedAttributes(srcFD, dstFD int) error {
	return copyExtendedAttributes(srcFD, dstFD)
}

func setPlatformTimestamps(parentFD, dstFD int, name string, atim, mtim unix.Timespec, isSymlink bool) error {
	if isSymlink {
		return setSymlinkTimestamps(parentFD, name, atim, mtim)
	}
	return syscall.UtimesNano(fmt.Sprintf("/dev/fd/%d", dstFD), []syscall.Timespec{
		{Sec: atim.Sec, Nsec: atim.Nsec},
		{Sec: mtim.Sec, Nsec: mtim.Nsec},
	})
}

// setSymlinkTimestamps uses setattrlistat because Darwin's x/sys package does
// not wrap utimensat. FSOPT_NOFOLLOW ensures a broken link is never followed.
func setSymlinkTimestamps(parentFD int, name string, atim, mtim unix.Timespec) error {
	path, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	attrs := unix.Attrlist{
		Bitmapcount: 5,
		Commonattr:  unix.ATTR_CMN_ACCTIME | unix.ATTR_CMN_MODTIME,
	}
	values := [2]unix.Timespec{mtim, atim}
	_, _, errno := unix.Syscall6(
		unix.SYS_SETATTRLISTAT,
		uintptr(parentFD),
		uintptr(unsafe.Pointer(path)),
		uintptr(unsafe.Pointer(&attrs)),
		uintptr(unsafe.Pointer(&values[0])),
		unsafe.Sizeof(values),
		uintptr(unix.FSOPT_NOFOLLOW),
	)
	if errno == unix.ENOSYS || errno == unix.EOPNOTSUPP {
		return nil
	}
	if errno != 0 {
		return errno
	}
	return nil
}
