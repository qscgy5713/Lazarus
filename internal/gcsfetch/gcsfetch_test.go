package gcsfetch

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lazarus/internal/config"
)

func TestGCSDownloadSuccess(t *testing.T) {
	content := "backup data from gcs payload"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "my-gcs-bucket") || !strings.Contains(r.URL.Path, "test.sql.gz") {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("alt") != "media" {
			t.Errorf("missing alt=media query param")
		}
		w.Header().Set("Content-Length", "28")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer srv.Close()

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "test.sql.gz")

	var progressBuf bytes.Buffer
	d := New().WithBaseURL(srv.URL).WithProgressWriter(&progressBuf)

	cfg := &config.GCSConfig{
		Bucket: "my-gcs-bucket",
		Object: "backups/test.sql.gz",
	}

	err := d.Download(context.Background(), cfg, destPath)
	if err != nil {
		t.Fatalf("Download() failed: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(got) != content {
		t.Errorf("got %q, want %q", string(got), content)
	}

	if !strings.Contains(progressBuf.String(), "[gcs] downloading...") {
		t.Errorf("progress output missing expected prefix: %s", progressBuf.String())
	}
}

func TestGCSDownloadNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	d := New().WithBaseURL(srv.URL)
	cfg := &config.GCSConfig{
		Bucket: "missing-bucket",
		Object: "missing.sql",
	}

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "missing.sql")

	err := d.Download(context.Background(), cfg, destPath)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %v", err)
	}
}
