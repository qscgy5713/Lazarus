// Package gcsfetch provides direct downloading of backups from Google Cloud Storage
// without requiring the gcloud CLI.
package gcsfetch

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lazarus/internal/backup"
	"lazarus/internal/config"
)

// Downloader handles downloading objects from Google Cloud Storage.
type Downloader struct {
	client         *http.Client
	baseURL        string
	ProgressWriter io.Writer
}

// New creates a new Downloader.
func New() *Downloader {
	return &Downloader{
		client:  &http.Client{},
		baseURL: "https://storage.googleapis.com",
	}
}

// WithProgressWriter sets a writer to receive download progress updates.
func (d *Downloader) WithProgressWriter(w io.Writer) *Downloader {
	d.ProgressWriter = w
	return d
}

// WithBaseURL overrides the endpoint URL (useful for testing).
func (d *Downloader) WithBaseURL(url string) *Downloader {
	d.baseURL = strings.TrimRight(url, "/")
	return d
}

// Download fetches the configured GCS object directly to destPath.
func (d *Downloader) Download(ctx context.Context, cfg *config.GCSConfig, destPath string) error {
	if cfg == nil {
		return fmt.Errorf("gcs config is nil")
	}
	if cfg.Bucket == "" {
		return fmt.Errorf("gcs bucket is required")
	}
	if cfg.Object == "" {
		return fmt.Errorf("gcs object is required")
	}

	escapedObj := url.PathEscape(strings.TrimPrefix(cfg.Object, "/"))
	reqURL := fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media", d.baseURL, url.PathEscape(cfg.Bucket), escapedObj)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("build gcs request: %w", err)
	}

	if cfg.CredentialsFile != "" {
		token, err := getAccessToken(ctx, d.client, cfg.CredentialsFile)
		if err != nil {
			// Falling back to an anonymous request would hide the real
			// problem behind a generic 401/403 from GCS.
			return fmt.Errorf("gcs credentials: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("execute gcs request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("gcs object %q in bucket %q not found (HTTP 404)", cfg.Object, cfg.Bucket)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("gcs returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	dir := filepath.Dir(destPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create dest directory: %w", err)
		}
	} else {
		dir = "."
	}

	tmpFile, err := os.CreateTemp(dir, ".lazarus-gcs-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp download file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	var reader io.Reader = resp.Body
	if d.ProgressWriter != nil {
		pw := &progressTracker{
			writer:     d.ProgressWriter,
			totalBytes: resp.ContentLength,
			startTime:  time.Now(),
		}
		reader = io.TeeReader(resp.Body, pw)
	}

	if _, err := io.Copy(tmpFile, reader); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write downloaded gcs object: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	_ = os.Chmod(tmpPath, 0644)
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("move completed download to dest %q: %w", destPath, err)
	}

	if d.ProgressWriter != nil {
		fmt.Fprintln(d.ProgressWriter)
	}
	return nil
}

type serviceAccountKey struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

var tokenCache struct {
	sync.Mutex
	token     string
	expiresAt time.Time
}

func getAccessToken(ctx context.Context, client *http.Client, credsFile string) (string, error) {
	tokenCache.Lock()
	if tokenCache.token != "" && time.Now().Before(tokenCache.expiresAt) {
		tok := tokenCache.token
		tokenCache.Unlock()
		return tok, nil
	}
	tokenCache.Unlock()

	data, err := os.ReadFile(credsFile)
	if err != nil {
		return "", err
	}
	var sa serviceAccountKey
	if err := json.Unmarshal(data, &sa); err != nil {
		return "", err
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return "", fmt.Errorf("invalid service account json")
	}

	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("failed to parse private key pem")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse pkcs8 private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("private key is not RSA")
	}

	now := time.Now().Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claimsJSON := fmt.Sprintf(`{"iss":"%s","scope":"https://www.googleapis.com/auth/devstorage.read_only","aud":"https://oauth2.googleapis.com/token","exp":%d,"iat":%d}`, sa.ClientEmail, now+3600, now)
	claims := base64.RawURLEncoding.EncodeToString([]byte(claimsJSON))

	sigInput := header + "." + claims
	hash := sha256.Sum256([]byte(sigInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	signedJWT := sigInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signedJWT},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request returned status %d", resp.StatusCode)
	}
	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	tokenCache.Lock()
	tokenCache.token = res.AccessToken
	tokenCache.expiresAt = time.Now().Add(time.Duration(res.ExpiresIn-60) * time.Second)
	tokenCache.Unlock()

	return res.AccessToken, nil
}

type progressTracker struct {
	writer      io.Writer
	totalBytes  int64
	copiedBytes int64
	startTime   time.Time
	lastLog     time.Time
}

func (pt *progressTracker) Write(p []byte) (int, error) {
	n := len(p)
	pt.copiedBytes += int64(n)

	now := time.Now()
	if now.Sub(pt.lastLog) >= 500*time.Millisecond || (pt.totalBytes > 0 && pt.copiedBytes == pt.totalBytes) {
		pt.lastLog = now
		elapsed := now.Sub(pt.startTime).Seconds()
		rate := float64(pt.copiedBytes) / elapsed
		if elapsed <= 0 {
			rate = 0
		}

		copiedHuman := backup.HumanSize(pt.copiedBytes)
		rateHuman := backup.HumanSize(int64(rate))

		if pt.totalBytes > 0 {
			pct := float64(pt.copiedBytes) / float64(pt.totalBytes) * 100
			totalHuman := backup.HumanSize(pt.totalBytes)
			fmt.Fprintf(pt.writer, "\r[gcs] downloading... %s/%s (%.0f%%) at %s/s  ", copiedHuman, totalHuman, pct, rateHuman)
		} else {
			fmt.Fprintf(pt.writer, "\r[gcs] downloading... %s at %s/s  ", copiedHuman, rateHuman)
		}
	}
	return n, nil
}
