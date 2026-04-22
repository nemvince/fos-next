package actions

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
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

// printProgressBar writes an in-place progress line to stderr.
// part/total are 1-based partition indices; bpm is bits per minute from partclone.
func printProgressBar(part, total, pct int, bpm int64) {
	const width = 30
	filled := pct * width / 100
	var bar string
	if filled >= width {
		bar = strings.Repeat("=", width)
	} else {
		bar = strings.Repeat("=", filled) + ">" + strings.Repeat(" ", width-filled-1)
	}
	mbpm := float64(bpm) / (8 * 1024 * 1024)
	fmt.Fprintf(os.Stderr, "\r  part %d/%d  [%s] %3d%%  %.1f MB/min   ",
		part, total, bar, pct, mbpm)
}
