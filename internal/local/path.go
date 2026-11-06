package local

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"fs-relay/internal/wire"
)

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}

func oneName(name string) error {
	if name == "" {
		return wire.ErrParams
	}
	if name == "." || name == ".." || strings.Contains(name, "/") || strings.ContainsRune(name, 0) {
		return syscall.EINVAL
	}
	return nil
}

func joinRel(parent, name string) (string, error) {
	if err := oneName(name); err != nil {
		return "", err
	}
	if parent == "" {
		return name, nil
	}
	return parent + "/" + name, nil
}

func (t *Tree) abs(ns, rel string) (string, error) {
	if strings.HasPrefix(rel, "/") || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") || strings.HasSuffix(rel, "/..") {
		return "", syscall.EINVAL
	}
	dir := filepath.Join(t.root, ns)
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		return "", syscall.ENOTDIR
	}
	if rel == "" {
		return dir, nil
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if !inside(dir, full) {
		return "", syscall.EINVAL
	}
	if parentDir := filepath.Dir(full); parentDir != dir {
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return "", err
		}
		realParent, err := filepath.EvalSymlinks(parentDir)
		if err != nil {
			return "", err
		}
		if !inside(realDir, realParent) {
			return "", syscall.EINVAL
		}
	}
	return full, nil
}

func inside(root, full string) bool {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func child(t *Tree, ns string, parent uint64, name string) (rel, abs string, err error) {
	parentRel, err := t.relOf(ns, parent)
	if err != nil {
		return "", "", err
	}
	rel, err = joinRel(parentRel, name)
	if err != nil {
		return "", "", err
	}
	abs, err = t.abs(ns, rel)
	return rel, abs, err
}
