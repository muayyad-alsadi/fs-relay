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

| Concern | NFS and SMB | s3fs | fs-relay |
|---|---|---|---|
| Privileges | Root daemons and kernel modules | Unprivileged FUSE mount | Unprivileged FUSE mount (`fusermount3`) |
| Network | Dedicated RPC/SMB ports and firewall rules | HTTP to an object store (one request per object call) | Plain HTTP and WebSocket (`-post` fallback if a proxy blocks WS) |
| Multi-host locks | Server locks (often brittle across WANs) | No POSIX byte-range locks | Server-enforced POSIX byte-range and `flock` locks when one `fs-relay` serves the data directory (safe for SQLite rollback journals; `-force-multi-serve` does not share a lock table across servers) |

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

Download the prebuilt release archive from [GitHub Releases](https://github.com/muayyad-alsadi/fs-relay/releases), or build from a local checkout:

```bash
go build -o fs-relay ./cmd/fs-relay
go build -o fs-relay-bind ./cmd/fs-relay-bind
```

Even better, we have a [justfile](https://just.systems/man/en/) to help you do **production build** like this

```bash
just build
```

You can place the binaries in `~/.local/bin/` or `/usr/local/bin` (and ensure that directory is on your `PATH` if you invoke `fs-relay` or `fs-relay-bind` without `./`).
The binary `fs-relay` goes on the storage machine, and `fs-relay-bind` to as many client machines as you want.
There is no cross dependency between the two and there is no problem if you put both on every machine.

## Run

For simplicity, we are running the binaries relative to their build directory.

```bash
mkdir -p data/demo /mnt/fs-relay
./fs-relay -listen 127.0.0.1:8080 -data data -token local-demo
./fs-relay-bind -url http://127.0.0.1:8080 -ns demo -token local-demo /mnt/fs-relay
```

Generate the token once and keep it in a file only you can read:

```bash
install -d -m 700 ~/.config/fs-relay
(umask 077; openssl rand -hex 16 > ~/.config/fs-relay/token)
./fs-relay -token-file ~/.config/fs-relay/token
./fs-relay-bind -token-file ~/.config/fs-relay/token /mnt/fs-relay
```

### Flags and subcommands

| Binary | Flag / Subcommand | Default | Purpose |
|---|---|---|---|
| `fs-relay` | `-listen` | `127.0.0.1:8080` | Address to listen on |
| `fs-relay` | `-data` | `data` | Directory holding one subfolder per namespace |
| `fs-relay` | `-token-file` | | Read shared secret from a mode-0600 file (recommended) |
| `fs-relay` | `-token` | random on stderr | Pass secret on command line (visible in `ps aux`) |
| `fs-relay` | `-force-multi-serve` | `false` | Serve a `-data` directory already locked by another `fs-relay` |
| `fs-relay-bind` | `-url` | `http://127.0.0.1:8080` | Server URL (`http://` is added when the scheme is omitted) |
| `fs-relay-bind` | `-ns` | `default` | Namespace directory under `-data` on the server |
| `fs-relay-bind` | `-token-file` | | Read shared secret from a mode-0600 file |
| `fs-relay-bind` | `-token` | | Pass shared secret on command line |
| `fs-relay-bind` | `-post` | `false` | Send each operation as an HTTP POST instead of a WebSocket |
| `fs-relay-bind` | `-raw` | `true` | Stream large reads and writes over the raw splice socket |
| `fs-relay-bind` | `-allow-other` | `false` | Allow other local users on the client host to read the mount |
| `fs-relay-bind` | `-force-existing-mnt` | `false` | Mount over a non-empty mountpoint directory |
| `fs-relay-bind` | `-debug` | `false` | Log FUSE traffic |
| Both | `update` | | Self-update the binary (and its companion binary if present in the same directory) from the latest GitHub release |
| Both | `version` / `-version` | | Print build version and exit |
| Both | `help` / `-h` | | Print usage and exit |

`-token-file` reads a regular file, and the process stops if another user can
read it, so keep the file at mode 600. `-token` still works, but the value
sits in the process list (`ps aux`), which every local user can read. Leave
both off the server and it prints a random secret on its error stream; copy
that value onto every mount. A secret you pass yourself must be at least 8
characters, using letters, digits, hyphens, and underscores, and capitals count
as different characters. A missing or wrong secret is refused, and the mount
stops instead of trying again. The welcome page does not use the secret, and
the server never sends it back.

The server creates the `-data` directory if it is missing. It takes an advisory
lock on the data root, so a second `fs-relay` exits with an error that can name
the holding PID instead of serving the same tree unless `-force-multi-serve` is
passed. Programs that access the data directory directly without honoring the
lock are not blocked. The server never creates `data/<namespace>`. Create each
namespace directory before a mount joins it; a missing namespace is refused with
HTTP 404. The mount checks its
mountpoint before connecting, refuses a missing or non-directory path, and
refuses a non-empty directory unless `-force-existing-mnt` is passed.

Open the server URL in a browser for a welcome page. `curl` against the same
URL returns a short status. Unmount with `fusermount3 -u /mnt/fs-relay`.
Interrupting `fs-relay-bind` unmounts cleanly. If files remain open, it
detaches lazily with `fusermount3 -uz` and exits with an error so the terminal
does not hang.

### Updating in place

Both binaries can update themselves (and their companion binary if it is installed beside the executable in the same directory) from the latest GitHub release:

```bash
./fs-relay update
```

`update` verifies the release archive against its SHA-256 digest, preserves existing file permissions, stages the updated binaries in temporary files in the target directory, and rolls back if either binary fails to swap. Run `update` separately on any other hosts or directories. On startup, release builds of `fs-relay` check once every 12 hours in the background whether a newer release is out and print a notice on stderr (`fs-relay-bind` and development builds skip the check). Set `FS_RELAY_NO_UPDATE_CHECK=1` to disable the startup check.

### Smoke check

On a new mount, confirm `mount` shows the intended directory and not an empty path. Create a directory, write and read a small file, make a symlink, rename, and delete. Mount the same name a second time, write on one, and read on the other. Copy a file from one path on the mount to another. Append with `>>`. Write about 10 MiB with `dd bs=10k` and read it back. Repeat that write with `-raw=false` and compare the bytes.

Two mounts of one name exclude each other for advisory locks. `flock -n` on the second mount fails while the first holds the file, and an `fcntl` write lock on one mount makes the other's write lock fail with `EAGAIN` until it is released. A SQLite database opened through two mounts, in rollback journal mode, commits from both sides, and `PRAGMA integrity_check` returns `ok` on each:

```bash
sqlite3 /mnt/fs-relay/app.db "PRAGMA journal_mode=DELETE; CREATE TABLE IF NOT EXISTS runs (id INTEGER PRIMARY KEY, host TEXT); INSERT INTO runs (host) VALUES (hex(randomblob(4))); PRAGMA integrity_check;"
```

A lock is advisory: a process that opens the files under `data/` directly is outside the table.

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

## Contributors

<a href="https://github.com/muayyad-alsadi/fs-relay/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=muayyad-alsadi/fs-relay" alt="Contributors to muayyad-alsadi/fs-relay" />
</a>

## Star History

<a href="https://www.star-history.com/#muayyad-alsadi/fs-relay&Date">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=muayyad-alsadi/fs-relay&type=Date&theme=dark" />
    <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=muayyad-alsadi/fs-relay&type=Date" />
    <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=muayyad-alsadi/fs-relay&type=Date" />
  </picture>
</a>
