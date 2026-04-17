Here's the rewritten plan:

---

# Plan: fos-next — FOG Operating System, rewritten

## TL;DR

Build a minimal, modern Linux environment for network imaging that replaces the legacy FOS shell-script stack with a single static Go binary (`fos-agent`). Built with Buildroot, targeting x86-64 only. Communicates exclusively with fog-next's REST API using a short-lived **boot token** issued at handshake time. Drops legacy tools (partimage, chntpw, cabextract, photorec, testdisk), keeps partclone as the imaging engine. All image data is transferred over HTTP — NFS is not used.

**New repo**: `fos-next/` (separate from fog-next)
**fog-next changes required**: new unauthenticated `/fog/api/v1/boot/` endpoints + streaming image download endpoint

---

## Phase 1 — Repository & Scaffolding

1. Create `fos-next/` repo with documented structure
2. Set up `agent/` Go module (`fos-agent` binary)
3. Set up `buildroot/` directory with board files, kernel configs, rootfs overlay
4. Write `build.sh` (thin wrapper around Buildroot make)
5. Write GitHub Actions CI for x64 builds

**Relevant files**:
- `fos/build.sh` — reference for build flow
- `fos/configs/kernelx64.config` — kernel config base
- `fos/configs/fsx64.config` — Buildroot defconfig base

**Considerations**:
- Dropping ARM64 eliminates the matrix build and cross-compilation toolchain setup. The CI becomes a single job and build times drop significantly.
- The repo should be kept separate from fog-next to allow independent release cadences. The agent and the server can be versioned independently as long as the boot API contract is stable.
- A `COMPAT.md` file should document the minimum fog-next version required for a given fos-next release.

---

## Phase 2 — fog-next Boot API (server-side prerequisite)

New unauthenticated endpoints under `/fog/api/v1/boot/` — the handshake issues a **boot token** (short-lived signed JWT or opaque token) that authenticates all subsequent requests for that boot session.

- `POST /fog/api/v1/boot/handshake` — machine presents its MACs; server responds with `{boot_token, task_id, action, image_id, storage_node, ...}` or `{action: "register"}` for unknown hosts. The boot token is scoped to the task and expires after a configurable TTL (default: 2 hours).
- `POST /fog/api/v1/boot/register` — submit inventory (CPU, RAM, disk, MACs, UUID) from an unknown host; creates a pending MAC or auto-registers. Does **not** issue a boot token (no task is associated yet).
- `POST /fog/api/v1/boot/progress` — report task progress `{task_id, percent, bpm, bytes_transferred}`. Authenticated by `Authorization: Bearer <boot_token>`.
- `POST /fog/api/v1/boot/complete` — mark task done or failed. Authenticated by boot token.
- `GET /fog/api/v1/boot/images/{image_id}/download?part={n}` — stream a partition image file to the client. Authenticated by boot token. Supports `Range` headers for resumable transfers.

**Relevant files**:
- `fog-next/internal/api/server.go` — add new route group
- `fog-next/internal/api/handlers/` — new `boot_api.go` handler
- `fog-next/internal/store/` — reuse existing task/host/image stores

**Considerations**:
- The boot token must be treated as a secret on the client side. `fos-agent` should store it only in memory — never write it to disk or pass it through kernel args.
- Token TTL should be long enough to cover a full imaging session on slow links (large image + slow NIC), but short enough to limit exposure if a machine is left booted into FOS. Two hours is a reasonable default; it should be server-configurable.
- The `register` endpoint intentionally issues no token because registration is a background task — no session follows immediately. A subsequent PXE boot after approval will go through handshake and receive a token at that point.
- The image download endpoint should stream directly from the storage node or through fog-next acting as a proxy. Direct-from-storage is more efficient but requires the storage node to also validate the boot token. Proxying through fog-next is simpler to implement first and can be optimised later.
- `Range` header support on the download endpoint is essential — a dropped connection mid-image should be resumable without starting over.

---

## Phase 3 — fos-agent Core

`fos-agent` is a single static Go binary built with `CGO_ENABLED=0`. Responsibilities:

1. **cmdline parser** — reads `/proc/cmdline`, extracts `fog_server`, `fog_action`, `fog_host`
2. **network bringup** — enumerate NICs, run udhcpc, wait for connectivity, detect primary MAC
3. **API client** — thin HTTP client for the `/fog/api/v1/boot/` endpoints; holds boot token in memory after handshake
4. **action dispatcher** — calls correct handler based on the action returned by handshake
5. **progress reporter** — goroutine that POSTs progress on a ticker

**Key packages**:
- `internal/cmdline` — parse `/proc/cmdline`
- `internal/netup` — NIC enumeration + DHCP wait
- `internal/api` — boot API client (carries boot token, handles retries)
- `internal/imaging` — partclone subprocess wrapper with progress parsing; streams data from HTTP
- `internal/inventory` — read `/proc/cpuinfo`, `/proc/meminfo`, `/sys/class/net/`, sysfs (no dmidecode)
- `internal/partition` — parted/sgdisk wrappers

**Considerations**:
- The API client package should centralise all HTTP logic including token attachment, retry with backoff, and timeout handling. No other package should make raw HTTP calls.
- `fos-agent` is PID 1 in the initramfs. It must reap zombie processes (use `cmd.Wait()` correctly) and handle `SIGCHLD` if spawning multiple subprocesses in parallel.
- Logging should go to the kernel ring buffer via `/dev/kmsg` so it's visible over serial and in `dmesg`. A structured log format (key=value pairs) is preferred over free-form text.
- Binary size matters. Use `ldflags="-s -w"` and strip the binary. Target is ~12 MB stripped. Avoid importing packages that pull in `net/http`'s TLS stack unnecessarily — the boot API can be HTTP-only on the LAN, with TLS an opt-in for environments that require it.

---

## Phase 4 — Actions

Each action is a self-contained function; the dispatcher calls them after a successful handshake.

### register
1. Collect inventory via sysfs/procfs (no dmidecode)
2. Collect all MACs from `/sys/class/net/`
3. POST to `/fog/api/v1/boot/register` (unauthenticated — no token yet)
4. Log result; poweroff or reboot

**Considerations**: Registration happens before any task exists, so there is no boot token. The server should respond with a human-readable status that fos-agent can write to the console, so a technician watching the screen knows whether the machine was accepted or queued for approval.

---

### deploy
1. `handshake` → receive boot token + task details (image ID, part count, fog-next server address)
2. Restore partition table (MBR or GPT) by downloading the partition table blob via `GET .../download?part=ptable`
3. For each partition: `GET .../download?part={n}` → pipe response body to `partclone.restore` stdin
4. Expand last partition to fill disk
5. POST complete

**Considerations**:
- Piping the HTTP response body directly to `partclone.restore` avoids writing the image to disk first, which means the client needs no temporary storage proportional to image size. This is the primary advantage of HTTP over NFS for this use case.
- The download loop must honour `Range` headers to support resuming a partition mid-stream if the TCP connection drops. Track the byte offset of the last successful `partclone` write and resume from there.
- Partition count and sizes should be part of the handshake response so progress can be reported accurately as a percentage of total bytes, not just per-partition progress.
- If the server sends a `Content-Length`, use it to pre-validate that the image data matches expectations before committing to disk.

---

### capture
1. Shrink last partition (resize2fs or ntfsresize as appropriate)
2. Save MBR or GPT partition table: `PUT .../upload?part=ptable`
3. For each partition: run `partclone.clone` with stdout piped to a chunked `PUT .../upload?part={n}`
4. POST complete

**Considerations**:
- Uploading from partclone stdout to an HTTP PUT stream means fog-next must handle chunked transfer encoding or a streaming body on the upload endpoint. Verify this works with the Go HTTP server's default behavior before designing the client side.
- If a capture upload fails mid-partition, the partial image must be discarded server-side. The server should not retain incomplete partitions. Consider a two-phase commit: upload to a staging path, then move to final path on POST complete.
- Filesystem shrink before capture can fail on a dirty filesystem. fos-agent should check filesystem state before attempting to shrink and surface a clear error if the fs needs a check first.

---

### multicast
1. Handshake → get udpcast port + session ID
2. Run `udp-receiver`, pipe to `partclone.restore`
3. POST complete

**Considerations**: Multicast is inherently incompatible with the "everything over HTTP" principle for image data — `udp-receiver` is the exception. The boot token still applies to the handshake and progress/complete calls. Consider whether multicast is worth the complexity for v1; it could be deferred to a later phase without affecting any other action.

---

### debug
1. Print a banner with the boot token redacted
2. Drop to `/bin/sh` (BusyBox ash)

**Considerations**: The boot token must never appear in the debug banner or shell prompt. Zero it out in memory before exec-ing the shell if possible. Dropbear SSH access in debug mode should reuse the same mechanism — no token leakage in logs or environment variables.

---

### wipe
1. Run `shred` or `dd if=/dev/urandom` over target disk
2. POST complete

**Considerations**: Wipe can take a very long time on large disks. The progress reporter goroutine is essential here — the server should not time out the boot token during a legitimate multi-hour wipe. Either the token TTL should reset on progress POSTs, or the wipe action should receive an extended-TTL token at handshake time.

---

## Phase 5 — Buildroot Integration

Kernel config (base from fos, stripped down):
- Remove: staging drivers, obscure filesystems, debug options, ARM-specific drivers
- Keep: virtio (for VM testing), common NIC drivers, USB storage, SCSI, NVMe, USB HID (keyboard for debug)
- Add: Realtek r8125/r8126/r8168 patches

Rootfs packages:

| Package | Keep | Notes |
|---|---|---|
| busybox | ✅ | Shell + core utils |
| partclone | ✅ | Primary imaging engine |
| parted | ✅ | Partition management |
| sgdisk | ✅ | GPT support |
| ntfs-3g | ✅ | NTFS mounts |
| e2fsprogs | ✅ | ext2/3/4 tools |
| xfsprogs | ✅ | XFS tools |
| fos-agent | ✅ | New — replaces all shell scripts |
| dropbear | ✅ | Lightweight SSH for debug access |
| nfs-utils | ❌ | No longer needed — HTTP replaces NFS |
| partimage | ❌ | Superseded by partclone |
| chntpw | ❌ | Drop |
| cabextract | ❌ | Drop |
| photorec | ❌ | Drop |
| testdisk | ❌ | Drop |
| MBR templates | ❌ | Drop |

Rootfs overlay:
```
rootfs_overlay/
  etc/
    inittab              — BusyBox init
    init.d/
      S20network         — ip link up loopback only (agent does the rest)
      S99fos             — exec /sbin/fos-agent
  sbin/
    fos-agent            — copied from agent build output
```

**Considerations**:
- Dropping nfs-utils removes a non-trivial set of dependencies (rpcbind, libtirpc, etc.) and simplifies the rootfs considerably. Verify the size impact — it should meaningfully reduce the initramfs size.
- Dropping ARM64 means only one kernel config and one Buildroot defconfig to maintain. The kernel build is the slowest part of CI; a single-arch build roughly halves CI time.
- The rootfs should be reproducible: pin all Buildroot package versions and the Buildroot release itself. Floating `master` in CI causes silent breakage when upstream packages change.

---

## Phase 6 — Build Pipeline

- `build.sh` — wraps Buildroot; flags: `-k` kernel only, `-f` fs only
- GitHub Actions: single x64 build job; publishes `bzImage` and `init.xz` as release assets
- Makefile target in fog-next: `fetch-kernels` downloads from fos-next releases

**Considerations**:
- Cache the Buildroot `dl/` directory in CI to avoid re-downloading package tarballs on every run. Use a cache key based on the Buildroot version + defconfig hash.
- Release assets should include a `sha256sums` file so fog-next's `fetch-kernels` target can verify integrity before deploying.
- Consider a `latest` release alias in GitHub so fog-next can always fetch the current kernel without hardcoding a tag — but version-pin in production deployments.

---

## Size targets

| Artifact | Target |
|---|---|
| fos-agent (stripped static Go binary) | ~12 MB |
| init.xz (x64, compressed) | < 55 MB |
| bzImage (x64) | ~12 MB |

*(ARM64 targets removed. init.xz target reduced slightly due to nfs-utils removal.)*

---

## Decisions

- **Drop x86 (32-bit)**: no modern hardware needs it
- **Drop ARM64**: simplifies build pipeline substantially; can be re-added later if demand emerges
- **HTTP for all image transfers**: NFS removed entirely. Piping HTTP response bodies to partclone avoids temporary disk storage, enables resumable transfers via `Range` headers, and removes the nfs-utils dependency from the rootfs
- **Boot token**: issued at handshake, held in memory by fos-agent, attached to all subsequent requests as `Authorization: Bearer`. Scoped to the task, expires after a configurable TTL. Never persisted to disk or exposed to userspace outside the agent process
- **No dmidecode**: read inventory from sysfs/procfs only
- **Excluded scope**: Windows password reset, data recovery tools, capone mode, multicast (v1)