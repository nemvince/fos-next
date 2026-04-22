package actions

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// syscallExec execs a new process, replacing the current one.
// On Linux initramfs, fos-agent runs as PID 1, so this switches to a shell.
func syscallExec(path string) error {
	return syscall.Exec(path, []string{path}, []string{"TERM=linux", "HOME=/root", "PATH=/sbin:/bin:/usr/sbin:/usr/bin"})
}

// syncPipe returns a synchronised io.PipeReader/Writer pair.
func syncPipe() (io.ReadCloser, *io.PipeWriter) {
	return io.Pipe()
}

// bytesReader wraps a byte slice in an io.Reader.
func bytesReader(b []byte) io.Reader {
	return io.LimitReader(bytesReaderOf(b), int64(len(b)))
}

type byteSliceReader struct {
	data []byte
	pos  int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func bytesReaderOf(b []byte) io.Reader {
	return &byteSliceReader{data: b}
}

// readAll is a thin wrapper around io.ReadAll.
func readAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

// partMeta is the JSON sentinel uploaded in place of partclone data for
// partition types that do not need to be cloned (currently only swap).
type partMeta struct {
	Type string `json:"type"`
	UUID string `json:"uuid,omitempty"`
}

// readPartitionUUID reads the UUID of a partition using blkid.
func readPartitionUUID(dev string) string {
	out, err := exec.Command("blkid", "-s", "UUID", "-o", "value", dev).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// makeSwap initialises a swap partition, preserving the original UUID if known.
func makeSwap(dev, uuid string) error {
	var cmd *exec.Cmd
	if uuid != "" {
		cmd = exec.Command("mkswap", "-U", uuid, dev)
	} else {
		cmd = exec.Command("mkswap", dev)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkswap %s: %w\n%s", dev, err, out)
	}
	return nil
}

// ProgressDisplay renders an in-place TUI progress block to stderr.
// It keeps track of start time so it can compute ETA.
//
// Usage:
//
//	pd := newProgressDisplay(partNum, totalParts, label)
//	pd.update(pct, bpm)  // call from imaging.ProgressFunc
//	pd.done()            // print final newline after the part finishes
type ProgressDisplay struct {
	part      int
	total     int
	label     string
	startedAt time.Time
	lastPct   int
}

func newProgressDisplay(part, total int, label string) *ProgressDisplay {
	// Print the partition header once before the bar starts.
	fmt.Fprintf(os.Stderr, "\n  Partition %d/%d  (%s)\n", part, total, label)
	return &ProgressDisplay{
		part:      part,
		total:     total,
		label:     label,
		startedAt: time.Now(),
	}
}

// update redraws the progress line in-place.
func (p *ProgressDisplay) update(pct int, bpm int64) {
	p.lastPct = pct
	const barWidth = 40
	filled := pct * barWidth / 100
	var bar string
	if filled >= barWidth {
		bar = strings.Repeat("█", barWidth)
	} else {
		bar = strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
	}

	mbps := float64(bpm) / (8 * 1024 * 1024) // bits/min → MiB/s  (÷8 bytes, ÷60 sec, ÷1024² — approximated via /min unit)
	// bpm is bits-per-minute from partclone; convert to MiB/min for display.
	mbpm := float64(bpm) / (8 * 1024 * 1024)
	_ = mbps

	var eta string
	elapsed := time.Since(p.startedAt)
	if pct > 0 && pct < 100 {
		totalEstimate := time.Duration(float64(elapsed) / (float64(pct) / 100.0))
		remaining := totalEstimate - elapsed
		if remaining > 0 {
			eta = fmt.Sprintf("  ETA %s", remaining.Round(time.Second))
		}
	} else if pct >= 100 {
		eta = fmt.Sprintf("  done in %s", elapsed.Round(time.Second))
	}

	fmt.Fprintf(os.Stderr, "\r  [%s] %3d%%  %5.1f MiB/min%s  ",
		bar, pct, mbpm, eta)
}

// done moves to the next line after a partition completes.
func (p *ProgressDisplay) done() {
	if p.lastPct < 100 {
		p.update(100, 0)
	}
	fmt.Fprintln(os.Stderr)
}
