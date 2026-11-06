// Package local applies one relay call to a directory tree.
package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"fs-relay/internal/wire"
	"golang.org/x/sys/unix"
)

// Tree is one data directory. Inodes and open files live only as long as the process.
type Tree struct {
	root string

	op sync.RWMutex
	mu sync.Mutex
	ns map[string]*names

	fmu      sync.Mutex
	files    map[uint64]*openFile
	nextFH   uint64
	lockFile *os.File
}

type entry struct {
	rel   string
	locks *lockRec
}

type names struct {
	next   uint64
	byIno  map[uint64]*entry
	byPath map[string]uint64
}

type openFile struct {
	f    *os.File
	ino  uint64
	refs int
	drop bool
}

// New serves root as the parent of every namespace directory.
func New(root string) *Tree {
	return &Tree{
		root:   root,
		ns:     map[string]*names{},
		files:  map[uint64]*openFile{},
		nextFH: 1,
	}
}

// Prepare creates the data root if needed and verifies it is a directory.
// It does not create a namespace.
func (t *Tree) Prepare() error {
	abs, err := filepath.Abs(t.root)
	if err != nil {
		return fmt.Errorf("data directory %s: %w", t.root, err)
	}
	t.root = abs
	info, err := os.Stat(abs)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("data directory %s is not a directory", abs)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return fmt.Errorf("data directory %s: %w", abs, err)
		}
	default:
		return fmt.Errorf("data directory %s: %w", abs, err)
	}
	return nil
}

// Lock takes an exclusive non-blocking lock on the data root so a second
// server cannot serve the same directory unless forced.
func (t *Tree) Lock() error {
	path := filepath.Join(t.root, ".fs-relay.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("data directory %s: %w", t.root, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("data directory %s: %w", t.root, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return fmt.Errorf("data directory %s: %s is not a regular file", t.root, path)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		b, _ := os.ReadFile(path)
		_ = f.Close()
		pid := strings.TrimSpace(string(b))
		if pid != "" {
			return fmt.Errorf("data directory %s is already served by pid %s", t.root, pid)
		}
		return fmt.Errorf("data directory %s is already served", t.root)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	t.fmu.Lock()
	old := t.lockFile
	t.lockFile = f
	t.fmu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// CheckNamespace stats data/<ns> every time. Connections use it so a
// namespace that was removed is refused.
// CheckNamespace stats data/<ns> and requires a directory. It runs on every
// call on purpose: a cached hit would let calls through after the directory
// is removed.
func (t *Tree) CheckNamespace(ns string) error {
	if err := wire.CheckNS(ns); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(t.root, ns))
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return syscall.ENOTDIR
	}
	return nil
}

// BlockSize is the data directory's preferred I/O size, read once at startup.
func (t *Tree) BlockSize() (int, error) {
	var st unix.Stat_t
	if err := unix.Stat(t.root, &st); err != nil {
		return 0, fmt.Errorf("stat %s: %w", t.root, err)
	}
	if st.Blksize <= 0 {
		return 0, fmt.Errorf("stat %s: block size", t.root)
	}
	return int(st.Blksize), nil
}

// Close closes every open file and releases the data directory lock.
func (t *Tree) Close() error {
	t.fmu.Lock()
	files := t.files
	t.files = map[uint64]*openFile{}
	lf := t.lockFile
	t.lockFile = nil
	t.fmu.Unlock()
	var err error
	for _, h := range files {
		err = errors.Join(err, h.f.Close())
	}
	if lf != nil {
		err = errors.Join(err, lf.Close())
	}
	return err
}

// Call runs one method. Filesystem failures are syscall.Errno, possibly wrapped.
func (t *Tree) Call(ctx context.Context, method string, p wire.Params) (wire.Result, error) {
	if err := ctx.Err(); err != nil {
		return wire.Result{}, err
	}
	if err := wire.CheckNS(p.Namespace); err != nil {
		return wire.Result{}, err
	}
	if err := t.CheckNamespace(p.Namespace); err != nil {
		return wire.Result{}, err
	}
	fn, ok := operations[method]
	if !ok {
		return wire.Result{}, wire.ErrUnknown
	}
	return fn(ctx, t, p)
}

func (t *Tree) withR(fn func() (wire.Result, error)) (wire.Result, error) {
	t.op.RLock()
	defer t.op.RUnlock()
	return fn()
}

func (t *Tree) withW(fn func() (wire.Result, error)) (wire.Result, error) {
	t.op.Lock()
	defer t.op.Unlock()
	return fn()
}

func newNames() *names {
	return &names{
		next:   1,
		byIno:  map[uint64]*entry{1: {rel: ""}},
		byPath: map[string]uint64{"": 1},
	}
}

func (t *Tree) state(ns string) *names {
	st := t.ns[ns]
	if st == nil {
		st = newNames()
		t.ns[ns] = st
	}
	return st
}

func (t *Tree) assign(ns, rel string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state(ns)
	if id, ok := st.byPath[rel]; ok {
		return id
	}
	st.next++
	st.byPath[rel] = st.next
	st.byIno[st.next] = &entry{rel: rel}
	return st.next
}

func (t *Tree) relOf(ns string, ino uint64) (string, error) {
	if ino == 0 {
		return "", wire.ErrParams
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.ns[ns]
	if st == nil {
		if ino == 1 {
			return "", nil
		}
		return "", syscall.ENOENT
	}
	rel, ok := st.byIno[ino]
	if !ok {
		return "", syscall.ENOENT
	}
	return rel.rel, nil
}

func (t *Tree) forget(ns, rel string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.ns[ns]
	if st == nil {
		return
	}
	st.drop(rel)
}

func (st *names) drop(rel string) {
	prefix := rel + "/"
	for path, id := range st.byPath {
		if path == rel || (rel != "" && len(path) > len(prefix)-1 && path[:len(rel)+1] == prefix) {
			delete(st.byPath, path)
			delete(st.byIno, id)
		}
	}
}

// shift keeps inode numbers when a path moves. The destination's old inode is dropped.
func (t *Tree) shift(ns, oldRel, newRel string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.ns[ns]
	if st == nil {
		return
	}
	if newRel != oldRel {
		st.drop(newRel)
	}
	type move struct {
		from, to string
		id       uint64
	}
	prefix := oldRel + "/"
	var moves []move
	for path, id := range st.byPath {
		if path == oldRel || len(path) >= len(prefix) && path[:len(prefix)] == prefix {
			moves = append(moves, move{path, newRel + path[len(oldRel):], id})
		}
	}
	for _, m := range moves {
		delete(st.byPath, m.from)
	}
	for _, m := range moves {
		st.byPath[m.to] = m.id
		if e := st.byIno[m.id]; e != nil {
			e.rel = m.to
			continue
		}
		st.byIno[m.id] = &entry{rel: m.to}
	}
}
