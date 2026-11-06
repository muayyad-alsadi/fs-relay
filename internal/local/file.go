package local

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"fs-relay/internal/wire"
	"golang.org/x/sys/unix"
)

func create(ctx context.Context, t *Tree, p wire.Params) (wire.Result, error) {
	if err := ctx.Err(); err != nil {
		return wire.Result{}, err
	}
	if len(p.Data) > wire.MaxWrite {
		return wire.Result{}, wire.ErrParams
	}
	return t.withW(func() (wire.Result, error) {
		rel, abs, err := child(t, p.Namespace, p.Inode, p.Name)
		if err != nil {
			return wire.Result{}, wrap("create", err)
		}
		mode := os.FileMode(p.Mode & 0o777)
		if mode == 0 {
			mode = 0o644
		}
		f, err := os.OpenFile(abs, openFlags(p.Flags)|os.O_CREATE, mode)
		if err != nil {
			return wire.Result{}, wrap("create", err)
		}
		ino := t.assign(p.Namespace, rel)
		a, err := statAttr(abs, ino)
		if err != nil {
			f.Close()
			return wire.Result{}, wrap("create", err)
		}
		return wire.Result{Attr: &a, Fh: t.add(f, ino)}, nil
	})
}

func open(ctx context.Context, t *Tree, p wire.Params) (wire.Result, error) {
	if err := ctx.Err(); err != nil {
		return wire.Result{}, err
	}
	return t.withW(func() (wire.Result, error) {
		rel, err := t.relOf(p.Namespace, p.Inode)
		if err != nil {
			return wire.Result{}, wrap("open", err)
		}
		abs, err := t.abs(p.Namespace, rel)
		if err != nil {
			return wire.Result{}, wrap("open", err)
		}
		var st unix.Stat_t
		if err := unix.Lstat(abs, &st); err != nil {
			return wire.Result{}, wrap("open", err)
		}
		switch st.Mode & syscall.S_IFMT {
		case syscall.S_IFDIR:
			return wire.Result{}, wrap("open", syscall.EISDIR)
		case syscall.S_IFLNK:
			return wire.Result{}, wrap("open", syscall.EINVAL)
		}
		f, err := os.OpenFile(abs, openFlags(p.Flags), 0)
		if err != nil {
			return wire.Result{}, wrap("open", err)
		}
		return wire.Result{Fh: t.add(f, p.Inode)}, nil
	})
}

func openFlags(flags uint32) int {
	out := int(flags & syscall.O_ACCMODE)
	if flags&syscall.O_TRUNC != 0 {
		out |= os.O_TRUNC
	}
	if flags&syscall.O_EXCL != 0 {
		out |= os.O_EXCL
	}
	if flags&syscall.O_SYNC != 0 {
		out |= os.O_SYNC
	}
	return out
}

func read(ctx context.Context, t *Tree, p wire.Params) (wire.Result, error) {
	if err := ctx.Err(); err != nil {
		return wire.Result{}, err
	}
	if p.Size > wire.MaxWrite {
		return wire.Result{}, wire.ErrParams
	}
	f, done, err := t.source(p.Namespace, p.Inode, p.Fh)
	if err != nil {
		return wire.Result{}, wrap("read", err)
	}
	defer done()
	return readFile(f, p)
}

// source opens the file a read names: the handle when fh is set, else the inode read-only.
// done releases what source took. It is nil when err is set.
func (t *Tree) source(ns string, ino, fh uint64) (*os.File, func(), error) {
	if fh != 0 {
		h, err := t.use(fh)
		if err != nil {
			return nil, nil, err
		}
		return h.f, func() { t.done(h) }, nil
	}
	t.op.RLock()
	defer t.op.RUnlock()
	rel, err := t.relOf(ns, ino)
	if err != nil {
		return nil, nil, err
	}
	abs, err := t.abs(ns, rel)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { f.Close() }, nil
}

func reads(ctx context.Context, t *Tree, p wire.Params) (wire.Result, error) {
	if err := ctx.Err(); err != nil {
		return wire.Result{}, err
	}
	if len(p.Inodes) == 0 || len(p.Inodes) > wire.MaxReads {
		return wire.Result{}, wire.ErrParams
	}
	blobs := make([]wire.Blob, 0, len(p.Inodes))
	total := 0
	for _, ino := range p.Inodes {
		if err := ctx.Err(); err != nil {
			return wire.Result{}, err
		}
		b := readCapped(t, p.Namespace, ino)
		if b == nil {
			continue
		}
		if total+len(b) > wire.MaxWrite {
			break
		}
		total += len(b)
		blobs = append(blobs, wire.Blob{Inode: ino, Data: b})
	}
	return wire.Result{Blobs: blobs}, nil
}

func readCapped(t *Tree, ns string, ino uint64) []byte {
	f, size, ok := openCapped(t, ns, ino)
	if !ok {
		return nil
	}
	defer f.Close()
	buf := make([]byte, size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil
	}
	return buf
}

func openCapped(t *Tree, ns string, ino uint64) (*os.File, int64, bool) {
	t.op.RLock()
	defer t.op.RUnlock()
	rel, err := t.relOf(ns, ino)
	if err != nil {
		return nil, 0, false
	}
	abs, err := t.abs(ns, rel)
	if err != nil {
		return nil, 0, false
	}
	var st unix.Stat_t
	if err := unix.Lstat(abs, &st); err != nil {
		return nil, 0, false
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Size == 0 || st.Size > int64(wire.MaxPrefetch) {
		return nil, 0, false
	}
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, false
	}
	return f, st.Size, true
}

func readFile(f *os.File, p wire.Params) (wire.Result, error) {
	buf := make([]byte, p.Size)
	n, err := f.ReadAt(buf, p.Offset)
	if err != nil && err != io.EOF {
		return wire.Result{}, wrap("read", err)
	}
	return wire.Result{Data: buf[:n]}, nil
}

func copyRange(ctx context.Context, t *Tree, p wire.Params) (wire.Result, error) {
	if err := ctx.Err(); err != nil {
		return wire.Result{}, err
	}
	if p.Flags != 0 {
		return wire.Result{}, wrap("copyfilerange", syscall.EINVAL)
	}
	if p.FhOut == 0 {
		return wire.Result{}, wire.ErrParams
	}
	n := p.Size
	if n > wire.MaxCopy {
		n = wire.MaxCopy
	}
	out, err := t.use(p.FhOut)
	if err != nil {
		return wire.Result{}, wrap("copyfilerange", err)
	}
	defer t.done(out)
	in, done, err := t.source(p.Namespace, p.Inode, p.Fh)
	if err != nil {
		return wire.Result{}, wrap("copyfilerange", err)
	}
	defer done()
	copied, err := copyFDs(in, p.Offset, out.f, p.OffsetOut, int(n))
	if err != nil {
		return wire.Result{}, wrap("copyfilerange", err)
	}
	return wire.Result{Written: uint32(copied)}, nil
}

func copyFDs(in *os.File, offIn int64, out *os.File, offOut int64, n int) (int, error) {
	if n == 0 {
		return 0, nil
	}
	inSc, err := in.SyscallConn()
	if err != nil {
		return 0, err
	}
	outSc, err := out.SyscallConn()
	if err != nil {
		return 0, err
	}
	copied, opErr := 0, error(nil)
	ctrlErr := inSc.Control(func(rfd uintptr) {
		inner := outSc.Control(func(wfd uintptr) {
			copied, opErr = copyLoop(int(rfd), offIn, int(wfd), offOut, n)
		})
		if inner != nil {
			opErr = inner
		}
	})
	if ctrlErr != nil {
		return copied, ctrlErr
	}
	return copied, opErr
}

func copyLoop(rfd int, offIn int64, wfd int, offOut int64, n int) (int, error) {
	copied := 0
	for copied < n {
		roff := offIn + int64(copied)
		woff := offOut + int64(copied)
		c, err := unix.CopyFileRange(rfd, &roff, wfd, &woff, n-copied, 0)
		copied += c
		if err == nil {
			if c == 0 {
				return copied, nil
			}
			continue
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return copied, err
	}
	return copied, nil
}

// SpliceRead moves up to n bytes from the named file into pipe. A short count is EOF.
func (t *Tree) SpliceRead(ctx context.Context, ns string, ino, fh uint64, off int64, n int, pipe *Pipe) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f, done, err := t.source(ns, ino, fh)
	if err != nil {
		return 0, wrap("read", err)
	}
	defer done()
	return spliceFile(f, &off, pipe.WriteFd(), n)
}

// SpliceWrite moves n bytes from pipe into fh at off. A filesystem that refuses splice is copied.
func (t *Tree) SpliceWrite(ctx context.Context, ns string, fh uint64, off int64, pipe *Pipe, n int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	h, err := t.use(fh)
	if err != nil {
		return 0, wrap("write", err)
	}
	defer t.done(h)
	moved, err := spliceInto(pipe.ReadFd(), h.f, &off, n)
	if err == nil || moved > 0 || !errors.Is(err, syscall.EINVAL) {
		return moved, err
	}
	return copyFromPipe(pipe, h.f, off, n)
}

func spliceFile(f *os.File, off *int64, wfd, n int) (int, error) {
	sc, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var moved int
	var opErr error
	ctrlErr := sc.Control(func(fd uintptr) {
		moved, opErr = spliceExact(int(fd), off, wfd, nil, n, unix.SPLICE_F_MOVE)
	})
	if ctrlErr != nil {
		return moved, ctrlErr
	}
	return moved, opErr
}

func spliceInto(rfd int, f *os.File, off *int64, n int) (int, error) {
	sc, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var moved int
	var opErr error
	ctrlErr := sc.Control(func(fd uintptr) {
		moved, opErr = spliceExact(rfd, nil, int(fd), off, n, unix.SPLICE_F_MOVE)
	})
	if ctrlErr != nil {
		return moved, ctrlErr
	}
	return moved, opErr
}

func spliceExact(rfd int, roff *int64, wfd int, woff *int64, n, flags int) (int, error) {
	got := 0
	for got < n {
		c, err := unix.Splice(rfd, roff, wfd, woff, n-got, flags)
		if c > 0 {
			got += int(c)
		}
		if err == nil {
			if c == 0 {
				return got, nil
			}
			continue
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return got, err
	}
	return got, nil
}

func copyFromPipe(pipe *Pipe, f *os.File, off int64, n int) (int, error) {
	buf := make([]byte, n)
	got := 0
	for got < n {
		c, err := unix.Read(pipe.ReadFd(), buf[got:])
		if c > 0 {
			got += c
		}
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return got, err
		}
		if c == 0 {
			break
		}
	}
	if _, err := f.WriteAt(buf[:got], off); err != nil {
		return 0, err
	}
	return got, nil
}

func write(ctx context.Context, t *Tree, p wire.Params) (wire.Result, error) {
	if err := ctx.Err(); err != nil {
		return wire.Result{}, err
	}
	n, err := writeAt(t, p, "write")
	if err != nil {
		return wire.Result{}, err
	}
	return wire.Result{Written: uint32(n)}, nil
}

func flush(ctx context.Context, t *Tree, p wire.Params) (wire.Result, error) {
	if err := ctx.Err(); err != nil {
		return wire.Result{}, err
	}
	h, err := t.use(p.Fh)
	if err != nil {
		return wire.Result{}, wrap("flush", err)
	}
	t.done(h)
	return wire.Result{}, nil
}

func fsync(ctx context.Context, t *Tree, p wire.Params) (wire.Result, error) {
	return syncFH(ctx, t, p, "fsync")
}

func syncFH(ctx context.Context, t *Tree, p wire.Params, op string) (wire.Result, error) {
	if err := ctx.Err(); err != nil {
		return wire.Result{}, err
	}
	h, err := t.use(p.Fh)
	if err != nil {
		return wire.Result{}, wrap(op, err)
	}
	defer t.done(h)
	if err := h.f.Sync(); err != nil {
		return wire.Result{}, wrap(op, err)
	}
	return wire.Result{}, nil
}

func release(ctx context.Context, t *Tree, p wire.Params) (wire.Result, error) {
	if err := ctx.Err(); err != nil {
		return wire.Result{}, err
	}
	if p.Fh == 0 {
		if p.Owner != 0 {
			t.DropOwner(p.Namespace, p.Inode, p.Client, p.Owner)
		}
		return wire.Result{}, nil
	}
	ino := p.Inode
	if ino == 0 {
		ino, _ = t.inoOf(p.Fh)
	}
	dropLocks := func() { t.DropFH(p.Namespace, ino, p.Fh) }
	if len(p.Data) > 0 {
		n, err := writeAt(t, p, "release")
		if err != nil || n != len(p.Data) {
			_ = t.dropFH(p.Fh)
			dropLocks()
			if err != nil {
				return wire.Result{}, err
			}
			return wire.Result{}, wrap("release", io.ErrShortWrite)
		}
	}
	if err := t.dropFH(p.Fh); err != nil {
		dropLocks()
		return wire.Result{}, wrap("release", err)
	}
	dropLocks()
	return wire.Result{}, nil
}

func writeAt(t *Tree, p wire.Params, op string) (int, error) {
	if len(p.Data) > wire.MaxWrite {
		return 0, wire.ErrParams
	}
	h, err := t.use(p.Fh)
	if err != nil {
		return 0, wrap(op, err)
	}
	n, err := h.f.WriteAt(p.Data, p.Offset)
	t.done(h)
	if err != nil {
		return n, wrap(op, err)
	}
	return n, nil
}

func (t *Tree) add(f *os.File, ino uint64) uint64 {
	t.fmu.Lock()
	defer t.fmu.Unlock()
	t.nextFH++
	id := t.nextFH
	t.files[id] = &openFile{f: f, ino: ino}
	return id
}

func (t *Tree) use(fh uint64) (*openFile, error) {
	if fh == 0 {
		return nil, wire.ErrParams
	}
	t.fmu.Lock()
	defer t.fmu.Unlock()
	h, ok := t.files[fh]
	if !ok {
		return nil, syscall.EBADF
	}
	h.refs++
	return h, nil
}

func (t *Tree) done(h *openFile) {
	t.fmu.Lock()
	h.refs--
	closeNow := h.drop && h.refs == 0
	t.fmu.Unlock()
	if closeNow {
		h.f.Close()
	}
}

func (t *Tree) dropFH(fh uint64) error {
	if fh == 0 {
		return wire.ErrParams
	}
	t.fmu.Lock()
	h, ok := t.files[fh]
	if !ok {
		t.fmu.Unlock()
		return syscall.EBADF
	}
	delete(t.files, fh)
	h.drop = true
	closeNow := h.refs == 0
	t.fmu.Unlock()
	if closeNow {
		return h.f.Close()
	}
	return nil
}
