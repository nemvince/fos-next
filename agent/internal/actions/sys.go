package actions

import (
	"io"
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
