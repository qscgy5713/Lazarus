package s3fetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lazarus/internal/config"
)

func TestDownloader_Success(t *testing.T) {
	content := "dummy backup data"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Errorf("missing or invalid Authorization header: %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-amz-date") == "" {
			t.Errorf("missing x-amz-date header")
		}
		if r.URL.Path != "/test-bucket/backups/db.sql" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, content)
	}))
	defer server.Close()

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "downloaded.sql")

	d := New()
	cfg := &config.S3Config{
		Bucket:          "test-bucket",
		Key:             "backups/db.sql",
		Endpoint:        server.URL,
		Region:          "us-east-1",
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}

	err := d.Download(context.Background(), cfg, destPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if string(got) != content {
		t.Fatalf("expected content %q, got %q", content, string(got))
	}
}

func TestDownloader_Errors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "NoSuchKey")
	}))
	defer server.Close()

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "db.sql")

	d := New()
	cfg := &config.S3Config{
		Bucket:   "test-bucket",
		Key:      "missing.sql",
		Endpoint: server.URL,
	}

	err := d.Download(context.Background(), cfg, destPath)
	if err == nil {
		t.Fatalf("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 in error, got %v", err)
	}
}

func TestDownloader_WithProgress(t *testing.T) {
	content := "large backup dummy content for progress tracking"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, content)
	}))
	defer server.Close()

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "downloaded.sql")

	var progressBuf bytes.Buffer
	d := New().WithProgressWriter(&progressBuf)

	cfg := &config.S3Config{
		Bucket:   "test-bucket",
		Key:      "db.sql",
		Endpoint: server.URL,
	}

	err := d.Download(context.Background(), cfg, destPath)
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}

	if !strings.Contains(progressBuf.String(), "[s3] downloading...") {
		t.Errorf("expected progress output, got: %s", progressBuf.String())
	}
}
