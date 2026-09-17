// Package fetch runs a configured shell command to pull a backup onto this
// machine before it's located. A plain local path only reaches a backup
// that's already sitting on the machine running Lazarus — most real backups
// live in S3, on a dedicated backup host, or somewhere else entirely, so
// this hands off to whatever command a team already uses to grab one from
// there, rather than Lazarus trying to speak every remote storage API
// itself.
package fetch

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Run executes command through a shell (so pipes, redirects, and multiple
// commands all work exactly as they would typed by hand), giving it up to
// timeout to finish. timeout <= 0 means no limit beyond ctx's own.
//
// A failure's error includes the command's own combined output — a fetch
// tool's own diagnostics ("Access Denied", "no such bucket", "Permission
// denied (publickey)") are exactly what's needed to fix it, and are
// otherwise lost the moment the process exits.
func Run(ctx context.Context, command string, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fetch command failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
