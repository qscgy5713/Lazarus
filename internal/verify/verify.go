// Package verify runs the full proof for one backup: find it, restore it
// into a throwaway database, assert it carries real data, then throw the
// sandbox away.
package verify

import (
	"context"
	"fmt"
	"sync"
	"time"

	"lazarus/internal/backup"
	"lazarus/internal/check"
	"lazarus/internal/config"
	"lazarus/internal/fetch"
	"lazarus/internal/restore"
	"lazarus/internal/sandbox"
	"lazarus/internal/sqlitecheck"
	"lazarus/internal/state"
)

// Stage names the step a target reached, so a failure says where it broke.
type Stage string

const (
	StageFetch     Stage = "fetch"
	StageLocate    Stage = "locate"
	StageAge       Stage = "age"
	StageSizeDrift Stage = "size_drift"
	StageSandbox   Stage = "sandbox"
	StageRestore   Stage = "restore"
	StageRTO       Stage = "rto"
	StageChecks    Stage = "checks"
	StageDone      Stage = "done"
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

	// DebugHint is a ready-to-run command for connecting to this target's
	// sandbox (or SQLite temp file) after a failure, set only when
	// keepOnFailure asked for it to be kept instead of torn down. Empty
	// otherwise.
	DebugHint string
}

// Run verifies a single target end to end. It only returns an error-free,
// passing result when the backup actually restored and every check held.
// baseline is the backup size recorded from this target's last fully-passed
// run, if any (hasBaseline is false on a target's first-ever run).
// keepOnFailure, when true, leaves the sandbox container (or SQLite temp
// file) in place instead of tearing it down whenever the target ends up
// failing at or after the restore step, and populates the result's
// DebugHint with how to connect to it — for looking at exactly what did or
// didn't make it into the restored database, rather than guessing from the
// error message alone. gpgPassphrase decrypts the backup first when it's
// GPG-encrypted; ignored for a target whose backup isn't.
func Run(ctx context.Context, target config.Target, baseline int64, hasBaseline bool, keepOnFailure bool, gpgPassphrase string) (result Result) {
	started := time.Now()
	result = Result{Target: target.Name, Stage: StageFetch}
	// A named return value, not a "finish() Result" helper returning a plain
	// copy: the keep-on-failure defers below mutate result.DebugHint after
	// deciding whether to tear down the sandbox, and only a named return
	// value lets a defer's mutation actually reach the caller — a plain
	// `return someCopy` would have already locked in its value before any
	// deferred function ran.
	defer func() {
		result.Duration = time.Since(started)
	}()

	if target.FetchCommand != "" {
		if err := fetch.Run(ctx, target.FetchCommand, target.FetchTimeout); err != nil {
			result.Err = err
			return
		}
	}

	result.Stage = StageLocate
	file, err := backup.Locate(target.Path)
	if err != nil {
		result.Err = err
		return
	}
	result.Backup = file

	result.Stage = StageAge
	if target.MaxAge > 0 {
		if age := file.Age(time.Now()); age > target.MaxAge {
			result.Err = fmt.Errorf("newest backup is %s old, older than the %s limit", age.Round(time.Minute), target.MaxAge)
			return
		}
	}

	result.Stage = StageSizeDrift
	if target.SizeDrift != nil && hasBaseline {
		if exceedsSizeDrift(file.Size, baseline, target.SizeDrift.MaxDecreasePct) {
			result.Err = sizeDriftError(file.Size, baseline, target.SizeDrift.MaxDecreasePct)
			return
		}
	}

	// Everything past this point differs only in how a target gets "restored"
	// and how its checks run against the result: a container plus a real
	// client for Postgres/MySQL, or just a disposable file copy for SQLite.
	// runChecks is filled in by whichever path succeeds. RestoreDuration is
	// timed inside each branch, starting only once any container is already
	// up — sandbox startup isn't part of the restore this metric promises to
	// report (see max_restore_duration in the README).
	var runChecks func(context.Context) []check.Result

	if target.Engine == config.EngineSQLite {
		result.Stage = StageRestore
		restoreStarted := time.Now()

		path, cleanup, err := sqlitecheck.Prepare(ctx, file, gpgPassphrase)
		if err != nil {
			result.Err = err
			return
		}
		// result is captured live: by the time this runs, result.Err
		// reflects whatever the rest of Run ended up setting (or not), so
		// this correctly decides keep-vs-clean-up after the fact regardless
		// of which return path got there.
		defer func() {
			if keepOnFailure && result.Err != nil {
				result.DebugHint = fmt.Sprintf("sqlite3 %s", path)
				return
			}
			cleanup()
		}()

		if err := sqlitecheck.IntegrityCheck(ctx, path); err != nil {
			result.Err = err
			return
		}
		result.RestoreDuration = time.Since(restoreStarted)
		runChecks = func(ctx context.Context) []check.Result {
			return sqlitecheck.RunChecks(ctx, path, target.Checks)
		}
	} else {
		result.Stage = StageSandbox
		sb, err := sandbox.Start(ctx, target.Engine, target.Image)
		if err != nil {
			result.Err = err
			return
		}
		defer func() {
			if keepOnFailure && result.Err != nil {
				result.DebugHint = debugHint(sb)
				return
			}
			sb.Stop()
		}()

		result.Stage = StageRestore
		restoreStarted := time.Now()
		if _, err := restore.Run(ctx, sb, target.Engine, file, gpgPassphrase); err != nil {
			result.Err = err
			return
		}
		result.RestoreDuration = time.Since(restoreStarted)
		runChecks = func(ctx context.Context) []check.Result {
			return check.RunAll(ctx, sb, target.Engine, target.Checks)
		}
	}

	result.Stage = StageRTO
	if exceedsRTO(result.RestoreDuration, target.MaxRestoreDuration) {
		result.Err = rtoError(result.RestoreDuration, target.MaxRestoreDuration)
		return
	}

	result.Stage = StageChecks
	result.Checks = runChecks(ctx)
	for _, c := range result.Checks {
		if !c.Passed {
			result.Err = fmt.Errorf("check %q failed: %s", c.Name, c.Reason)
			return
		}
	}

	result.Stage = StageDone
	result.Passed = true
	return
}

// debugHint builds a ready-to-paste command for connecting to a kept
// sandbox. The container's port is never published to the host (see
// sandbox.Start), so this goes through `docker exec` rather than a direct
// client connection from outside the container.
func debugHint(sb *sandbox.Sandbox) string {
	switch sb.Engine {
	case config.EngineMySQL:
		return fmt.Sprintf("docker exec -it %s mysql --user=root --password=%s %s",
			sb.Name, sandbox.Password(), sandbox.DBName())
	default: // Postgres
		return fmt.Sprintf("docker exec -it %s psql --username %s --dbname %s",
			sb.Name, sandbox.User(), sandbox.DBName())
	}
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

// exceedsSizeDrift reports whether current is a suspicious shrink from
// baseline. Growth, or staying the same, never counts as drift — only a
// backup getting smaller signals data quietly going missing.
func exceedsSizeDrift(current, baseline int64, maxDecreasePct float64) bool {
	if baseline <= 0 || current >= baseline {
		return false
	}
	decreasePct := float64(baseline-current) / float64(baseline) * 100
	return decreasePct > maxDecreasePct
}

func sizeDriftError(current, baseline int64, maxDecreasePct float64) error {
	decreasePct := float64(baseline-current) / float64(baseline) * 100
	return fmt.Errorf("backup shrank %.0f%% since the last verified run (%s -> %s), exceeding the %.0f%% limit",
		decreasePct, backup.HumanSize(baseline), backup.HumanSize(current), maxDecreasePct)
}

// RunAll verifies every target, continuing past failures so one broken
// backup doesn't hide the state of the others. st supplies each target's
// size-drift baseline and is updated in place with the size from every
// target that fully passes — a run that fails never moves the baseline, so
// a genuinely broken backup can't quietly become the new normal.
//
// Up to parallelism targets are verified concurrently — each one's restore
// is already fully isolated (its own sandbox container, or its own
// throwaway file for SQLite), so there's no shared state between them to
// serialize on other than st, which is already safe for concurrent use.
// parallelism <= 0 is treated as 1. Results are returned in the same order
// as targets, regardless of which finished first. keepOnFailure is passed
// straight through to each target's Run — with several targets failing at
// once, each one that qualifies gets its own kept sandbox, left for the
// caller to inspect and clean up individually. gpgPassphrase is passed
// straight through to each target's Run.
func RunAll(ctx context.Context, targets []config.Target, st *state.State, parallelism int, keepOnFailure bool, gpgPassphrase string) []Result {
	if parallelism <= 0 {
		parallelism = 1
	}

	results := make([]Result, len(targets))
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup

	for i, target := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, target config.Target) {
			defer wg.Done()
			defer func() { <-sem }()

			var baseline int64
			var hasBaseline bool
			if ts, ok := st.Get(target.Name); ok {
				baseline, hasBaseline = ts.LastSizeBytes, true
			}

			result := Run(ctx, target, baseline, hasBaseline, keepOnFailure, gpgPassphrase)
			results[i] = result

			if result.Passed && result.Backup != nil {
				st.Set(target.Name, state.TargetState{
					LastSizeBytes: result.Backup.Size,
					UpdatedAt:     time.Now(),
				})
			}
		}(i, target)
	}

	wg.Wait()
	return results
}
