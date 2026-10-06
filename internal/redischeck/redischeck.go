// Package redischeck prepares Redis RDB backups for disposable sandbox verification.
package redischeck

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"lazarus/internal/backup"
	"lazarus/internal/decrypt"
)

// Prepare extracts and decrypts an RDB backup into a temporary directory containing dump.rdb,
// suitable for mounting into a Redis sandbox container at /data.
func Prepare(ctx context.Context, file *backup.File, gpgPassphrase string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "lazarus-redis-*")
	if err != nil {
		return "", nil, fmt.Errorf("create redis temp dir: %w", err)
	}

	var cleanups []func()
	cleanupAll := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
		_ = os.RemoveAll(tmpDir)
	}

	srcPath := file.Path
	if file.Encrypted {
		decryptedPath, cleanup, err := decrypt.Decrypt(ctx, srcPath, gpgPassphrase)
		if err != nil {
			cleanupAll()
			return "", nil, err
		}
		srcPath = decryptedPath
		cleanups = append(cleanups, cleanup)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		cleanupAll()
		return "", nil, fmt.Errorf("open redis backup %q: %w", srcPath, err)
	}
	defer src.Close()

	decomp, err := file.OpenDecompressor(src)
	if err != nil {
		cleanupAll()
		return "", nil, err
	}
	defer decomp.Close()
	reader := decomp

	dstPath := filepath.Join(tmpDir, "dump.rdb")
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		cleanupAll()
		return "", nil, fmt.Errorf("create temp dump.rdb: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, reader); err != nil {
		cleanupAll()
		return "", nil, fmt.Errorf("copy dump.rdb content: %w", err)
	}

	return tmpDir, cleanupAll, nil
}
