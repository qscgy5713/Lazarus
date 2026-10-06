// Package s3fetch provides direct downloading of backups from AWS S3,
// Cloudflare R2, MinIO, or any S3-compatible object storage using standard
// AWS Signature Version 4 (SigV4) without external CLI dependencies.
package s3fetch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lazarus/internal/config"
)

const (
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Downloader handles downloading objects from S3-compatible storage.
type Downloader struct {
	client         *http.Client
	ProgressWriter io.Writer
}

// New creates a new Downloader with default HTTP client.
func New() *Downloader {
	return &Downloader{
		client: &http.Client{
			// Do not set fixed http.Client.Timeout: for multi-gigabyte backup downloads,
			// a client timeout aborts long-running transfers. Cancellation is governed
			// by the passed-in context.Context instead.
		},
	}
}

// WithProgressWriter sets a writer to receive download progress updates.
func (d *Downloader) WithProgressWriter(w io.Writer) *Downloader {
	d.ProgressWriter = w
	return d
}

// Download fetches the configured S3 object directly to destPath.
func (d *Downloader) Download(ctx context.Context, cfg *config.S3Config, destPath string) error {
	if cfg == nil {
		return fmt.Errorf("s3 config is nil")
	}
	if cfg.Bucket == "" {
		return fmt.Errorf("s3 bucket is required")
	}
	if cfg.Key == "" {
		return fmt.Errorf("s3 key is required")
	}

	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	rawURL, host, err := buildURL(cfg)
	if err != nil {
		return fmt.Errorf("build s3 url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("create s3 request: %w", err)
	}

	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	// If credentials are provided, sign with AWS SigV4
	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		signRequest(req, cfg, host, region, dateStamp, amzDate)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("s3 get request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("s3 download failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}

	// Download to a temporary file in the same directory first, then atomically rename
	tmpFile, err := os.CreateTemp(destDir, ".lazarus-s3-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp download file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	reader := io.Reader(resp.Body)
	if d.ProgressWriter != nil && resp.ContentLength > 0 {
		reader = &progressReader{
			reader:    resp.Body,
			total:     resp.ContentLength,
			out:       d.ProgressWriter,
			startTime: time.Now(),
		}
	}

	if _, err := io.Copy(tmpFile, reader); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write backup file: %w", err)
	}

	if d.ProgressWriter != nil && resp.ContentLength > 0 {
		fmt.Fprintf(d.ProgressWriter, "\n")
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp backup file: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("finalize backup file: %w", err)
	}

	return nil
}

type progressReader struct {
	reader     io.Reader
	total      int64
	downloaded int64
	out        io.Writer
	startTime  time.Time
	lastReport time.Time
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.downloaded += int64(n)

	now := time.Now()
	if now.Sub(pr.lastReport) >= 300*time.Millisecond || pr.downloaded == pr.total {
		pr.lastReport = now
		elapsed := now.Sub(pr.startTime).Seconds()
		rateMB := 0.0
		if elapsed > 0 {
			rateMB = (float64(pr.downloaded) / (1024 * 1024)) / elapsed
		}
		pct := float64(pr.downloaded) * 100 / float64(pr.total)
		fmt.Fprintf(pr.out, "\r[s3] downloading... %.1f/%.1f MB (%.0f%%) at %.1f MB/s",
			float64(pr.downloaded)/(1024*1024),
			float64(pr.total)/(1024*1024),
			pct,
			rateMB)
	}

	return n, err
}

// buildURL constructs the target URL and host header for S3 or S3-compatible endpoints.
func buildURL(cfg *config.S3Config) (string, string, error) {
	key := strings.TrimPrefix(cfg.Key, "/")

	if cfg.Endpoint != "" {
		endpoint := strings.TrimRight(cfg.Endpoint, "/")
		u, err := url.Parse(endpoint)
		if err != nil {
			return "", "", fmt.Errorf("invalid endpoint: %w", err)
		}
		// Path-style: endpoint/bucket/key
		fullPath := fmt.Sprintf("/%s/%s", cfg.Bucket, key)
		u.Path = fullPath
		return u.String(), u.Host, nil
	}

	// Default AWS S3 endpoint
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	// Buckets with dots in their name cannot use virtual-hosted style over HTTPS
	// because wildcard TLS certificates (*.s3.amazonaws.com) only match a single
	// domain level. Fall back to path-style for dotted buckets so TLS verification succeeds.
	if strings.Contains(cfg.Bucket, ".") {
		var host string
		if region == "us-east-1" {
			host = "s3.amazonaws.com"
		} else {
			host = fmt.Sprintf("s3.%s.amazonaws.com", region)
		}
		return fmt.Sprintf("https://%s/%s/%s", host, cfg.Bucket, key), host, nil
	}

	if region == "us-east-1" {
		host := fmt.Sprintf("%s.s3.amazonaws.com", cfg.Bucket)
		return fmt.Sprintf("https://%s/%s", host, key), host, nil
	}
	host := fmt.Sprintf("%s.s3.%s.amazonaws.com", cfg.Bucket, region)
	return fmt.Sprintf("https://%s/%s", host, key), host, nil
}

func signRequest(req *http.Request, cfg *config.S3Config, host, region, dateStamp, amzDate string) {
	req.Header.Set("Host", host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", emptyPayloadHash)
	if cfg.SessionToken != "" {
		req.Header.Set("x-amz-security-token", cfg.SessionToken)
	}

	// Canonical headers
	var signedHeaders string
	var canonicalHeaders string
	if cfg.SessionToken != "" {
		signedHeaders = "host;x-amz-content-sha256;x-amz-date;x-amz-security-token"
		canonicalHeaders = fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\nx-amz-security-token:%s\n",
			host, emptyPayloadHash, amzDate, cfg.SessionToken)
	} else {
		signedHeaders = "host;x-amz-content-sha256;x-amz-date"
		canonicalHeaders = fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
			host, emptyPayloadHash, amzDate)
	}

	path := req.URL.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	canonicalURI := uriEncode(path, false)

	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		canonicalURI,
		"", // query string
		canonicalHeaders,
		signedHeaders,
		emptyPayloadHash,
	}, "\n")

	credScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credScope,
		hex.EncodeToString(sha256Hash([]byte(canonicalRequest))),
	}, "\n")

	signingKey := getSignatureKey(cfg.SecretAccessKey, dateStamp, region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		cfg.AccessKeyID, credScope, signedHeaders, signature)

	req.Header.Set("Authorization", authHeader)
}

func sha256Hash(data []byte) []byte {
	h := sha256.New()
	h.Write(data)
	return h.Sum(nil)
}

func hmacSHA256(key []byte, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func getSignatureKey(secret, dateStamp, regionName, serviceName string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(regionName))
	kService := hmacSHA256(kRegion, []byte(serviceName))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

// uriEncode implements RFC 3986 URI encoding for AWS SigV4 canonical URI.
func uriEncode(input string, encodeSlash bool) string {
	var result strings.Builder
	for i := 0; i < len(input); i++ {
		b := input[i]
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '-' || b == '~' || b == '.' {
			result.WriteByte(b)
		} else if b == '/' && !encodeSlash {
			result.WriteByte('/')
		} else {
			result.WriteString(fmt.Sprintf("%%%02X", b))
		}
	}
	return result.String()
}
