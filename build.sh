#!/usr/bin/env bash
# build.sh — fos-next Buildroot wrapper
#
# Usage:
#   ./build.sh [-k] [-f] [-p <path>] [-n] [-h]
#
#   -k   Build the kernel only (skips rootfs)
#   -f   Build the filesystem only (skips kernel)
#   -p   Override the build/download directory (default: ./build)
#   -n   Non-interactive: don't prompt for confirmation
#   -h   Show this help

set -euo pipefail

BUILDROOT_VERSION="2026.02"
KERNEL_VERSION="6.18.22"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_PATH="${SCRIPT_DIR}/build"
BUILDROOT_DIR="${BUILD_PATH}/buildroot-${BUILDROOT_VERSION}"
BUILDROOT_TAR="${BUILD_PATH}/buildroot-${BUILDROOT_VERSION}.tar.xz"
BUILDROOT_URL="https://buildroot.org/downloads/buildroot-${BUILDROOT_VERSION}.tar.xz"

BUILD_KERNEL=true
BUILD_FS=true
CONFIRM=true

usage() {
    cat <<'EOF'
Usage: ./build.sh [OPTIONS]

Options:
  -k    Kernel only (skip rootfs/initramfs build)
  -f    Filesystem only (skip kernel build)
  -p    Override build directory path (default: ./build)
  -n    Non-interactive (skip confirmation prompt)
  -h    Show this help and exit

Outputs written to <build_path>/images/:
  bzImage    — compressed x86_64 kernel
  rootfs.cpio.xz  — compressed initramfs (installed as init.xz by CI)

Environment:
  BUILDROOT_VERSION   Override Buildroot version (default: 2026.02)
  KERNEL_VERSION      Override Linux version (default: 6.18.22)
EOF
    exit 0
}

# ── Argument parsing ──────────────────────────────────────────────────────────
while getopts "kfp:nh" opt; do
    case $opt in
        k) BUILD_FS=false ;;
        f) BUILD_KERNEL=false ;;
        p) BUILD_PATH="$OPTARG" ;;
        n) CONFIRM=false ;;
        h) usage ;;
        *) usage ;;
    esac
done

mkdir -p "${BUILD_PATH}"

# ── Confirmation ──────────────────────────────────────────────────────────────
if [[ "$CONFIRM" == "true" ]]; then
    echo "fos-next build"
    echo "  Buildroot: ${BUILDROOT_VERSION}"
    echo "  Kernel:    ${KERNEL_VERSION}"
    echo "  Build dir: ${BUILD_PATH}"
    echo "  Target:    x86_64"
    echo "  Kernel:    $( [[ "$BUILD_KERNEL" == "true" ]] && echo yes || echo no )"
    echo "  RootFS:    $( [[ "$BUILD_FS"     == "true" ]] && echo yes || echo no )"
    read -rp "Continue? [y/N] " ans
    [[ "$ans" =~ ^[yY] ]] || exit 0
fi

# ── Download Buildroot ────────────────────────────────────────────────────────
if [[ ! -d "${BUILDROOT_DIR}" ]]; then
    if [[ ! -f "${BUILDROOT_TAR}" ]]; then
        echo "Downloading Buildroot ${BUILDROOT_VERSION}..."
        curl -sSL --retry 3 -o "${BUILDROOT_TAR}" "${BUILDROOT_URL}"
    fi
    echo "Extracting Buildroot..."
    tar -xf "${BUILDROOT_TAR}" -C "${BUILD_PATH}"
fi

# ── Configure Buildroot ───────────────────────────────────────────────────────
BR2_EXTERNAL="${SCRIPT_DIR}/buildroot"
O="${BUILD_PATH}/output"
mkdir -p "${O}"

echo "Configuring Buildroot..."
make -C "${BUILDROOT_DIR}" \
    O="${O}" \
    BR2_EXTERNAL="${BR2_EXTERNAL}" \
    BR2_DL_DIR="${BUILD_PATH}/dl" \
    fsx64_defconfig

# ── Build ─────────────────────────────────────────────────────────────────────
TARGETS=()
if [[ "$BUILD_KERNEL" == "true" ]]; then
    TARGETS+=("linux")
fi
if [[ "$BUILD_FS" == "true" ]]; then
    TARGETS+=("fos-agent" "rootfs-cpio")
fi
if [[ ${#TARGETS[@]} -eq 0 ]]; then
    echo "Error: nothing to build — both -k and -f cannot be skipped simultaneously."
    exit 1
fi

echo "Building: ${TARGETS[*]}"
make -C "${BUILDROOT_DIR}" \
    O="${O}" \
    BR2_EXTERNAL="${BR2_EXTERNAL}" \
    BR2_DL_DIR="${BUILD_PATH}/dl" \
    BR2_CCACHE_DIR="${BUILD_PATH}/ccache" \
    "${TARGETS[@]}"

# ── Collect outputs ───────────────────────────────────────────────────────────
IMAGES_DIR="${SCRIPT_DIR}/images"
mkdir -p "${IMAGES_DIR}"

if [[ "$BUILD_KERNEL" == "true" ]]; then
    cp "${O}/images/bzImage" "${IMAGES_DIR}/bzImage"
    echo "Kernel: ${IMAGES_DIR}/bzImage"
fi
if [[ "$BUILD_FS" == "true" ]]; then
    cp "${O}/images/rootfs.cpio.xz" "${IMAGES_DIR}/init.xz"
    echo "InitRD: ${IMAGES_DIR}/init.xz"
fi

# Generate sha256 checksums alongside artifacts.
( cd "${IMAGES_DIR}" && sha256sum ./* > sha256sums )
echo "Done. Artifacts: ${IMAGES_DIR}/"
