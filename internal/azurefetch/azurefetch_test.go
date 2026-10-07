package azurefetch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
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

// The SharedKey signature must cover the request's encoded path, so it holds
// for path-style endpoints (Azurite) and blob names needing escaping.
func TestSharedKeySignsEncodedPathStyleResource(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		sts := fmt.Sprintf("GET\n\n\n\n\n\n\n\n\n\n\n\nx-ms-date:%s\nx-ms-version:2020-10-02\n/acct%s", r.Header.Get("x-ms-date"), r.URL.EscapedPath())
		kb, _ := base64.StdEncoding.DecodeString(key)
		h := hmac.New(sha256.New, kb)
		h.Write([]byte(sts))
		want := "SharedKey acct:" + base64.StdEncoding.EncodeToString(h.Sum(nil))
		if r.Header.Get("Authorization") != want {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out")
	err := New().WithBaseURL(srv.URL+"/acct").Download(context.Background(), &config.AzureConfig{
		AccountName: "acct", Container: "c", Blob: "dir/my dump.sql", AccountKey: key,
	}, dest)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if gotPath != "/acct/c/dir/my%20dump.sql" {
		t.Errorf("path = %q, want segments escaped with literal /", gotPath)
	}
}

func TestInvalidAccountKeyFailsFast(t *testing.T) {
	err := New().WithBaseURL("http://127.0.0.1:1").Download(context.Background(), &config.AzureConfig{
		AccountName: "a", Container: "c", Blob: "b", AccountKey: "not base64!!",
	}, filepath.Join(t.TempDir(), "x"))
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("err = %v, want base64 error before any request", err)
	}
}
