package redischeck

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"lazarus/internal/backup"
)

func TestPrepareUncompressedRDB(t *testing.T) {
	dir := t.TempDir()
	rdbPath := filepath.Join(dir, "dump.rdb")
	data := []byte("REDIS0009fakecontent")
	if err := os.WriteFile(rdbPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	file := &backup.File{Path: rdbPath, Format: backup.FormatRedisRDB}
	tmpDir, cleanup, err := Prepare(context.Background(), file, "")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer cleanup()

	extracted := filepath.Join(tmpDir, "dump.rdb")
	content, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatalf("read extracted dump.rdb: %v", err)
	}
	if !bytes.Equal(content, data) {
		t.Errorf("extracted content mismatch")
	}
}

func TestPrepareCompressedRDB(t *testing.T) {
	dir := t.TempDir()
	gzPath := filepath.Join(dir, "dump.rdb.gz")
	data := []byte("REDIS0011gzippeddata")

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write(data)
	gw.Close()
	if err := os.WriteFile(gzPath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	file := &backup.File{Path: gzPath, Compressed: true, Format: backup.FormatRedisRDB}
	tmpDir, cleanup, err := Prepare(context.Background(), file, "")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer cleanup()

	extracted := filepath.Join(tmpDir, "dump.rdb")
	content, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatalf("read extracted dump.rdb: %v", err)
	}
	if !bytes.Equal(content, data) {
		t.Errorf("extracted content mismatch")
	}
}
