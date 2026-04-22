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
// fs is the filesystem type string passed to partclone (e.g. "ext4", "ntfs").
func Restore(ctx context.Context, device, fs string, src io.Reader, progress ProgressFunc) error {
	bin := partcloneBin(fs)
	slog.Info("partclone restore", "device", device, "fs", fs, "bin", bin)
	os.Remove(statusFile)
	cmd := exec.CommandContext(ctx, bin, "--restore", "--overwrite", "--source", "-", "--output", device)
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
