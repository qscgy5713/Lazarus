// Package verify runs the full proof for one backup: find it, restore it
// into a throwaway database, assert it carries real data, then throw the
// sandbox away.
package verify

import (
	"context"
	"fmt"
	"time"

	"lazarus/internal/backup"
	"lazarus/internal/check"
	"lazarus/internal/config"
	"lazarus/internal/restore"
	"lazarus/internal/sandbox"
)

// Stage names the step a target reached, so a failure says where it broke.
type Stage string

const (
	StageLocate  Stage = "locate"
	StageAge     Stage = "age"
	StageSandbox Stage = "sandbox"
	StageRestore Stage = "restore"
	StageRTO     Stage = "rto"
	StageChecks  Stage = "checks"
	StageDone    Stage = "done"
)

// Result is the verdict for one target.
type Result struct {
	Target          string
	Passed          bool
	Stage           Stage
	Err             error
	Backup          *backup.File
	Checks          []check.Result
	Duration        time.Duration
	RestoreDuration time.Duration
}

// Run verifies a single target end to end. It only returns an error-free,
// passing result when the backup actually restored and every check held.
func Run(ctx context.Context, target config.Target) Result {
	started := time.Now()
	result := Result{Target: target.Name, Stage: StageLocate}
	finish := func() Result {
		result.Duration = time.Since(started)
		return result
	}

	file, err := backup.Locate(target.Path)
	if err != nil {
		result.Err = err
		return finish()
	}
	result.Backup = file

	result.Stage = StageAge
	if target.MaxAge > 0 {
		if age := file.Age(time.Now()); age > target.MaxAge {
			result.Err = fmt.Errorf("newest backup is %s old, older than the %s limit", age.Round(time.Minute), target.MaxAge)
			return finish()
		}
	}

	result.Stage = StageSandbox
	sb, err := sandbox.Start(ctx, target.Engine, target.Image)
	if err != nil {
		result.Err = err
		return finish()
	}
	defer sb.Stop()

	result.Stage = StageRestore
	restoreStarted := time.Now()
	if _, err := restore.Run(ctx, sb, target.Engine, file); err != nil {
		result.Err = err
		return finish()
	}
	result.RestoreDuration = time.Since(restoreStarted)

	result.Stage = StageRTO
	if exceedsRTO(result.RestoreDuration, target.MaxRestoreDuration) {
		result.Err = rtoError(result.RestoreDuration, target.MaxRestoreDuration)
		return finish()
	}

	result.Stage = StageChecks
	result.Checks = check.RunAll(ctx, sb, target.Engine, target.Checks)
	for _, c := range result.Checks {
		if !c.Passed {
			result.Err = fmt.Errorf("check %q failed: %s", c.Name, c.Reason)
			return finish()
		}
	}

	result.Stage = StageDone
	result.Passed = true
	return finish()
}

// exceedsRTO reports whether a restore blew through its configured time
// budget. limit <= 0 means no budget was set.
func exceedsRTO(actual, limit time.Duration) bool {
	return limit > 0 && actual > limit
}

func rtoError(actual, limit time.Duration) error {
	// Millisecond precision: a sub-second restore against a very tight
	// limit would otherwise round down to a nonsensical "0s".
	return fmt.Errorf("restore took %s, longer than the %s RTO limit",
		actual.Round(time.Millisecond), limit)
}

// RunAll verifies every target, continuing past failures so one broken
// backup doesn't hide the state of the others.
func RunAll(ctx context.Context, targets []config.Target) []Result {
	results := make([]Result, 0, len(targets))
	for _, target := range targets {
		results = append(results, Run(ctx, target))
	}
	return results
}
