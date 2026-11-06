# fs-relay

Share a directory over HTTP and mount it to as many machines as you like, treating it like a local filesystem on remote machines.

- It's ~10x faster than the alternatives.
- It's written in `golang`, has no runtime dependencies except fuse v3 (fusermount3).
- Runs as HTTP service on any port.
- No special privileges. It needs no root, and it skips the exposure of root-run NFS or SMB and of firewall rules that are hard to lock down.

Can be used for:

- shared folders (eg. heavily used by content creators and media producers as an alternative to SMB/Samba)
- shared code space by coding agents
- decoupling **Data engine** from **SQL execution engine**, as in our [DuckDB Parquet benchmark](Benchmark.md)


## What you get

- A normal directory: create, read, write, rename, symlink, and delete.
- One connection for the mount, which is the faster way to run it. `-post` sends each operation as its own request instead.
- Several hosts can mount the same name at once and see the same files.
- SQLite works with its default rollback journal. `PRAGMA journal_mode=WAL` fails, because WAL needs a shared-memory file this filesystem cannot provide.
- Writes reach other mounts when the file is closed or synced.

## How it compares

On each filesystem we unpacked an archive, packed the tree again, copied the archive onto the mount, and listed that copy with `tar -tf`. A local directory is the floor. [kvfs](https://github.com/muayyad-alsadi/kvfs) is our ancestor in-memory filesystem (backed by Redis/Valkey). [s3fs](https://github.com/s3fs-fuse/s3fs-fuse) is mounted on [rustfs](https://github.com/rustfs/rustfs). [fuse-nfs](https://github.com/sahlberg/fuse-nfs) is NFS through FUSE.

`ansible.tar` is 27 MiB and 5,868 files.

| | local directory | kvfs | fs-relay | fs-relay `-post` | s3fs on rustfs | fuse-nfs |
|---|---:|---:|---:|---:|---:|---:|
| unpack | 0.14 s | 25.76 s | 15.47 s | 18.11 s | 203.56 s | 107.89 s |
| pack | 0.10 s | 12.58 s | 3.76 s | 4.18 s | 57.53 s | 39.68 s |
| copy the archive | 0.02 s | 0.48 s | 0.12 s | 0.11 s | 0.38 s | 0.45 s |
| list the archive | 0.03 s | 0.82 s | 0.16 s | 0.19 s | 0.16 s | 0.48 s |

The default mount unpacks about thirteen times faster than s3fs (15.47 s against 203.56 s) and about seven times faster than fuse-nfs (15.47 s against 107.89 s). Packing is about fifteen times faster than s3fs (3.76 s against 57.53 s) and about ten and a half times faster than fuse-nfs (3.76 s against 39.68 s).

`Antigravity.tar.gz` is mostly a few large files, about 500 MiB unpacked.

| | local directory | kvfs | fs-relay | fs-relay `-post` | s3fs on rustfs | fuse-nfs |
|---|---:|---:|---:|---:|---:|---:|
| unpack | 5.60 s | 30.71 s | 9.78 s | 9.36 s | 21.01 s | 36.13 s |
| pack | 0.49 s | 19.77 s | 2.38 s | 2.40 s | 5.31 s | 10.37 s |
| copy the archive | 0.13 s | 3.12 s | 0.65 s | 0.79 s | 1.14 s | 2.65 s |
| list the archive | 5.62 s | 7.21 s | 6.13 s | 6.12 s | 6.74 s | 10.30 s |

The tables above have fs-relay ahead of rustfs through s3fs, and both of those mounts use FUSE. A separate DuckDB run reads the same files once through the fs-relay mount and once straight from rustfs over S3, with no FUSE on the S3 path. The mount is still substantially faster, so the lead is not an artifact of FUSE overhead on s3fs. The method and the results are in [Benchmark.md](./Benchmark.md).

On this archive the default mount unpacks ahead of s3fs (9.78 s against 21.01 s) and ahead of fuse-nfs (9.78 s against 36.13 s). Packing is 2.38 s on the default mount, 5.31 s on s3fs, and 10.37 s on fuse-nfs.

## Building

You can build it using

```bash
go build -o fs-relay ./cmd/fs-relay
go build -o fs-relay-bind ./cmd/fs-relay-bind
```

Even better, we have a [justfile](https://just.systems/man/en/) to help you do **production build** like this

```bash
just build
```

You can place the binaries in `~/.local/bin/` or `/usr/local/bin`.
The binary `fs-relay` goes on the storage machine, and `fs-relay-bind` to as many client machines as you want.
There is no cross dependency between the two and there is no problem if you put both on every machine.

## Run

For simplicity, we are running the binaries relative to their build directory.

```bash
mkdir -p data/demo /mnt/fs-relay
./fs-relay -listen 127.0.0.1:8080 -data data -token local-demo
./fs-relay-bind -url http://127.0.0.1:8080 -ns demo -token local-demo /mnt/fs-relay
```

`-version` prints the build version and exits, and `-h` prints the usage. `-listen` defaults to `127.0.0.1:8080` and `-data` to `data`. Each name under `data` is one independent tree. `-url` defaults to `http://127.0.0.1:8080` and `-ns` to `default`. A bare `host:port` is accepted and gets the `http` scheme, as it does in `curl`. Leave `-token` off the server and it prints a secret on its error stream; copy that value onto every mount. A secret you pass yourself must be at least 8 characters, using letters, digits, hyphens, and underscores, and capitals count as different characters. The mount sends it with each connection. A missing or wrong secret is refused, and the mount stops instead of trying again. The server records the machine and the reason. A restarted server that was not given `-token` prints a new secret. The welcome page does not use the secret, and the server never sends it back.

Generate the token once and keep it in a file only you can read:

```bash
install -d -m 700 ~/.config/fs-relay
(umask 077; openssl rand -hex 16 > ~/.config/fs-relay/token)
./fs-relay -token-file ~/.config/fs-relay/token
./fs-relay-bind -token-file ~/.config/fs-relay/token /mnt/fs-relay
```

`-token-file` reads a regular file, and the process stops if another user can
read it, so keep the file at mode 600. `-token` still works, but the value
sits in the process list (`ps aux`), which every local user can read.

The server creates the `-data` directory if it is missing. It takes an advisory
lock on the data root, so a second `fs-relay` exits with an error that can name
the holding PID instead of serving the same tree unless `-force-multi-serve` is
passed. Programs that access the data directory directly without honoring the
lock are not blocked. The server never creates `data/<namespace>`. Create each
namespace directory before a mount joins it; a missing namespace is refused with
HTTP 404. The mount checks its
mountpoint before connecting, refuses a missing or non-directory path, and
refuses a non-empty directory unless `-force-existing-mnt` is passed.

Add `-post` to send each operation as its own request. `-raw=false` sends large reads and writes on the main connection. `-allow-other` lets other users on the mount host use the mount. `-debug` logs FUSE traffic.

Open the server URL in a browser for a welcome page. `curl` against the same URL returns a short status. Unmount with `fusermount3 -u /mnt/fs-relay`.

On a new mount, create a directory, write and read a small file, make a symlink, rename, and delete. Mount the same name a second time, write on one, and read on the other. Copy a file from one path on the mount to another. Append with `>>`. Write about 10 MiB with `dd bs=10k` and read it back. Repeat that write with `-raw=false` and compare the bytes.

Two mounts of one name exclude each other for advisory locks. `flock -n` on the second mount fails while the first holds the file, and an `fcntl` write lock on one mount makes the other's write lock fail with `EAGAIN` until it is released. A SQLite database opened through two mounts, in rollback journal mode, commits from both sides, and `PRAGMA integrity_check` returns `ok` on each. A lock is advisory: a process that opens the files under `data/` directly is outside the table.

## How such performance is even possible?

We used zero-copy Linux kernel features similar to [sendfile(2)](https://linux.die.net/man/2/sendfile) as in [my article](https://medium.com/@alsadi/secret-sauce-is-zero-copy-single-context-switching-b749bd614dca). Those features are:

- [splice(2)](https://linux.die.net/man/2/splice)
- [copy_file_range(2)](https://linux.die.net/man/2/copy_file_range)

Where these calls are used, the bytes skip the allocate, read, copy, write, and free cycle.
On a large file they also skip a context switch for every block.

## Earlier designs

This section is for people changing the server. Do not bring these back. They were measured, and they stayed out.

MessagePack was the first wire format. FlatBuffers replaced it. On the Ansible tree, `-ws` with FlatBuffers was 6% to 15% faster than `-ws` with MessagePack: about 15% unpacking and about 6% packing. On the Antigravity archive it was 11% faster at both.

JSON can carry file bytes as `multipart/form-data`: one `application/json` part for the call, and one `application/octet-stream` part for the bytes. On the Ansible tree that plain-text body kept up with MessagePack and FlatBuffers. It is not in this server. One body shape is the smaller program.

The protocol is in [`SPEC.md`](./SPEC.md). What was tried and set aside is also in [`ROADMAP.md`](./ROADMAP.md).

We have implemented a filesystem on top of Redis/Valkey and name it [KVFS](https://github.com/muayyad-alsadi/kvfs).
KVFS is in-memory and that's a positive and a negative at the same time (faster because it's in-memory but it limits how large the files can be). Also KVFS can't use the special kernel features we talked about.

## License

This project is dual-licensed under either:

- [MIT License](LICENSE-MIT.txt)
- [Waqf Digital Public License v1.0](LICENSE-WaqfDPL-1.0.md)

See [LICENSE.md](LICENSE.md) for details.

