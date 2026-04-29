// Package api provides the fos-agent HTTP client for the fog-next boot API.
// All requests after handshake carry the boot token as a Bearer header.
// This package is the ONLY place in the agent that makes HTTP calls.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Client wraps the fog-next boot API with automatic token attachment and retries.
type Client struct {
	base         string
	httpClient   *http.Client // used for short API calls (30 s timeout)
	streamClient *http.Client // used for streaming upload/download (no timeout)
	token        string       // boot token; set after Handshake
}

// New creates a Client pointed at baseURL (e.g. "http://10.0.0.1").
func New(baseURL string) *Client {
	return &Client{
		base: baseURL + "/fog/api/v1/boot",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		// Dedicated transport for streaming operations:
		// - Own connection pool (no sharing with httpClient)
		// - TCP keepalive every 15 s so NAT/firewall doesn't drop idle connections
		//   during partclone pauses between filesystem blocks
		// - No Timeout on the Client — context cancellation handles abort
		streamClient: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   2 * time.Minute,
					KeepAlive: 15 * time.Second,
				}).DialContext,
				ResponseHeaderTimeout: 0, // no timeout waiting for response
				IdleConnTimeout:       0, // keep connections alive indefinitely
				DisableKeepAlives:     false,
			},
		},
	}
}

// ------------------------------------------------------------------
// Request / response types
// ------------------------------------------------------------------

// HandshakeRequest is sent by the agent to identify itself.
type HandshakeRequest struct {
	MACs []string `json:"macs"`
}

// HandshakeResponse is returned by the server after a successful handshake.
type HandshakeResponse struct {
	BootToken      string `json:"bootToken"`
	TaskID         string `json:"taskId"`
	Action         string `json:"action"`
	ImageID        string `json:"imageId,omitempty"`
	PartCount      int    `json:"partCount,omitempty"`
	TotalBytes     int64  `json:"totalBytes,omitempty"`
	StorageNodeURL string `json:"storageNodeUrl,omitempty"`

	// ImageType controls resize behaviour during capture and deploy.
	// "resizable" — shrink on capture, expand on deploy (default for new images)
	// "fixed"     — capture and restore at exact block size, no resize
	// "dd"        — raw block-for-block copy regardless of filesystem
	ImageType string `json:"imageType,omitempty"`

	// FixedSizePartitions lists partition numbers (1-based) that must NOT be
	// resized during deploy even when ImageType is "resizable".  The server
	// populates this from metadata reported by a prior capture run (e.g.
	// partitions that could not be shrunk: XFS, F2FS, recovery partitions).
	FixedSizePartitions []int `json:"fixedSizePartitions,omitempty"`
}

// RegisterRequest carries hardware inventory for an unknown host.
type RegisterRequest struct {
	MACs      []string `json:"macs"`
	CPUModel  string   `json:"cpuModel"`
	CPUCores  int      `json:"cpuCores"`
	RAMBytes  int64    `json:"ramBytes"`
	DiskBytes int64    `json:"diskBytes"`
	UUID      string   `json:"uuid,omitempty"`
}

// ProgressRequest reports imaging progress.
type ProgressRequest struct {
	TaskID           string `json:"taskId"`
	Percent          int    `json:"percent"`
	BitsPerMinute    int64  `json:"bitsPerMinute"`
	BytesTransferred int64  `json:"bytesTransferred"`
}

// CompleteRequest marks a task as done or failed.
type CompleteRequest struct {
	TaskID  string `json:"taskId"`
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// ImageMetaRequest carries capture-time metadata the server stores alongside
// the image so it can populate HandshakeResponse correctly during future
// deploy tasks.
type ImageMetaRequest struct {
	TaskID              string `json:"taskId"`
	ImageID             string `json:"imageId"`
	ImageType           string `json:"imageType"`           // "resizable" or "fixed"
	FixedSizePartitions []int  `json:"fixedSizePartitions"` // partitions that could not be shrunk
	PartCount           int    `json:"partCount"`           // total number of partitions captured
}

// ------------------------------------------------------------------
// API calls
// ------------------------------------------------------------------

// Handshake performs the boot handshake and stores the returned boot token.
func (c *Client) Handshake(ctx context.Context, req HandshakeRequest) (*HandshakeResponse, error) {
	var resp HandshakeResponse
	if err := c.post(ctx, "/handshake", req, &resp, false); err != nil {
		return nil, err
	}
	c.token = resp.BootToken
	slog.Info("handshake complete", "action", resp.Action, "taskId", resp.TaskID)
	return &resp, nil
}

// Register submits hardware inventory for an unrecognised host.
func (c *Client) Register(ctx context.Context, req RegisterRequest) error {
	return c.post(ctx, "/register", req, nil, false)
}

// ReportProgress posts incremental progress to the server.
func (c *Client) ReportProgress(ctx context.Context, req ProgressRequest) error {
	return c.post(ctx, "/progress", req, nil, true)
}

// SetImageMeta stores capture-time metadata for an image.  Called by the
// Capture action after all partitions have been uploaded so the server can
// record which partitions are fixed-size for future deploy operations.
func (c *Client) SetImageMeta(ctx context.Context, req ImageMetaRequest) error {
	return c.post(ctx, "/images/meta", req, nil, true)
}

// Complete marks the task as finished or failed.
func (c *Client) Complete(ctx context.Context, req CompleteRequest) error {
	return c.post(ctx, "/complete", req, nil, true)
}

// DownloadPart opens a streaming GET for a partition image part.
// The caller is responsible for closing the returned ReadCloser.
// rangeStart is the byte offset to resume from (0 for a fresh download).
func (c *Client) DownloadPart(ctx context.Context, imageID string, part int, rangeStart int64) (io.ReadCloser, int64, error) {
	url := fmt.Sprintf("%s/images/%s/download?part=%d", c.base, imageID, part)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if rangeStart > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", rangeStart))
	}

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("download part %d: server returned %s", part, resp.Status)
	}
	return resp.Body, resp.ContentLength, nil
}

// UploadPart streams partition image data to the server via chunked PUT.
func (c *Client) UploadPart(ctx context.Context, imageID string, part int, body io.Reader) error {
	url := fmt.Sprintf("%s/images/%s/upload?part=%d", c.base, imageID, part)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	// Use chunked transfer — do not set Content-Length.

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("upload part %d: server returned %s", part, resp.Status)
	}
	return nil
}

// ------------------------------------------------------------------
// internal helpers
// ------------------------------------------------------------------

func (c *Client) post(ctx context.Context, path string, body any, out any, auth bool) error {
	var attempt int
	backoff := 2 * time.Second
	for {
		err := c.doPost(ctx, path, body, out, auth)
		if err == nil {
			return nil
		}
		attempt++
		if attempt >= 5 {
			return err
		}
		slog.Warn("API call failed, retrying", "path", path, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
		}
	}
}

func (c *Client) doPost(ctx context.Context, path string, body any, out any, auth bool) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s: server returned %s", path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
