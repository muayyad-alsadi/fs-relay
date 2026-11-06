# Roadmap

## Done

- a web server that servers the shared directories over a custom wire protocol on top of HTTP
- a rootless remote client that connects to the server and mounts a share into a directory using Linux Fuse v3
- token based security
- Linux Kernel specific optimizations: zero-copy routines reducing context switching, memory allocation and fragmentation, ..etc.
- specialized end point for large files
- get sqlite working; `fs-relay-bind` drains write-behind before unlocking and drops cached attributes and small-file bytes on lock grant.
- Benchmark serving millions of rows to DuckDB.
- CLI ergonomics: `-version` prints the build, `-h` prints purpose-driven help, and flags after the mountpoint get a hint.
- Startup validation. The server creates and logs the data root, locks it against a second `fs-relay` process unless `-force-multi-serve` is passed, refuses a path that is not a directory, and never creates a namespace; a missing namespace is 404. The mount checks its mountpoint before connecting, and a non-empty directory needs `-force-existing-mnt`.
- CLI ergonomics: `-version` prints the build, `-h` prints purpose-driven help, and flags after the mountpoint get a hint.
- Lock release hardening: the wake-up is a coalescing broadcast, a failed close still drops its locks, an owner tracks up to two handles with an overflow so any recorded close drops it, and `setlkw` records its waiter under the lock mutex on `EAGAIN` and wakes readers on write-to-read downgrades while keeping waiters active across multi-waiter retries.
- Advisory record locks. `getlk`, `setlk` and `setlkw` reach the server, which owns one table per namespace, so two mounts of one name exclude each other. A holder is the mount's client id plus the kernel owner, because a classic POSIX lock is per process and two mounts never share one. The table keeps one record per locked inode: counts for the pending byte, the reserved byte and the 510-byte shared range, and an overflow list for any other range. A release is one event, sent only when a mount is waiting; a commit nobody waits on sends nothing. `flock` shares the table as a whole-file entry. A mount that does not rejoin loses its locks when the 8s hold expires. `SPEC.md` has the contract. Verified 2026-11-03 on Linux 6.12.113, SQLite 3.46.1, rollback journal: two mounts, four writers split across them committed 200 rows with four distinct markers, and `PRAGMA integrity_check` was `ok` from both mounts. A reader on one mount held a transaction for 2s and a writer on the other waited 1.4s, then committed, with no `database is locked`. A new reader was held at the pending byte while a writer waited, then granted after it finished. Killing a mount that held a write lock released it after 8s. `flock` conflicted and released across mounts. A read-only open takes no server handle; its close drops only the owners that locked through it, so a `cat` on one mount cannot drop a worker's locks on the same mount.

## Yet to be done

The calls a single program uses are in: create, open, read, write, close, fsync, mkdir, rmdir, unlink, rename, symlink, getattr, setattr, readdir, statfs, access, and the advisory record locks. What remains below is either outside a one-token mount, or a refusal POSIX already allows. Each one says what it would take, what refusing costs, and whether a program handles the error.

### Outside this system

Both ends run as one non-root user. A client that presents the token acts as the server's user. The client's uids and the server's uids are numbers from two machines, and they do not name the same people. `-allow_other` lets another local user see the mount. That user still presents the same token, so the server still sees its own user. There is no second principal to grant a right to.

ACLs. An ACL names users and groups. This server has one user, the one it runs as, so an entry for any other uid would point at someone who never connects. Base POSIX does not require ACLs. `getfacl` and `setfacl` see `EOPNOTSUPP` and stop. Programs that open files follow the mode bits, which are the server user's bits. They do not mis-read a file because an ACL was absent.

Extended attributes. These are the named blobs beside a file. `security.selinux` and `security.capability` are the SELinux and capability labels, and setting them takes a privilege neither end has. `system.posix_acl_*` is the ACL above. `trusted.*` takes `CAP_SYS_ADMIN`. The class a normal user can write is `user.*`, which some backup tools copy when asked (`tar --xattrs`, `rsync -X`). Base POSIX does not include any of them. The kernel turns the first `ENOSYS` into `EOPNOTSUPP` and stops asking. Default tar, rsync, SQLite and DuckDB never look. A tool that was asked to copy attributes reports the error on that file and still copies the bytes.

Device nodes. FUSE has `mknod`, and the kernel accepts it. A character or block device needs `CAP_MKNOD` on the client and again on the server. Both ends are non-root, so the call is `EPERM` on a local disk too. Returning `EPERM` is the POSIX result. `mknod` of a device fails in the caller's hands, and nothing else changes.

`access` today runs `faccessat` as the server user. That matches the token: the client is that user. It is not a stand-in for the client's local uid.

### Possible later, and safe to refuse

POSIX allows a filesystem to refuse these. The caller gets an error on that call. The rest of the file still reads and writes.

Hard links. FUSE implements `link`, and a non-root process can create one. The rule is the same as a local disk: both names in one filesystem. Here that filesystem is one namespace, the tree under `data/<namespace>/`. A link that would leave the namespace is `EXDEV`. The name map is one path per inode today (`byIno` holds one string). A link is a second path on the same inode, with `nlink` above 1, and unlinking one path leaves the inode while a path remains. The host call is `link` inside that directory. Leaving it unimplemented is conforming: `link(2)` returns `EPERM` when the filesystem has no hard links. SQLite and DuckDB do not make hard links. `tar` and `rsync` of a tree that contains some do not invent a shared inode. They print the error for that member. `ansible.tar` has none.

Fifos. FUSE `mknod` of `S_IFIFO`, and a socket, need no capability. A non-root server can `mkfifo` inside the namespace. Opening the fifo blocks until the other end opens, and that wait has to sit off the FUSE handler or the mount stalls. A program that needs a named pipe sees `ENOSYS` today and reports it. Pipes between programs on one client use the kernel pipe, which never comes to the server.

`flock`. A whole-file advisory lock on one open. Linux SQLite uses the byte-range locks above, not `flock`. The same table can hold it as one whole-file entry. It conflicts with other `flock` locks. It does not conflict with a byte-range lock. Today the kernel keeps `flock` inside one mount. A second mount does not see it. Programs that use `flock` across two mounts can both hold it, the same hole as record locks, and the fix is the same table.

`fallocate`. Reserving a range is an optimization. POSIX allows `EOPNOTSUPP`, which is what the caller sees now. SQLite writes the bytes instead and continues.

`RENAME_EXCHANGE` and `RENAME_WHITEOUT`. These are Linux flags, not POSIX. `EINVAL` is the right refusal. `RENAME_NOREPLACE` already works. `mv` of a file does not need the exchange flag.

Directory `fsync`. Committing a rename can be done by the server, on the parent directory, inside `rename`, `create` and `unlink`. The client then has nothing to wait for. A client `fsync` of a directory is `ENOSYS` today. Tools that check the error treat `EOPNOTSUPP` as "this filesystem cannot", and the server-side sync is what actually makes the name durable.

Shared maps. `MAP_SHARED` on one mount is that mount's page cache. Another mount does not see those bytes through its own mapping. WAL needs the `-shm` file mapped that way by every process, so WAL fails, and that stays out. A program that only reads and writes, without mapping, is unaffected. An eviction still drops cached bytes when it arrives. A missed one stays stale until the 1s attribute timeout.

## macOS

Nothing in this tree runs on macOS yet. The mount and the server stop in different places.

Last verified 2026-11-03, from source, on a Linux host. go-fuse is the pin in `go.mod`, v2.11.0. macFUSE notes are 5.1.0 (2025-10-30) and 5.5.0 (2026-10-17). `GOOS=darwin GOARCH=arm64 go build` was run for both commands. No Mac was available, so nothing was mounted.

| Bridge | macOS | What the kernel sees |
| --- | --- | --- |
| macFUSE kernel extension, the default | The 5.1.0 package lists macOS 12 through 26 | Its own `/dev/fuse`, ABI 7.8 through 7.19. Apple silicon loads the extension with Reduced Security. `FUSE_FSYNC` is on its feature list. |
| macFUSE FSKit, `-o backend=fskit`, local volume | 15.4 and later. 5.1.0 calls the `local` option experimental | FSKit. Slower than the extension, no FUSE notification API, most kernel mount options absent. |
| macFUSE FSKit, non-local volume | 26 and later. This relay is non-local | Same FSKit limits. No per-file `fsync` callback. On macOS 27 a flush error cannot come back from `close`. |
| FUSE-T, NFS or SMB | Its current builds; no kernel extension | A local NFS or SMB mount. The NFS backend ignores the daemon's attribute timeouts, and the kernel can take locks without a FUSE call. |
| FUSE-T, FSKit | 26 and later | FSKit, through FUSE-T. |
| FSKit, called directly | The API is 15.4 and later; a non-local volume is 26 and later | A Swift or Objective-C file-system extension. A second frontend beside go-fuse. |

libfuse3 on the macFUSE FSKit backend arrived in 5.1.0. 5.5.0 tracks libfuse 3.18.3. Through mid-2025 that backend had no libfuse3.

### macOS: Mount (`fs-relay-bind`)

`fs-relay-bind` is a go-fuse node filesystem. The calls this wire makes are the low-level shape, a parent inode and a basename. `lookup` is an inode and a name. macFUSE's libfuse 2 high-level API is the other shape, a full path, with the library holding the inode map. That path API would fight the protocol, and it is optional. The low-level API is inode and name, and macFUSE has shipped it beside the path API for years. libfuse3 on macFUSE is that same shape. Inode and basename are not what keeps the mount off macOS.

go-fuse v2.11.0 still mounts on the macFUSE kernel extension without cgo. `fuse/mount_darwin.go` execs `mount_macfuse`, or `mount_osxfuse` if that is what is installed, and takes the device fd back over a socketpair. `fuse/request_darwin.go` negotiates minor 19 (`_OUR_MINOR_VERSION`), where the Linux file negotiates 28. `fuse/splice_darwin.go` turns splice off. A pure-Go mount on the kext, at that lower ABI, is the first thing to try. It was not run here.

This tree does not reach that mount. `cmd/fs-relay-bind` imports `github.com/hanwen/go-fuse/v2/splice`, and that package is `//go:build linux`. The Darwin build stops there, before go-fuse's own mount code is linked. `ReadResultPipe` and the pipe resize live in the same Linux-only splice path. `copy_file_range` is protocol minor 28, past the minor this go-fuse advertises on Darwin and past macFUSE's published list, which stops at `FUSE_FALLOCATE`. On that ABI a copy arrives as a read and a write. Birth time, BSD flags, resource forks, and Finder's extended attributes sit outside this protocol. Extended attributes are already out of scope.

Linking libfuse 3 or FUSE-T is the path that needs cgo. cgo drops a pure-Go static build and an easy cross-compile, and it forces macOS CI runners. That cost belongs to leaving go-fuse, not to the first mount on the kext.

Finder and Spotlight will write `.DS_Store` and `._*` AppleDouble files and will index the volume. macFUSE options to weigh are `noappledouble`, `noapplexattr`, `nobrowse`, and `volname`. A `.metadata_never_index` file keeps Spotlight off the tree. go-fuse's Darwin mount already passes `iosize` from `MaxWrite`.

Durability follows the bridge. On the kernel extension, `FUSE_FSYNC` is a supported operation, so an application `fsync` can become the server's `fsync` and reach disk. FSKit has no per-file `fsync` callback, so an `fsync` on an FSKit mount can return without the server having synced. On macOS 27 a flush failure is logged and does not come back from `close`. A macOS FSKit mount is weaker on durability than a Linux mount.

Hosted macOS CI cannot load a kernel extension, and it cannot be relied on to mount an FSKit volume. Mount tests are manual, or they run on a self-hosted Mac. Server tests need no mount. They can run in CI once the Darwin build below is green.

Bundling is a license choice, made before either bridge is shipped. macFUSE's license (read 2026-11-03) allows source and binary redistribution with its notices, and forbids bundling the binary with commercial software, including an automated download or install, without prior written permission. The kernel extension has been closed source since 3.9 (May 2019). FUSE-T's binary license (read the same day) is free for non-commercial use. Commercial use or bundling needs a commercial license from its authors. The libfuse it carries is LGPL. The open choice is whether the user installs the bridge or this project does.

### macOS: Zero-copy on the mount

A large read on Linux is spliced from the raw socket into a pipe, and go-fuse splices that pipe into `/dev/fuse` (`ReadResultPipe`). On Darwin, go-fuse's splice is compiled out, and macOS has no `splice(2)`. The macFUSE device takes a buffer. libfuse's own splice path looks for `FUSE_CAP_SPLICE_WRITE` and for `/proc/sys/fs/pipe-max-size`, then copies.

The read that remains is one copy, from the socket into the buffer handed back with `ReadResultData`. A pooled buffer keeps Go from copying again before the kernel copies. A large write is already one `writev` of the 16-byte header and the bytes FUSE placed in a buffer. FUSE-T copies once more, through NFS or SMB.

`iosize`, which the Darwin mount helper already sets from `MaxWrite`, is the size the kernel asks for in one read. macOS readahead is weaker than Linux, so the mount's own readahead and the existing small-file prefetch carry more of the sequential read than they do on Linux. `-raw` stays off while the splice import does not build.

### macOS: Server (`fs-relay`)

`fs-relay` is a user-space process on a local directory. It mounts nothing, so it needs none of macFUSE, FUSE-T, or FSKit.

`GOOS=darwin GOARCH=arm64 go build ./cmd/fs-relay` fails in `internal/local`. The compiler stops after ten errors. The ones it printed, and the Linux-only symbols past that cutoff:

- `dir.go` copies `os.FileInfo.Sys()` as `*syscall.Stat_t` and reads `Atim`, `Mtim`, `Ctim`. On Darwin those Go fields are `Atimespec`, `Mtimespec`, `Ctimespec`, and `Birthtimespec`. `attr.go` reads the same `Atim` names on `unix.Stat_t`, which uses the same Darwin names. The C struct spells them `st_atimespec` and so on. `Birthtimespec` has no field on this wire.
- `unix.Statfs_t` has no `Namelen` or `Frsize` (`attr.go` `statfs`).
- `unix.UTIME_OMIT` and `unix.UTIME_NOW` are undefined (`attr.go`).
- `unix.Renameat2` and `unix.RENAME_NOREPLACE` are undefined (`dir.go`). `renameatx_np(2)` is the rename that takes flags. `RENAME_EXCL` and `RENAME_SWAP` cover `RENAME_NOREPLACE` and `RENAME_EXCHANGE`.
- `setattr` on an open handle builds a `/proc/self/fd/N` path (`attr.go`). That path is Linux.
- `unix.CopyFileRange`, `unix.Splice`, and `F_SETPIPE_SZ` exist in `golang.org/x/sys` v0.49.0 for Linux and are absent from its Darwin files. The compiler had already stopped before them.

A first server maps the two rename flags, copies a byte range with `read` and `write`, serves `/raw` as a buffered copy, and keeps HTTP, the websocket, and FlatBuffers as they are.

APFS is case-insensitive by default, and it normalizes names. A Linux client assumes case-sensitive bytes. `A` and `a` collide, a case-only rename does not behave as it does on ext4, and two names that differ by normalization can be one directory entry. Query the volume with `pathconf(_PC_CASE_SENSITIVE)` or `getattrlist`, and advertise the result on the handshake, so a mount can show what it was given.

`fsync` in `internal/local` is `os.File.Sync`. On Darwin that is `fcntl(F_FULLFSYNC)` (`internal/poll/fd_fsync_darwin.go` in the Go tree), which pushes to the platter. It falls back to `fsync` when the volume returns `ENOTSUP`, as some SMB mounts do. A raw `unix.Fsync` does not. `F_FULLFSYNC` is slow, so the server stays on `Sync` and does not grow a second call. The mount side is the other half: an FSKit mount may never deliver that `fsync`, so the durable call on the server only runs when the bridge actually sends `FUSE_FSYNC`.

An audit of this tree found no `O_DIRECT`, `O_PATH`, `O_TMPFILE`, `statx`, `fallocate`, `posix_fadvise`, or inotify. A later change that wants one of those maps it first: `O_DIRECT` is `fcntl(F_NOCACHE)`, `fallocate` is `F_PREALLOCATE` plus `ftruncate`, and directory watches are FSEvents or kqueue. `O_PATH`, `O_TMPFILE`, and `statx` have no Darwin twin of the same shape.

macOS TCC blocks a process from `~/Documents`, `~/Desktop`, and external volumes unless it has Full Disk Access. A server run as a daemon needs that permission, or it fails those trees with a permission error that looks like a relay bug.

### macOS: Zero-copy on the server

`/raw` is two splices through a pipe grown with `F_SETPIPE_SZ`. A read is file to pipe to socket. A write is socket to pipe to file. `copyfilerange` is one `copy_file_range(2)` between two open files, at the two offsets.

Darwin has no `splice(2)` and no `F_SETPIPE_SZ`. A pipe stays at the kernel's fixed capacity, and it cannot join a file to a socket. The closest call is BSD `sendfile(2)`:

```c
int sendfile(int fd, int s, off_t offset, off_t *len,
             struct sf_hdtr *hdtr, int flags);
```

`fd` is a regular file and `s` is a stream socket. The kernel sends the file's pages to the socket. `sf_hdtr` attaches a header and a trailer in the same call, which is where the 16-byte raw reply header goes, and those few bytes are copied from user space. That is the read half. Nothing sends a socket into a file. An upload is `read` and `write`.

If the header is a separate write ahead of `sendfile`, Darwin's coalesce flag is `TCP_NOPUSH` on the socket. This tree never sets `TCP_CORK`. Set `TCP_NOPUSH` before that header write and clear it after `sendfile`, or the header goes out as its own small segment.

`copy_file_range(2)` has no range-shaped counterpart. `clonefile(2)` and `fclonefileat(2)`, since 10.12, make an APFS clone of a whole file at a new path on the same volume. The destination must not exist, and a cross-volume clone returns `EXDEV`. `copyfilerange` writes a byte count into an existing handle at `offset_out`. `copyfile(3)` with `COPYFILE_CLONE` is that same whole-file clone, and otherwise copies in user space. A Darwin server copies the range.

## Tried and dismissed

MessagePack was the wire, and it was replaced by FlatBuffers. A JSON body that carried file bytes as multipart was implemented on a side branch and left out. The README records both trials. The rest of this list was not built.

- JSON as the call body, including `multipart/form-data` with an `application/json` part and an `application/octet-stream` part. It can carry file bytes. It stayed out so the server has one encoding.
- A different HTTP status per errno: the errno belongs in `error.code`; statuses stay 200, 400, 500.
- Host `st_ino` as the protocol inode: the namespace directory is not inode 1, and a copy would change it.
- A durable inode map: a restart is a remount.
- A path-only protocol: the wire mirrors FUSE inodes so relaying stays thin.
- JSON-RPC ids on the POST body: the HTTP exchange already correlates. The websocket carries its own id because many calls share the socket.
- MessagePack as the wire. FlatBuffers is faster on the same mounts and is the only encoding now.
- A line-oriented join stream. The join response is length-prefixed FlatBuffers frames. A short read ends the stream, and the mount rejoins.
- kvfs shard levels: there is one directory tree.
- Blocking `setlkw` inside one plain request: a dropped connection could not rejoin the wait.
- WAL for M3: it needs shared memory between processes, which the mount does not provide.
- A generic `batch` method, including one with `$id.field` references. The mount already knows the inodes a listing will read, and one batch response would wait on its slowest call. `reads` and `readdirs` are the narrow forms that match a listing.
- Hardlinks, xattrs, ACLs, devices, FUSE 2, Windows, accounts, TLS.
- `tee`: nothing fans file bytes out to more than one reader.
- `vmsplice`: the Go garbage collector may reuse gifted pages, and the FUSE device copies them anyway.
- `MSG_ZEROCOPY`: go-fuse reuses its request buffer when the handler returns.
- Splice of a websocket write: the client must mask every frame it sends, and a splice cannot XOR.
- Splice out of a chunked HTTP body: the chunk parser has already copied the bytes into a buffer.
