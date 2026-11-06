package local

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fs-relay/internal/wire"
	"golang.org/x/sys/unix"
)

func newTree(t *testing.T) (*Tree, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := New(dir)
	t.Cleanup(func() { tr.Close() })
	return tr, dir
}

func call(t *testing.T, tr *Tree, method string, p wire.Params) wire.Result {
	t.Helper()
	p.Namespace = "demo"
	res, err := tr.Call(context.Background(), method, p)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return res
}

func callErr(t *testing.T, tr *Tree, method string, p wire.Params) error {
	t.Helper()
	if p.Namespace == "" {
		p.Namespace = "demo"
	}
	_, err := tr.Call(context.Background(), method, p)
	if err == nil {
		t.Fatalf("%s: want error", method)
	}
	return err
}

func errnoOf(t *testing.T, err error) syscall.Errno {
	t.Helper()
	var n syscall.Errno
	if !errors.As(err, &n) {
		t.Fatalf("not errno: %v", err)
	}
	return n
}

func TestReadsSkipsWhatItCannotTake(t *testing.T) {
	tr, _ := newTree(t)
	small := call(t, tr, wire.Create, wire.Params{Inode: 1, Name: "a", Mode: 0o644, Flags: syscall.O_RDWR})
	call(t, tr, wire.Write, wire.Params{Fh: small.Fh, Data: []byte("hi")})
	call(t, tr, wire.Release, wire.Params{Fh: small.Fh})
	big := call(t, tr, wire.Create, wire.Params{Inode: 1, Name: "b", Mode: 0o644, Flags: syscall.O_RDWR})
	call(t, tr, wire.Write, wire.Params{Fh: big.Fh, Data: make([]byte, wire.MaxPrefetch+1)})
	call(t, tr, wire.Release, wire.Params{Fh: big.Fh})
	got := call(t, tr, wire.Reads, wire.Params{Inodes: []uint64{small.Attr.Ino, big.Attr.Ino, 99}})
	if len(got.Blobs) != 1 || got.Blobs[0].Inode != small.Attr.Ino || string(got.Blobs[0].Data) != "hi" {
		t.Fatalf("blobs %+v", got.Blobs)
	}
	err := callErr(t, tr, wire.Reads, wire.Params{Inodes: make([]uint64, wire.MaxReads+1)})
	if !errors.Is(err, wire.ErrParams) {
		t.Fatalf("reads: %v", err)
	}
}

func TestAppendWritesAtSentOffset(t *testing.T) {
	tr, _ := newTree(t)
	file := call(t, tr, wire.Create, wire.Params{
		Inode: 1, Name: "a", Mode: 0o644, Flags: syscall.O_WRONLY | syscall.O_APPEND,
	})
	call(t, tr, wire.Write, wire.Params{Fh: file.Fh, Offset: 0, Data: []byte("hello")})
	call(t, tr, wire.Write, wire.Params{Fh: file.Fh, Offset: 5, Data: []byte("!")})
	call(t, tr, wire.Release, wire.Params{Fh: file.Fh})
	rd := call(t, tr, wire.Read, wire.Params{Inode: file.Attr.Ino, Size: 16})
	if string(rd.Data) != "hello!" {
		t.Fatalf("read %q", rd.Data)
	}
}

func TestSpliceRoundTrip(t *testing.T) {
	tr, _ := newTree(t)
	file := call(t, tr, wire.Create, wire.Params{Inode: 1, Name: "a", Mode: 0o644, Flags: syscall.O_RDWR})
	pipe, err := NewPipe(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pipe.Close() })
	body := []byte("hello!!")
	if _, err := unix.Write(pipe.WriteFd(), body); err != nil {
		t.Fatal(err)
	}
	n, err := tr.SpliceWrite(context.Background(), "demo", file.Fh, 3, pipe, len(body))
	if err != nil || n != len(body) {
		t.Fatalf("write %d %v", n, err)
	}
	m, err := tr.SpliceRead(context.Background(), "demo", file.Attr.Ino, 0, 0, 100, pipe)
	if err != nil || m != 3+len(body) {
		t.Fatalf("read %d %v", m, err)
	}
	got := make([]byte, m)
	if _, err := unix.Read(pipe.ReadFd(), got); err != nil {
		t.Fatal(err)
	}
	if string(got[3:]) != "hello!!" {
		t.Fatalf("body %q", got)
	}
	_, err = tr.SpliceWrite(context.Background(), "demo", 99, 0, pipe, 1)
	if errnoOf(t, err) != syscall.EBADF {
		t.Fatalf("fh %v", err)
	}
	if _, err := unix.Write(pipe.WriteFd(), []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := pipe.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(pipe.ReadFd(), true); err != nil {
		t.Fatal(err)
	}
	c, err := unix.Read(pipe.ReadFd(), make([]byte, 8))
	if c > 0 || !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("after reset %d %v", c, err)
	}
	if _, err := tr.BlockSize(); err != nil {
		t.Fatal(err)
	}
	if PipeCapacity(1<<20) < 64<<10 {
		t.Fatal("pipe capacity")
	}
}

func TestSpliceMovePipeCapacity(t *testing.T) {
	pipe, err := NewPipe(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pipe.Close() })
	if pipe.Cap() <= wire.RawSpare {
		t.Fatalf("pipe %d", pipe.Cap())
	}
	if err := moveN(t, pipe, pipe.Cap()-wire.RawSpare); err != nil {
		t.Fatalf("under capacity: %v", err)
	}
	if err := pipe.Reset(); err != nil {
		t.Fatal(err)
	}
	err = moveN(t, pipe, pipe.Cap())
	if err != nil && !strings.Contains(err.Error(), "stalled") {
		t.Fatal(err)
	}
}

func moveN(t *testing.T, pipe *Pipe, n int) error {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	go writeAll(fds[0], bytes.Repeat([]byte("z"), n))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch := make(chan error, 1)
	go func() {
		ch <- SpliceMove(ctx, fds[1], pipe.WriteFd(), n, fds[1], unix.POLLIN)
	}()
	select {
	case err := <-ch:
		return err
	case <-time.After(4 * time.Second):
		t.Fatal("splice hung")
	}
	return nil
}

func writeAll(fd int, body []byte) {
	off := 0
	for off < len(body) {
		n, err := unix.Write(fd, body[off:])
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			return
		}
		off += n
	}
}

func TestSpliceMoveWaits(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
	}
	pipe, err := NewPipe(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pipe.Close() })
	body := bytes.Repeat([]byte("q"), 200_000)
	done := make(chan struct{})
	go func() {
		defer close(done)
		off := 0
		for off < len(body) {
			n, err := unix.Write(fds[0], body[off:])
			if err != nil {
				if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
					continue
				}
				return
			}
			off += n
		}
		unix.Close(fds[0])
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := SpliceMove(ctx, fds[1], pipe.WriteFd(), len(body), fds[1], unix.POLLIN); err != nil {
		t.Fatal(err)
	}
	<-done
	unix.Close(fds[1])
	got := make([]byte, len(body))
	n := 0
	for n < len(got) {
		c, err := unix.Read(pipe.ReadFd(), got[n:])
		if err != nil {
			t.Fatal(err)
		}
		n += c
	}
	if !bytes.Equal(got, body) {
		t.Fatal("mismatch")
	}
}

func TestCopyFileRange(t *testing.T) {
	tr, _ := newTree(t)
	src := call(t, tr, wire.Create, wire.Params{Inode: 1, Name: "src", Mode: 0o644, Flags: syscall.O_RDWR})
	call(t, tr, wire.Write, wire.Params{Fh: src.Fh, Data: []byte("abcdef")})
	dst := call(t, tr, wire.Create, wire.Params{Inode: 1, Name: "dst", Mode: 0o644, Flags: syscall.O_RDWR})

	byFH := call(t, tr, wire.CopyFileRange, wire.Params{Fh: src.Fh, Offset: 1, Size: 3, FhOut: dst.Fh})
	if byFH.Written != 3 {
		t.Fatalf("by fh written %d", byFH.Written)
	}
	byIno := call(t, tr, wire.CopyFileRange, wire.Params{
		Inode: src.Attr.Ino, Size: 6, FhOut: dst.Fh, OffsetOut: 3,
	})
	if byIno.Written != 6 {
		t.Fatalf("by inode written %d", byIno.Written)
	}
	rd := call(t, tr, wire.Read, wire.Params{Fh: dst.Fh, Size: 16})
	if string(rd.Data) != "bcdabcdef" {
		t.Fatalf("dst %q", rd.Data)
	}

	err := callErr(t, tr, wire.CopyFileRange, wire.Params{Fh: src.Fh, Size: 1, FhOut: dst.Fh, Flags: 1})
	if errnoOf(t, err) != syscall.EINVAL {
		t.Fatalf("flags: %v", err)
	}
	err = callErr(t, tr, wire.CopyFileRange, wire.Params{Fh: src.Fh, Size: 1, FhOut: 99})
	if errnoOf(t, err) != syscall.EBADF {
		t.Fatalf("fh: %v", err)
	}
	err = callErr(t, tr, wire.CopyFileRange, wire.Params{Inode: src.Attr.Ino, Size: 1})
	if !errors.Is(err, wire.ErrParams) {
		t.Fatalf("missing fh_out: %v", err)
	}

	past := call(t, tr, wire.CopyFileRange, wire.Params{Inode: src.Attr.Ino, Offset: 4, Size: 100, FhOut: dst.Fh})
	if past.Written != 2 {
		t.Fatalf("short %d", past.Written)
	}
	got := tr.Changed(wire.CopyFileRange, wire.Params{Namespace: "demo", FhOut: dst.Fh})
	want := []wire.Change{{Parent: 1, Name: "dst"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("changed %+v", got)
	}
}

func TestReleaseWritesThenCloses(t *testing.T) {
	tr, _ := newTree(t)
	file := call(t, tr, wire.Create, wire.Params{Inode: 1, Name: "a", Mode: 0o644, Flags: syscall.O_RDWR})
	call(t, tr, wire.Release, wire.Params{Fh: file.Fh, Data: []byte("hi")})
	rd := call(t, tr, wire.Read, wire.Params{Inode: file.Attr.Ino, Size: 8})
	if string(rd.Data) != "hi" {
		t.Fatalf("read %q", rd.Data)
	}
	err := callErr(t, tr, wire.Release, wire.Params{Fh: file.Fh})
	if errnoOf(t, err) != syscall.EBADF {
		t.Fatalf("second release %v", err)
	}
}

func TestReaddirsBatchesChildDirectories(t *testing.T) {
	tr, _ := newTree(t)
	sub := call(t, tr, wire.Mkdir, wire.Params{Inode: 1, Name: "sub", Mode: 0o755})
	call(t, tr, wire.Create, wire.Params{Inode: sub.Attr.Ino, Name: "a", Mode: 0o644, Flags: syscall.O_RDWR})
	got := call(t, tr, wire.Readdirs, wire.Params{Inodes: []uint64{1, sub.Attr.Ino, 99}})
	if len(got.Listings) != 2 {
		t.Fatalf("listings %d", len(got.Listings))
	}
	if got.Listings[0].Inode != 1 || len(got.Listings[0].Entries) != 1 || got.Listings[0].Entries[0].Name != "sub" {
		t.Fatalf("root %+v", got.Listings[0])
	}
	if got.Listings[1].Entries[0].Name != "a" || got.Listings[1].Entries[0].Mode&syscall.S_IFMT != syscall.S_IFREG {
		t.Fatalf("child %+v", got.Listings[1].Entries)
	}
	err := callErr(t, tr, wire.Readdirs, wire.Params{Inodes: make([]uint64, wire.MaxReads+1)})
	if !errors.Is(err, wire.ErrParams) {
		t.Fatalf("bound %v", err)
	}
}

func TestReadByInode(t *testing.T) {
	tr, _ := newTree(t)
	file := call(t, tr, wire.Create, wire.Params{Inode: 1, Name: "a", Mode: 0o644, Flags: syscall.O_RDWR})
	call(t, tr, wire.Write, wire.Params{Fh: file.Fh, Data: []byte("hi")})
	call(t, tr, wire.Release, wire.Params{Fh: file.Fh})
	rd := call(t, tr, wire.Read, wire.Params{Inode: file.Attr.Ino, Size: 8})
	if string(rd.Data) != "hi" {
		t.Fatalf("read %q", rd.Data)
	}
}

func TestSession(t *testing.T) {
	tr, _ := newTree(t)
	sub := call(t, tr, wire.Mkdir, wire.Params{Inode: 1, Name: "sub", Mode: 0o755})
	file := call(t, tr, wire.Create, wire.Params{Inode: sub.Attr.Ino, Name: "hello", Mode: 0o644, Flags: syscall.O_RDWR})
	body := []byte("hi")
	wr := call(t, tr, wire.Write, wire.Params{Fh: file.Fh, Data: body})
	if wr.Written != uint32(len(body)) {
		t.Fatalf("written %d", wr.Written)
	}
	rd := call(t, tr, wire.Read, wire.Params{Fh: file.Fh, Size: 8})
	if string(rd.Data) != "hi" {
		t.Fatalf("read %q", rd.Data)
	}
	call(t, tr, wire.Flush, wire.Params{Fh: file.Fh})
	call(t, tr, wire.Fsync, wire.Params{Fh: file.Fh})
	dirIno := sub.Attr.Ino
	fileIno := file.Attr.Ino
	call(t, tr, wire.Rename, wire.Params{Inode: 1, Name: "sub", NewInode: 1, NewName: "dir"})
	got := call(t, tr, wire.Lookup, wire.Params{Inode: 1, Name: "dir"})
	if got.Attr.Ino != dirIno {
		t.Fatalf("dir ino %d want %d", got.Attr.Ino, dirIno)
	}
	child := call(t, tr, wire.Lookup, wire.Params{Inode: dirIno, Name: "hello"})
	if child.Attr.Ino != fileIno {
		t.Fatalf("file ino %d want %d", child.Attr.Ino, fileIno)
	}
	ents := call(t, tr, wire.Readdir, wire.Params{Inode: dirIno})
	if len(ents.Entries) != 1 || ents.Entries[0].Ino != fileIno || ents.Entries[0].Size != 2 {
		t.Fatalf("readdir %+v", ents.Entries)
	}
	raw, err := wire.MarshalResult(ents)
	if err != nil {
		t.Fatal(err)
	}
	back, err := wire.DecodeResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Entries[0].Ino != fileIno || back.Entries[0].Size != 2 {
		t.Fatalf("dirent wire %+v", back.Entries[0])
	}
	again := call(t, tr, wire.Readdir, wire.Params{Inode: dirIno})
	if again.Entries[0].Ino != fileIno {
		t.Fatalf("ino changed")
	}
	sz := call(t, tr, wire.Setattr, wire.Params{Inode: fileIno, Valid: wire.SetSize, Size: 1})
	if sz.Attr.Size != 1 {
		t.Fatalf("size %d", sz.Attr.Size)
	}
	link := call(t, tr, wire.Symlink, wire.Params{Inode: 1, Name: "link", Target: "dir/hello"})
	target := call(t, tr, wire.Readlink, wire.Params{Inode: link.Attr.Ino})
	if target.Target != "dir/hello" {
		t.Fatalf("target %q", target.Target)
	}
	dh := call(t, tr, wire.Opendir, wire.Params{Inode: 1})
	call(t, tr, wire.Releasedir, wire.Params{Fh: dh.Fh})
	call(t, tr, wire.Release, wire.Params{Fh: file.Fh})
	call(t, tr, wire.Unlink, wire.Params{Inode: dirIno, Name: "hello"})
	call(t, tr, wire.Rmdir, wire.Params{Inode: 1, Name: "dir"})
	st := call(t, tr, wire.Statfs, wire.Params{Inode: 1})
	if st.Stat == nil || st.Stat.Bsize == 0 {
		t.Fatalf("statfs %+v", st.Stat)
	}
	call(t, tr, wire.Access, wire.Params{Inode: 1, Mask: unix.R_OK})
}

func TestRejects(t *testing.T) {
	tr, _ := newTree(t)
	cases := []struct {
		name string
		p    wire.Params
		want syscall.Errno
		is   error
	}{
		{"dotdot", wire.Params{Namespace: "demo", Inode: 1, Name: ".."}, syscall.EINVAL, nil},
		{"slash", wire.Params{Namespace: "demo", Inode: 1, Name: "a/b"}, syscall.EINVAL, nil},
		{"missing", wire.Params{Namespace: "demo", Inode: 1, Name: "nope"}, syscall.ENOENT, nil},
		{"badns", wire.Params{Namespace: "../x", Inode: 1, Name: "a"}, 0, wire.ErrParams},
		{"noname", wire.Params{Namespace: "demo", Inode: 1}, 0, wire.ErrParams},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := callErr(t, tr, wire.Lookup, tc.p)
			if tc.is != nil {
				if !errors.Is(err, tc.is) {
					t.Fatalf("%v", err)
				}
				return
			}
			if got := errnoOf(t, err); got != tc.want {
				t.Fatalf("%v", got)
			}
		})
	}
}

func TestBadNamespaceWritesNothing(t *testing.T) {
	tr, dir := newTree(t)
	err := callErr(t, tr, wire.Lookup, wire.Params{Namespace: "../x", Inode: 1, Name: "a"})
	if !errors.Is(err, wire.ErrParams) {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "demo" {
		t.Fatalf("namespace escaped: %v", ents)
	}
}

func TestRenameRules(t *testing.T) {
	tr, _ := newTree(t)
	call(t, tr, wire.Mkdir, wire.Params{Inode: 1, Name: "d", Mode: 0o755})
	call(t, tr, wire.Create, wire.Params{Inode: 1, Name: "f", Mode: 0o644})
	call(t, tr, wire.Mkdir, wire.Params{Inode: 1, Name: "full", Mode: 0o755})
	full := call(t, tr, wire.Lookup, wire.Params{Inode: 1, Name: "full"})
	call(t, tr, wire.Create, wire.Params{Inode: full.Attr.Ino, Name: "c", Mode: 0o644})
	err := callErr(t, tr, wire.Rmdir, wire.Params{Inode: 1, Name: "full"})
	if errnoOf(t, err) != syscall.ENOTEMPTY {
		t.Fatal(err)
	}
	err = callErr(t, tr, wire.Rename, wire.Params{Inode: 1, Name: "f", NewInode: 1, NewName: "d"})
	if errnoOf(t, err) != syscall.EISDIR {
		t.Fatal(err)
	}
	err = callErr(t, tr, wire.Rename, wire.Params{Inode: 1, Name: "d", NewInode: 1, NewName: "f"})
	if errnoOf(t, err) != syscall.ENOTDIR {
		t.Fatal(err)
	}
	call(t, tr, wire.Create, wire.Params{Inode: 1, Name: "g", Mode: 0o644})
	err = callErr(t, tr, wire.Rename, wire.Params{
		Inode: 1, Name: "f", NewInode: 1, NewName: "g", Flags: uint32(unix.RENAME_NOREPLACE),
	})
	if errnoOf(t, err) != syscall.EEXIST {
		t.Fatal(err)
	}
	err = callErr(t, tr, wire.Rename, wire.Params{
		Inode: 1, Name: "f", NewInode: 1, NewName: "g", Flags: uint32(unix.RENAME_EXCHANGE),
	})
	if errnoOf(t, err) != syscall.EINVAL {
		t.Fatal(err)
	}
	call(t, tr, wire.Lookup, wire.Params{Inode: 1, Name: "f"})
	f := call(t, tr, wire.Open, wire.Params{Inode: call(t, tr, wire.Lookup, wire.Params{Inode: 1, Name: "f"}).Attr.Ino, Flags: syscall.O_RDONLY})
	call(t, tr, wire.Release, wire.Params{Fh: f.Fh})
	err = callErr(t, tr, wire.Read, wire.Params{Fh: f.Fh, Size: 1})
	if errnoOf(t, err) != syscall.EBADF {
		t.Fatal(err)
	}
}

func TestNamespacesDoNotShare(t *testing.T) {
	tr, dir := newTree(t)
	for _, ns := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(dir, ns), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a, err := tr.Call(context.Background(), wire.Create, wire.Params{Namespace: "a", Inode: 1, Name: "f", Mode: 0o644})
	if err != nil {
		t.Fatal(err)
	}
	b, err := tr.Call(context.Background(), wire.Create, wire.Params{Namespace: "b", Inode: 1, Name: "f", Mode: 0o644})
	if err != nil {
		t.Fatal(err)
	}
	if a.Attr.Ino != 2 || b.Attr.Ino != 2 {
		t.Fatalf("inos %d %d", a.Attr.Ino, b.Attr.Ino)
	}
	_, err = tr.Call(context.Background(), wire.Lookup, wire.Params{Namespace: "a", Inode: 1, Name: "onlyb"})
	if errnoOf(t, err) != syscall.ENOENT {
		t.Fatal(err)
	}
}

func TestPrepareCreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	tr := New(root)
	t.Cleanup(func() { _ = tr.Close() })
	if err := tr.Prepare(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("root %v %v", info, err)
	}
}

func TestPrepareRejectsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := New(file).Prepare(); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("file root: %v", err)
	}
}

func TestCheckNamespace(t *testing.T) {
	tr, dir := newTree(t)
	if err := tr.CheckNamespace("demo"); err != nil {
		t.Fatal(err)
	}
	if err := tr.CheckNamespace("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "later"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tr.CheckNamespace("later"); err != nil {
		t.Fatal(err)
	}
}

func TestCallChecksNamespaceEveryTime(t *testing.T) {
	tr, dir := newTree(t)
	if _, err := tr.Call(context.Background(), wire.Getattr, wire.Params{Namespace: "demo", Inode: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "demo")); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Call(context.Background(), wire.Getattr, wire.Params{Namespace: "demo", Inode: 1}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("call after delete: %v", err)
	}
}

func TestCallMissingNamespace(t *testing.T) {
	tr, _ := newTree(t)
	_, err := tr.Call(context.Background(), wire.Lookup, wire.Params{Namespace: "missing", Inode: 1})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("call: %v", err)
	}
}

func TestCheckNamespaceAfterDelete(t *testing.T) {
	tr, dir := newTree(t)
	if err := tr.CheckNamespace("demo"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "demo")); err != nil {
		t.Fatal(err)
	}
	if err := tr.CheckNamespace("demo"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted namespace: %v", err)
	}
}

func TestReleaseFailureDropsLocks(t *testing.T) {
	tr, _ := newTree(t)
	ino, fh := lockFile(t, tr, "a")
	call(t, tr, wire.Setlk, lp(ino, fh, "A", 1, wire.LockWrite, 100, 1))

	tr.fmu.Lock()
	f := tr.files[fh].f
	tr.fmu.Unlock()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := tr.Call(context.Background(), wire.Release, wire.Params{
		Namespace: "demo", Inode: ino, Fh: fh, Data: []byte("x"),
	})
	if err == nil {
		t.Fatal("release with a closed file succeeded")
	}
	call(t, tr, wire.Setlk, lp(ino, 0, "B", 2, wire.LockWrite, 100, 1))
}

// TestTreeLockRefusesSecondServerAndReleasesOnClose verifies that Lock refuses
// a second Tree on the same data directory and releases the flock on Close.
func TestTreeLockRefusesSecondServerAndReleasesOnClose(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	first := New(root)
	t.Cleanup(func() { _ = first.Close() })
	if err := first.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := first.Lock(); err != nil {
		t.Fatalf("first.Lock: %v", err)
	}

	second := New(root)
	t.Cleanup(func() { _ = second.Close() })
	if err := second.Prepare(); err != nil {
		t.Fatal(err)
	}
	err := second.Lock()
	if err == nil || !strings.Contains(err.Error(), "is already served by pid ") {
		t.Fatalf("second.Lock = %v, want already served by pid error", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first.Close: %v", err)
	}
	if err := second.Lock(); err != nil {
		t.Fatalf("second.Lock after first.Close: %v", err)
	}
}

// TestTreeLockRejectsSymlinkAndProtectsLockFile verifies that Lock refuses a
// symlinked .fs-relay.lock without truncating the target and that namespace
// operations through a parent symlink cannot unlink .fs-relay.lock.
func TestTreeLockRejectsSymlinkAndProtectsLockFile(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "data")
	tr := New(root)
	t.Cleanup(func() { _ = tr.Close() })
	if err := tr.Prepare(); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, ".fs-relay.lock")
	if err := os.Symlink(victim, lockPath); err != nil {
		t.Fatal(err)
	}
	if err := tr.Lock(); err == nil {
		t.Fatal("Lock succeeded on a symlinked .fs-relay.lock")
	}
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != "keep" {
		t.Fatalf("victim = %q, %v; want keep", got, err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tr.Lock(); err != nil {
		t.Fatalf("tr.Lock: %v", err)
	}
	up := call(t, tr, wire.Symlink, wire.Params{Inode: 1, Name: "up", Target: ".."})
	err = callErr(t, tr, wire.Unlink, wire.Params{Inode: up.Attr.Ino, Name: ".fs-relay.lock"})
	if errnoOf(t, err) != syscall.EINVAL {
		t.Fatalf("unlink via .. symlink = %v, want EINVAL", err)
	}
	if err := os.Symlink(".", filepath.Join(root, "symns")); err != nil {
		t.Fatal(err)
	}
	_, err = tr.Call(context.Background(), wire.Unlink, wire.Params{Namespace: "symns", Inode: 1, Name: ".fs-relay.lock"})
	if errnoOf(t, err) != syscall.ENOTDIR {
		t.Fatalf("unlink via symlinked namespace = %v, want ENOTDIR", err)
	}
	second := New(root)
	t.Cleanup(func() { _ = second.Close() })
	if err := second.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := second.Lock(); err == nil {
		t.Fatal("second.Lock succeeded after attempted symlink unlink")
	}
}
