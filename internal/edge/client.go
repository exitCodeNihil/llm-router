package edge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// Version is the edge build version, reported to the control plane for skew
// detection. Overridable via -ldflags at build time.
var Version = "dev"

var edgeStart = time.Now()

// Client runs on an edge node: it keeps the snapshot holder fresh via
// long-polls and caches the last snapshot on disk (HMAC-sealed with the node
// token) so the node can serve through control-plane outages and restarts.
type Client struct {
	ControlPlaneURL string
	NodeToken       string
	CachePath       string
	Snapshots       *snapshot.Holder

	http *http.Client
}

func NewClient(controlPlaneURL, nodeToken, cachePath string, h *snapshot.Holder) *Client {
	return &Client{
		ControlPlaneURL: controlPlaneURL,
		NodeToken:       nodeToken,
		CachePath:       cachePath,
		Snapshots:       h,
		http:            &http.Client{Timeout: 70 * time.Second},
	}
}

// Run loads the disk cache, then long-polls forever.
func (c *Client) Run(ctx context.Context) {
	if strings.HasPrefix(c.ControlPlaneURL, "http://") && !strings.Contains(c.ControlPlaneURL, "localhost") &&
		!strings.Contains(c.ControlPlaneURL, "127.0.0.1") {
		slog.Warn("edge: control plane URL is plain HTTP — snapshots carry provider secrets; use HTTPS in production")
	}
	if wire, err := c.loadCache(); err == nil {
		c.Snapshots.Set(wire.Snapshot())
		slog.Info("edge: serving from cached snapshot", "version", wire.Version)
	} else if !os.IsNotExist(err) {
		slog.Warn("edge: snapshot cache unusable", "err", err)
	}
	for ctx.Err() == nil {
		if err := c.pollOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("edge: snapshot poll failed", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (c *Client) pollOnce(ctx context.Context) error {
	var version int64
	if s := c.Snapshots.Get(); s != nil {
		version = s.Version
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.ControlPlaneURL+"/edge/v1/snapshot?wait=55s", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.NodeToken)
	req.Header.Set("If-None-Match", `"`+strconv.FormatInt(version, 10)+`"`)
	// self-reported ops metrics (heartbeat). ponytail: mem from runtime; cpu omitted.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	memPct := 0.0
	if ms.Sys > 0 {
		memPct = float64(ms.Alloc) / float64(ms.Sys) * 100
	}
	req.Header.Set("X-Llmr-Node-Version", Version)
	req.Header.Set("X-Llmr-Uptime-S", strconv.FormatInt(int64(time.Since(edgeStart).Seconds()), 10))
	req.Header.Set("X-Llmr-Mem-Pct", strconv.FormatFloat(memPct, 'f', 1, 64))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	case http.StatusOK:
		var wire snapshot.Wire
		if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
			return fmt.Errorf("bad snapshot payload: %w", err)
		}
		c.Snapshots.Set(wire.Snapshot())
		if err := c.saveCache(&wire); err != nil {
			slog.Warn("edge: snapshot cache write failed", "err", err)
		}
		slog.Info("edge: snapshot updated", "version", wire.Version)
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("node token rejected by control plane")
	default:
		return fmt.Errorf("control plane returned %d", resp.StatusCode)
	}
}

func (c *Client) cacheKey() []byte {
	sum := sha256.Sum256([]byte("llmr-snapshot-cache:" + c.NodeToken))
	return sum[:]
}

// saveCache writes JSON + "\n" + hex HMAC, 0600.
func (c *Client) saveCache(w *snapshot.Wire) error {
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, c.cacheKey())
	mac.Write(data)
	payload := append(data, '\n')
	payload = append(payload, fmt.Sprintf("%x", mac.Sum(nil))...)
	if err := os.MkdirAll(filepath.Dir(c.CachePath), 0o700); err != nil {
		return err
	}
	tmp := c.CachePath + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.CachePath)
}

func (c *Client) loadCache() (*snapshot.Wire, error) {
	payload, err := os.ReadFile(c.CachePath)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(payload, '\n')
	if i < 0 {
		return nil, fmt.Errorf("malformed cache")
	}
	data, sigHex := payload[:i], payload[i+1:]
	mac := hmac.New(sha256.New, c.cacheKey())
	mac.Write(data)
	if fmt.Sprintf("%x", mac.Sum(nil)) != string(sigHex) {
		return nil, fmt.Errorf("cache HMAC mismatch (token changed or file tampered)")
	}
	var w snapshot.Wire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, err
	}
	return &w, nil
}
