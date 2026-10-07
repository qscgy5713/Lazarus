// Package inspect provides offline diagnostics and format inspection for backup files.
package inspect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"lazarus/internal/backup"
)

// Report contains diagnostic details of a located backup file.
type Report struct {
	Path        string             `json:"path"`
	FileName    string             `json:"file_name"`
	Size        int64              `json:"size"`
	HumanSize   string             `json:"human_size"`
	ModTime     time.Time          `json:"mod_time"`
	Age         time.Duration      `json:"age"`
	Format      backup.Format      `json:"format"`
	Compression backup.Compression `json:"compression"`
	Compressed  bool               `json:"compressed"`
	Encrypted   bool               `json:"encrypted"`
	MagicBytes  string             `json:"magic_bytes_hex"`
	SHA256      string             `json:"sha256"`
}

// InspectFile inspects an existing backup file or glob pattern without restoring it.
func InspectFile(path string) (*Report, error) {
	file, err := backup.Locate(path)
	if err != nil {
		return nil, fmt.Errorf("locate backup: %w", err)
	}

	f, err := os.Open(file.Path)
	if err != nil {
		return nil, fmt.Errorf("open backup file: %w", err)
	}
	defer f.Close()

	// Read magic bytes header
	header := make([]byte, 16)
	n, _ := f.Read(header)
	magicHex := hex.EncodeToString(header[:n])

	// Compute SHA-256 checksum
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek backup file: %w", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return nil, fmt.Errorf("hash backup file: %w", err)
	}
	sha256Str := hex.EncodeToString(hasher.Sum(nil))

	return &Report{
		Path:        file.Path,
		FileName:    filepath.Base(file.Path),
		Size:        file.Size,
		HumanSize:   backup.HumanSize(file.Size),
		ModTime:     file.ModTime,
		Age:         file.Age(time.Now()),
		Format:      file.Format,
		Compression: file.Compression,
		Compressed:  file.Compressed,
		Encrypted:   file.Encrypted,
		MagicBytes:  magicHex,
		SHA256:      sha256Str,
	}, nil
}
