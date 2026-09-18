//go:build darwin && cgo

package main

/*
#include <copyfile.h>
#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <sys/stat.h>

static int walk_copyfile_fds(int src, int dst, unsigned int flags) {
	copyfile_state_t state = copyfile_state_alloc();
	if (state == NULL) {
		return ENOMEM;
	}

	int rc = fcopyfile(src, dst, state, flags);
	int err = errno;

	// COPYFILE_DATA_SPARSE is a hint. On kernels or filesystems that reject it,
	// retry with a regular data copy while retaining the requested metadata.
	if (rc != 0 && err == EINVAL && (flags & COPYFILE_DATA_SPARSE)) {
		rc = fcopyfile(src, dst, state, flags & ~(unsigned int)COPYFILE_DATA_SPARSE);
		err = errno;
	}

	copyfile_state_free(state);
	return rc == 0 ? 0 : err;
}

static int walk_utimensat(int dirfd, const char *path,
	long access_seconds, long access_nanoseconds,
	long modification_seconds, long modification_nanoseconds) {
	struct timespec times[2] = {
		{access_seconds, access_nanoseconds},
		{modification_seconds, modification_nanoseconds},
	};
	if (utimensat(dirfd, path, times, AT_SYMLINK_NOFOLLOW) == 0) {
		return 0;
	}
	return errno;
}
*/
import "C"

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const preservesDarwinACL = true

func createRegularDestination(srcFD, dstParent int, dstName string, mode uint32, flags uint64) (int, bool, error) {
	dstFD, err := unix.Openat(dstParent, dstName,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			err = unix.EEXIST
		}
		return -1, false, err
	}

	cloneFlags := C.COPYFILE_CLONE | C.COPYFILE_DATA_SPARSE
	if errno := C.walk_copyfile_fds(C.int(srcFD), C.int(dstFD), C.uint(cloneFlags)); errno != 0 {
		dataFlags := C.COPYFILE_DATA | C.COPYFILE_DATA_SPARSE
		if errno := C.walk_copyfile_fds(C.int(srcFD), C.int(dstFD), C.uint(dataFlags)); errno != 0 {
			_ = unix.Close(dstFD)
			return -1, false, copyfileResult(errno)
		}
	}
	if flags != 0 {
		if err := unix.Fchflags(dstFD, 0); err != nil {
			_ = unix.Close(dstFD)
			return -1, false, err
		}
	}
	return dstFD, true, nil
}

func copyRegularData(srcFD, dstFD int, _ int64) error {
	const flags = C.COPYFILE_DATA | C.COPYFILE_DATA_SPARSE
	return copyfileResult(C.walk_copyfile_fds(C.int(srcFD), C.int(dstFD), C.uint(flags)))
}

func copyACLAndExtendedAttributes(srcFD, dstFD int) error {
	const flags = C.COPYFILE_ACL | C.COPYFILE_XATTR
	return copyfileResult(C.walk_copyfile_fds(C.int(srcFD), C.int(dstFD), C.uint(flags)))
}

func copyfileResult(errno C.int) error {
	if errno == 0 {
		return nil
	}
	return unix.Errno(errno)
}

func setPlatformTimestamps(parentFD, dstFD int, name string, atim, mtim unix.Timespec, isSymlink bool) error {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	errno := C.walk_utimensat(
		C.int(parentFD), cName,
		C.long(atim.Sec), C.long(atim.Nsec),
		C.long(mtim.Sec), C.long(mtim.Nsec),
	)
	if err := copyfileResult(errno); err != nil {
		if !errors.Is(err, unix.ENOSYS) {
			return err
		}
		if isSymlink {
			return nil
		}
		return syscall.Futimes(dstFD, []syscall.Timeval{
			{Sec: atim.Sec, Usec: int32(atim.Nsec / 1000)},
			{Sec: mtim.Sec, Usec: int32(mtim.Nsec / 1000)},
		})
	}
	return nil
}
