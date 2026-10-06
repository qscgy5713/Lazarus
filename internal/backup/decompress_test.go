package backup

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestOpenDecompressorGzipAndZstd(t *testing.T) {
	orig := []byte("SELECT * FROM users WHERE active = 1;\nINSERT INTO logs VALUES ('restore');\n")

	// 1. Test Gzip
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(orig); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	gw.Close()

	gzFile := &File{
		Path:        "dump.sql.gz",
		Compressed:  true,
		Compression: CompressionGzip,
	}
	gzReader, err := gzFile.OpenDecompressor(&gzBuf)
	if err != nil {
		t.Fatalf("OpenDecompressor gzip error = %v", err)
	}
	defer gzReader.Close()
	gotGz, err := io.ReadAll(gzReader)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if !bytes.Equal(gotGz, orig) {
		t.Errorf("gzip decompressed = %q, want %q", gotGz, orig)
	}

	// 2. Test Zstd
	var zstBuf bytes.Buffer
	zw, err := zstd.NewWriter(&zstBuf)
	if err != nil {
		t.Fatalf("zstd writer error: %v", err)
	}
	if _, err := zw.Write(orig); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	zw.Close()

	zstFile := &File{
		Path:        "dump.sql.zst",
		Compressed:  true,
		Compression: CompressionZstd,
	}
	zstReader, err := zstFile.OpenDecompressor(&zstBuf)
	if err != nil {
		t.Fatalf("OpenDecompressor zstd error = %v", err)
	}
	defer zstReader.Close()
	gotZst, err := io.ReadAll(zstReader)
	if err != nil {
		t.Fatalf("read zstd: %v", err)
	}
	if !bytes.Equal(gotZst, orig) {
		t.Errorf("zstd decompressed = %q, want %q", gotZst, orig)
	}

	// 3. Test uncompressed
	rawFile := &File{
		Path:       "dump.sql",
		Compressed: false,
	}
	rawReader, err := rawFile.OpenDecompressor(bytes.NewReader(orig))
	if err != nil {
		t.Fatalf("OpenDecompressor raw error = %v", err)
	}
	defer rawReader.Close()
	gotRaw, err := io.ReadAll(rawReader)
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !bytes.Equal(gotRaw, orig) {
		t.Errorf("raw read = %q, want %q", gotRaw, orig)
	}
}
