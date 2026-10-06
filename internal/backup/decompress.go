package backup

import (
	"compress/gzip"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// OpenDecompressor wraps an io.Reader in a decompressor based on the file's compression type.
// If the file is not compressed, it returns r wrapped as an io.NopCloser.
func (f *File) OpenDecompressor(r io.Reader) (io.ReadCloser, error) {
	if !f.Compressed {
		return io.NopCloser(r), nil
	}

	switch f.Compression {
	case CompressionZstd:
		decoder, err := zstd.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("backup %q is not readable as zstd: %w", f.Path, err)
		}
		return decoder.IOReadCloser(), nil
	case CompressionGzip:
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("backup %q is not readable as gzip: %w", f.Path, err)
		}
		return gz, nil
	default:
		// Fallback to gzip for backwards compatibility
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("backup %q is not readable as gzip: %w", f.Path, err)
		}
		return gz, nil
	}
}
