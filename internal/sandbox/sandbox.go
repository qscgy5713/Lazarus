// Package sandbox runs a throwaway database container to restore a backup
// into. Nothing here touches a real database — that's the point: the only way
// to know a backup restores is to actually restore it somewhere disposable.
package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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

// Start launches a container for engine using image and waits until the
// database inside it is accepting connections.
func Start(ctx context.Context, engine config.Engine, image string) (*Sandbox, error) {
	name := fmt.Sprintf("lazarus-verify-%d", time.Now().UnixNano())

	args := []string{"run", "--detach", "--name", name, "--rm"}
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
	default:
		return nil, fmt.Errorf("unsupported engine %q", engine)
	}
	args = append(args, image)

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
