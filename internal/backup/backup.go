// Package backup locates the backup file a restore would actually reach for,
// and reports what shape it's in.
package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Format describes how a dump has to be fed back into a database.
type Format string

const (
	// FormatPlainSQL is a text dump replayed through the database's own
	// client (psql / mysql).
	FormatPlainSQL Format = "plain-sql"
	// FormatPostgresCustom is pg_dump's custom/tar format, which needs
	// pg_restore rather than psql.
	FormatPostgresCustom Format = "postgres-custom"
)

// File is a located backup, ready to restore.
type File struct {
	Path       string
	Size       int64
	ModTime    time.Time
	Compressed bool // gzip
	Encrypted  bool // GPG — decrypted before Compressed is ever checked
	Format     Format
}

func (f File) Age(now time.Time) time.Duration {
	return now.Sub(f.ModTime)
}

// HumanSize renders a byte count the way a person reads it, e.g. "14.4 KB".
func HumanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// Locate resolves pattern (a plain path or a glob) to the most recently
// modified match. Newest wins because that's the one you'd actually restore
// in an emergency — verifying an older file would prove the wrong thing.
func Locate(pattern string) (*File, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("bad path pattern %q: %w", pattern, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no backup file matches %q", pattern)
	}

	var newest string
	var newestInfo os.FileInfo
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			return nil, fmt.Errorf("stat %q: %w", match, err)
		}
		if info.IsDir() {
			continue
		}
		if newestInfo == nil || info.ModTime().After(newestInfo.ModTime()) {
			newest, newestInfo = match, info
		}
	}
	if newestInfo == nil {
		return nil, fmt.Errorf("no backup file matches %q (only directories)", pattern)
	}

	file := &File{
		Path:      newest,
		Size:      newestInfo.Size(),
		ModTime:   newestInfo.ModTime(),
		Encrypted: hasEncryptedSuffix(newest),
	}

	// Compression is encrypt-last convention's second-to-outermost suffix
	// (shop.sql.gz.gpg), so it has to be checked against the name with any
	// encrypted suffix stripped off first — otherwise "shop.sql.gz.gpg"
	// would never match ".gz" at all, since the name doesn't end with it.
	nameUnderEncryption := newest
	if file.Encrypted {
		nameUnderEncryption = strings.TrimSuffix(newest, filepath.Ext(newest))
	}
	file.Compressed = strings.HasSuffix(strings.ToLower(nameUnderEncryption), ".gz")

	format, err := detectFormat(newest, file.Compressed || file.Encrypted)
	if err != nil {
		return nil, err
	}
	file.Format = format

	return file, nil
}

// encryptedSuffixes are the conventional extensions for a GPG-encrypted
// file — binary OpenPGP output (.gpg, .pgp) or ASCII-armored (.asc).
var encryptedSuffixes = []string{".gpg", ".pgp", ".asc"}

func hasEncryptedSuffix(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range encryptedSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// pgDumpCustomMagic is the marker pg_dump writes at the start of its
// custom-format archives; psql can't read those, pg_restore has to.
var pgDumpCustomMagic = []byte("PGDMP")

func detectFormat(path string, opaque bool) (Format, error) {
	// A gzipped or encrypted dump is almost always plain SQL underneath;
	// pg_dump's custom format is already compressed internally, so people
	// rarely gzip (let alone encrypt) it on top. Reading the magic bytes
	// would mean decompressing/decrypting here, which isn't worth it — the
	// restore step streams through both anyway.
	if opaque {
		return FormatPlainSQL, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open backup %q: %w", path, err)
	}
	defer f.Close()

	header := make([]byte, len(pgDumpCustomMagic))
	n, err := f.Read(header)
	if err != nil && n == 0 {
		// An empty file is a broken backup, but that's the restore step's
		// verdict to deliver with a useful error — not a format question.
		return FormatPlainSQL, nil
	}

	if string(header[:n]) == string(pgDumpCustomMagic) {
		return FormatPostgresCustom, nil
	}
	return FormatPlainSQL, nil
}
