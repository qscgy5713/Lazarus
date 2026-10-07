// Package backup locates the backup file a restore would actually reach for,
// and reports what shape it's in.
package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	FormatRedisRDB       Format = "redis-rdb"
	FormatMongoArchive   Format = "mongo-archive"
)

// Compression describes the compression algorithm wrapping a backup dump.
type Compression string

const (
	CompressionNone Compression = ""
	CompressionGzip Compression = "gzip"
	CompressionZstd Compression = "zstd"
)

// File is a located backup, ready to restore.
type File struct {
	Path        string
	Size        int64
	ModTime     time.Time
	Compressed  bool // true for gzip or zstd
	Compression Compression
	Encrypted   bool // GPG — decrypted before Compressed is ever checked
	Format      Format
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
	files, err := LocateAll(pattern)
	if err != nil {
		return nil, err
	}
	return files[0], nil
}

type fileCandidate struct {
	path string
	info os.FileInfo
}

// LocateAll resolves pattern to all matching backup files, sorted newest first.
func LocateAll(pattern string) ([]*File, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("bad path pattern %q: %w", pattern, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no backup file matches %q", pattern)
	}

	var candidates []fileCandidate
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			return nil, fmt.Errorf("stat %q: %w", match, err)
		}
		if info.IsDir() {
			continue
		}
		candidates = append(candidates, fileCandidate{path: match, info: info})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no backup file matches %q (only directories)", pattern)
	}

	// Sort newest first
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].info.ModTime().After(candidates[j].info.ModTime())
	})

	var out []*File
	for _, c := range candidates {
		f, err := parseBackupFile(c.path, c.info)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}

	return out, nil
}

func parseBackupFile(path string, info os.FileInfo) (*File, error) {
	file := &File{
		Path:      path,
		Size:      info.Size(),
		ModTime:   info.ModTime(),
		Encrypted: hasEncryptedSuffix(path),
	}

	nameUnderEncryption := path
	if file.Encrypted {
		nameUnderEncryption = strings.TrimSuffix(path, filepath.Ext(path))
	}
	lowerName := strings.ToLower(nameUnderEncryption)
	if strings.HasSuffix(lowerName, ".gz") {
		file.Compressed = true
		file.Compression = CompressionGzip
	} else if strings.HasSuffix(lowerName, ".zst") || strings.HasSuffix(lowerName, ".zstd") {
		file.Compressed = true
		file.Compression = CompressionZstd
	}

	format, err := detectFormat(path, file.Compressed || file.Encrypted)
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
var redisRDBMagic = []byte("REDIS")

func stripEnvelopeExtensions(path string) string {
	base := filepath.Base(path)
	for {
		rawExt := filepath.Ext(base)
		if rawExt == "" {
			break
		}
		ext := strings.ToLower(rawExt)
		if ext == ".gpg" || ext == ".pgp" || ext == ".asc" || ext == ".gz" || ext == ".zst" || ext == ".zstd" {
			base = base[:len(base)-len(rawExt)]
		} else {
			break
		}
	}
	return base
}

func detectFormat(path string, opaque bool) (Format, error) {
	stripped := stripEnvelopeExtensions(path)
	ext := strings.ToLower(filepath.Ext(stripped))
	if ext == ".rdb" {
		return FormatRedisRDB, nil
	}
	if ext == ".archive" {
		return FormatMongoArchive, nil
	}

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
	if n >= len(redisRDBMagic) && string(header[:len(redisRDBMagic)]) == string(redisRDBMagic) {
		return FormatRedisRDB, nil
	}
	return FormatPlainSQL, nil
}
