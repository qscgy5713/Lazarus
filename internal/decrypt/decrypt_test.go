package decrypt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireGPG(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found on PATH")
	}
}

// encryptSymmetric produces a real GPG-encrypted file using the real gpg
// CLI, so these tests exercise the actual decrypt path against actual GPG
// output rather than a hand-built fixture.
func encryptSymmetric(t *testing.T, dir, name, plaintext, passphrase string) string {
	t.Helper()
	src := filepath.Join(dir, name+".plain")
	if err := os.WriteFile(src, []byte(plaintext), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, name+".gpg")
	cmd := exec.Command("gpg", "--batch", "--yes",
		"--passphrase", passphrase,
		"--symmetric", "--cipher-algo", "AES256",
		"-o", out, src)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("encrypt fixture: %v: %s", err, combined)
	}
	return out
}

func TestDecryptSymmetricWithCorrectPassphrase(t *testing.T) {
	requireGPG(t)
	dir := t.TempDir()
	encPath := encryptSymmetric(t, dir, "backup", "CREATE TABLE users (id int);\nINSERT INTO users VALUES (1);\n", "correct-horse-battery-staple")

	path, cleanup, err := Decrypt(context.Background(), encPath, "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	defer cleanup()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read decrypted file: %v", err)
	}
	if !strings.Contains(string(content), "INSERT INTO users VALUES (1);") {
		t.Errorf("decrypted content = %q, want it to contain the original plaintext", content)
	}
}

func TestDecryptFailsWithWrongPassphrase(t *testing.T) {
	requireGPG(t)
	dir := t.TempDir()
	encPath := encryptSymmetric(t, dir, "backup", "secret data", "the-real-passphrase")

	_, _, err := Decrypt(context.Background(), encPath, "totally-wrong-passphrase")
	if err == nil {
		t.Fatal("Decrypt() error = nil, want an error for a wrong passphrase")
	}
}

func TestDecryptFailsOnNonGPGFile(t *testing.T) {
	requireGPG(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "not-encrypted.txt")
	if err := os.WriteFile(path, []byte("this is plain text, not a gpg message"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := Decrypt(context.Background(), path, "irrelevant")
	if err == nil {
		t.Fatal("Decrypt() error = nil, want an error for a file that isn't GPG output at all")
	}
}

func TestDecryptCleansUpTempFileOnFailure(t *testing.T) {
	requireGPG(t)
	dir := t.TempDir()
	encPath := encryptSymmetric(t, dir, "backup", "secret data", "the-real-passphrase")

	before, _ := os.ReadDir(os.TempDir())

	_, _, err := Decrypt(context.Background(), encPath, "wrong-passphrase")
	if err == nil {
		t.Fatal("expected an error")
	}

	after, _ := os.ReadDir(os.TempDir())
	if len(after) > len(before) {
		t.Errorf("temp dir has %d entries after a failed decrypt, had %d before — a temp file was left behind", len(after), len(before))
	}
}

func TestDecryptRespectsContextCancellation(t *testing.T) {
	requireGPG(t)
	dir := t.TempDir()
	encPath := encryptSymmetric(t, dir, "backup", "secret data", "whatever")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Decrypt even starts

	start := time.Now()
	_, _, err := Decrypt(ctx, encPath, "whatever")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Decrypt() error = nil, want an error when ctx is already cancelled")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Decrypt() took %s, want it to fail almost instantly on a cancelled context", elapsed)
	}
}

func TestDecryptedTempFileIsNotWorldOrGroupReadable(t *testing.T) {
	requireGPG(t)
	dir := t.TempDir()
	encPath := encryptSymmetric(t, dir, "backup", "sensitive plaintext", "pw")

	path, cleanup, err := Decrypt(context.Background(), encPath, "pw")
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("decrypted temp file mode = %v, want no group/other permission bits — a backup that was encrypted for compliance reasons must not end up world-readable once decrypted", info.Mode().Perm())
	}
}
