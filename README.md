# fos-next — FOG Operating System, rewritten

A minimal, modern Linux initramfs for network imaging. Replaces the legacy FOS shell-script stack with a single static Go binary (`fos-agent`). Built with Buildroot, targeting x86_64.

Communicates exclusively with **fog-next**'s REST API using a short-lived **boot token** issued at handshake time.

See [COMPAT.md](COMPAT.md) for the minimum fog-next version required.

---

## Architecture

```
PXE boot → iPXE → fog-next /fog/boot (gets bzImage + init.xz)
        → Linux boots → fos-agent (PID 1)
           ├── parse /proc/cmdline (fog_server=, fog_action=)
           ├── bring up NICs (udhcpc)
           ├── POST /fog/api/v1/boot/handshake  → boot token + action
           └── dispatch action:
               ├── register  → collect inventory → POST /boot/register
               ├── deploy    → download partitions via HTTP → partclone.restore
               ├── capture   → partclone.clone → PUT upload stream
               ├── wipe      → shred disk
               └── debug     → drop to ash shell
```

## Repository layout

```
fos-next/
  agent/                  — fos-agent Go module
    cmd/fos-agent/        — main entry point (PID 1)
    internal/
      cmdline/            — /proc/cmdline parser
      netup/              — NIC bringup + DHCP
      api/                — fog-next boot API client (holds boot token)
      imaging/            — partclone subprocess wrapper
      inventory/          — hardware info from sysfs/procfs
      partition/          — parted/sgdisk wrappers
      actions/            — register, deploy, capture, wipe, debug
  buildroot/
    configs/              — Buildroot defconfig (fsx64_defconfig)
    board/fos-next/
      rootfs_overlay/     — etc/inittab, init.d scripts
      kernel-patches/     — kernel config fragment
    package/fos-agent/    — Buildroot package for fos-agent
  .github/workflows/      — CI: agent tests + Buildroot x86_64 build
  build.sh                — Buildroot wrapper (flags: -k kernel, -f fs)
  Makefile
  COMPAT.md               — fog-next version compatibility matrix
```

## Build

**Prerequisites:** Docker or a Linux host with build-essential, Go 1.23+, curl

```bash
# Build everything (downloads Buildroot automatically)
./build.sh -n

# Kernel only
./build.sh -k -n

# Filesystem only (faster iteration)
./build.sh -f -n

# Output: images/bzImage + images/init.xz + images/sha256sums
```

## Quick agent build (no Buildroot)

```bash
make agent
# Output: images/fos-agent (static, ~12 MB stripped)
```

## Kernel command line parameters

| Parameter | Description |
|-----------|-------------|
| `fog_server=http://10.0.0.1` | fog-next server URL **(required)** |
| `fog_action=debug` | Override the action returned by handshake |
| `fog_host=aa:bb:cc:dd:ee:ff` | Primary MAC hint (optional) |
| `fog_debug=1` | Enable verbose logging |

## Size targets

| Artifact | Target |
|----------|--------|
| fos-agent binary (stripped) | ~12 MB |
| init.xz (compressed initramfs) | < 55 MB |
| bzImage | ~12 MB |

## License

GPL-3.0
