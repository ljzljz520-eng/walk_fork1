//go:build darwin || linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

type fileIdentity struct {
	dev uint64
	ino uint64
}

type fileMetadata struct {
	id    fileIdentity
	mode  uint32
	uid   uint32
	gid   uint32
	rdev  int
	size  int64
	nlink uint64
	atim  unix.Timespec
	mtim  unix.Timespec
}

type hardLinkTarget struct {
	dirFD int
	name  string
	flags uint64
}

type hardLinkPlan struct {
	id        fileIdentity
	canonical []string
}

type treeCopier struct {
	hardLinks   map[fileIdentity]hardLinkTarget
	hardPlans   map[fileIdentity]hardLinkPlan
	sourceDirs  map[string]fileIdentity
	createdDirs map[string]fileIdentity
}

func copyTree(src, dst string) (err error) {
	dstParent, err := openDirectory(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := unix.Close(dstParent); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	var rootStat unix.Stat_t
	if err := unix.Fstatat(unix.AT_FDCWD, src, &rootStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	rootMeta := metadataFromStat(&rootStat)
	copier := &treeCopier{
		hardLinks: make(map[fileIdentity]hardLinkTarget),
		hardPlans: make(map[fileIdentity]hardLinkPlan),
	}
	defer copier.close()

	switch rootMeta.mode & unix.S_IFMT {
	case unix.S_IFDIR:
		srcRootFD, err := unix.Openat(unix.AT_FDCWD, src,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(srcRootFD)
		if err := verifyIdentity(srcRootFD, rootMeta.id); err != nil {
			return err
		}

		copier.sourceDirs = map[string]fileIdentity{"": rootMeta.id}
		if err := copier.planDirectoryHardLinks(srcRootFD, nil); err != nil {
			return err
		}

		copier.createdDirs = make(map[string]fileIdentity)
		if err := copier.createDirectorySkeleton(srcRootFD, dstParent, filepath.Base(dst), rootMeta); err != nil {
			return err
		}
		if err := copier.createCanonicalHardLinks(srcRootFD, dstParent, filepath.Base(dst)); err != nil {
			return err
		}
		if err := copier.copyEntry(unix.AT_FDCWD, src, dstParent, filepath.Base(dst), nil); err != nil {
			return err
		}

	case unix.S_IFREG:
		srcRootFD, err := unix.Openat(unix.AT_FDCWD, src,
			unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(srcRootFD)
		if err := verifyIdentity(srcRootFD, rootMeta.id); err != nil {
			return err
		}
		if rootMeta.nlink > 1 {
			copier.hardPlans[rootMeta.id] = hardLinkPlan{id: rootMeta.id}
		}
		if err := copier.createCanonicalHardLinks(srcRootFD, dstParent, filepath.Base(dst)); err != nil {
			return err
		}
		if err := copier.copyEntry(unix.AT_FDCWD, src, dstParent, filepath.Base(dst), nil); err != nil {
			return err
		}

	default:
		if err := copier.copyEntry(unix.AT_FDCWD, src, dstParent, filepath.Base(dst), nil); err != nil {
			return err
		}
	}

	return copier.applyDeferredFlags()
}

func metadataFromStat(st *unix.Stat_t) fileMetadata {
	return fileMetadata{
		id: fileIdentity{
			dev: uint64(st.Dev),
			ino: st.Ino,
		},
		mode:  uint32(st.Mode),
		uid:   st.Uid,
		gid:   st.Gid,
		rdev:  int(st.Rdev),
		size:  st.Size,
		nlink: uint64(st.Nlink),
		atim:  st.Atim,
		mtim:  st.Mtim,
	}
}

func openDirectory(path string) (int, error) {
	return unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

func (c *treeCopier) copyEntry(srcParent int, srcName string, dstParent int, dstName string, rel []string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(srcParent, srcName, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	meta := metadataFromStat(&st)

	switch meta.mode & unix.S_IFMT {
	case unix.S_IFDIR:
		srcFD, err := unix.Openat(srcParent, srcName,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(srcFD)
		if err := verifyIdentity(srcFD, meta.id); err != nil {
			return err
		}
		return c.copyDirectory(srcFD, dstParent, dstName, meta, rel)

	case unix.S_IFREG:
		srcFD, err := unix.Openat(srcParent, srcName,
			unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(srcFD)
		if err := verifyIdentity(srcFD, meta.id); err != nil {
			return err
		}
		return c.copyRegular(srcFD, dstParent, dstName, meta, rel)

	case unix.S_IFLNK:
		return c.copySymlink(srcParent, srcName, dstParent, dstName, meta)

	default:
		return copySpecial(dstParent, dstName, meta)
	}
}

func verifyIdentity(fd int, want fileIdentity) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	got := fileIdentity{dev: uint64(st.Dev), ino: st.Ino}
	if got != want {
		return fmt.Errorf("source file changed while it was being copied")
	}
	return nil
}

func relativeKey(parts []string) string {
	return strings.Join(parts, "\x00")
}

func sameRelativePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func lessRelativePath(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func childPath(parent []string, name string) []string {
	child := make([]string, 0, len(parent)+1)
	child = append(child, parent...)
	return append(child, name)
}

func rememberHardLinkPlan(plans map[fileIdentity]hardLinkPlan, id fileIdentity, path []string) {
	existing, ok := plans[id]
	if !ok || lessRelativePath(path, existing.canonical) {
		canonical := make([]string, len(path))
		copy(canonical, path)
		plans[id] = hardLinkPlan{id: id, canonical: canonical}
	}
}

func (c *treeCopier) planDirectoryHardLinks(srcFD int, rel []string) error {
	names, err := readDirectoryNames(srcFD)
	if err != nil {
		return err
	}
	for _, name := range names {
		childRel := childPath(rel, name)
		var st unix.Stat_t
		if err := unix.Fstatat(srcFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		meta := metadataFromStat(&st)
		switch meta.mode & unix.S_IFMT {
		case unix.S_IFREG:
			if meta.nlink > 1 {
				rememberHardLinkPlan(c.hardPlans, meta.id, childRel)
			}

		case unix.S_IFDIR:
			childFD, err := unix.Openat(srcFD, name,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			if err := verifyIdentity(childFD, meta.id); err != nil {
				_ = unix.Close(childFD)
				return err
			}
			c.sourceDirs[relativeKey(childRel)] = meta.id
			err = c.planDirectoryHardLinks(childFD, childRel)
			_ = unix.Close(childFD)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *treeCopier) createDirectorySkeleton(srcRootFD int, dstParent int, dstName string, rootMeta fileMetadata) error {
	if err := unix.Mkdirat(dstParent, dstName, 0o700); err != nil {
		return err
	}
	dstRootFD, err := unix.Openat(dstParent, dstName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dstRootFD)

	rootID, err := identityFromFD(dstRootFD)
	if err != nil {
		return err
	}
	c.createdDirs[""] = rootID
	_ = ignoreOperationNotPermitted(unix.Fchown(dstRootFD, int(rootMeta.uid), int(rootMeta.gid)))

	return c.createDirectorySkeletonChildren(srcRootFD, dstRootFD, nil)
}

func (c *treeCopier) createDirectorySkeletonChildren(srcFD, dstFD int, rel []string) error {
	names, err := readDirectoryNames(srcFD)
	if err != nil {
		return err
	}
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(srcFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		meta := metadataFromStat(&st)
		if meta.mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}

		childRel := childPath(rel, name)
		if err := unix.Mkdirat(dstFD, name, 0o700); err != nil {
			return err
		}
		childDstFD, err := unix.Openat(dstFD, name,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		childID, err := identityFromFD(childDstFD)
		if err != nil {
			_ = unix.Close(childDstFD)
			return err
		}
		c.createdDirs[relativeKey(childRel)] = childID
		_ = ignoreOperationNotPermitted(unix.Fchown(childDstFD, int(meta.uid), int(meta.gid)))

		childSrcFD, err := unix.Openat(srcFD, name,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			_ = unix.Close(childDstFD)
			return err
		}
		if err := verifyIdentity(childSrcFD, meta.id); err != nil {
			_ = unix.Close(childDstFD)
			_ = unix.Close(childSrcFD)
			return err
		}
		err = c.createDirectorySkeletonChildren(childSrcFD, childDstFD, childRel)
		_ = unix.Close(childSrcFD)
		_ = unix.Close(childDstFD)
		if err != nil {
			return err
		}
	}
	return nil
}

func identityFromFD(fd int) (fileIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fileIdentity{}, err
	}
	return fileIdentity{dev: uint64(st.Dev), ino: st.Ino}, nil
}

func (c *treeCopier) createCanonicalHardLinks(srcRootFD int, dstParent int, dstName string) error {
	plans := make([]hardLinkPlan, 0, len(c.hardPlans))
	for _, plan := range c.hardPlans {
		plans = append(plans, plan)
	}
	sort.Slice(plans, func(i, j int) bool {
		return lessRelativePath(plans[i].canonical, plans[j].canonical)
	})

	for _, plan := range plans {
		var err error
		if len(plan.canonical) == 0 {
			err = c.createRootCanonicalHardLink(srcRootFD, dstParent, dstName, plan)
		} else {
			err = c.createNestedCanonicalHardLink(srcRootFD, dstParent, dstName, plan)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *treeCopier) createRootCanonicalHardLink(srcRootFD, dstParent int, dstName string, plan hardLinkPlan) error {
	var st unix.Stat_t
	if err := unix.Fstat(srcRootFD, &st); err != nil {
		return err
	}
	meta := metadataFromStat(&st)
	if meta.id != plan.id || meta.mode&unix.S_IFMT != unix.S_IFREG || meta.nlink <= 1 {
		return fmt.Errorf("planned hard link source changed")
	}
	return c.createCanonicalRegular(srcRootFD, dstParent, dstName, meta)
}

func (c *treeCopier) createNestedCanonicalHardLink(srcRootFD, dstParent int, dstName string, plan hardLinkPlan) error {
	parentRel := plan.canonical[:len(plan.canonical)-1]
	baseName := plan.canonical[len(plan.canonical)-1]

	srcParentFD, err := openVerifiedDirectoryPath(srcRootFD, parentRel, c.sourceDirs)
	if err != nil {
		return err
	}
	defer unix.Close(srcParentFD)
	srcFD, err := openRegularRelative(srcParentFD, baseName, plan.id)
	if err != nil {
		return err
	}
	defer unix.Close(srcFD)

	dstRootFD, err := unix.Openat(dstParent, dstName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dstRootFD)
	if err := verifyFDIdentity(dstRootFD, c.createdDirs[""]); err != nil {
		return err
	}
	dstCanonicalParent, err := openVerifiedDirectoryPath(dstRootFD, parentRel, c.createdDirs)
	if err != nil {
		return err
	}
	defer unix.Close(dstCanonicalParent)

	var st unix.Stat_t
	if err := unix.Fstat(srcFD, &st); err != nil {
		return err
	}
	meta := metadataFromStat(&st)
	if meta.id != plan.id || meta.mode&unix.S_IFMT != unix.S_IFREG || meta.nlink <= 1 {
		return fmt.Errorf("planned hard link source changed")
	}
	return c.createCanonicalRegular(srcFD, dstCanonicalParent, baseName, meta)
}

func openVerifiedDirectoryPath(rootFD int, rel []string, ids map[string]fileIdentity) (int, error) {
	fd, err := unix.Dup(rootFD)
	if err != nil {
		return -1, err
	}
	for i, name := range rel {
		nextFD, err := unix.Openat(fd, name,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, err
		}
		id, err := identityFromFD(nextFD)
		if err != nil {
			_ = unix.Close(nextFD)
			return -1, err
		}
		if id != ids[relativeKey(rel[:i+1])] {
			_ = unix.Close(nextFD)
			return -1, fmt.Errorf("source directory changed while it was being copied")
		}
		fd = nextFD
	}
	return fd, nil
}

func openRegularRelative(parentFD int, name string, want fileIdentity) (int, error) {
	fd, err := unix.Openat(parentFD, name,
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := verifyIdentity(fd, want); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func verifyFDIdentity(fd int, want fileIdentity) error {
	id, err := identityFromFD(fd)
	if err != nil {
		return err
	}
	if id != want {
		return fmt.Errorf("destination directory changed while it was being copied")
	}
	return nil
}

func (c *treeCopier) copyDirectory(srcFD int, dstParent int, dstName string, meta fileMetadata, rel []string) error {
	if err := unix.Mkdirat(dstParent, dstName, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}

	dstFD, err := unix.Openat(dstParent, dstName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dstFD)
	if expected := c.createdDirs[relativeKey(rel)]; expected == (fileIdentity{}) {
		return fmt.Errorf("destination directory was not created by this copy")
	} else if err := verifyFDIdentity(dstFD, expected); err != nil {
		return err
	}

	// Set ownership before descending so setgid inheritance and ownership rules
	// match the source. Unprivileged callers cannot take ownership of other
	// users' files; preserve everything else instead of failing the whole copy.
	_ = ignoreOperationNotPermitted(unix.Fchown(dstFD, int(meta.uid), int(meta.gid)))

	names, err := readDirectoryNames(srcFD)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := c.copyEntry(srcFD, name, dstFD, name, childPath(rel, name)); err != nil {
			return err
		}
	}

	if err := copyACLAndExtendedAttributes(srcFD, dstFD); err != nil {
		return err
	}
	if err := unix.Fchmod(dstFD, meta.mode&0o7777); err != nil {
		return err
	}
	if err := setPlatformTimestamps(dstParent, dstFD, dstName, meta.atim, meta.mtim, false); err != nil {
		return err
	}
	flags, ok := getFileFlags(srcFD)
	if ok {
		return setFileFlags(dstFD, flags)
	}
	return nil
}

func readDirectoryNames(fd int) ([]string, error) {
	dupFD, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(dupFD), "source-directory")
	defer file.Close()

	entries, err := file.ReadDir(-1)
	// Dup shares the source directory's file offset, so rewind the original
	// descriptor for later traversal passes.
	_, _ = unix.Seek(fd, 0, unix.SEEK_SET)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func (c *treeCopier) copyRegular(srcFD int, dstParent int, dstName string, meta fileMetadata, rel []string) error {
	if meta.nlink > 1 {
		plan := c.hardPlans[meta.id]
		target, copied := c.hardLinks[meta.id]
		isCanonical := sameRelativePath(rel, plan.canonical)
		if copied {
			if isCanonical {
				return nil
			}
			return unix.Linkat(target.dirFD, target.name, dstParent, dstName, 0)
		}
		if !isCanonical {
			return fmt.Errorf("canonical hard link was not copied before its links")
		}
	}
	return c.createCanonicalRegular(srcFD, dstParent, dstName, meta)
}

func (c *treeCopier) createCanonicalRegular(srcFD int, dstParent int, dstName string, meta fileMetadata) error {
	flags, _ := getFileFlags(srcFD)
	dstFD, dataCopied, err := createRegularDestination(srcFD, dstParent, dstName, meta.mode&0o7777, flags)
	if err != nil {
		return err
	}
	defer unix.Close(dstFD)

	if !dataCopied {
		if err := copyRegularData(srcFD, dstFD, meta.size); err != nil {
			return err
		}
	}
	if err := copyACLAndExtendedAttributes(srcFD, dstFD); err != nil {
		return err
	}
	if err := ignoreOperationNotPermitted(unix.Fchown(dstFD, int(meta.uid), int(meta.gid))); err != nil {
		return err
	}
	if err := unix.Fchmod(dstFD, meta.mode&0o7777); err != nil {
		return err
	}
	if err := setPlatformTimestamps(dstParent, dstFD, dstName, meta.atim, meta.mtim, false); err != nil {
		return err
	}

	if meta.nlink > 1 {
		dupFD, err := unix.Dup(dstParent)
		if err != nil {
			return err
		}
		c.hardLinks[meta.id] = hardLinkTarget{dirFD: dupFD, name: dstName, flags: flags}
		return nil
	}
	if flags != 0 {
		return setFileFlags(dstFD, flags)
	}
	return nil
}

func (c *treeCopier) copySymlink(srcParent int, srcName string, dstParent int, dstName string, meta fileMetadata) error {
	target, err := readLinkName(srcParent, srcName)
	if err != nil {
		return err
	}
	if err := unix.Symlinkat(target, dstParent, dstName); err != nil {
		return err
	}
	if err := copySymlinkMetadata(srcParent, srcName, dstParent, dstName); err != nil {
		return fmt.Errorf("copy symlink metadata: %w", err)
	}

	if err := ignoreOperationNotPermitted(unix.Fchownat(
		dstParent, dstName, int(meta.uid), int(meta.gid), unix.AT_SYMLINK_NOFOLLOW,
	)); err != nil {
		return fmt.Errorf("set symlink owner: %w", err)
	}
	if err := setPlatformTimestamps(dstParent, -1, dstName, meta.atim, meta.mtim, true); err != nil {
		return fmt.Errorf("set symlink timestamps: %w", err)
	}
	return nil
}

func readLinkName(parentFD int, name string) (string, error) {
	size := 256
	for {
		buffer := make([]byte, size)
		n, err := unix.Readlinkat(parentFD, name, buffer)
		if err != nil {
			return "", err
		}
		if n < len(buffer) {
			return string(buffer[:n]), nil
		}
		if size >= 1<<20 {
			return "", fmt.Errorf("symbolic link target is too long: %w", unix.ENAMETOOLONG)
		}
		size *= 2
	}
}

func (c *treeCopier) applyDeferredFlags() error {
	for _, target := range c.hardLinks {
		if target.flags == 0 {
			continue
		}
		fd, err := unix.Openat(target.dirFD, target.name,
			unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		err = setFileFlags(fd, target.flags)
		closeErr := unix.Close(fd)
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (c *treeCopier) close() {
	for _, target := range c.hardLinks {
		_ = unix.Close(target.dirFD)
	}
}

func copyExtendedAttributes(srcFD, dstFD int) error {
	size, err := unix.Flistxattr(srcFD, nil)
	if err != nil {
		if isNoExtendedAttribute(err) || isUnsupportedMetadata(err) {
			return nil
		}
		return err
	}
	if size == 0 {
		return nil
	}

	buf := make([]byte, size)
	size, err = unix.Flistxattr(srcFD, buf)
	if err != nil {
		if isNoExtendedAttribute(err) {
			return nil
		}
		return err
	}
	for _, rawName := range bytes.Split(bytes.TrimRight(buf[:size], "\x00"), []byte{0}) {
		if len(rawName) == 0 {
			continue
		}
		name := string(rawName)
		valueSize, err := unix.Fgetxattr(srcFD, name, nil)
		if err != nil {
			if isNoExtendedAttribute(err) {
				continue
			}
			return err
		}
		value := make([]byte, valueSize)
		if _, err = unix.Fgetxattr(srcFD, name, value); err != nil {
			return err
		}
		if err = unix.Fsetxattr(dstFD, name, value, 0); err != nil {
			if isUnsupportedMetadata(err) || isOperationNotPermitted(err) {
				continue
			}
			return err
		}
	}
	return nil
}

func copySparseFile(srcFD, dstFD int, size int64) error {
	if err := unix.Ftruncate(dstFD, size); err != nil {
		return err
	}
	if size == 0 {
		return nil
	}

	position := int64(0)
	for position < size {
		hole, err := unix.Seek(srcFD, position, unix.SEEK_HOLE)
		if err != nil {
			if isUnsupportedSeek(err) {
				return copyByteRange(srcFD, dstFD, 0, size)
			}
			return err
		}
		if hole > size {
			hole = size
		}
		if hole > position {
			if err := copyByteRange(srcFD, dstFD, position, hole-position); err != nil {
				return err
			}
		}
		if hole >= size {
			return nil
		}

		position, err = unix.Seek(srcFD, hole, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				return nil
			}
			if isUnsupportedSeek(err) {
				return copyByteRange(srcFD, dstFD, hole, size-hole)
			}
			return err
		}
		if position >= size {
			return nil
		}
	}
	return nil
}

func copyByteRange(srcFD, dstFD int, start, length int64) error {
	buffer := make([]byte, 1<<20)
	for length > 0 {
		chunk := int64(len(buffer))
		if length < chunk {
			chunk = length
		}

		n, err := unix.Pread(srcFD, buffer[:chunk], start)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("unexpected end of file while copying sparse file")
		}
		if err := writeAllAt(dstFD, buffer[:n], start); err != nil {
			return err
		}
		start += int64(n)
		length -= int64(n)
	}
	return nil
}

func writeAllAt(fd int, buffer []byte, offset int64) error {
	for len(buffer) > 0 {
		n, err := unix.Pwrite(fd, buffer, offset)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no progress while writing copied file")
		}
		buffer = buffer[n:]
		offset += int64(n)
	}
	return nil
}

func isNoExtendedAttribute(err error) bool {
	return errors.Is(err, noExtendedAttributeError())
}

func isUnsupportedMetadata(err error) bool {
	return errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.EINVAL)
}

func isUnsupportedSeek(err error) bool {
	return isUnsupportedMetadata(err)
}

func isOperationNotPermitted(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
}

func ignoreOperationNotPermitted(err error) error {
	if isOperationNotPermitted(err) {
		return nil
	}
	return err
}
