// Package azurefetch provides direct downloading of backups from Azure Blob Storage
// without requiring the az CLI.
package azurefetch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lazarus/internal/backup"
	"lazarus/internal/config"
)

// Downloader handles downloading objects from Azure Blob Storage.
type Downloader struct {
	client         *http.Client
	baseURL        string
	ProgressWriter io.Writer
}

// New creates a new Downloader.
func New() *Downloader {
	return &Downloader{
		client: &http.Client{},
	}
}

// WithProgressWriter sets a writer to receive download progress updates.
func (d *Downloader) WithProgressWriter(w io.Writer) *Downloader {
	d.ProgressWriter = w
	return d
}

// WithBaseURL overrides the endpoint URL (useful for testing).
func (d *Downloader) WithBaseURL(url string) *Downloader {
	d.baseURL = strings.TrimRight(url, "/")
	return d
}

// Download fetches the configured Azure Blob directly to destPath.
func (d *Downloader) Download(ctx context.Context, cfg *config.AzureConfig, destPath string) error {
	if cfg == nil {
		return fmt.Errorf("azure config is nil")
	}
	if cfg.AccountName == "" {
		return fmt.Errorf("azure account_name is required")
	}
	if cfg.Container == "" {
		return fmt.Errorf("azure container is required")
	}
	if cfg.Blob == "" {
		return fmt.Errorf("azure blob is required")
	}

	cleanBlob := strings.TrimPrefix(cfg.Blob, "/")
	var reqURL string
	if d.baseURL != "" {
		reqURL = fmt.Sprintf("%s/%s/%s", d.baseURL, url.PathEscape(cfg.Container), url.PathEscape(cleanBlob))
	} else {
		reqURL = fmt.Sprintf("https://%s.blob.core.windows.net/%s/%s", cfg.AccountName, url.PathEscape(cfg.Container), url.PathEscape(cleanBlob))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("build azure request: %w", err)
	}

	now := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("x-ms-date", now)
	req.Header.Set("x-ms-version", "2020-10-02")

	if cfg.AccountKey != "" {
		keyBytes, err := base64.StdEncoding.DecodeString(cfg.AccountKey)
		if err == nil {
			canonicalizedHeaders := fmt.Sprintf("x-ms-date:%s\nx-ms-version:2020-10-02", now)
			canonicalizedResource := fmt.Sprintf("/%s/%s/%s", cfg.AccountName, cfg.Container, cleanBlob)
			stringToSign := fmt.Sprintf("GET\n\n\n\n\n\n\n\n\n\n\n\n%s\n%s", canonicalizedHeaders, canonicalizedResource)

			h := hmac.New(sha256.New, keyBytes)
			h.Write([]byte(stringToSign))
			signature := base64.StdEncoding.EncodeToString(h.Sum(nil))

			req.Header.Set("Authorization", fmt.Sprintf("SharedKey %s:%s", cfg.AccountName, signature))
		}
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("execute azure request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("azure blob %q in container %q not found (HTTP 404)", cfg.Blob, cfg.Container)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("azure returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	dir := filepath.Dir(destPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create dest directory: %w", err)
		}
	} else {
		dir = "."
	}

	tmpFile, err := os.CreateTemp(dir, ".lazarus-az-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp download file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	var reader io.Reader = resp.Body
	if d.ProgressWriter != nil {
		pw := &progressTracker{
			writer:     d.ProgressWriter,
			totalBytes: resp.ContentLength,
			startTime:  time.Now(),
		}
		reader = io.TeeReader(resp.Body, pw)
	}

	if _, err := io.Copy(tmpFile, reader); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write downloaded azure blob: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	_ = os.Chmod(tmpPath, 0644)
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("move completed download to dest %q: %w", destPath, err)
	}

	if d.ProgressWriter != nil {
		fmt.Fprintln(d.ProgressWriter)
	}
	return nil
}

type progressTracker struct {
	writer      io.Writer
	totalBytes  int64
	copiedBytes int64
	startTime   time.Time
	lastLog     time.Time
}

func (pt *progressTracker) Write(p []byte) (int, error) {
	n := len(p)
	pt.copiedBytes += int64(n)

	now := time.Now()
	if now.Sub(pt.lastLog) >= 500*time.Millisecond || (pt.totalBytes > 0 && pt.copiedBytes == pt.totalBytes) {
		pt.lastLog = now
		elapsed := now.Sub(pt.startTime).Seconds()
		rate := float64(pt.copiedBytes) / elapsed
		if elapsed <= 0 {
			rate = 0
		}

		copiedHuman := backup.HumanSize(pt.copiedBytes)
		rateHuman := backup.HumanSize(int64(rate))

		if pt.totalBytes > 0 {
			pct := float64(pt.copiedBytes) / float64(pt.totalBytes) * 100
			totalHuman := backup.HumanSize(pt.totalBytes)
			fmt.Fprintf(pt.writer, "\r[azure] downloading... %s/%s (%.0f%%) at %s/s  ", copiedHuman, totalHuman, pct, rateHuman)
		} else {
			fmt.Fprintf(pt.writer, "\r[azure] downloading... %s at %s/s  ", copiedHuman, rateHuman)
		}
	}
	return n, nil
}
