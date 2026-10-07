package inspect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "dump.sql")
	content := "-- PostgreSQL database dump\nSELECT 1;\n"
	if err := os.WriteFile(filePath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	report, err := InspectFile(filePath)
	if err != nil {
		t.Fatalf("InspectFile failed: %v", err)
	}

	if report.FileName != "dump.sql" {
		t.Errorf("FileName = %q, want dump.sql", report.FileName)
	}
	if report.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", report.Size, len(content))
	}
	if len(report.SHA256) != 64 {
		t.Errorf("SHA256 len = %d, want 64", len(report.SHA256))
	}
	if !strings.HasPrefix(report.MagicBytes, "2d2d") { // "--" in hex
		t.Errorf("MagicBytes = %q, expected prefix 2d2d", report.MagicBytes)
	}
}

func TestInspectFileNotFound(t *testing.T) {
	_, err := InspectFile("/non/existent/path/*.sql")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}
