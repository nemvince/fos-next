// Package imaging wraps partclone for streaming image capture and restore.
// All image data flows through stdin/stdout — no temporary files are written.
package imaging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// statusFile is where the patched partclone writes machine-readable progress.
// Format (one line, refreshed in-place):
//
//	speed+unit @ elapsed @ remaining @ current_size @ total_size @ percent @ total_bytes
//
// e.g. "  1.23GB/min@00:00:01@00:00:03@128 MiB@1 GiB@ 25.00@1073741824.000000"
const statusFile = "/tmp/status.fog"

// ProgressFunc is called with percent complete and bits-per-minute.
type ProgressFunc func(percent int, bpm int64)

// Restore streams src into partclone.restore targeting the given device.
// partclone.restore is the filesystem-agnostic restore binary; it reads the
// partclone stream header to determine the format automatically.  The fs
// parameter is kept for logging only.
func Restore(ctx context.Context, device, fs string, src io.Reader, progress ProgressFunc) error {
	// Always use partclone.restore regardless of fs type — it auto-detects the
	// image format from the stream header.  Using a type-specific binary for
	// restore (e.g. partclone.ntfs) is fragile because on the deploy side the
	// partition is empty, blkid returns nothing, and we would fall back to
	// partclone.dd which interprets the partclone stream as raw data.
	slog.Info("partclone restore", "device", device, "fs", fs)
	os.Remove(statusFile)
	cmd := exec.CommandContext(ctx, "partclone.restore", "--restore", "--overwrite", "--source", "-", "--output", device)
	cmd.Stdin = src
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start partclone: %w", err)
	}
	done := make(chan struct{})
	go pollStatus(done, progress)
	err := cmd.Wait()
	close(done)
	if err != nil {
		return fmt.Errorf("partclone.restore: %w", err)
	}
	return nil
}

// Clone captures device to dst, streaming raw partclone output.
func Clone(ctx context.Context, device, fs string, dst io.Writer, progress ProgressFunc) error {
	bin := partcloneBin(fs)
	slog.Info("partclone clone", "device", device, "fs", fs, "bin", bin)
	os.Remove(statusFile)
	cmd := exec.CommandContext(ctx, bin, "--clone", "--source", device, "--output", "-")
	cmd.Stdout = dst
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start partclone: %w", err)
	}
	done := make(chan struct{})
	go pollStatus(done, progress)
	err := cmd.Wait()
	close(done)
	if err != nil {
		return fmt.Errorf("partclone.clone: %w", err)
	}
	return nil
}

// pollStatus reads /tmp/status.fog every 500 ms and calls fn with the latest values.
// It stops when done is closed.
func pollStatus(done <-chan struct{}, fn ProgressFunc) {
	if fn == nil {
		return
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			// One final read so the caller gets 100% if partclone wrote it.
			readStatus(fn)
			return
		case <-ticker.C:
			readStatus(fn)
		}
	}
}

// readStatus reads /tmp/status.fog and calls fn if the file is valid.
func readStatus(fn ProgressFunc) {
	data, err := os.ReadFile(statusFile)
	if err != nil || len(data) == 0 {
		return
	}
	// Take the last non-empty line in case the file has stale content above.
	line := strings.TrimSpace(string(data))
	if i := strings.LastIndex(line, "\n"); i >= 0 {
		line = strings.TrimSpace(line[i+1:])
	}
	if line == "" {
		return
	}
	// Format: speed+unit @ elapsed @ remaining @ current_size @ total_size @ percent @ total_bytes
	parts := strings.Split(line, "@")
	if len(parts) < 7 {
		return
	}
	pct, err := strconv.ParseFloat(strings.TrimSpace(parts[5]), 64)
	if err != nil {
		return
	}
	bpm := parseSpeedBpm(strings.TrimSpace(parts[0]))
	fn(int(pct), bpm)
}

// parseSpeedBpm parses a partclone speed string like "  1.23GB/min" into bits-per-minute.
func parseSpeedBpm(s string) int64 {
	// s looks like "  1.23GB/min" — find the first digit
	start := strings.IndexAny(s, "0123456789.")
	if start < 0 {
		return 0
	}
	// Find where the unit begins (first letter after the number)
	end := start
	for end < len(s) && (s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	val, err := strconv.ParseFloat(s[start:end], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToUpper(strings.TrimSpace(s[end:]))
	var multiplier float64
	switch unit {
	case "KB/MIN":
		multiplier = 8 * 1e3
	case "MB/MIN":
		multiplier = 8 * 1e6
	case "GB/MIN":
		multiplier = 8 * 1e9
	case "TB/MIN":
		multiplier = 8 * 1e12
	default:
		multiplier = 1
	}
	return int64(val * multiplier)
}

// partcloneBin maps a filesystem label to the appropriate partclone binary.
func partcloneBin(fs string) string {
	switch strings.ToLower(fs) {
	case "ext2", "ext3", "ext4":
		return "partclone.ext4"
	case "ntfs":
		return "partclone.ntfs"
	case "fat16", "fat32", "vfat":
		return "partclone.fat"
	case "xfs":
		return "partclone.xfs"
	case "btrfs":
		return "partclone.btrfs"
	default:
		return "partclone.dd"
	}
}

// CanShrink reports whether the filesystem type can be non-destructively
// shrunk before capture to produce a smaller image.
func CanShrink(fs string) bool {
	switch strings.ToLower(fs) {
	case "ext2", "ext3", "ext4", "ntfs", "btrfs":
		return true
	default:
		return false
	}
}

// Shrink reduces the filesystem on device to its minimum safe size so that
// less data needs to be transferred during capture.  Returns (true, nil) if
// the filesystem was shrunk, (false, nil) if shrinking is not supported for
// this filesystem (caller should mark the partition as fixed-size), or
// (false, err) on a hard failure.
func Shrink(ctx context.Context, device, fs string) (shrunk bool, err error) {
	switch strings.ToLower(fs) {
	case "ntfs":
		return shrinkNTFS(ctx, device)
	case "ext2", "ext3", "ext4":
		return shrinkEXT(ctx, device)
	case "btrfs":
		return shrinkBTRFS(ctx, device)
	default:
		return false, nil
	}
}

// Expand grows the filesystem on device to fill the entire partition.  It
// is safe to call even if the partition was not previously shrunk.
// Non-fatal: unsupported filesystem types are silently skipped.
func Expand(ctx context.Context, device, fs string) error {
	switch strings.ToLower(fs) {
	case "ntfs":
		return expandNTFS(ctx, device)
	case "ext2", "ext3", "ext4":
		return expandEXT(ctx, device)
	case "btrfs":
		return expandBTRFS(ctx, device)
	case "xfs":
		return expandXFS(ctx, device)
	case "f2fs":
		return expandF2FS(ctx, device)
	default:
		// FAT, swap, dd — nothing to do.
		return nil
	}
}

// ------------------------------------------------------------------
// NTFS
// ------------------------------------------------------------------

func shrinkNTFS(ctx context.Context, device string) (bool, error) {
	// Dry-run first to confirm ntfsresize thinks it can shrink.
	out, err := exec.CommandContext(ctx, "ntfsresize", "-fns", device).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("ntfsresize dry-run: %w\n%s", err, out)
	}
	// Actually shrink. -f force, -s 0 (minimum size).
	out, err = exec.CommandContext(ctx, "ntfsresize", "-f", "--size", "0", device).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("ntfsresize shrink: %w\n%s", err, out)
	}
	return true, nil
}

func expandNTFS(ctx context.Context, device string) error {
	// ntfsresize with no --size fills the partition automatically.
	out, err := exec.CommandContext(ctx, "ntfsresize", "-f", "-b", "-P", device).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ntfsresize expand: %w\n%s", err, out)
	}
	return nil
}

// NtfsFixDirty clears the NTFS dirty bit and journal on a freshly-restored
// NTFS partition.  Without this, Windows will run chkdsk on the first boot
// after every deploy.  Best-effort — errors are logged, not returned.
func NtfsFixDirty(device string) {
	// -b: clear bad sector list, -d: clear dirty flag
	out, err := exec.Command("ntfsfix", "-b", "-d", device).CombinedOutput()
	if err != nil {
		slog.Warn("ntfsfix failed (non-fatal)", "device", device, "err", err, "output", string(out))
	} else {
		slog.Info("ntfsfix: cleared dirty bit", "device", device)
	}
}

// ------------------------------------------------------------------
// EXT2/3/4
// ------------------------------------------------------------------

func shrinkEXT(ctx context.Context, device string) (bool, error) {
	// Force fsck before resize to ensure a clean filesystem.
	if out, err := exec.CommandContext(ctx, "e2fsck", "-fy", device).CombinedOutput(); err != nil {
		return false, fmt.Errorf("e2fsck before shrink: %w\n%s", err, out)
	}
	// Shrink to minimum size.
	out, err := exec.CommandContext(ctx, "resize2fs", "-M", device).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("resize2fs shrink: %w\n%s", err, out)
	}
	return true, nil
}

func expandEXT(ctx context.Context, device string) error {
	// resize2fs with no size argument fills the partition.
	out, err := exec.CommandContext(ctx, "resize2fs", device).CombinedOutput()
	if err != nil {
		return fmt.Errorf("resize2fs expand: %w\n%s", err, out)
	}
	return nil
}

// ------------------------------------------------------------------
// BTRFS
// ------------------------------------------------------------------

// btrfsMountPoint is the temporary mount used for btrfs resize operations.
const btrfsMountPoint = "/tmp/fog-btrfs-resize"

func shrinkBTRFS(ctx context.Context, device string) (bool, error) {
	if err := os.MkdirAll(btrfsMountPoint, 0o700); err != nil {
		return false, fmt.Errorf("btrfs shrink: mkdir: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "mount", "-t", "btrfs", device, btrfsMountPoint).CombinedOutput(); err != nil {
		return false, fmt.Errorf("btrfs shrink: mount: %w\n%s", err, out)
	}
	defer func() {
		_ = exec.CommandContext(ctx, "umount", btrfsMountPoint).Run()
	}()
	// btrfs resize to minimum.  "min" keyword requires btrfs-progs >= 5.10.
	out, err := exec.CommandContext(ctx, "btrfs", "filesystem", "resize", "1:min", btrfsMountPoint).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("btrfs resize min: %w\n%s", err, out)
	}
	return true, nil
}

func expandBTRFS(ctx context.Context, device string) error {
	if err := os.MkdirAll(btrfsMountPoint, 0o700); err != nil {
		return fmt.Errorf("btrfs expand: mkdir: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "mount", "-t", "btrfs", device, btrfsMountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("btrfs expand: mount: %w\n%s", err, out)
	}
	defer func() {
		_ = exec.CommandContext(ctx, "umount", btrfsMountPoint).Run()
	}()
	out, err := exec.CommandContext(ctx, "btrfs", "filesystem", "resize", "max", btrfsMountPoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs resize max: %w\n%s", err, out)
	}
	return nil
}

// ------------------------------------------------------------------
// XFS (can grow but not shrink)
// ------------------------------------------------------------------

func expandXFS(ctx context.Context, device string) error {
	mp := "/tmp/fog-xfs-grow"
	if err := os.MkdirAll(mp, 0o700); err != nil {
		return fmt.Errorf("xfs grow: mkdir: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "mount", "-t", "xfs", device, mp).CombinedOutput(); err != nil {
		return fmt.Errorf("xfs grow: mount: %w\n%s", err, out)
	}
	defer func() {
		_ = exec.CommandContext(ctx, "umount", mp).Run()
	}()
	out, err := exec.CommandContext(ctx, "xfs_growfs", mp).CombinedOutput()
	if err != nil {
		return fmt.Errorf("xfs_growfs: %w\n%s", err, out)
	}
	return nil
}

// ------------------------------------------------------------------
// F2FS (can grow but not shrink)
// ------------------------------------------------------------------

func expandF2FS(ctx context.Context, device string) error {
	out, err := exec.CommandContext(ctx, "resize.f2fs", device).CombinedOutput()
	if err != nil {
		return fmt.Errorf("resize.f2fs: %w\n%s", err, out)
	}
	return nil
}

// ------------------------------------------------------------------
// Bitlocker detection
// ------------------------------------------------------------------

// IsBitlockerEncrypted returns true if the partition appears to be a
// Bitlocker-encrypted volume.  The check reads the NTFS boot sector magic
// bytes at offset 3 (which Bitlocker replaces with "-FVE-FS-").
func IsBitlockerEncrypted(device string) bool {
	// Prefer blkid probe (fastest, no reads beyond the boot sector).
	out, err := exec.Command("blkid", "-p", "-o", "value", "-s", "TYPE", device).Output()
	if err == nil && strings.TrimSpace(string(out)) == "BitLocker" {
		return true
	}
	// Fallback: read the OEM ID field at bytes 3–10 of the boot sector.
	f, err := os.Open(device)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 11)
	if _, err := f.Read(buf); err != nil {
		return false
	}
	return string(buf[3:11]) == "-FVE-FS-"
}

// ------------------------------------------------------------------
// NTFS pre-capture cleanup
// ------------------------------------------------------------------

// CleanNTFS mounts the NTFS partition read-write and removes pagefile.sys
// and hiberfil.sys to reduce the image size.  It is a best-effort operation;
// errors are logged but do not abort the capture.
func CleanNTFS(ctx context.Context, device string) {
	mp := "/tmp/fog-ntfs-clean"
	if err := os.MkdirAll(mp, 0o700); err != nil {
		slog.Warn("ntfs clean: mkdir failed", "err", err)
		return
	}
	out, err := exec.CommandContext(ctx, "ntfs-3g", "-o", "remove_hiberfile", device, mp).CombinedOutput()
	if err != nil {
		slog.Warn("ntfs clean: mount failed", "device", device, "err", err, "output", string(out))
		return
	}
	defer func() {
		if out, err := exec.CommandContext(ctx, "umount", mp).CombinedOutput(); err != nil {
			slog.Warn("ntfs clean: umount failed", "err", err, "output", string(out))
		}
	}()
	for _, name := range []string{"pagefile.sys", "hiberfil.sys", "swapfile.sys"} {
		path := mp + "/" + name
		if err := os.Remove(path); err == nil {
			slog.Info("ntfs clean: removed", "file", name)
		}
	}
}
