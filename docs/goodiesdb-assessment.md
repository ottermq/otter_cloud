# GoodiesDB Assessment: FreeBSD install and managed-service readiness

Status: assessment, 2026-10-20. Superseded in detail by the full production-readiness review filed upstream as [ottermq/goodiesdb#36](https://github.com/ottermq/goodiesdb/issues/36) (5 epics, issues #15–#30). This page keeps the Otter-specific view: the jail install and the M10 gate. Nothing was changed in the GoodiesDB repo or
in the `goodiesdb` jail.

Scope:

1. Does GoodiesDB install and run on FreeBSD, as deployed in a jail?
2. How far is it from being a dogfooded managed service (roadmap M10)?

Method:

- Inspected the jail read-only.
- Read the source: `github.com/ottermq/goodiesdb` at `main` (`9cb2aeb`)
  and the unmerged `fix/freebsd` branch (`51130b2`, 2026-07-02).
- Built and ran it on the reference node as an unprivileged user, in `/tmp`, bound to
  `127.0.0.1`, with a real RESP client exchange and crash tests. The
  temporary files were removed afterwards.

## 1. Verdict

| Question | Answer |
|---|---|
| Does it **build** on FreeBSD? | **Yes on `fix/freebsd`; no on `main` with the base `make`.** `main` uses GNU `$(shell …)`, which BSD make rejects. The branch fixes it and adds `make install`, but has not been merged since 2026-07-02 |
| Does it **run** on FreeBSD? | **Yes.** No platform-specific code. It serves RESP, persists to RDB and AOF, and shuts down cleanly on SIGTERM |
| Is the **jail install** working? | **No. GoodiesDB has never run in that jail.** See §2 |
| Ready as a **managed service**? | **No.** Two blockers found by testing: the password is not enforced, and acknowledged writes are lost on a crash (§3) |

## 2. Why the jail install does not work

Observed in a GoodiesDB jail (`/usr/local/jails/containers/goodiesdb`):

| # | Finding | Effect |
|---|---|---|
| J1 | **No binary.** `/usr/local/bin/` does not exist in the jail | Nothing to run |
| J2 | **Name mismatch.** The Makefile builds `goodiesdb-server`; the rc.d script runs `/usr/local/bin/goodiesdb` | Even after `make install`, rc.d would not find it |
| J3 | **rc.d script is mode 0644.** `service goodiesdb status` → "does not exist … or is not executable" | `rc` skips it at boot. `goodiesdb_enable=YES` is in `rc.conf`, but nothing happens |
| J4 | **No rc.d script in the repo.** It was hand-written from the owner's generic rc.d deployment note. OtterMQ ships `packaging/freebsd/rc.d` plus `make install` | Every install repeats the manual steps |
| J5 | **`DATA_DIR=data` is relative.** `daemon(8)` starts in `/`, so data would land in `/data`, not `/var/db/goodiesdb` (which does not exist either) | Data outside the expected place, and outside a dedicated dataset |
| J6 | **`HOST=localhost`.** In a non-VNET jail, `127.0.0.1` maps to the jail's own address (with `ip4=inherit`, any host address; with an alias, the LAN alias) | It would listen on the LAN, not on loopback |
| J7 | **Runs as root.** There is no `goodiesdb_user` and no `daemon -u` | Any RCE is root in the jail |
| J8 | **No restart on crash.** `daemon -f -P` without `-r` | A panic leaves it down until someone notices |
| J9 | **Dynamically linked** (default CGO on FreeBSD) | A binary built on the host depends on the jail's libc matching. `CGO_ENABLED=0` makes it portable across jails and templates |
| J10 | `godotenv.Load()` looks for `.env` in the working directory and logs "Error loading .env file" on every start | Harmless noise; the rc.d script already exports the env file |

### Fix list (all in the GoodiesDB repo)

1. Merge `fix/freebsd`. Build with `CGO_ENABLED=0`.
2. Pick one binary name (`goodiesdb-server` is fine) and use it in rc.d.
3. Ship `packaging/freebsd/rc.d/goodiesdb` with `make install`, mode 0755.
   Mirror OtterMQ's layout. Add:
   - `goodiesdb_user="goodiesdb"`;
   - `daemon -u ${goodiesdb_user} -r -P …`;
   - a `start_precmd` that creates `/var/db/goodiesdb` owned by that user.
4. Default `DATA_DIR` to an absolute path (`/var/db/goodiesdb`), and refuse
   a relative one when not running interactively.
5. Treat an empty or missing `.env` silently. Log only when `GOODIESDB_ENV`
   names a file that is missing.

After 1–5, the existing jail install is: `make install`, then `sysrc
goodiesdb_enable=YES`, then `service goodiesdb start`.

## 3. Correctness and security (not FreeBSD-specific)

| # | Finding | Evidence | Severity |
|---|---|---|---|
| G1 | **`PASSWORD` is not enforced.** Every command's `RequiresAuth()` returns `false` (18 are marked `// TODO: refactor authentication`) | Live: an unauthenticated connection ran `GET k` and got the value | **Blocker** for anything reachable from a network |
| G2 | **Acknowledged writes are lost on a crash.** Recovery loads `dump.rdb` and **skips the AOF whenever the RDB loads**, so writes since the last snapshot (up to 60 s) disappear even though they are in the AOF | Live: `SET k2` → `+OK`, AOF grew, `kill -9` after 1 s → after restart `GET k2` = nil | **Blocker** for a data store |
| G3 | RDB is written in place (`os.Create` on `dump.rdb`). A crash mid-write leaves a truncated snapshot, and, given G2, possibly no recoverable data | Code: `internal/persistence/rdb/rdb.go` | High |
| G4 | No fsync on the AOF or the RDB. `bufio.Flush` reaches only the page cache | Code: `aof.go` | Medium (document the guarantee, or add `appendfsync`-style options) |
| G5 | The AOF is never rewritten or truncated. It grows forever and is not used for recovery while an RDB exists | Code + G2 | Medium |
| G6 | Shutdown closes the AOF channel without waiting for the writer, and without closing client connections first. A late write could hit a closed channel (panic) or be lost | Code: `server.go` `Shutdown()` (inferred, not reproduced) | Medium |
| G7 | No memory limit or eviction (`maxmemory`) | Code | High for multi-tenant use: under rctl the process is killed rather than refusing writes |
| G8 | Password compared with `!=` (not constant time); the default password is `guest` | Code | Low once G1 is fixed |
| G9 | Inline commands (`PING\r\n`) are rejected byte by byte. `redis-cli` and client libraries use RESP arrays, so they are unaffected | Live | Low |
| G10 | An internal Go type leaks in an error: `-ERR expected array, got protocol.ErrorString` | Live | Low |

## 4. Managed-service readiness (M10)

The isolation model is right: a protocol-only service with no shell
(`open-questions.md` Q13). The gaps are inside GoodiesDB.

Must have, before customers:

- G1 fixed, with the password supplied per instance by the Otter worker
  (env file written via JailDeck exec, mode 0600, owned by the service user).
- G2 and G3 fixed:
  - recovery = RDB, then replay the AOF tail written after it;
  - atomic RDB writes (temp file, fsync, rename);
  - AOF rewrite after each snapshot.
- G7: `maxmemory` with a `noeviction` default, so a full instance returns
  errors instead of being killed by rctl.
- §2 items 1–4, so the template build is `make install` plus `sysrc`.

Nice to have: `INFO` fields for memory and persistence status (for the
health check and `/metrics`), `COMMAND`/`COMMAND COUNT` (some clients
probe it), and ACLs later.

**TLS:** not needed in GoodiesDB. Traefik terminates it as for PostgreSQL
(Q4), as long as clients use TLS with SNI (`rediss://`). That works for
most Redis clients, but it belongs in the M10 test matrix.
