// Package decrypt runs GPG to turn an encrypted backup into a plain file
// the rest of Lazarus can treat exactly like any other backup. Many real
// backups are GPG-encrypted at rest for compliance reasons — without this,
// Lazarus can't even attempt to verify them, which is a worse failure mode
// than a verification that runs and reports something wrong.
package decrypt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// IsAgeEncrypted reports whether path has an .age extension.
func IsAgeEncrypted(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".age")
}

// Decrypt runs either `gpg --decrypt` or `age --decrypt` on path and writes the plaintext
// to a new throwaway file, returning its path.
func Decrypt(ctx context.Context, path, passphrase string) (string, func(), error) {
	return DecryptWithOptions(ctx, path, passphrase, "")
}

// DecryptWithOptions decrypts path using GPG or age according to file extension.
func DecryptWithOptions(ctx context.Context, path, passphrase, ageIdentity string) (string, func(), error) {
	if IsAgeEncrypted(path) {
		return decryptAge(ctx, path, ageIdentity)
	}
	return decryptGPG(ctx, path, passphrase)
}

func decryptAge(ctx context.Context, path, identity string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "lazarus-decrypt-age-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp age decrypt dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	tmpPath := filepath.Join(tmpDir, "decrypted.bak")
	args := []string{"--decrypt", "-o", tmpPath}

	var idPath string
	if identity != "" {
		idFile := filepath.Join(tmpDir, "identity.txt")
		if err := os.WriteFile(idFile, []byte(identity), 0o600); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("write age identity: %w", err)
		}
		args = append(args, "-i", idFile)
		idPath = idFile
	}

	args = append(args, path)

	cmd := exec.CommandContext(ctx, "age", args...)
	out, err := cmd.CombinedOutput()
	if idPath != "" {
		_ = os.Remove(idPath)
	}

	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("age decrypt failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	if err := os.Chmod(tmpPath, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("restrict permissions on decrypted file: %w", err)
	}

	return tmpPath, cleanup, nil
}

func decryptGPG(ctx context.Context, path, passphrase string) (string, func(), error) {
	// Create a private 0700 directory so decrypted data is never exposed
	// to other local users, even during gpg execution before chmod.
	tmpDir, err := os.MkdirTemp("", "lazarus-decrypt-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp decrypt dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	tmpPath := filepath.Join(tmpDir, "decrypted.bak")
	args := []string{"--batch", "--yes", "--output", tmpPath}

	// The passphrase travels over its own file descriptor (3), never as a
	// command-line argument (visible to anyone on the box via `ps`) and
	// never mixed into the same stream as the ciphertext itself.
	var extraFiles []*os.File
	if passphrase != "" {
		pr, pw, err := os.Pipe()
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("create passphrase pipe: %w", err)
		}
		if _, err := pw.WriteString(passphrase); err != nil {
			pr.Close()
			pw.Close()
			cleanup()
			return "", nil, fmt.Errorf("write passphrase: %w", err)
		}
		pw.Close()
		extraFiles = []*os.File{pr}
		args = append(args, "--passphrase-fd", "3")
	}

	args = append(args, "--decrypt", path)

	cmd := exec.CommandContext(ctx, "gpg", args...)
	cmd.ExtraFiles = extraFiles

	out, err := cmd.CombinedOutput()
	for _, f := range extraFiles {
		f.Close()
	}
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("gpg decrypt failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// gpg's --output re-creates the file rather than writing into the one
	// os.CreateTemp already made, so it lands back at the umask's default
	// (typically 0644, world-readable) instead of CreateTemp's 0600 — which
	// would leave the plaintext of a backup that was encrypted specifically
	// for compliance reasons readable by any other local user while it sits
	// in this temp file.
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("restrict permissions on decrypted file: %w", err)
	}

	return tmpPath, cleanup, nil
}
