// Package actions implements the fos-agent action dispatcher and each
// supported action: register, deploy, capture, debug, and wipe.
package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	fogapi "github.com/nemvince/fos-next/internal/api"
	"github.com/nemvince/fos-next/internal/disk"
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
	targetDisk, err := disk.Primary()
	if err != nil {
		return reportFail(ctx, client, resp.TaskID, "disk detection failed: "+err.Error())
	}

	isResizable := resp.ImageType == "resizable"
	fixedSet := make(map[int]bool, len(resp.FixedSizePartitions))
	for _, n := range resp.FixedSizePartitions {
		fixedSet[n] = true
	}

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
	if err := partition.Restore(targetDisk, tableBytes); err != nil {
		return reportFail(ctx, client, resp.TaskID, "restoring partition table: "+err.Error())
	}

	totalBytes := resp.TotalBytes
	var transferred int64
	progressFn := makeProgressFn(ctx, client, resp.TaskID, &transferred, totalBytes)

	// For resizable images, expand the last partition table entry to fill the
	// disk before restoring content so that imaging.Expand sees the full
	// partition size when it grows the filesystem.
	if isResizable {
		if err := partition.ExpandLast(targetDisk); err != nil {
			slog.Warn("expand last partition table entry failed (non-fatal)", "err", err)
		}
	}

	for part := 1; part <= resp.PartCount; part++ {
		dev := disk.PartitionDevice(targetDisk, part)
		slog.Info("deploying partition", "part", part, "device", dev)

		var partErr error
		var resumeOffset int64
		for attempt := 0; attempt < 3; attempt++ {
			body, _, dlErr := client.DownloadPart(ctx, resp.ImageID, part, resumeOffset)
			if dlErr != nil {
				partErr = dlErr
				slog.Warn("download failed, retrying", "part", part, "attempt", attempt, "err", dlErr)
				time.Sleep(5 * time.Second)
				continue
			}

			// Peek the first byte: '{' means this is a JSON metadata sentinel
			// (e.g. a swap partition), not raw partclone data.
			peek := make([]byte, 1)
			n, _ := io.ReadFull(body, peek)
			full := io.MultiReader(bytes.NewReader(peek[:n]), body)

			if n == 1 && peek[0] == '{' {
				raw, readErr := io.ReadAll(full)
				body.Close()
				if readErr != nil {
					partErr = readErr
					slog.Warn("reading part sentinel failed, retrying", "part", part, "attempt", attempt, "err", readErr)
					time.Sleep(5 * time.Second)
					continue
				}
				var meta partMeta
				_ = json.Unmarshal(raw, &meta)
				if meta.Type == "swap" {
					slog.Info("recreating swap partition", "part", part, "device", dev, "uuid", meta.UUID)
					if mkErr := makeSwap(dev, meta.UUID); mkErr != nil {
						partErr = mkErr
						slog.Warn("mkswap failed, retrying", "part", part, "attempt", attempt, "err", mkErr)
						time.Sleep(5 * time.Second)
						continue
					}
				}
				partErr = nil
				break
			}

			fs := detectFilesystem(dev)
			imgErr := imaging.Restore(ctx, dev, fs, full, progressFn)
			body.Close()
			if imgErr != nil {
				partErr = imgErr
				slog.Warn("restore failed, retrying", "part", part, "attempt", attempt, "err", imgErr)
				time.Sleep(5 * time.Second)
				continue
			}

			// Re-detect the filesystem now that the partition has been
			// restored and has a valid superblock.  This is the type used
			// for expand, not for the partclone restore above.
			restoredFS := detectFilesystem(dev)

			// Clear the NTFS dirty bit so Windows doesn't run chkdsk on
			// the first boot after deploy.
			if strings.ToLower(restoredFS) == "ntfs" {
				imaging.NtfsFixDirty(dev)
			}

			// Expand the filesystem to fill its partition.  Only done for
			// resizable images and partitions not marked fixed-size.
			if isResizable && !fixedSet[part] {
				slog.Info("expanding filesystem after restore", "part", part, "device", dev, "fs", restoredFS)
				if expErr := imaging.Expand(ctx, dev, restoredFS); expErr != nil {
					slog.Warn("filesystem expand failed (non-fatal)", "part", part, "err", expErr)
				}
			}
			partErr = nil
			break
		}
		if partErr != nil {
			return reportFail(ctx, client, resp.TaskID,
				"partition "+strconv.Itoa(part)+" restore failed after 3 attempts: "+partErr.Error())
		}
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
	targetDisk, err := disk.Primary()
	if err != nil {
		return reportFail(ctx, client, resp.TaskID, "disk detection failed: "+err.Error())
	}

	isResizable := resp.ImageType == "resizable"

	// Backup partition table.
	tableBytes, err := partition.Backup(targetDisk)
	if err != nil {
		return reportFail(ctx, client, resp.TaskID, "partition table backup failed: "+err.Error())
	}
	if err := client.UploadPart(ctx, resp.ImageID, 0, bytesReader(tableBytes)); err != nil {
		return reportFail(ctx, client, resp.TaskID, "partition table upload failed: "+err.Error())
	}

	// Determine partition count from the disk.
	parts := disk.DiscoverPartitions(targetDisk)
	var fixedSizePartitions []int

	for i, dev := range parts {
		partNum := i + 1
		fs := detectFilesystem(dev)
		slog.Info("capturing partition", "part", partNum, "device", dev, "fs", fs)

		// Abort if Bitlocker encryption is detected — cloning an encrypted
		// volume produces an unusable image.
		if fs == "ntfs" && imaging.IsBitlockerEncrypted(dev) {
			return reportFail(ctx, client, resp.TaskID,
				"Bitlocker encryption detected on partition "+strconv.Itoa(partNum)+" ("+dev+") — cannot capture encrypted volume")
		}

		// Swap partitions cannot be cloned with partclone. Save the UUID as a
		// small JSON sentinel so Deploy can recreate the swap with mkswap.
		if fs == "swap" {
			uuid := readPartitionUUID(dev)
			slog.Info("swap partition — saving UUID instead of cloning", "part", partNum, "uuid", uuid)
			sentinel, _ := json.Marshal(partMeta{Type: "swap", UUID: uuid})
			if err := client.UploadPart(ctx, resp.ImageID, partNum, bytes.NewReader(sentinel)); err != nil {
				return reportFail(ctx, client, resp.TaskID, "swap sentinel upload failed for part "+strconv.Itoa(partNum)+": "+err.Error())
			}
			continue
		}

		if isResizable {
			// Clean NTFS Windows files that bloat the image.
			if fs == "ntfs" {
				slog.Info("cleaning NTFS Windows files before capture", "part", partNum)
				imaging.CleanNTFS(ctx, dev)
			}
			// Attempt to shrink the filesystem.
			if imaging.CanShrink(fs) {
				slog.Info("shrinking filesystem before capture", "part", partNum, "fs", fs)
				shrunk, shrinkErr := imaging.Shrink(ctx, dev, fs)
				if shrinkErr != nil {
					slog.Warn("filesystem shrink failed, capturing at full size", "part", partNum, "err", shrinkErr)
					fixedSizePartitions = append(fixedSizePartitions, partNum)
				} else if !shrunk {
					fixedSizePartitions = append(fixedSizePartitions, partNum)
				}
			} else {
				// XFS, F2FS, FAT etc. cannot be shrunk.
				fixedSizePartitions = append(fixedSizePartitions, partNum)
			}
		}

		pd := newProgressDisplay(partNum, len(parts), dev+" ("+fs+")")
		progressFn := func(pct int, bpm int64) {
			pd.update(pct, bpm)
			_ = client.ReportProgress(ctx, fogapi.ProgressRequest{
				TaskID:        resp.TaskID,
				Percent:       pct,
				BitsPerMinute: bpm,
			})
		}
		pr, pw := syncPipe()
		errCh := make(chan error, 1)
		go func() {
			err := imaging.Clone(ctx, dev, fs, pw, progressFn)
			pw.CloseWithError(err)
			errCh <- err
		}()
		if upErr := client.UploadPart(ctx, resp.ImageID, partNum, pr); upErr != nil {
			pd.done()
			return reportFail(ctx, client, resp.TaskID, "upload failed for part "+strconv.Itoa(partNum)+": "+upErr.Error())
		}
		if err := <-errCh; err != nil {
			pd.done()
			return reportFail(ctx, client, resp.TaskID, "partclone failed for part "+strconv.Itoa(partNum)+": "+err.Error())
		}
		pd.done()
	}

	// Report image metadata so future deploy operations know the image type
	// and which partitions are fixed-size.
	imageType := "fixed"
	if isResizable {
		imageType = "resizable"
	}
	if err := client.SetImageMeta(ctx, fogapi.ImageMetaRequest{
		TaskID:              resp.TaskID,
		ImageID:             resp.ImageID,
		ImageType:           imageType,
		FixedSizePartitions: fixedSizePartitions,
		PartCount:           len(parts),
	}); err != nil {
		slog.Warn("SetImageMeta failed (non-fatal)", "err", err)
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
	targetDisk, err := disk.Primary()
	if err != nil {
		return reportFail(ctx, client, resp.TaskID, "disk detection failed: "+err.Error())
	}
	if err := partition.Wipe(targetDisk, 1); err != nil {
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
