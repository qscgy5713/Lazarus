// Package config parses the YAML file describing which backups to verify
// and what "a good restore" means for each of them.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Engine string

const (
	EnginePostgres Engine = "postgres"
	EngineMySQL    Engine = "mysql"
	// EngineSQLite needs no sandbox container: a SQLite backup is already a
	// complete, self-contained database file, so verifying it is just
	// copying that file somewhere disposable and querying it directly.
	EngineSQLite Engine = "sqlite"
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

	// Format is "slack" (default), "discord", or "generic" (raw JSON).
	Format string `yaml:"format"`

	// When is "on_failure" (default), "always", or "never".
	//
	// "always" is worth considering: if Lazarus itself stops running, no
	// message looks exactly like every backup being fine.
	When string `yaml:"when"`
}

const webhookURLEnvVar = "LAZARUS_WEBHOOK_URL"

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

	Checks []Check `yaml:"checks"`
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
	Name  string `yaml:"name"`
	SQL   string `yaml:"sql"`
	Min   *int64 `yaml:"expect_min"`
	Max   *int64 `yaml:"expect_max"`
	Equal *int64 `yaml:"expect_equal"`
}

const (
	defaultPostgresImage = "postgres:16-alpine"
	defaultMySQLImage    = "mysql:8"
)

const defaultStateFile = "lazarus-state.json"

const defaultParallelism = 4

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
		case "":
			return fmt.Errorf("target %q: engine is required (postgres, mysql or sqlite)", t.Name)
		default:
			return fmt.Errorf("target %q: unsupported engine %q (expected postgres, mysql or sqlite)", t.Name, t.Engine)
		}

		if t.SizeDrift != nil {
			if t.SizeDrift.MaxDecreasePct <= 0 || t.SizeDrift.MaxDecreasePct > 100 {
				return fmt.Errorf("target %q: size_drift.max_decrease_pct must be > 0 and <= 100", t.Name)
			}
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

	if c.Notify.Format == "" {
		c.Notify.Format = defaultNotifyFormat
	}
	switch c.Notify.Format {
	case "slack", "discord", "generic":
	default:
		return fmt.Errorf("notify.format %q is not slack, discord or generic", c.Notify.Format)
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
	if c.SQL == "" {
		return fmt.Errorf("target %q check %d: sql is required", targetName, index)
	}
	if c.Name == "" {
		c.Name = c.SQL
	}

	expectations := 0
	for _, set := range []bool{c.Min != nil, c.Max != nil, c.Equal != nil} {
		if set {
			expectations++
		}
	}
	if expectations == 0 {
		return fmt.Errorf("target %q check %q: needs one of expect_min, expect_max or expect_equal", targetName, c.Name)
	}
	if c.Equal != nil && (c.Min != nil || c.Max != nil) {
		return fmt.Errorf("target %q check %q: expect_equal can't be combined with expect_min/expect_max", targetName, c.Name)
	}
	return nil
}
