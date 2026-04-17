// Package partition wraps parted and sgdisk for partition table operations.
package partition

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// TableType represents the partition table format.
type TableType string

const (
	TableMBR TableType = "msdos"
	TableGPT TableType = "gpt"
)

// Restore writes the given partition table blob to the disk.
// The blob is the raw output of `sgdisk --backup` (GPT) or a
// 512-byte MBR template.
func Restore(disk string, table []byte) error {
	slog.Info("restoring partition table", "disk", disk)
	cmd := exec.Command("sgdisk", "--load-backup=/dev/stdin", disk)
	cmd.Stdin = strings.NewReader(string(table))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sgdisk restore: %w\n%s", err, out)
	}
	return nil
}

// Backup dumps the partition table from disk to bytes using sgdisk.
func Backup(disk string) ([]byte, error) {
	slog.Info("backing up partition table", "disk", disk)
	cmd := exec.Command("sgdisk", "--backup=/dev/stdout", disk)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sgdisk backup: %w", err)
	}
	return out, nil
}

// ExpandLast resizes the last partition on the disk to fill all available space.
// Uses parted's `resizepart` command.
func ExpandLast(disk string) error {
	slog.Info("expanding last partition to fill disk", "disk", disk)
	cmd := exec.Command("parted", "-s", disk, "resizepart", "100%", "100%")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("parted resizepart: %w\n%s", err, out)
	}
	return nil
}

// Wipe runs shred over the entire disk to overwrite all data.
func Wipe(disk string, passes int) error {
	slog.Info("wiping disk", "disk", disk, "passes", passes)
	args := []string{"-v", "-n", fmt.Sprintf("%d", passes), disk}
	cmd := exec.Command("shred", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("shred: %w", err)
	}
	return nil
}
