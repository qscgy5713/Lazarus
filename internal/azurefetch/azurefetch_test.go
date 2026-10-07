package azurefetch

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

func TestAzureDownloadSuccess(t *testing.T) {
	content := "azure blob backup content"
	var authHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		if !strings.Contains(r.URL.Path, "mycontainer") || !strings.Contains(r.URL.Path, "backup.sql.gz") {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", "25")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer srv.Close()

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "downloaded.sql.gz")

	var progressBuf bytes.Buffer
	d := New().WithBaseURL(srv.URL).WithProgressWriter(&progressBuf)

	cfg := &config.AzureConfig{
		AccountName: "testaccount",
		Container:   "mycontainer",
		Blob:        "backup.sql.gz",
		AccountKey:  "dGVzdGtleQ==", // base64 encoded "testkey"
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

	if !strings.HasPrefix(authHeader, "SharedKey testaccount:") {
		t.Errorf("expected SharedKey auth header, got %q", authHeader)
	}

	if !strings.Contains(progressBuf.String(), "[azure] downloading...") {
		t.Errorf("progress output missing expected prefix: %s", progressBuf.String())
	}
}

func TestAzureDownloadNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	d := New().WithBaseURL(srv.URL)
	cfg := &config.AzureConfig{
		AccountName: "testaccount",
		Container:   "mycontainer",
		Blob:        "notfound.sql",
	}

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "notfound.sql")

	err := d.Download(context.Background(), cfg, destPath)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %v", err)
	}
}
