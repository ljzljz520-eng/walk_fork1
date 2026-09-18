//go:build !(darwin || linux)

package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"
)

type genericRegularPlan struct {
	source string
	dest   string
	info   os.FileInfo
}

type genericRegularGroup struct {
	info          os.FileInfo
	canonicalSrc  string
	canonicalDest string
}

func copyTree(src, dst string) error {
	rootInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}
	rootMode := rootInfo.Mode()

	switch {
	case rootMode.IsDir():
		regulars, err := collectGenericRegulars(src, dst, nil)
		if err != nil {
			return err
		}
		if err := createGenericDirectorySkeleton(src, dst, rootInfo); err != nil {
			return err
		}
		canonicalBySource, err := createGenericCanonicalFiles(regulars)
		if err != nil {
			return err
		}
		return copyGenericTree(src, dst, rootInfo, canonicalBySource)

	case rootMode.IsRegular():
		return copyGenericRegular(src, dst, rootInfo)

	default:
		return copyGenericEntry(src, dst, rootInfo, nil)
	}
}

func collectGenericRegulars(src, dst string, regulars []genericRegularPlan) ([]genericRegularPlan, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		name := entry.Name()
		childSrc := filepath.Join(src, name)
		childDst := filepath.Join(dst, name)
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		switch {
		case info.Mode().IsDir():
			regulars, err = collectGenericRegulars(childSrc, childDst, regulars)
			if err != nil {
				return nil, err
			}
		case info.Mode().IsRegular():
			regulars = append(regulars, genericRegularPlan{
				source: childSrc,
				dest:   childDst,
				info:   info,
			})
		}
	}
	return regulars, nil
}

func createGenericDirectorySkeleton(src, dst string, rootInfo os.FileInfo) error {
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	return createGenericDirectorySkeletonChildren(src, dst)
}

func createGenericDirectorySkeletonChildren(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		childDst := filepath.Join(dst, name)
		if err := os.Mkdir(childDst, 0o700); err != nil {
			return err
		}
		if err := createGenericDirectorySkeletonChildren(filepath.Join(src, name), childDst); err != nil {
			return err
		}
	}
	return nil
}

func createGenericCanonicalFiles(plans []genericRegularPlan) (map[string]string, error) {
	groups := make([]genericRegularGroup, 0, len(plans))
	groupIndex := make([]int, len(plans))
	for i, plan := range plans {
		group := -1
		for j := range groups {
			if os.SameFile(plan.info, groups[j].info) {
				group = j
				break
			}
		}
		if group < 0 {
			groups = append(groups, genericRegularGroup{
				info:          plan.info,
				canonicalSrc:  plan.source,
				canonicalDest: plan.dest,
			})
			group = len(groups) - 1
		} else if plan.source < groups[group].canonicalSrc {
			groups[group].canonicalSrc = plan.source
			groups[group].canonicalDest = plan.dest
		}
		groupIndex[i] = group
	}

	for _, group := range groups {
		info, err := os.Lstat(group.canonicalSrc)
		if err != nil {
			return nil, err
		}
		if err := copyGenericRegular(group.canonicalSrc, group.canonicalDest, info); err != nil {
			return nil, err
		}
	}

	canonicalBySource := make(map[string]string, len(plans))
	for i, plan := range plans {
		canonicalBySource[plan.source] = groups[groupIndex[i]].canonicalDest
	}
	return canonicalBySource, nil
}

func copyGenericTree(src, dst string, rootInfo os.FileInfo, canonicalBySource map[string]string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		name := entry.Name()
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := copyGenericEntry(
			filepath.Join(src, name),
			filepath.Join(dst, name),
			info,
			canonicalBySource,
		); err != nil {
			return err
		}
	}
	if err := os.Chmod(dst, rootInfo.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, rootInfo.ModTime(), rootInfo.ModTime())
}

func copyGenericEntry(src, dst string, info os.FileInfo, canonicalBySource map[string]string) error {
	mode := info.Mode()
	switch {
	case mode.IsDir():
		if err := copyGenericTree(src, dst, info, canonicalBySource); err != nil {
			return err
		}

	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)

	case mode.IsRegular():
		canonical := canonicalBySource[src]
		if canonical == "" {
			return copyGenericRegular(src, dst, info)
		}
		if canonical == dst {
			return nil
		}
		return os.Link(canonical, dst)

	default:
		return &os.PathError{Op: "copy", Path: src, Err: os.ErrInvalid}
	}
	return nil
}

func copyGenericRegular(src, dst string, info os.FileInfo) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destinationFile, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(destinationFile, sourceFile); err != nil {
		destinationFile.Close()
		return err
	}
	if err := destinationFile.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}
