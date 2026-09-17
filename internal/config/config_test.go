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
