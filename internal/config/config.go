// Package config parses the YAML file describing which backups to verify
// and what "a good restore" means for each of them.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"lazarus/internal/schedule"
)

type Engine string

const (
	EnginePostgres Engine = "postgres"
	EngineMySQL    Engine = "mysql"
	// EngineSQLite needs no sandbox container: a SQLite backup is already a
	// complete, self-contained database file, so verifying it is just
	// copying that file somewhere disposable and querying it directly.
	EngineSQLite  Engine = "sqlite"
	EngineRedis   Engine = "redis"
	EngineMongoDB Engine = "mongodb"
)

type Config struct {
	Notify Notify `yaml:"notify"`

	// StateFile is where per-target history (currently just the last known-
	// good backup size, for size_drift) is remembered between runs. Defaults
	// to lazarus-state.json next to wherever the tool is run from.
	StateFile string `yaml:"state_file"`

	// Parallelism caps how many targets are verified at once. Each target's
	// restore is already fully isolated (its own sandbox container, or its
	// own throwaway file for SQLite), so there's nothing to unify them for —
	// running them one at a time just makes a cron window longer than it
	// needs to be. Defaults to 4; set to 1 to restore the old sequential
	// behavior.
	Parallelism int `yaml:"parallelism"`

	// GPGPassphrase decrypts any target's backup that's GPG-encrypted
	// (detected by a .gpg/.pgp/.asc suffix on the path). There's no YAML
	// field for it — it only ever comes from LAZARUS_GPG_PASSPHRASE, so a
	// passphrase can never end up committed to a config file by mistake. A
	// backup encrypted to a private key already in the local GPG keyring
	// needs no passphrase at all, so this can be left unset.
	GPGPassphrase string `yaml:"-"`

	// AgeIdentity decrypts modern age-encrypted backups (.age).
	// Sourced from LAZARUS_AGE_IDENTITY or LAZARUS_AGE_KEY_FILE env vars.
	AgeIdentity string `yaml:"-"`

	Targets []Target `yaml:"targets"`
}

// Notify controls where a run's outcome gets reported. Without it Lazarus
// only speaks through its exit code, which nobody reads when it runs from
// cron at 4am.
type Notify struct {
	// WebhookURL is empty by default (notifications off). The
	// LAZARUS_WEBHOOK_URL environment variable overrides it, so the URL
	// doesn't have to live in a file.
	WebhookURL string `yaml:"webhook_url"`

	// APIKey is an optional bearer token sent with webhook requests. The
	// LAZARUS_API_KEY environment variable overrides it.
	APIKey string `yaml:"api_key"`

	// Format is "slack" (default), "discord", "generic" (raw JSON), "lazarus", "pagerduty", or "email".
	Format string `yaml:"format"`

	// When is "on_failure" (default), "always", or "never".
	//
	// "always" is worth considering: if Lazarus itself stops running, no
	// message looks exactly like every backup being fine.
	When string `yaml:"when"`

	// SMTP configures direct email delivery when format is "email".
	SMTP *SMTPConfig `yaml:"smtp"`

	// DashboardURL is the Control Plane address linked from Slack/Teams
	// alerts. Leave empty to omit the link button: guessing a default would
	// ship a dead link (e.g. localhost) to whoever reads the alert.
	DashboardURL string `yaml:"dashboard_url"`
}

// SMTPConfig configures email delivery via SMTP.
type SMTPConfig struct {
	Host     string   `yaml:"host"`
	Port     int      `yaml:"port"`
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
}

const (
	webhookURLEnvVar    = "LAZARUS_WEBHOOK_URL"
	apiKeyEnvVar        = "LAZARUS_API_KEY"
	gpgPassphraseEnvVar = "LAZARUS_GPG_PASSPHRASE"
)

const (
	defaultNotifyFormat = "slack"
	defaultNotifyWhen   = "on_failure"
)

// Target is one backup to verify: where to find it, what to restore it into,
// and what must be true afterwards for the backup to count as usable.
type Target struct {
	Name   string `yaml:"name"`
	Engine Engine `yaml:"engine"`

	// Path to the backup file. May be a glob (e.g. "/backups/db-*.sql.gz"),
	// in which case the most recently modified match is used — that's the
	// one a restore would actually reach for in an emergency.
	Path string `yaml:"path"`

	// FetchCommand, if set, runs through a shell before Path is located —
	// for a backup that lives in S3, on a remote host, or anywhere else a
	// plain local path can't reach. It's responsible for placing a file (or
	// files, for a glob Path) somewhere Path can then find; whatever tool
	// already fetches the backup elsewhere (aws s3 cp, scp, rclone, ...)
	// works here unchanged. A non-zero exit fails the target before Path is
	// even looked at.
	FetchCommand string `yaml:"fetch_command"`

	// FetchTimeout caps how long FetchCommand may run before it's killed and
	// the target fails. Without this, a stalled network mount or a
	// credential prompt nobody's there to answer would hang a cron run
	// indefinitely. Defaults to 5 minutes when FetchCommand is set and this
	// is left at 0; meaningless without a FetchCommand.
	FetchTimeout time.Duration `yaml:"fetch_timeout"`

	// MaxAge fails the target if the newest backup is older than this. A
	// restorable backup from three months ago is still a failed backup.
	MaxAge time.Duration `yaml:"max_age"`

	// MaxRestoreDuration fails the target if the restore step alone (not
	// counting sandbox startup or checks) takes longer than this. A backup
	// that restores correctly but takes 6 hours is still a failed backup if
	// the service it belongs to can only tolerate an hour of downtime — this
	// is how most teams find out their real recovery time, since restore
	// duration is otherwise measured for the first time during an incident.
	// 0 (the default) means no limit; the duration is still reported either way.
	MaxRestoreDuration time.Duration `yaml:"max_restore_duration"`

	// Image is the container image used as the throwaway restore sandbox.
	// Defaults per engine if empty.
	Image string `yaml:"image"`

	// SizeDrift fails the target if the newest backup is drastically smaller
	// than the last backup that fully passed verification for this target —
	// a sign that something upstream (a filter added to the dump command, a
	// truncated export) is silently producing less data than before, even
	// though the existing checks might still pass against what's left. nil
	// (the default) disables the check. The first run for a target only
	// establishes the baseline; there's nothing yet to compare against.
	SizeDrift *SizeDrift `yaml:"size_drift"`

	// S3, if set, pulls the backup file directly from an S3-compatible bucket
	// (AWS S3, Cloudflare R2, MinIO, etc.) to Path before verification.
	S3 *S3Config `yaml:"s3"`

	// GCS, if set, pulls the backup file directly from Google Cloud Storage.
	GCS *GCSConfig `yaml:"gcs"`

	// Azure, if set, pulls the backup file directly from Azure Blob Storage.
	Azure *AzureConfig `yaml:"azure"`

	// CleanupBackup, when true, removes the file at Path after verification
	// finishes to reclaim disk space (useful for pulled remote/S3 backups).
	CleanupBackup bool `yaml:"cleanup_backup"`

	// MemoryLimit sets the container memory limit (e.g. "512m", "2g").
	MemoryLimit string `yaml:"memory_limit"`

	// ReadyTimeout caps how long to wait for the sandbox database to accept
	// queries before failing the target (default 3m).
	ReadyTimeout time.Duration `yaml:"ready_timeout"`

	// CPUs sets the container CPU quota (e.g. "1.5", "2").
	CPUs string `yaml:"cpus"`

	// Tags allows categorizing and filtering targets (e.g. ["prod", "us-east"]).
	Tags []string `yaml:"tags"`

	// Schedule is an optional cron expression (e.g. "0 4 * * *", "@daily")
	// for daemon mode, evaluated in local time. Takes precedence over
	// Interval when both are set.
	Schedule string `yaml:"schedule"`

	// Interval is an optional duration interval (e.g. "24h") for daemon mode.
	Interval time.Duration `yaml:"interval"`

	// SLARTO defines the service level agreement restoration time objective (e.g. 5m, 1h).
	// Used by Control Plane for SLA compliance tracking.
	SLARTO time.Duration `yaml:"sla_rto"`

	// FallbackOnFailure, when true, automatically tries previous backup archives
	// if the latest backup fails to restore, calculating the true achievable RPO.
	FallbackOnFailure bool `yaml:"fallback_on_failure"`

	// MaxFallbackDepth is the maximum number of previous backups to attempt (default: 3).
	MaxFallbackDepth int `yaml:"max_fallback_depth"`

	// AutoSchemaCheck, when true, introspects tables and row counts automatically
	// to detect missing tables or sudden empty tables compared to the previous baseline.
	AutoSchemaCheck bool `yaml:"auto_schema_check"`

	// SchemaBaseline lists table names expected to exist when AutoSchemaCheck is true.
	SchemaBaseline []string `yaml:"schema_baseline"`

	// Network specifies container network mode (e.g. "none" to isolate sandbox from network).
	Network string `yaml:"network"`

	// ReadOnlyRootfs mounts the container root filesystem as read-only for security hardening.
	ReadOnlyRootfs bool `yaml:"read_only_rootfs"`

	// PreDrillCommand is an optional hook executed before verification begins.
	PreDrillCommand string `yaml:"pre_drill_command"`

	// PostDrillCommand is an optional hook executed after verification concludes.
	PostDrillCommand string `yaml:"post_drill_command"`

	// HooksTimeout limits execution duration of pre/post drill hooks (default: 5m).
	HooksTimeout time.Duration `yaml:"hooks_timeout"`

	// Chaos deliberately corrupts the primary backup archive to test if
	// FallbackOnFailure and alerting mechanisms correctly rescue the drill.
	Chaos ChaosConfig `yaml:"chaos"`

	// IncrementalPatches specifies glob patterns for incremental/differential
	// dumps applied in chronological order after the base backup is restored.
	IncrementalPatches []string `yaml:"incremental_patches"`

	// AgeIdentity is an optional target-level age private key to decrypt .age envelopes.
	AgeIdentity string `yaml:"age_identity"`

	// Remediation configures automated incident remediation playbooks triggered
	// when disaster recovery drills encounter failures.
	Remediation *RemediationConfig `yaml:"remediation"`

	Checks []Check `yaml:"checks"`
}

// RemediationConfig defines automated on-call disaster recovery playbooks
// triggered when a drill encounters a fatal failure.
type RemediationConfig struct {
	Command   string        `yaml:"command"`
	TriggerOn string        `yaml:"trigger_on"` // "failure" (default) or "critical_drift"
	Timeout   time.Duration `yaml:"timeout"`    // default 5m
}

// ChaosConfig controls deliberate backup corruption testing to verify fallback resilience.
type ChaosConfig struct {
	Enabled      bool `yaml:"enabled"`
	CorruptBytes int  `yaml:"corrupt_bytes"`
}

// GCSConfig configures fetching a backup from Google Cloud Storage.
type GCSConfig struct {
	Bucket          string `yaml:"bucket"`
	Object          string `yaml:"object"`
	CredentialsFile string `yaml:"credentials_file"`
	// Endpoint overrides https://storage.googleapis.com (e.g. a private
	// endpoint or fake-gcs-server for testing).
	Endpoint string `yaml:"endpoint"`
}

// AzureConfig configures fetching a backup from Azure Blob Storage.
type AzureConfig struct {
	AccountName string `yaml:"account_name"`
	Container   string `yaml:"container"`
	Blob        string `yaml:"blob"`
	AccountKey  string `yaml:"account_key"`
	// Endpoint overrides https://<account>.blob.core.windows.net, e.g.
	// "http://127.0.0.1:10000/devstoreaccount1" for Azurite.
	Endpoint string `yaml:"endpoint"`
}

// S3Config configures direct backup retrieval from AWS S3, Cloudflare R2,
// MinIO, or other S3-compatible object storage without external CLI dependencies.
type S3Config struct {
	Bucket          string `yaml:"bucket"`
	Key             string `yaml:"key"`
	Endpoint        string `yaml:"endpoint"`          // optional: custom endpoint URL for MinIO, R2, etc.
	Region          string `yaml:"region"`            // optional: defaults to "us-east-1"
	AccessKeyID     string `yaml:"access_key_id"`     // optional: falls back to AWS_ACCESS_KEY_ID env
	SecretAccessKey string `yaml:"secret_access_key"` // optional: falls back to AWS_SECRET_ACCESS_KEY env
	SessionToken    string `yaml:"session_token"`     // optional: falls back to AWS_SESSION_TOKEN env
}

// SizeDrift configures the backup-shrank-suspiciously check.
type SizeDrift struct {
	// MaxDecreasePct fails the target when the newest backup is smaller than
	// the last known-good backup by more than this percentage.
	MaxDecreasePct float64 `yaml:"max_decrease_pct"`
}

// Check is a SQL assertion run against the restored database. Exactly one of
// the expectations should be set; a query returning a single numeric value is
// compared against it.
type Check struct {
	Name         string        `yaml:"name"`
	SQL          string        `yaml:"sql"`
	Command      string        `yaml:"command"`
	Min          *int64        `yaml:"expect_min"`
	Max          *int64        `yaml:"expect_max"`
	Equal        *int64        `yaml:"expect_equal"`
	ExpectString *string       `yaml:"expect_string"`      // exact string match (ideal for checksums / hashes)
	Pattern      string        `yaml:"expect_pattern"`     // regex that the output must match
	NotPattern   string        `yaml:"expect_not_pattern"` // regex that the output must NOT match
	MaxRPO       time.Duration `yaml:"max_rpo"`            // maximum acceptable data lag (e.g. "1h", "24h")
	MaxLag       time.Duration `yaml:"max_lag"`            // alias for max_rpo
}

const (
	defaultPostgresImage = "postgres:16-alpine"
	defaultMySQLImage    = "mysql:8"
	defaultRedisImage    = "redis:7-alpine"
	defaultMongoDBImage  = "mongo:7.0"
)

const defaultStateFile = "lazarus-state.json"

const defaultParallelism = 4

const defaultFetchTimeout = 5 * time.Minute

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.applyDefaultsAndValidate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaultsAndValidate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("config has no targets")
	}

	if err := c.applyNotifyDefaults(); err != nil {
		return err
	}

	if c.StateFile == "" {
		c.StateFile = defaultStateFile
	}

	if c.Parallelism <= 0 {
		c.Parallelism = defaultParallelism
	}

	c.GPGPassphrase = os.Getenv(gpgPassphraseEnvVar)
	c.AgeIdentity = os.Getenv("LAZARUS_AGE_IDENTITY")
	if c.AgeIdentity == "" && os.Getenv("LAZARUS_AGE_KEY_FILE") != "" {
		if data, err := os.ReadFile(os.Getenv("LAZARUS_AGE_KEY_FILE")); err == nil {
			c.AgeIdentity = strings.TrimSpace(string(data))
		}
	}

	seen := make(map[string]bool, len(c.Targets))
	for i := range c.Targets {
		t := &c.Targets[i]

		if t.Name == "" {
			return fmt.Errorf("target %d: name is required", i)
		}
		if seen[t.Name] {
			return fmt.Errorf("duplicate target name %q", t.Name)
		}
		seen[t.Name] = true

		if t.Path == "" {
			return fmt.Errorf("target %q: path is required", t.Name)
		}

		if t.FetchCommand != "" && t.FetchTimeout <= 0 {
			t.FetchTimeout = defaultFetchTimeout
		}

		remoteSources := 0
		if t.FetchCommand != "" {
			remoteSources++
		}
		if t.S3 != nil {
			remoteSources++
		}
		if t.GCS != nil {
			remoteSources++
		}
		if t.Azure != nil {
			remoteSources++
		}
		if t.S3 != nil && t.FetchCommand != "" {
			return fmt.Errorf("target %q: cannot configure both s3 and fetch_command", t.Name)
		}
		if remoteSources > 1 {
			return fmt.Errorf("target %q: cannot configure multiple remote fetch methods (fetch_command, s3, gcs, azure)", t.Name)
		}

		if t.S3 != nil {
			if strings.ContainsAny(t.Path, "*?[") {
				return fmt.Errorf("target %q: s3 download path %q cannot contain glob wildcards", t.Name, t.Path)
			}
			if t.S3.Bucket == "" {
				return fmt.Errorf("target %q: s3.bucket is required", t.Name)
			}
			if t.S3.Key == "" {
				return fmt.Errorf("target %q: s3.key is required", t.Name)
			}
			if t.S3.Region == "" {
				t.S3.Region = "us-east-1"
			}
			if t.S3.AccessKeyID == "" {
				t.S3.AccessKeyID = os.Getenv("AWS_ACCESS_KEY_ID")
			}
			if t.S3.SecretAccessKey == "" {
				t.S3.SecretAccessKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
			}
			if t.S3.SessionToken == "" {
				t.S3.SessionToken = os.Getenv("AWS_SESSION_TOKEN")
			}
		}

		if t.GCS != nil {
			if strings.ContainsAny(t.Path, "*?[") {
				return fmt.Errorf("target %q: gcs download path %q cannot contain glob wildcards", t.Name, t.Path)
			}
			if t.GCS.Bucket == "" {
				return fmt.Errorf("target %q: gcs.bucket is required", t.Name)
			}
			if t.GCS.Object == "" {
				return fmt.Errorf("target %q: gcs.object is required", t.Name)
			}
			if t.GCS.CredentialsFile == "" {
				t.GCS.CredentialsFile = os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
			}
		}

		if t.Azure != nil {
			if strings.ContainsAny(t.Path, "*?[") {
				return fmt.Errorf("target %q: azure download path %q cannot contain glob wildcards", t.Name, t.Path)
			}
			if t.Azure.AccountName == "" {
				return fmt.Errorf("target %q: azure.account_name is required", t.Name)
			}
			if t.Azure.Container == "" {
				return fmt.Errorf("target %q: azure.container is required", t.Name)
			}
			if t.Azure.Blob == "" {
				return fmt.Errorf("target %q: azure.blob is required", t.Name)
			}
			if t.Azure.AccountKey == "" {
				t.Azure.AccountKey = os.Getenv("AZURE_STORAGE_KEY")
			}
		}

		switch t.Engine {
		case EnginePostgres:
			if t.Image == "" {
				t.Image = defaultPostgresImage
			}
		case EngineMySQL:
			if t.Image == "" {
				t.Image = defaultMySQLImage
			}
		case EngineSQLite:
			// No sandbox container, so no image to default.
		case EngineRedis:
			if t.Image == "" {
				t.Image = defaultRedisImage
			}
		case EngineMongoDB:
			if t.Image == "" {
				t.Image = defaultMongoDBImage
			}
		case "":
			return fmt.Errorf("target %q: engine is required (postgres, mysql, sqlite, redis or mongodb)", t.Name)
		default:
			return fmt.Errorf("target %q: unsupported engine %q (expected postgres, mysql, sqlite, redis or mongodb)", t.Name, t.Engine)
		}

		if t.Schedule != "" {
			if _, err := schedule.Parse(t.Schedule); err != nil {
				return fmt.Errorf("target %q: invalid schedule: %w", t.Name, err)
			}
		}

		if t.FallbackOnFailure && t.MaxFallbackDepth <= 0 {
			t.MaxFallbackDepth = 3
		}
		if t.Chaos.Enabled && t.Chaos.CorruptBytes <= 0 {
			t.Chaos.CorruptBytes = 64
		}
		if (t.PreDrillCommand != "" || t.PostDrillCommand != "") && t.HooksTimeout <= 0 {
			t.HooksTimeout = 5 * time.Minute
		}
		if t.Remediation != nil {
			if t.Remediation.Command == "" {
				return fmt.Errorf("target %q: remediation.command is required", t.Name)
			}
			if t.Remediation.TriggerOn == "" {
				t.Remediation.TriggerOn = "failure"
			}
			switch t.Remediation.TriggerOn {
			case "failure", "critical_drift":
			default:
				// A typo here used to mean the playbook silently never ran.
				return fmt.Errorf("target %q: remediation.trigger_on %q is not failure or critical_drift", t.Name, t.Remediation.TriggerOn)
			}
			if t.Remediation.Timeout <= 0 {
				t.Remediation.Timeout = 5 * time.Minute
			}
		}

		if t.CPUs != "" {
			t.CPUs = strings.TrimSpace(t.CPUs)
			if _, err := strconv.ParseFloat(t.CPUs, 64); err != nil {
				return fmt.Errorf("target %q: cpus must be a valid number (e.g. \"1.5\"), got %q", t.Name, t.CPUs)
			}
		}

		if t.SizeDrift != nil {
			if t.SizeDrift.MaxDecreasePct <= 0 || t.SizeDrift.MaxDecreasePct > 100 {
				return fmt.Errorf("target %q: size_drift.max_decrease_pct must be > 0 and <= 100", t.Name)
			}
		}

		if t.CleanupBackup && strings.ContainsAny(t.Path, "*?[") {
			return fmt.Errorf("target %q: cleanup_backup cannot be combined with glob pattern path %q to prevent deleting rolling backup archives", t.Name, t.Path)
		}

		if t.AgeIdentity == "" {
			t.AgeIdentity = c.AgeIdentity
		}

		for j := range t.Checks {
			if err := validateCheck(t.Name, j, &t.Checks[j]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Config) applyNotifyDefaults() error {
	// The environment wins so a webhook URL never has to be committed.
	if fromEnv := os.Getenv(webhookURLEnvVar); fromEnv != "" {
		c.Notify.WebhookURL = fromEnv
	}
	if fromEnv := os.Getenv(apiKeyEnvVar); fromEnv != "" {
		c.Notify.APIKey = fromEnv
	}

	if c.Notify.Format == "" {
		c.Notify.Format = defaultNotifyFormat
	}
	switch c.Notify.Format {
	case "slack", "discord", "telegram", "teams", "generic", "lazarus", "pagerduty", "email":
	default:
		return fmt.Errorf("notify.format %q is not slack, discord, telegram, teams, generic, lazarus, pagerduty or email", c.Notify.Format)
	}

	if c.Notify.When == "" {
		c.Notify.When = defaultNotifyWhen
	}
	switch c.Notify.When {
	case "on_failure", "always", "never":
	default:
		return fmt.Errorf("notify.when %q is not on_failure, always or never", c.Notify.When)
	}

	return nil
}

func validateCheck(targetName string, index int, c *Check) error {
	if c.SQL == "" && c.Command != "" {
		c.SQL = c.Command
	}
	if c.SQL == "" {
		return fmt.Errorf("target %q check %d: sql is required", targetName, index)
	}
	if c.Name == "" {
		c.Name = c.SQL
	}

	if c.MaxRPO == 0 && c.MaxLag > 0 {
		c.MaxRPO = c.MaxLag
	}

	expectations := 0
	for _, set := range []bool{c.Min != nil, c.Max != nil, c.Equal != nil, c.Pattern != "", c.NotPattern != "", c.ExpectString != nil, c.MaxRPO > 0} {
		if set {
			expectations++
		}
	}
	if expectations == 0 {
		return fmt.Errorf("target %q check %q: needs one of expect_min, expect_max, expect_equal, expect_string, expect_pattern, expect_not_pattern or max_rpo", targetName, c.Name)
	}
	if c.ExpectString != nil && expectations > 1 {
		return fmt.Errorf("target %q check %q: expect_string cannot be combined with other expectations", targetName, c.Name)
	}
	if c.MaxRPO > 0 && expectations > 1 {
		return fmt.Errorf("target %q check %q: max_rpo cannot be combined with other expectations", targetName, c.Name)
	}
	if expectations > 1 && (c.Pattern != "" || c.NotPattern != "") && (c.Equal != nil || c.Min != nil || c.Max != nil) {
		return fmt.Errorf("target %q check %q: pattern expectations cannot be combined with numeric expectations", targetName, c.Name)
	}
	if c.Equal != nil && (c.Min != nil || c.Max != nil) {
		return fmt.Errorf("target %q check %q: expect_equal can't be combined with expect_min/expect_max", targetName, c.Name)
	}
	if c.Pattern != "" {
		if _, err := regexp.Compile(c.Pattern); err != nil {
			return fmt.Errorf("target %q check %q: invalid expect_pattern regex: %w", targetName, c.Name, err)
		}
	}
	if c.NotPattern != "" {
		if _, err := regexp.Compile(c.NotPattern); err != nil {
			return fmt.Errorf("target %q check %q: invalid expect_not_pattern regex: %w", targetName, c.Name, err)
		}
	}
	return nil
}
