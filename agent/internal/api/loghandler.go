package api

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	remoteBufferCap  = 2000 // max entries held before oldest are dropped
	remoteDrainBatch = 100  // max entries sent per flush tick
	remoteDrainTick  = time.Second
)

// LogEntry is the wire format for a single log record forwarded to fog-next.
type LogEntry struct {
	Time  string         `json:"time"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// sendLogsRequest is the JSON body posted to /boot/logs.
type sendLogsRequest struct {
	TaskID  string     `json:"taskId"`
	Entries []LogEntry `json:"entries"`
}

// RemoteHandler is a slog.Handler that fans out every record to a local
// delegate handler (for console output) AND buffers records for periodic
// forwarding to fog-next via the boot API.
//
// Use SetClient after a successful handshake to start streaming.
// Before SetClient is called all records are buffered; the buffer is flushed
// on the first SetClient call.
type RemoteHandler struct {
	delegate slog.Handler

	mu     sync.Mutex
	buf    []LogEntry // ring buffer (capped at remoteBufferCap)
	client *Client
	taskID string
}

// NewRemoteHandler wraps delegate.  Records are always forwarded to delegate
// synchronously; remote buffering is always active.
func NewRemoteHandler(delegate slog.Handler) *RemoteHandler {
	return &RemoteHandler{delegate: delegate, buf: make([]LogEntry, 0, remoteBufferCap)}
}

// SetClient activates remote log forwarding.  It should be called once after a
// successful handshake.  Any records that accumulated before the call are
// immediately queued for the first flush.
func (h *RemoteHandler) SetClient(c *Client, taskID string) {
	h.mu.Lock()
	h.client = c
	h.taskID = taskID
	h.mu.Unlock()

	go h.drainLoop()
}

// Enabled implements slog.Handler — delegates to the underlying handler.
func (h *RemoteHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.delegate.Enabled(ctx, level)
}

// Handle implements slog.Handler.
// It forwards the record to the delegate (non-blocking) and then appends a
// LogEntry to the in-memory buffer.  If the buffer is full the oldest entry is
// silently dropped so the agent is never blocked by a slow server.
func (h *RemoteHandler) Handle(ctx context.Context, r slog.Record) error {
	// Always forward to console/kmsg delegate first.
	_ = h.delegate.Handle(ctx, r)

	entry := recordToEntry(r)

	h.mu.Lock()
	if len(h.buf) >= remoteBufferCap {
		// Drop oldest entry to make room (ring-buffer behaviour).
		copy(h.buf, h.buf[1:])
		h.buf = h.buf[:len(h.buf)-1]
	}
	h.buf = append(h.buf, entry)
	h.mu.Unlock()

	return nil
}

// WithAttrs implements slog.Handler.
func (h *RemoteHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &RemoteHandler{
		delegate: h.delegate.WithAttrs(attrs),
		buf:      h.buf,
		client:   h.client,
		taskID:   h.taskID,
	}
}

// WithGroup implements slog.Handler.
func (h *RemoteHandler) WithGroup(name string) slog.Handler {
	return &RemoteHandler{
		delegate: h.delegate.WithGroup(name),
		buf:      h.buf,
		client:   h.client,
		taskID:   h.taskID,
	}
}

// drainLoop runs as a background goroutine after SetClient.
// It flushes up to remoteDrainBatch entries every remoteDrainTick.
// The loop terminates when the context is cancelled (not exposed here —
// agent lifetime is process lifetime so we rely on process exit).
func (h *RemoteHandler) drainLoop() {
	ticker := time.NewTicker(remoteDrainTick)
	defer ticker.Stop()
	for range ticker.C {
		h.flush()
	}
}

func (h *RemoteHandler) flush() {
	h.mu.Lock()
	if h.client == nil || len(h.buf) == 0 {
		h.mu.Unlock()
		return
	}
	n := len(h.buf)
	if n > remoteDrainBatch {
		n = remoteDrainBatch
	}
	batch := make([]LogEntry, n)
	copy(batch, h.buf[:n])
	h.buf = h.buf[n:]
	taskID := h.taskID
	client := h.client
	h.mu.Unlock()

	req := sendLogsRequest{TaskID: taskID, Entries: batch}
	if err := client.SendLogs(context.Background(), req); err != nil {
		// Write directly to the delegate to avoid an slog recursive call.
		fmt.Fprintf(delegateWriter(h.delegate), "time=%s level=WARN msg=\"remote log flush failed\" err=%v\n",
			time.Now().Format(time.RFC3339), err)
	}
}

// recordToEntry converts a slog.Record into the wire-format LogEntry.
func recordToEntry(r slog.Record) LogEntry {
	e := LogEntry{
		Time:  r.Time.UTC().Format(time.RFC3339Nano),
		Level: r.Level.String(),
		Msg:   r.Message,
	}
	r.Attrs(func(a slog.Attr) bool {
		if e.Attrs == nil {
			e.Attrs = make(map[string]any)
		}
		e.Attrs[a.Key] = a.Value.Any()
		return true
	})
	return e
}

// delegateWriter tries to extract an io.Writer from common handler types so
// we can write flush errors without going through slog (which would recurse).
// Falls back to /dev/null if extraction is not possible.
func delegateWriter(h slog.Handler) interface{ Write([]byte) (int, error) } {
	type writerGetter interface{ Writer() interface{ Write([]byte) (int, error) } }
	if wg, ok := h.(writerGetter); ok {
		return wg.Writer()
	}
	return nopWriter{}
}

type nopWriter struct{}

func (nopWriter) Write(b []byte) (int, error) { return len(b), nil }
