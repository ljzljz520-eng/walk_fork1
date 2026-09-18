//go:build darwin

package main

import (
	"bytes"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func copyTestTree(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(src, 0o750); err != nil {
		t.Fatal(err)
	}
	return src, dst
}

func darwinStat(t *testing.T, path string) *unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return &st
}

func setNanosecondTimestamps(t *testing.T, path string) {
	t.Helper()
	times := []syscall.Timespec{
		{Sec: 1_600_000_000, Nsec: 123},
		{Sec: 1_600_000_100, Nsec: 456},
	}
	if err := syscall.UtimesNano(path, times); err != nil {
		t.Fatal(err)
	}
}

func TestCopyTreePreservesFileMetadata(t *testing.T) {
	src, dst := copyTestTree(t)
	path := filepath.Join(src, "file.txt")
	data := []byte("hello\n")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	setNanosecondTimestamps(t, path)

	fd, err := unix.Open(path, unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Fsetxattr(fd, "user.walk-test", []byte("value"), 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chflags(path, unix.UF_NODUMP); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree returned error: %v", err)
	}

	copied := filepath.Join(dst, "file.txt")
	gotData, err := os.ReadFile(copied)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotData, data) {
		t.Fatalf("content = %q, want %q", gotData, data)
	}

	sourceStat := darwinStat(t, path)
	copiedStat := darwinStat(t, copied)
	if copiedStat.Mode != sourceStat.Mode {
		t.Fatalf("mode = %o, want %o", copiedStat.Mode, sourceStat.Mode)
	}
	if copiedStat.Uid != sourceStat.Uid || copiedStat.Gid != sourceStat.Gid {
		t.Fatalf("owner = %d:%d, want %d:%d", copiedStat.Uid, copiedStat.Gid, sourceStat.Uid, sourceStat.Gid)
	}
	if copiedStat.Mtim != sourceStat.Mtim {
		t.Fatalf("mtime = %+v, want %+v", copiedStat.Mtim, sourceStat.Mtim)
	}
	if copiedStat.Flags&unix.UF_NODUMP == 0 {
		t.Fatalf("flags = %#x, want UF_NODUMP", copiedStat.Flags)
	}

	copiedFD, err := unix.Open(copied, unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(copiedFD)
	value := make([]byte, len("value"))
	n, err := unix.Fgetxattr(copiedFD, "user.walk-test", value)
	if err != nil {
		t.Fatalf("xattr was not preserved: %v", err)
	}
	if string(value[:n]) != "value" {
		t.Fatalf("xattr = %q, want value", value[:n])
	}
}

func TestCopyTreePreservesSymlinks(t *testing.T) {
	src, dst := copyTestTree(t)
	targetPath := filepath.Join(src, "target")
	if err := os.WriteFile(targetPath, []byte("target"), 0o640); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(src, "link")
	if err := os.Symlink("target", linkPath); err != nil {
		t.Fatal(err)
	}
	linkFD, err := unix.Open(linkPath, unix.O_RDONLY|unix.O_SYMLINK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Fchflags(linkFD, unix.UF_HIDDEN); err != nil {
		_ = unix.Close(linkFD)
		t.Fatal(err)
	}
	if err := unix.Close(linkFD); err != nil {
		t.Fatal(err)
	}
	linkAtim := unix.Timespec{Sec: 1_600_000_000, Nsec: 123}
	linkMtim := unix.Timespec{Sec: 1_600_000_100, Nsec: 456}
	if err := setPlatformTimestamps(unix.AT_FDCWD, -1, linkPath, linkAtim, linkMtim, true); err != nil {
		t.Fatal(err)
	}
	brokenPath := filepath.Join(src, "broken")
	if err := os.Symlink("missing", brokenPath); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree returned error: %v", err)
	}

	for _, tc := range []struct {
		path   string
		target string
	}{
		{filepath.Join(dst, "link"), "target"},
		{filepath.Join(dst, "broken"), "missing"},
	} {
		stat := darwinStat(t, tc.path)
		if stat.Mode&unix.S_IFMT != unix.S_IFLNK {
			t.Fatalf("%s mode = %o, want symlink", tc.path, stat.Mode)
		}
		target, err := os.Readlink(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if target != tc.target {
			t.Fatalf("symlink target = %q, want %q", target, tc.target)
		}
	}
	copiedLinkStat := darwinStat(t, filepath.Join(dst, "link"))
	if copiedLinkStat.Flags&unix.UF_HIDDEN == 0 {
		t.Fatal("symlink flags were not preserved")
	}
	if copiedLinkStat.Mtim != linkMtim {
		t.Fatalf("symlink mtime = %+v, want %+v", copiedLinkStat.Mtim, linkMtim)
	}
	if darwinStat(t, filepath.Join(dst, "target")).Flags&unix.UF_HIDDEN != 0 {
		t.Fatal("symlink flags were applied to the link target")
	}
}

func TestCopyTreePreservesHardLinks(t *testing.T) {
	src, dst := copyTestTree(t)
	first := filepath.Join(src, "a")
	second := filepath.Join(src, "b")
	if err := os.WriteFile(first, []byte("linked"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree returned error: %v", err)
	}

	copiedFirst := filepath.Join(dst, "a")
	copiedSecond := filepath.Join(dst, "b")
	firstStat := darwinStat(t, copiedFirst)
	secondStat := darwinStat(t, copiedSecond)
	if firstStat.Dev != secondStat.Dev || firstStat.Ino != secondStat.Ino {
		t.Fatal("hard links were not preserved")
	}
}

func TestCopyTreePreservesHardLinksAcrossDirectories(t *testing.T) {
	src, dst := copyTestTree(t)
	if err := os.Mkdir(filepath.Join(src, "a"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "b"), 0o750); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(src, "b", "original")
	link := filepath.Join(src, "a", "link")
	if err := os.WriteFile(original, []byte("linked across directories"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, link); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree returned error: %v", err)
	}

	originalStat := darwinStat(t, filepath.Join(dst, "b", "original"))
	linkStat := darwinStat(t, filepath.Join(dst, "a", "link"))
	if originalStat.Dev != linkStat.Dev || originalStat.Ino != linkStat.Ino {
		t.Fatal("hard links across directories were not preserved")
	}
}

func TestCopyTreeDoesNotReplaceExistingDestination(t *testing.T) {
	src, dst := copyTestTree(t)
	sourceFile := filepath.Join(src, "file")
	if err := os.WriteFile(sourceFile, []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dst, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o640); err != nil {
		t.Fatal(err)
	}

	err := copyTree(src, dst)
	if !os.IsExist(err) {
		t.Fatalf("error = %v, want already-exists", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Fatalf("existing destination was modified: %q", got)
	}
}

func TestCopyTreeExistingSymlinkDestinationConflict(t *testing.T) {
	src, dst := copyTestTree(t)
	sourceFile := filepath.Join(src, "file")
	if err := os.WriteFile(sourceFile, []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "missing-target"), dst); err != nil {
		t.Fatal(err)
	}

	err := copyTree(sourceFile, dst)
	if !os.IsExist(err) {
		t.Fatalf("error = %v, want already-exists", err)
	}
	if target, err := os.Readlink(dst); err != nil || target != filepath.Join(src, "missing-target") {
		t.Fatalf("existing symlink was replaced: target=%q, err=%v", target, err)
	}
}

func TestCopyTreePreservesSparseHoles(t *testing.T) {
	src, dst := copyTestTree(t)
	sparse := filepath.Join(src, "sparse")
	file, err := os.OpenFile(sparse, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	const size = 128 << 20
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("start"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("end"), size-int64(len("end"))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree returned error: %v", err)
	}

	copiedPath := filepath.Join(dst, "sparse")
	sourceStat := darwinStat(t, sparse)
	copiedStat := darwinStat(t, copiedPath)
	if copiedStat.Size != size {
		t.Fatalf("size = %d, want %d", copiedStat.Size, size)
	}
	if copiedStat.Blocks >= size/512 {
		t.Fatalf("copied file is not sparse: blocks=%d", copiedStat.Blocks)
	}
	if copiedStat.Blocks > sourceStat.Blocks {
		t.Fatalf("copied sparse blocks = %d, source blocks = %d", copiedStat.Blocks, sourceStat.Blocks)
	}
}

func TestCopyTreePreservesACL(t *testing.T) {
	if !preservesDarwinACL {
		t.Skip("Darwin ACL preservation requires cgo and fcopyfile")
	}
	current, err := user.Current()
	if err != nil {
		t.Skip(err)
	}

	src, dst := copyTestTree(t)
	path := filepath.Join(src, "acl")
	if err := os.WriteFile(path, []byte("acl"), 0o640); err != nil {
		t.Fatal(err)
	}

	rule := "user:" + current.Username + " allow read"
	if output, err := exec.Command("/bin/chmod", "+a", rule, path).CombinedOutput(); err != nil {
		t.Skipf("cannot create ACL on this filesystem: %v: %s", err, output)
	}

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree returned error: %v", err)
	}

	output, err := exec.Command("/bin/ls", "-led", filepath.Join(dst, "acl")).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, []byte(rule)) {
		t.Fatalf("ACL was not preserved, ls output:\n%s", output)
	}
}

func TestCopyTreePreservesDirectoryTimestamps(t *testing.T) {
	src, dst := copyTestTree(t)
	child := filepath.Join(src, "child")
	if err := os.Mkdir(child, 0o750); err != nil {
		t.Fatal(err)
	}
	setNanosecondTimestamps(t, child)

	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}

	sourceStat := darwinStat(t, child)
	copiedStat := darwinStat(t, filepath.Join(dst, "child"))
	if copiedStat.Mtim != sourceStat.Mtim {
		t.Fatalf("directory mtime = %+v, want %+v", copiedStat.Mtim, sourceStat.Mtim)
	}
}
