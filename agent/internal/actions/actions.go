// Package actions implements the fos-agent action dispatcher and each
// supported action: register, deploy, capture, debug, and wipe.
package actions

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	fogapi "github.com/nemvince/fos-next/internal/api"
	"github.com/nemvince/fos-next/internal/imaging"
	"github.com/nemvince/fos-next/internal/inventory"
	"github.com/nemvince/fos-next/internal/partition"
)

// Dispatcher routes the handshake action to the correct handler.
func Dispatch(ctx context.Context, client *fogapi.Client, resp *fogapi.HandshakeResponse) error {
	slog.Info("dispatching action", "action", resp.Action)
	switch resp.Action {
	case "deploy":
		return Deploy(ctx, client, resp)
	case "capture":
		return Capture(ctx, client, resp)
	case "wipe":
		return Wipe(ctx, client, resp)
	case "debug":
		return Debug(ctx, resp)
	default:
		slog.Warn("unknown action, dropping to debug shell", "action", resp.Action)
		return Debug(ctx, resp)
	}
}

// ------------------------------------------------------------------
// Register — collect inventory and submit to server (no boot token)
// ------------------------------------------------------------------

func Register(ctx context.Context, client *fogapi.Client, macs []string) error {
	slog.Info("action: register")
	inv, err := inventory.Collect()
	if err != nil {
		slog.Warn("partial inventory", "err", err)
	}

	req := fogapi.RegisterRequest{
		MACs:      macs,
		CPUModel:  inv.CPUModel,
		CPUCores:  inv.CPUCores,
		RAMBytes:  inv.RAMBytes,
		DiskBytes: inv.DiskBytes,
		UUID:      inv.UUID,
	}
	if err := client.Register(ctx, req); err != nil {
		return err
	}
	slog.Info("registration submitted — powering off in 5s")
	time.Sleep(5 * time.Second)
	_ = exec.Command("poweroff").Run()
	return nil
}

// ------------------------------------------------------------------
// Deploy — restore image to disk
// ------------------------------------------------------------------

func Deploy(ctx context.Context, client *fogapi.Client, resp *fogapi.HandshakeResponse) error {
	slog.Info("action: deploy", "imageId", resp.ImageID, "parts", resp.PartCount)
	disk := primaryDisk()

	// Download and restore the partition table first.
	ptable, _, err := client.DownloadPart(ctx, resp.ImageID, 0, 0)
	if err != nil {
		return reportFail(ctx, client, resp.TaskID, "partition table download failed: "+err.Error())
	}
	defer ptable.Close()
	tableBytes, err := readAll(ptable)
	if err != nil {
		return reportFail(ctx, client, resp.TaskID, "reading partition table: "+err.Error())
	}
	if err := partition.Restore(disk, tableBytes); err != nil {
		return reportFail(ctx, client, resp.TaskID, "restoring partition table: "+err.Error())
	}

	totalBytes := resp.TotalBytes
	var transferred int64
	progressFn := makeProgressFn(ctx, client, resp.TaskID, &transferred, totalBytes)

	for part := 1; part <= resp.PartCount; part++ {
		dev := partitionDevice(disk, part)
		slog.Info("deploying partition", "part", part, "device", dev)

		var resumeOffset int64
		for attempt := 0; attempt < 3; attempt++ {
			body, _, dlErr := client.DownloadPart(ctx, resp.ImageID, part, resumeOffset)
			if dlErr != nil {
				slog.Warn("download failed, retrying", "part", part, "attempt", attempt, "err", dlErr)
				time.Sleep(5 * time.Second)
				continue
			}
			imgErr := imaging.Restore(ctx, dev, "ext4", body, progressFn)
			body.Close()
			if imgErr == nil {
				break
			}
			slog.Warn("restore failed, retrying", "part", part, "attempt", attempt, "err", imgErr)
			time.Sleep(5 * time.Second)
		}
	}

	if err := partition.ExpandLast(disk); err != nil {
		slog.Warn("expand last partition failed (non-fatal)", "err", err)
	}

	return client.Complete(ctx, fogapi.CompleteRequest{
		TaskID:  resp.TaskID,
		Success: true,
	})
}

// ------------------------------------------------------------------
// Capture — clone partitions and upload to server
// ------------------------------------------------------------------

func Capture(ctx context.Context, client *fogapi.Client, resp *fogapi.HandshakeResponse) error {
	slog.Info("action: capture", "imageId", resp.ImageID)
	disk := primaryDisk()

	// Backup partition table.
	tableBytes, err := partition.Backup(disk)
	if err != nil {
		return reportFail(ctx, client, resp.TaskID, "partition table backup failed: "+err.Error())
	}
	if err := client.UploadPart(ctx, resp.ImageID, 0, bytesReader(tableBytes)); err != nil {
		return reportFail(ctx, client, resp.TaskID, "partition table upload failed: "+err.Error())
	}

	// Determine partition count from the disk.
	parts := discoverPartitions(disk)
	for i, dev := range parts {
		partNum := i + 1
		fs := detectFilesystem(dev)
		slog.Info("capturing partition", "part", partNum, "device", dev, "fs", fs)
		pr, pw := syncPipe()
		errCh := make(chan error, 1)
		go func() {
			err := imaging.Clone(ctx, dev, fs, pw, nil)
			pw.CloseWithError(err)
			errCh <- err
		}()
		if upErr := client.UploadPart(ctx, resp.ImageID, partNum, pr); upErr != nil {
			return reportFail(ctx, client, resp.TaskID, "upload failed for part "+string(rune('0'+partNum))+": "+upErr.Error())
		}
		if err := <-errCh; err != nil {
			return reportFail(ctx, client, resp.TaskID, "partclone failed for part "+string(rune('0'+partNum))+": "+err.Error())
		}
	}

	return client.Complete(ctx, fogapi.CompleteRequest{
		TaskID:  resp.TaskID,
		Success: true,
	})
}

// ------------------------------------------------------------------
// Wipe — overwrite disk with random data
// ------------------------------------------------------------------

func Wipe(ctx context.Context, client *fogapi.Client, resp *fogapi.HandshakeResponse) error {
	slog.Info("action: wipe")
	disk := primaryDisk()
	if err := partition.Wipe(disk, 1); err != nil {
		return reportFail(ctx, client, resp.TaskID, "wipe failed: "+err.Error())
	}
	return client.Complete(ctx, fogapi.CompleteRequest{TaskID: resp.TaskID, Success: true})
}

// ------------------------------------------------------------------
// Debug — drop to a BusyBox ash shell
// ------------------------------------------------------------------

func Debug(ctx context.Context, resp *fogapi.HandshakeResponse) error {
	slog.Info("action: debug — dropping to shell")
	printDebugBanner(resp)
	shell, err := exec.LookPath("ash")
	if err != nil {
		shell = "/bin/sh"
	}
	// exec replaces the process; the boot token is not passed in env.
	return syscallExec(shell)
}

// ------------------------------------------------------------------
// Helpers
// ------------------------------------------------------------------

func reportFail(ctx context.Context, client *fogapi.Client, taskID, msg string) error {
	slog.Error("task failed", "msg", msg)
	_ = client.Complete(ctx, fogapi.CompleteRequest{TaskID: taskID, Success: false, Message: msg})
	return nil
}

func makeProgressFn(ctx context.Context, client *fogapi.Client, taskID string, transferred *int64, totalBytes int64) imaging.ProgressFunc {
	return func(pct int, bpm int64) {
		req := fogapi.ProgressRequest{
			TaskID:           taskID,
			Percent:          pct,
			BitsPerMinute:    bpm,
			BytesTransferred: *transferred,
		}
		if err := client.ReportProgress(ctx, req); err != nil {
			slog.Warn("progress report failed", "err", err)
		}
	}
}

func primaryDisk() string {
	candidates := []string{"/dev/sda", "/dev/nvme0n1", "/dev/vda"}
	for _, d := range candidates {
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	return "/dev/sda"
}

func partitionDevice(disk string, num int) string {
	// nvme0n1 uses p-suffix: nvme0n1p1
	if len(disk) > 4 && disk[len(disk)-2] == 'n' {
		return disk + "p" + strconv.Itoa(num)
	}
	return disk + strconv.Itoa(num)
}

func discoverPartitions(disk string) []string {
	var parts []string
	for i := 1; i <= 16; i++ {
		dev := partitionDevice(disk, i)
		if _, err := os.Stat(dev); err == nil {
			parts = append(parts, dev)
		}
	}
	return parts
}

// detectFilesystem uses blkid to determine the filesystem type of a partition.
// Falls back to "dd" (raw block copy) if blkid fails or returns an unknown type.
func detectFilesystem(dev string) string {
	out, err := exec.Command("blkid", "-s", "TYPE", "-o", "value", dev).Output()
	if err != nil {
		slog.Warn("blkid failed, falling back to dd", "dev", dev, "err", err)
		return "dd"
	}
	fs := strings.TrimSpace(string(out))
	slog.Info("detected filesystem", "dev", dev, "fs", fs)
	return fs
}

func printDebugBanner(resp *fogapi.HandshakeResponse) {
	slog.Info("=== fos-agent debug mode ===")
	slog.Info("taskId", "value", resp.TaskID)
	slog.Info("action", "value", resp.Action)
	slog.Info("imageId", "value", resp.ImageID)
	// Boot token is intentionally not printed.
}
