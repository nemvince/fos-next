// Package imaging wraps partclone for streaming image capture and restore.
// All image data flows through stdin/stdout — no temporary files are written.
package imaging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ProgressFunc is called with percent complete and bits-per-minute
// as partclone emits progress lines on stderr.
type ProgressFunc func(percent int, bpm int64)

// Restore streams src into partclone.restore targeting the given device.
// fs is the filesystem type string passed to partclone (e.g. "ext4", "ntfs").
func Restore(ctx context.Context, device, fs string, src io.Reader, progress ProgressFunc) error {
	bin := partcloneBin(fs)
	slog.Info("partclone restore", "device", device, "fs", fs, "bin", bin)
	cmd := exec.CommandContext(ctx, bin, "--restore", "--overwrite", "--source", "-", "--output", device)
	cmd.Stdin = src
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start partclone: %w", err)
	}
	go parseProgress(stderr, progress)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("partclone.restore: %w", err)
	}
	return nil
}

// Clone captures device to dst, streaming raw partclone output.
// fs is the filesystem type (e.g. "ext4"). dst is typically an
// HTTP chunked upload writer.
func Clone(ctx context.Context, device, fs string, dst io.Writer, progress ProgressFunc) error {
	bin := partcloneBin(fs)
	slog.Info("partclone clone", "device", device, "fs", fs, "bin", bin)
	cmd := exec.CommandContext(ctx, bin, "--clone", "--source", device, "--output", "-")
	cmd.Stdout = dst
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start partclone: %w", err)
	}
	go parseProgress(stderr, progress)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("partclone.clone: %w", err)
	}
	return nil
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

// progressRe matches partclone's progress output:
// "Elapsed: 00:00:01, Remaining: 00:00:03, Completed: 25.00%, Rate:  123.45MB/min"
var progressRe = regexp.MustCompile(`Completed:\s+(\d+(?:\.\d+)?)%.*?Rate:\s+([\d.]+)([KMGT]B/min)`)

func parseProgress(r io.Reader, fn ProgressFunc) {
	if fn == nil {
		io.Copy(io.Discard, r)
		return
	}
	buf := make([]byte, 4096)
	var tail string
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := tail + string(buf[:n])
			lines := strings.Split(chunk, "\r")
			// Retain incomplete last fragment
			tail = lines[len(lines)-1]
			for _, line := range lines[:len(lines)-1] {
				if m := progressRe.FindStringSubmatch(line); m != nil {
					pct, _ := strconv.ParseFloat(m[1], 64)
					rate, _ := strconv.ParseFloat(m[2], 64)
					bpm := rateToMbpm(rate, m[3])
					fn(int(pct), bpm)
				}
			}
		}
		if err != nil {
			break
		}
	}
}

func rateToMbpm(rate float64, unit string) int64 {
	var multiplier float64
	switch unit {
	case "KB/min":
		multiplier = 8 * 1e3
	case "MB/min":
		multiplier = 8 * 1e6
	case "GB/min":
		multiplier = 8 * 1e9
	case "TB/min":
		multiplier = 8 * 1e12
	default:
		multiplier = 1
	}
	return int64(rate * multiplier)
}
