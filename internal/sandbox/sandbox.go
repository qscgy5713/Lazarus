// Package sandbox runs a throwaway database container to restore a backup
// into. Nothing here touches a real database — that's the point: the only way
// to know a backup restores is to actually restore it somewhere disposable.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"lazarus/internal/config"
)

const (
	// Credentials for the throwaway container. These never protect anything
	// real: the container is created, restored into, inspected and destroyed
	// within one run, and its port is never published to the host.
	dbUser     = "lazarus"
	dbPassword = "lazarus"
	dbName     = "lazarus_verify"

	readyTimeout      = 90 * time.Second
	readyPollInterval = time.Second
	// readyStreak is how many consecutive successful probes mean the
	// database is really up, not just mid-bootstrap. See ping().
	readyStreak = 3
)

// Sandbox is a running throwaway database container.
type Sandbox struct {
	Name   string
	Engine config.Engine
}

// nameCounter disambiguates sandbox names started within the same
// nanosecond — a real possibility once targets are verified in parallel,
// where a UnixNano timestamp alone isn't guaranteed unique.
var nameCounter atomic.Uint64

// Options configures optional sandbox resource constraints and storage mounts.
type Options struct {
	DataDir        string
	MemoryLimit    string
	CPUs           string
	Network        string
	ReadOnlyRootfs bool
	DropCaps       bool
}

// Start launches a container for engine using image and waits until the
// database inside it is accepting connections.
func Start(ctx context.Context, engine config.Engine, image string) (*Sandbox, error) {
	return StartWithOptions(ctx, engine, image, Options{})
}

// StartWithMount launches a container, optionally mounting a host directory to /data in the container.
func StartWithMount(ctx context.Context, engine config.Engine, image, dataDir string) (*Sandbox, error) {
	return StartWithOptions(ctx, engine, image, Options{DataDir: dataDir})
}

// StartWithOptions launches a container with fine-grained mounts and resource constraints.
func StartWithOptions(ctx context.Context, engine config.Engine, image string, opts Options) (*Sandbox, error) {
	name := fmt.Sprintf("lazarus-verify-%d-%d", time.Now().UnixNano(), nameCounter.Add(1))

	netMode := "none"
	if opts.Network != "" {
		netMode = opts.Network
	}
	args := []string{
		"run", "--detach", "--name", name, "--rm", "--network", netMode,
		"--label", "lazarus.sandbox=true",
		"--label", fmt.Sprintf("lazarus.created_at=%d", time.Now().Unix()),
	}
	if opts.ReadOnlyRootfs {
		args = append(args, "--read-only", "--tmpfs", "/tmp", "--tmpfs", "/run")
	}
	if opts.DropCaps {
		args = append(args, "--cap-drop=ALL")
	}
	if opts.MemoryLimit != "" {
		args = append(args, "--memory", opts.MemoryLimit)
	}
	if opts.CPUs != "" {
		args = append(args, "--cpus", opts.CPUs)
	}
	if opts.DataDir != "" {
		args = append(args, "-v", opts.DataDir+":/data")
	}

	switch engine {
	case config.EnginePostgres:
		args = append(args,
			"--env", "POSTGRES_USER="+dbUser,
			"--env", "POSTGRES_PASSWORD="+dbPassword,
			"--env", "POSTGRES_DB="+dbName,
		)
	case config.EngineMySQL:
		args = append(args,
			"--env", "MYSQL_ROOT_PASSWORD="+dbPassword,
			"--env", "MYSQL_DATABASE="+dbName,
		)
	case config.EngineRedis:
		// Redis in sandbox runs with no auth
	case config.EngineMongoDB:
		args = append(args,
			"--env", "MONGO_INITDB_ROOT_USERNAME="+dbUser,
			"--env", "MONGO_INITDB_ROOT_PASSWORD="+dbPassword,
			"--env", "MONGO_INITDB_DATABASE="+dbName,
		)
	default:
		return nil, fmt.Errorf("unsupported engine %q", engine)
	}
	args = append(args, image)
	if engine == config.EngineRedis {
		args = append(args, "redis-server", "--dir", "/data", "--appendonly", "no", "--save", "")
	}

	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("start %s sandbox (%s): %w: %s", engine, image, err, strings.TrimSpace(string(out)))
	}

	s := &Sandbox{Name: name, Engine: engine}

	if err := s.waitReady(ctx); err != nil {
		// Leaving a half-started container behind would leak resources on
		// every failed run, so tear it down before reporting the failure.
		s.Stop()
		return nil, err
	}
	return s, nil
}

// Stop destroys the container. Safe to call more than once.
func (s *Sandbox) Stop() {
	if s == nil || s.Name == "" {
		return
	}
	// Uses a fresh context: teardown must still happen when the run was
	// cancelled or timed out, which is exactly when ctx is already dead.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = exec.CommandContext(ctx, "docker", "rm", "--force", s.Name).Run()
	s.Name = ""
}

// Exec runs a command inside the sandbox, optionally piping stdin into it.
func (s *Sandbox) Exec(ctx context.Context, stdin string, args ...string) (string, error) {
	full := append([]string{"exec", "--interactive", s.Name}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// LogsTail fetches the last N lines of stdout/stderr from the sandbox container.
func (s *Sandbox) LogsTail(ctx context.Context, lines int) string {
	if s == nil || s.Name == "" || lines <= 0 {
		return ""
	}
	cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", fmt.Sprintf("%d", lines), s.Name)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (s *Sandbox) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(readyTimeout)
	var lastErr error
	consecutive := 0

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := s.ping(ctx); err != nil {
			lastErr = err
			consecutive = 0
		} else {
			consecutive++
			if consecutive >= readyStreak {
				return nil
			}
		}
		time.Sleep(readyPollInterval)
	}
	return fmt.Errorf("sandbox %s never became ready within %s: %w", s.Engine, readyTimeout, lastErr)
}

// ping runs a real query rather than pg_isready/mysqladmin ping. Both
// images bootstrap by starting a temporary server to run init scripts, then
// stopping it and starting the real one — during that window the lightweight
// probes report "ready" against a server that's about to vanish, and the
// restore then fails with a confusing "no such socket" error. Requiring a
// real query to succeed readyStreak times in a row rides out that restart.
func (s *Sandbox) ping(ctx context.Context) error {
	var args []string
	switch s.Engine {
	case config.EnginePostgres:
		args = []string{
			"psql", "--username", dbUser, "--dbname", dbName,
			"--tuples-only", "--no-align", "--command", "SELECT 1",
		}
	case config.EngineMySQL:
		args = []string{
			"mysql", "--user=root", "--password=" + dbPassword,
			"--skip-column-names", "--batch", "--execute=SELECT 1", dbName,
		}
	case config.EngineRedis:
		args = []string{"redis-cli", "PING"}
	case config.EngineMongoDB:
		args = []string{
			"mongosh", "--username", dbUser, "--password=" + dbPassword,
			"--authenticationDatabase", "admin", "--quiet", "--eval", "db.adminCommand('ping')",
		}
	}

	_, err := s.Exec(ctx, "", args...)
	return err
}

// DBName is the database a backup gets restored into.
func DBName() string { return dbName }

// User is the database user used inside the sandbox.
func User() string { return dbUser }

// Password is the sandbox database password.
func Password() string { return dbPassword }

// ReapOrphans scans for leftover lazarus sandbox containers that have exceeded
// maxAge and force-removes them to prevent host resource starvation.
// Also cleans up lingering /tmp/lazarus-sqlite-* files older than maxAge.
func ReapOrphans(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		maxAge = 2 * time.Hour
	}
	reaped := 0

	// 1. Docker containers with lazarus.sandbox=true label
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label=lazarus.sandbox=true",
		"--format", `{{.ID}}\t{{.Label "lazarus.created_at"}}`,
	).Output()

	if err == nil {
		cutoffUnix := time.Now().Add(-maxAge).Unix()
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, l := range lines {
			parts := strings.Split(l, "\t")
			if len(parts) >= 1 && strings.TrimSpace(parts[0]) != "" {
				id := strings.TrimSpace(parts[0])
				createdAtUnix := int64(0)
				if len(parts) >= 2 {
					createdAtUnix, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
				}
				// If createdAt timestamp is older than cutoff, or not recorded, reap it
				if createdAtUnix == 0 || createdAtUnix < cutoffUnix {
					_ = exec.CommandContext(ctx, "docker", "rm", "-f", id).Run()
					reaped++
				}
			}
		}
	}

	// 2. Lingering sqlite temp files in os.TempDir()
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "lazarus-sqlite-*"))
	cutoff := time.Now().Add(-maxAge)
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(m)
		}
	}

	return reaped, nil
}
