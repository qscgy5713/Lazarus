package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lazarusBin is a real compiled binary, built once in TestMain — these
// tests exercise the actual CLI as a subprocess, the same way a person
// running --check-config from a terminal would, rather than calling main's
// internals directly (which os.Exit makes awkward to test in-process
// anyway).
var lazarusBin string

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "lazarus-test-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	lazarusBin = filepath.Join(tmpDir, "lazarus")
	build := exec.Command("go", "build", "-o", lazarusBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build lazarus: %v: %s\n", err, out)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lazarus.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func runLazarus(args ...string) (stdout, stderr string, exitCode int, err error) {
	cmd := exec.Command(lazarusBin, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	exitCode = 0
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else if runErr != nil {
		err = runErr
	}
	return outBuf.String(), errBuf.String(), exitCode, err
}

func TestCheckConfigValidConfigPassesAndSummarizesTargets(t *testing.T) {
	path := writeConfig(t, `
parallelism: 2
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
    max_age: 26h
    checks:
      - name: users exist
        sql: SELECT count(*) FROM users
        expect_min: 1
  - name: local-sqlite
    engine: sqlite
    path: /backups/app.db
`)

	stdout, stderr, exitCode, err := runLazarus("--config", path, "--check-config")
	if err != nil {
		t.Fatalf("run lazarus: %v (stderr: %s)", err, stderr)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 for a valid config (stderr: %s)", exitCode, stderr)
	}

	for _, want := range []string{"prod-db", "local-sqlite", "postgres", "sqlite", "26h0m0s", "checks: 1"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestCheckConfigInvalidConfigFailsWithExit2(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    checks:
      - sql: SELECT 1
`)

	_, stderr, exitCode, err := runLazarus("--config", path, "--check-config")
	if err != nil {
		t.Fatalf("run lazarus: %v", err)
	}
	if exitCode != 2 {
		t.Errorf("exit code = %d, want 2 for an invalid config (stderr: %s)", exitCode, stderr)
	}
	if !strings.Contains(stderr, "path is required") {
		t.Errorf("stderr = %q, want it to explain what's wrong", stderr)
	}
}

func TestCheckConfigNeverRunsFetchCommand(t *testing.T) {
	// A fetch_command that sleeps and then fails proves --check-config
	// neither runs it nor waits on it — if it did, this test would either
	// take 30s or exit non-zero.
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
    fetch_command: "sleep 30; exit 1"
    checks:
      - sql: SELECT 1
        expect_min: 1
`)

	start := time.Now()
	_, stderr, exitCode, err := runLazarus("--config", path, "--check-config")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("run lazarus: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0 — the fetch command's own exit 1 must never be reached (stderr: %s)", exitCode, stderr)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s, want --check-config to return almost instantly instead of running fetch_command", elapsed)
	}
}

func TestCheckConfigNeverTouchesDocker(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
    image: this-image-does-not-exist:latest
    checks:
      - sql: SELECT 1
        expect_min: 1
`)

	// A bogus image would fail loudly (and slowly, after a failed pull) if
	// --check-config actually tried to start a sandbox from it.
	start := time.Now()
	_, stderr, exitCode, err := runLazarus("--config", path, "--check-config")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("run lazarus: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0 (stderr: %s)", exitCode, stderr)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s, want --check-config to never attempt a Docker pull/run", elapsed)
	}
}

func TestCheckConfigRespectsTargetFilter(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: a
    engine: postgres
    path: /backups/a.sql
  - name: b
    engine: mysql
    path: /backups/b.sql
`)

	stdout, _, exitCode, err := runLazarus("--config", path, "--check-config", "--target", "b")
	if err != nil {
		t.Fatalf("run lazarus: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if strings.Contains(stdout, "- a (") {
		t.Errorf("stdout mentions target 'a', want only the filtered target 'b':\n%s", stdout)
	}
	if !strings.Contains(stdout, "- b (") {
		t.Errorf("stdout missing filtered target 'b':\n%s", stdout)
	}
}

func TestCheckConfigWithUnknownTargetFailsWithExit2(t *testing.T) {
	path := writeConfig(t, `
targets:
  - name: a
    engine: postgres
    path: /backups/a.sql
`)

	_, stderr, exitCode, err := runLazarus("--config", path, "--check-config", "--target", "nope")
	if err != nil {
		t.Fatalf("run lazarus: %v", err)
	}
	if exitCode != 2 {
		t.Errorf("exit code = %d, want 2 for an unknown --target (stderr: %s)", exitCode, stderr)
	}
}

func TestCheckConfigWithJSONProducesValidMachineReadableOutput(t *testing.T) {
	// Regression test: --json used to be silently ignored whenever it was
	// combined with --check-config, printing the human-readable summary
	// instead — exactly the kind of thing that quietly breaks a CI step
	// piping the output into a JSON parser.
	path := writeConfig(t, `
parallelism: 2
targets:
  - name: prod-db
    engine: postgres
    path: /backups/prod.sql
    fetch_command: "aws s3 cp s3://bucket/prod.sql /backups/prod.sql"
    max_age: 26h
    size_drift:
      max_decrease_pct: 50
    checks:
      - name: users exist
        sql: SELECT count(*) FROM users
        expect_min: 1
`)

	stdout, stderr, exitCode, err := runLazarus("--config", path, "--check-config", "--json")
	if err != nil {
		t.Fatalf("run lazarus: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", exitCode, stderr)
	}

	var decoded struct {
		Parallelism int `json:"parallelism"`
		Targets     []struct {
			Name                    string  `json:"name"`
			Engine                  string  `json:"engine"`
			FetchCommand            string  `json:"fetch_command"`
			Checks                  int     `json:"checks"`
			SizeDriftMaxDecreasePct float64 `json:"size_drift_max_decrease_pct"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, stdout)
	}

	if decoded.Parallelism != 2 {
		t.Errorf("parallelism = %d, want 2", decoded.Parallelism)
	}
	if len(decoded.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(decoded.Targets))
	}
	target := decoded.Targets[0]
	if target.Name != "prod-db" || target.Engine != "postgres" {
		t.Errorf("target = %+v, want name=prod-db engine=postgres", target)
	}
	if target.FetchCommand == "" {
		t.Error("fetch_command missing from JSON output")
	}
	if target.Checks != 1 {
		t.Errorf("checks = %d, want 1", target.Checks)
	}
	if target.SizeDriftMaxDecreasePct != 50 {
		t.Errorf("size_drift_max_decrease_pct = %v, want 50", target.SizeDriftMaxDecreasePct)
	}
}

func TestVersionFlagPrintsVersionWithoutNeedingAConfigFile(t *testing.T) {
	// --version has to work with no --config anywhere in sight — someone
	// checking what they just downloaded hasn't written a config yet.
	stdout, stderr, exitCode, err := runLazarus("--version")
	if err != nil {
		t.Fatalf("run lazarus: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", exitCode, stderr)
	}
	if !strings.Contains(stdout, "lazarus") {
		t.Errorf("stdout = %q, want it to mention lazarus", stdout)
	}
}
