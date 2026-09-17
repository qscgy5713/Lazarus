package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lazarus.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadParsesTargetsAndChecks(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod-*.dump
    max_age: 24h
    checks:
      - name: users exist
        sql: SELECT count(*) FROM users
        expect_min: 1
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(cfg.Targets))
	}

	target := cfg.Targets[0]
	if target.Name != "prod-db" || target.Engine != EnginePostgres {
		t.Errorf("target = %+v, want name=prod-db engine=postgres", target)
	}
	if target.MaxAge != 24*time.Hour {
		t.Errorf("MaxAge = %v, want 24h", target.MaxAge)
	}
	if len(target.Checks) != 1 || target.Checks[0].Min == nil || *target.Checks[0].Min != 1 {
		t.Errorf("checks = %+v, want one check with expect_min=1", target.Checks)
	}
}

func TestLoadAppliesDefaultImagePerEngine(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: pg
    engine: postgres
    path: /backups/pg.sql
  - name: my
    engine: mysql
    path: /backups/my.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Targets[0].Image != defaultPostgresImage {
		t.Errorf("postgres image = %q, want %q", cfg.Targets[0].Image, defaultPostgresImage)
	}
	if cfg.Targets[1].Image != defaultMySQLImage {
		t.Errorf("mysql image = %q, want %q", cfg.Targets[1].Image, defaultMySQLImage)
	}
}

func TestLoadAcceptsSQLiteWithNoImage(t *testing.T) {
	// SQLite needs no sandbox container, so it should neither require an
	// image nor have one defaulted for it.
	path := writeConfig(t, `
targets:
  - name: local
    engine: sqlite
    path: /backups/app.db
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Targets[0].Image != "" {
		t.Errorf("Image = %q, want empty for a sqlite target", cfg.Targets[0].Image)
	}
}

func TestLoadKeepsExplicitImage(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: pg
    engine: postgres
    path: /backups/pg.sql
    image: postgres:14
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Targets[0].Image != "postgres:14" {
		t.Errorf("image = %q, want postgres:14 (explicit value must win)", cfg.Targets[0].Image)
	}
}

func TestCheckNameDefaultsToItsSQL(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: pg
    engine: postgres
    path: /backups/pg.sql
    checks:
      - sql: SELECT count(*) FROM users
        expect_min: 1
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Targets[0].Checks[0].Name; got != "SELECT count(*) FROM users" {
		t.Errorf("check name = %q, want it to fall back to the SQL", got)
	}
}

func TestLoadRejectsInvalidConfigs(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "no targets",
			body:    "targets: []",
			wantErr: "no targets",
		},
		{
			name:    "missing name",
			body:    "targets:\n  - engine: postgres\n    path: /b.sql",
			wantErr: "name is required",
		},
		{
			name:    "duplicate names",
			body:    "targets:\n  - name: a\n    engine: postgres\n    path: /b.sql\n  - name: a\n    engine: mysql\n    path: /c.sql",
			wantErr: "duplicate target name",
		},
		{
			name:    "missing path",
			body:    "targets:\n  - name: a\n    engine: postgres",
			wantErr: "path is required",
		},
		{
			name:    "missing engine",
			body:    "targets:\n  - name: a\n    path: /b.sql",
			wantErr: "engine is required",
		},
		{
			name:    "unsupported engine",
			body:    "targets:\n  - name: a\n    engine: mongodb\n    path: /b.sql",
			wantErr: "unsupported engine",
		},
		{
			name:    "check without sql",
			body:    "targets:\n  - name: a\n    engine: postgres\n    path: /b.sql\n    checks:\n      - expect_min: 1",
			wantErr: "sql is required",
		},
		{
			name:    "check without expectation",
			body:    "targets:\n  - name: a\n    engine: postgres\n    path: /b.sql\n    checks:\n      - sql: SELECT 1",
			wantErr: "needs one of expect_min",
		},
		{
			name:    "check mixing equal with range",
			body:    "targets:\n  - name: a\n    engine: postgres\n    path: /b.sql\n    checks:\n      - sql: SELECT 1\n        expect_equal: 1\n        expect_min: 1",
			wantErr: "can't be combined",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("Load() error = nil, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Load() error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yml")); err == nil {
		t.Fatal("Load() error = nil, want an error for a missing file")
	}
}

func TestNotifyDefaults(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: pg
    engine: postgres
    path: /backups/pg.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Notify.WebhookURL != "" {
		t.Errorf("WebhookURL = %q, want empty (notifications off by default)", cfg.Notify.WebhookURL)
	}
	if cfg.Notify.Format != defaultNotifyFormat {
		t.Errorf("Format = %q, want %q", cfg.Notify.Format, defaultNotifyFormat)
	}
	if cfg.Notify.When != defaultNotifyWhen {
		t.Errorf("When = %q, want %q", cfg.Notify.When, defaultNotifyWhen)
	}
}

func TestNotifyFromConfigFile(t *testing.T) {
	path := writeConfig(t, `
notify:
  webhook_url: https://hooks.example.com/abc
  format: discord
  when: always
targets:
  - name: pg
    engine: postgres
    path: /backups/pg.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Notify.WebhookURL != "https://hooks.example.com/abc" {
		t.Errorf("WebhookURL = %q", cfg.Notify.WebhookURL)
	}
	if cfg.Notify.Format != "discord" || cfg.Notify.When != "always" {
		t.Errorf("Notify = %+v, want discord/always", cfg.Notify)
	}
}

func TestWebhookURLEnvVarOverridesConfigFile(t *testing.T) {
	// The URL is a secret; it should never have to live in a committed file.
	t.Setenv(webhookURLEnvVar, "https://hooks.example.com/from-env")

	path := writeConfig(t, `
notify:
  webhook_url: https://hooks.example.com/from-file
targets:
  - name: pg
    engine: postgres
    path: /backups/pg.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Notify.WebhookURL != "https://hooks.example.com/from-env" {
		t.Errorf("WebhookURL = %q, want the environment value to win", cfg.Notify.WebhookURL)
	}
}

func TestNotifyRejectsUnknownFormatAndWhen(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "bad format",
			body:    "notify:\n  format: carrier-pigeon\ntargets:\n  - name: a\n    engine: postgres\n    path: /b.sql",
			wantErr: "notify.format",
		},
		{
			name:    "bad when",
			body:    "notify:\n  when: sometimes\ntargets:\n  - name: a\n    engine: postgres\n    path: /b.sql",
			wantErr: "notify.when",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("Load() error = nil, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadParsesMaxRestoreDuration(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
    max_restore_duration: 1h
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Targets[0].MaxRestoreDuration != time.Hour {
		t.Errorf("MaxRestoreDuration = %v, want 1h", cfg.Targets[0].MaxRestoreDuration)
	}
}

func TestMaxRestoreDurationDefaultsToZeroMeaningNoLimit(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Targets[0].MaxRestoreDuration != 0 {
		t.Errorf("MaxRestoreDuration = %v, want 0 (no limit) by default", cfg.Targets[0].MaxRestoreDuration)
	}
}

func TestStateFileDefaultsWhenNotSet(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.StateFile != defaultStateFile {
		t.Errorf("StateFile = %q, want default %q", cfg.StateFile, defaultStateFile)
	}
}

func TestStateFileKeepsExplicitValue(t *testing.T) {
	path := writeConfig(t, `
state_file: /var/lib/lazarus/state.json
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.StateFile != "/var/lib/lazarus/state.json" {
		t.Errorf("StateFile = %q, want the explicit value to win", cfg.StateFile)
	}
}

func TestParallelismDefaultsWhenNotSet(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Parallelism != defaultParallelism {
		t.Errorf("Parallelism = %d, want default %d", cfg.Parallelism, defaultParallelism)
	}
}

func TestParallelismKeepsExplicitValue(t *testing.T) {
	path := writeConfig(t, `
parallelism: 1
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Parallelism != 1 {
		t.Errorf("Parallelism = %d, want the explicit value 1 to win", cfg.Parallelism)
	}
}

func TestParallelismRejectsNonPositiveExplicitValue(t *testing.T) {
	// 0 is ambiguous between "not set" and "explicitly disabled", and a
	// negative worker count makes no sense, so both fall back to the
	// default rather than silently becoming 1 or panicking on a bad
	// channel size.
	for _, v := range []string{"0", "-1"} {
		path := writeConfig(t, "parallelism: "+v+"\ntargets:\n  - name: a\n    engine: postgres\n    path: /b.sql")

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Parallelism != defaultParallelism {
			t.Errorf("parallelism: %s -> Parallelism = %d, want it to fall back to the default %d", v, cfg.Parallelism, defaultParallelism)
		}
	}
}

func TestLoadParsesFetchCommand(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
    fetch_command: aws s3 cp s3://bucket/prod.sql /backups/prod.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Targets[0].FetchCommand != "aws s3 cp s3://bucket/prod.sql /backups/prod.sql" {
		t.Errorf("FetchCommand = %q", cfg.Targets[0].FetchCommand)
	}
}

func TestFetchTimeoutDefaultsWhenFetchCommandSetWithoutOne(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
    fetch_command: aws s3 cp s3://bucket/prod.sql /backups/prod.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Targets[0].FetchTimeout != defaultFetchTimeout {
		t.Errorf("FetchTimeout = %v, want the default %v", cfg.Targets[0].FetchTimeout, defaultFetchTimeout)
	}
}

func TestFetchTimeoutKeepsExplicitValue(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
    fetch_command: aws s3 cp s3://bucket/prod.sql /backups/prod.sql
    fetch_timeout: 30s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Targets[0].FetchTimeout != 30*time.Second {
		t.Errorf("FetchTimeout = %v, want the explicit 30s to win", cfg.Targets[0].FetchTimeout)
	}
}

func TestFetchTimeoutStaysZeroWithoutFetchCommand(t *testing.T) {
	// No fetch_command means fetch_timeout is meaningless — it shouldn't get
	// a default applied it'll never be used for.
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Targets[0].FetchTimeout != 0 {
		t.Errorf("FetchTimeout = %v, want 0 when there's no fetch_command", cfg.Targets[0].FetchTimeout)
	}
}

func TestLoadParsesSizeDrift(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
    size_drift:
      max_decrease_pct: 50
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	sd := cfg.Targets[0].SizeDrift
	if sd == nil || sd.MaxDecreasePct != 50 {
		t.Errorf("SizeDrift = %+v, want max_decrease_pct=50", sd)
	}
}

func TestSizeDriftNilWhenNotConfigured(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Targets[0].SizeDrift != nil {
		t.Errorf("SizeDrift = %+v, want nil when not configured (check disabled)", cfg.Targets[0].SizeDrift)
	}
}

func TestLoadRejectsInvalidSizeDriftThreshold(t *testing.T) {
	cases := []struct {
		name string
		pct  string
	}{
		{name: "zero", pct: "0"},
		{name: "negative", pct: "-10"},
		{name: "over 100", pct: "150"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "targets:\n  - name: a\n    engine: postgres\n    path: /b.sql\n    size_drift:\n      max_decrease_pct: " + tc.pct
			_, err := Load(writeConfig(t, body))
			if err == nil {
				t.Fatal("Load() error = nil, want an error for an out-of-range max_decrease_pct")
			}
			if !strings.Contains(err.Error(), "size_drift.max_decrease_pct") {
				t.Errorf("error = %q, want it to mention size_drift.max_decrease_pct", err)
			}
		})
	}
}
