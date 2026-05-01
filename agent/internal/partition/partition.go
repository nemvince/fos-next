// Package partition wraps sgdisk, sfdisk, and parted for partition table
// operations.  Both GPT and MBR (DOS) tables are handled:
//
//   - GPT  → backup/restore via sgdisk --backup / --load-backup
//   - MBR  → backup via sfdisk -d + raw 512-byte boot sector;
//     restore via dd (boot sector) + sfdisk
//
// The partition table blob exchanged with the server is a JSON envelope so
// the restore side always knows which path to take:
//
//	{"type":"gpt","sgdisk":"<base64>"}
//	{"type":"mbr","sfdisk":"<sfdisk -d output>","mbr":"<base64 of 512 bytes>"}
package partition

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// TableType represents the partition table format.
type TableType string

const (
	TableMBR TableType = "mbr"
	TableGPT TableType = "gpt"
)

// tableBlob is the JSON structure stored as part 0 of each image.
type tableBlob struct {
	Type   TableType `json:"type"`
	SGDisk string    `json:"sgdisk,omitempty"` // GPT: base64-encoded sgdisk backup
	SFDisk string    `json:"sfdisk,omitempty"` // MBR: sfdisk -d output (text)
	MBRRaw string    `json:"mbr,omitempty"`    // MBR: base64 of raw 512-byte boot sector
}

// DetectTableType returns the partition table type of disk by reading the
// PTTYPE attribute via blkid.  Falls back to GPT on any error.
func DetectTableType(disk string) TableType {
	out, err := exec.Command("blkid", "-p", "-o", "value", "-s", "PTTYPE", disk).Output()
	if err != nil {
		slog.Warn("blkid PTTYPE failed, assuming GPT", "disk", disk, "err", err)
		return TableGPT
	}
	switch strings.TrimSpace(string(out)) {
	case "dos":
		return TableMBR
	default:
		return TableGPT
	}
}

// Backup captures the partition table of disk and returns it as a JSON blob.
// GPT disks use a binary sgdisk backup; MBR disks save the sfdisk table dump
// plus the raw first 512 bytes of the disk (contains GRUB/bootloader code).
func Backup(disk string) ([]byte, error) {
	slog.Info("detecting partition table type", "disk", disk)
	tt := DetectTableType(disk)
	slog.Info("backing up partition table", "disk", disk, "type", tt)

	switch tt {
	case TableMBR:
		return backupMBR(disk)
	default:
		return backupGPT(disk)
	}
}

// Restore writes the partition table blob (produced by Backup) back to disk.
// It dispatches to the appropriate restore path based on the blob type field.
func Restore(disk string, blob []byte) error {
	slog.Info("restoring partition table", "disk", disk)
	var tb tableBlob
	if err := json.Unmarshal(blob, &tb); err != nil {
		// Legacy: raw sgdisk binary blob from older fos-next versions.
		slog.Warn("blob is not JSON, attempting legacy sgdisk restore", "disk", disk)
		return restoreSGDiskRaw(disk, blob)
	}
	switch tb.Type {
	case TableMBR:
		return restoreMBR(disk, tb)
	default:
		return restoreGPT(disk, tb)
	}
}

// ExpandLast resizes the last partition on the disk to fill all available
// space using parted's resizepart command.  This only adjusts the partition
// table entry; filesystem resizing is handled by imaging.Expand.
func ExpandLast(disk string) error {
	slog.Info("expanding last partition to fill disk", "disk", disk)
	lastN, err := lastPartitionNumber(disk)
	if err != nil {
		return fmt.Errorf("find last partition: %w", err)
	}
	partN := fmt.Sprintf("%d", lastN)
	out, err := exec.Command("parted", "-s", disk, "resizepart", partN, "100%").CombinedOutput()
	if err != nil {
		return fmt.Errorf("parted resizepart %s: %w\n%s", partN, err, out)
	}
	// Re-read partition table so the kernel sees the new size.
	_ = exec.Command("partprobe", disk).Run()
	settlePartTable()
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

// ------------------------------------------------------------------
// GPT helpers
// ------------------------------------------------------------------

func backupGPT(disk string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "sgdisk-backup-*")
	if err != nil {
		return nil, fmt.Errorf("sgdisk backup: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)
	if out, err := exec.Command("sgdisk", "--backup="+tmpName, disk).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sgdisk backup: %w\n%s", err, out)
	}
	raw, err := os.ReadFile(tmpName)
	if err != nil {
		return nil, fmt.Errorf("sgdisk backup: read temp file: %w", err)
	}
	tb := tableBlob{
		Type:   TableGPT,
		SGDisk: base64.StdEncoding.EncodeToString(raw),
	}
	return json.Marshal(tb)
}

func restoreGPT(disk string, tb tableBlob) error {
	raw, err := base64.StdEncoding.DecodeString(tb.SGDisk)
	if err != nil {
		return fmt.Errorf("sgdisk restore: base64 decode: %w", err)
	}
	return restoreSGDiskRaw(disk, raw)
}

func restoreSGDiskRaw(disk string, raw []byte) error {
	// Zap any existing (possibly corrupt) GPT/MBR structures before restoring.
	// sgdisk --zap-all regularly exits non-zero on corrupt or blank disks — that
	// is the expected condition here, so the error is intentionally ignored.
	zapOut, zapErr := exec.Command("sgdisk", "--zap-all", disk).CombinedOutput()
	if zapErr != nil {
		slog.Warn("sgdisk --zap-all returned non-zero (ignored)", "disk", disk, "err", zapErr, "output", string(zapOut))
	}

	tmp, err := os.CreateTemp("", "sgdisk-restore-*")
	if err != nil {
		return fmt.Errorf("sgdisk restore: create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("sgdisk restore: write temp file: %w", err)
	}
	tmp.Close()
	out, err := exec.Command("sgdisk", "--load-backup="+tmp.Name(), disk).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sgdisk restore: %w\n%s", err, out)
	}
	_ = exec.Command("partprobe", disk).Run()
	settlePartTable()
	return nil
}

// ------------------------------------------------------------------
// MBR helpers
// ------------------------------------------------------------------

func backupMBR(disk string) ([]byte, error) {
	// 1. Dump the partition table via sfdisk (human-readable text format).
	sfdiskOut, err := exec.Command("sfdisk", "-d", disk).Output()
	if err != nil {
		return nil, fmt.Errorf("sfdisk dump: %w", err)
	}

	// 2. Save the raw first 512 bytes which contain the bootloader (GRUB stage 1
	//    or other MBR code) in the first 446 bytes, followed by the partition
	//    table and magic signature.
	f, err := os.Open(disk)
	if err != nil {
		return nil, fmt.Errorf("open disk for MBR read: %w", err)
	}
	defer f.Close()
	mbrRaw := make([]byte, 512)
	if _, err := f.Read(mbrRaw); err != nil {
		return nil, fmt.Errorf("read MBR: %w", err)
	}

	tb := tableBlob{
		Type:   TableMBR,
		SFDisk: string(sfdiskOut),
		MBRRaw: base64.StdEncoding.EncodeToString(mbrRaw),
	}
	return json.Marshal(tb)
}

func restoreMBR(disk string, tb tableBlob) error {
	// 1. Write only the bootloader bytes (first 446 bytes) back to the disk.
	//    sfdisk will write the correct partition table in the next step.
	mbrRaw, err := base64.StdEncoding.DecodeString(tb.MBRRaw)
	if err != nil {
		return fmt.Errorf("MBR restore: base64 decode: %w", err)
	}
	if len(mbrRaw) < 446 {
		return fmt.Errorf("MBR restore: boot sector too short (%d bytes)", len(mbrRaw))
	}
	f, err := os.OpenFile(disk, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open disk for MBR write: %w", err)
	}
	if _, err := f.Write(mbrRaw[:446]); err != nil {
		f.Close()
		return fmt.Errorf("write bootloader bytes: %w", err)
	}
	f.Close()

	// 2. Restore the partition table via sfdisk.
	cmd := exec.Command("sfdisk", "--force", disk)
	cmd.Stdin = strings.NewReader(tb.SFDisk)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sfdisk restore: %w\n%s", err, out)
	}

	// 3. Re-read the partition table.
	_ = exec.Command("partprobe", disk).Run()
	settlePartTable()
	return nil
}

// ------------------------------------------------------------------
// Internal helpers
// ------------------------------------------------------------------

// settlePartTable asks udevd to settle after a partition-table change so that
// device nodes (e.g. /dev/sda1) are present before the caller proceeds.
// Falls back to a 500 ms sleep if udevadm is not available (e.g. during tests).
func settlePartTable() {
	if err := exec.Command("udevadm", "settle", "--timeout=10").Run(); err != nil {
		time.Sleep(500 * time.Millisecond)
	}
}

// lastPartitionNumber uses `parted -m -s <disk> print` to find the highest
// partition number on disk.
func lastPartitionNumber(disk string) (int, error) {
	out, err := exec.Command("parted", "-m", "-s", disk, "print").Output()
	if err != nil {
		return 0, fmt.Errorf("parted print: %w", err)
	}
	last := 0
	for _, line := range strings.Split(string(out), "\n") {
		// Machine-readable line format: number:start:end:size:fs:name:flags;
		fields := strings.SplitN(line, ":", 2)
		if len(fields) < 2 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(fields[0], "%d", &n); err == nil && n > last {
			last = n
		}
	}
	if last == 0 {
		return 0, fmt.Errorf("no partitions found on %s", disk)
	}
	return last, nil
}
