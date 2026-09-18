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
	"strings"
)

// Decrypt runs `gpg --decrypt` on path and writes the plaintext to a new
// throwaway file, returning its path. gpg detects on its own whether the
// file is symmetrically or public-key encrypted — this doesn't need to
// know which. The caller must run the returned cleanup func once done.
//
// passphrase unlocks a symmetric encryption, or a passphrase-protected
// private key already in the local GPG keyring. Empty means neither is
// needed — an unprotected key, or one gpg-agent already has cached. --batch
// mode means a passphrase that turns out to be needed anyway makes gpg fail
// cleanly and immediately rather than hang on an interactive prompt nobody
// is there to answer.
func Decrypt(ctx context.Context, path, passphrase string) (string, func(), error) {
	tmp, err := os.CreateTemp("", "lazarus-decrypt-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	cleanup := func() { os.Remove(tmpPath) }

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
