// Package verify runs the full proof for one backup: find it, restore it
// into a throwaway database, assert it carries real data, then throw the
// sandbox away.
package verify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lazarus/internal/azurefetch"
	"lazarus/internal/backup"
	"lazarus/internal/check"
	"lazarus/internal/config"
	"lazarus/internal/fetch"
	"lazarus/internal/gcsfetch"
	"lazarus/internal/redischeck"
	"lazarus/internal/restore"
	"lazarus/internal/s3fetch"
	"lazarus/internal/sandbox"
	"lazarus/internal/schema"
	"lazarus/internal/sqlitecheck"
	"lazarus/internal/state"
)

// Stage names the step a target reached, so a failure says where it broke.
type Stage string

const (
	StagePreHook   Stage = "pre_hook"
	StageFetch     Stage = "fetch"
	StageLocate    Stage = "locate"
	StageAge       Stage = "age"
	StageSizeDrift Stage = "size_drift"
	StageSandbox   Stage = "sandbox"
	StageRestore   Stage = "restore"
	StageRTO       Stage = "rto"
	StageSchema    Stage = "schema"
	StageChecks    Stage = "checks"
	StagePostHook  Stage = "post_hook"
	StageDone      Stage = "done"
)

// Result is the verdict for one target.
type Result struct {
	Target          string
	Tags            []string
	Passed          bool
	Stage           Stage
	Err             error
	Backup          *backup.File
	Checks          []check.Result
	Duration        time.Duration
	RestoreDuration time.Duration

	// SLARTO is the target's configured recovery-time objective, carried on
	// the result so downstream reports can judge SLA compliance without the
	// original config. Zero means no SLA was configured.
	SLARTO time.Duration

	// LogsTail contains the trailing logs from the container if failure occurred.
	LogsTail string

	// DebugHint is a ready-to-run command for connecting to this target's
	// sandbox (or SQLite temp file) after a failure, set only when
	// keepOnFailure asked for it to be kept instead of torn down. Empty
	// otherwise.
	DebugHint string

	// Fallback drill results
	FallbackUsed    bool
	FallbackBackup  *backup.File
	FallbackRPO     time.Duration
	FallbackMessage string

	// Schema drift analysis
	SchemaDrift *schema.DriftReport

	// Chaos testing result
	ChaosInjected bool
	ChaosPassed   bool
	ChaosMessage  string

	// Sandbox resource usage profiling
	PeakMemoryBytes    int64
	DiskFootprintBytes int64

	// Incremental/Differential backup replay chain
	IncrementalPatchesApplied []string

	// Incident remediation playbook execution result
	Remediation *RemediationResult

	// RPO data lag evaluation
	HasRPOCheck bool
	MaxRPOLag   time.Duration
	RPOViolated bool
}

// RemediationResult captures the execution details and outcome of an incident remediation playbook.
type RemediationResult struct {
	Triggered bool          `json:"triggered"`
	Command   string        `json:"command"`
	Success   bool          `json:"success"`
	Output    string        `json:"output,omitempty"`
	Error     string        `json:"error,omitempty"`
	Duration  time.Duration `json:"duration"`
}

// ProgressCallback is invoked as a target transitions between drill stages.
type ProgressCallback func(target string, stage Stage, detail string, done bool, passed bool)

// Run verifies a single target end to end.
func Run(ctx context.Context, target config.Target, baseline int64, hasBaseline bool, keepOnFailure bool, gpgPassphrase string) Result {
	return RunWithProgress(ctx, target, baseline, hasBaseline, keepOnFailure, gpgPassphrase, nil)
}

// RunWithProgress verifies a single target with live stage progress reporting.
func RunWithProgress(ctx context.Context, target config.Target, baseline int64, hasBaseline bool, keepOnFailure bool, gpgPassphrase string, onProgress ProgressCallback) (result Result) {
	started := time.Now()
	result = Result{Target: target.Name, Tags: target.Tags, Stage: StagePreHook, SLARTO: target.SLARTO}

	reportStage := func(s Stage) {
		result.Stage = s
		if onProgress != nil {
			onProgress(target.Name, s, "", false, false)
		}
	}
	reportStage(StagePreHook)

	defer func() {
		result.Duration = time.Since(started)
		if onProgress != nil {
			onProgress(target.Name, result.Stage, "", true, result.Passed)
		}
		// Run post_drill_command if configured
		if target.PostDrillCommand != "" {
			hookErr := runHook(ctx, target.PostDrillCommand, target.HooksTimeout, map[string]string{
				"LAZARUS_TARGET":               target.Name,
				"LAZARUS_ENGINE":               string(target.Engine),
				"LAZARUS_STATUS":               hookStatus(result.Passed),
				"LAZARUS_STAGE":                string(result.Stage),
				"LAZARUS_ERROR":                errString(result.Err),
				"LAZARUS_DURATION_MS":          fmt.Sprintf("%d", result.Duration.Milliseconds()),
				"LAZARUS_RESTORE_DURATION_MS":  fmt.Sprintf("%d", result.RestoreDuration.Milliseconds()),
				"LAZARUS_BACKUP_PATH":          backupPath(result.Backup),
				"LAZARUS_FALLBACK_USED":        fmt.Sprintf("%t", result.FallbackUsed),
				"LAZARUS_FALLBACK_RPO_SECONDS": fmt.Sprintf("%.0f", result.FallbackRPO.Seconds()),
			})
			if hookErr != nil {
				if result.Err == nil {
					result.Passed = false
					result.Stage = StagePostHook
					result.Err = fmt.Errorf("post_drill_command failed: %w", hookErr)
				}
			}
		}

		// Trigger automated incident remediation playbook if configured and criteria met
		if target.Remediation != nil && target.Remediation.Command != "" {
			shouldTrigger := false
			triggerOn := target.Remediation.TriggerOn
			if triggerOn == "" || triggerOn == "failure" {
				shouldTrigger = !result.Passed || result.Err != nil
			} else if triggerOn == "critical_drift" {
				shouldTrigger = result.Stage == StageSizeDrift || (result.SchemaDrift != nil && len(result.SchemaDrift.MissingTables) > 0)
			}
			if shouldTrigger {
				remRes := runRemediation(ctx, target.Remediation.Command, target.Remediation.Timeout, map[string]string{
					"LAZARUS_TARGET":              target.Name,
					"LAZARUS_ENGINE":              string(target.Engine),
					"LAZARUS_STATUS":              hookStatus(result.Passed),
					"LAZARUS_STAGE":               string(result.Stage),
					"LAZARUS_ERROR":               errString(result.Err),
					"LAZARUS_BACKUP_PATH":         backupPath(result.Backup),
					"LAZARUS_REMEDIATION_TRIGGER": triggerOn,
				})
				result.Remediation = &remRes
			}
		}
	}()

	if target.CleanupBackup {
		defer func() {
			if !result.Passed || result.Err != nil {
				return
			}
			if result.Backup != nil && result.Backup.Path != "" {
				_ = os.Remove(result.Backup.Path)
			} else if target.Path != "" {
				_ = os.Remove(target.Path)
			}
		}()
	}

	// 1. Pre-drill hook
	if target.PreDrillCommand != "" {
		reportStage(StagePreHook)
		if err := runHook(ctx, target.PreDrillCommand, target.HooksTimeout, map[string]string{
			"LAZARUS_TARGET": target.Name,
			"LAZARUS_ENGINE": string(target.Engine),
			"LAZARUS_STAGE":  string(StagePreHook),
		}); err != nil {
			result.Err = fmt.Errorf("pre_drill_command failed: %w", err)
			return
		}
	}

	// 2. Fetch
	result.Stage = StageFetch
	if target.FetchCommand != "" {
		if err := fetch.Run(ctx, target.FetchCommand, target.FetchTimeout); err != nil {
			result.Err = err
			return
		}
	}

	if target.S3 != nil {
		downloader := s3fetch.New()
		if isTerminal(os.Stderr) {
			downloader.ProgressWriter = os.Stderr
		}
		if err := downloader.Download(ctx, target.S3, target.Path); err != nil {
			result.Err = fmt.Errorf("s3 download: %w", err)
			return
		}
	}

	if target.GCS != nil {
		downloader := gcsfetch.New()
		if isTerminal(os.Stderr) {
			downloader.ProgressWriter = os.Stderr
		}
		if err := downloader.Download(ctx, target.GCS, target.Path); err != nil {
			result.Err = fmt.Errorf("gcs download: %w", err)
			return
		}
	}

	if target.Azure != nil {
		downloader := azurefetch.New()
		if isTerminal(os.Stderr) {
			downloader.ProgressWriter = os.Stderr
		}
		if err := downloader.Download(ctx, target.Azure, target.Path); err != nil {
			result.Err = fmt.Errorf("azure download: %w", err)
			return
		}
	}

	// 3. Locate
	result.Stage = StageLocate
	allFiles, err := backup.LocateAll(target.Path)
	if err != nil {
		result.Err = err
		return
	}
	file := allFiles[0]
	result.Backup = file

	var primaryErr error
	var primaryStage Stage

	// Check if chaos testing is enabled for primary backup
	if target.Chaos.Enabled {
		corruptPath, cErr := createCorruptedBackupCopy(file.Path, target.Chaos.CorruptBytes)
		if cErr == nil {
			defer os.Remove(corruptPath)
			result.ChaosInjected = true
			chaosFile := &backup.File{Path: corruptPath, Size: file.Size, ModTime: file.ModTime}
			primaryErr, primaryStage = executeSingleDrill(ctx, target, chaosFile, baseline, hasBaseline, keepOnFailure, gpgPassphrase, &result, onProgress)
			if primaryErr == nil {
				// Corrupted backup unexpectedly passed
				result.ChaosPassed = false
				result.ChaosMessage = fmt.Sprintf("Chaos drill warning: corrupted primary backup %s unexpectedly passed restore", filepath.Base(file.Path))
				result.Stage = StageDone
				result.Passed = true
				return
			}
		} else {
			// Fallback to normal execution if corruption copy failed
			primaryErr, primaryStage = executeSingleDrill(ctx, target, file, baseline, hasBaseline, keepOnFailure, gpgPassphrase, &result, onProgress)
		}
	} else {
		// Run drill against primary (newest) backup normally
		primaryErr, primaryStage = executeSingleDrill(ctx, target, file, baseline, hasBaseline, keepOnFailure, gpgPassphrase, &result, onProgress)
	}

	if primaryErr == nil {
		result.Stage = StageDone
		result.Passed = true
		return
	}

	// Primary drill failed. Check if fallback recovery drill is enabled.
	if !target.FallbackOnFailure || len(allFiles) <= 1 {
		result.Stage = primaryStage
		result.Err = primaryErr
		if target.Chaos.Enabled && result.ChaosInjected {
			result.ChaosPassed = false
			result.ChaosMessage = fmt.Sprintf("Chaos drill failed: primary corrupted backup failed, but fallback is disabled or no older backups exist")
		}
		return
	}

	// Attempt fallback drills on older backups
	maxDepth := target.MaxFallbackDepth
	if maxDepth <= 0 {
		maxDepth = 3
	}

	var fallbackFound bool
	for i := 1; i < len(allFiles) && i <= maxDepth; i++ {
		fallbackFile := allFiles[i]
		fbResult := Result{Target: target.Name, Tags: target.Tags, SLARTO: target.SLARTO, Backup: fallbackFile}
		fbErr, _ := executeSingleDrill(ctx, target, fallbackFile, baseline, hasBaseline, keepOnFailure, gpgPassphrase, &fbResult, onProgress)
		if fbErr == nil {
			// Fallback succeeded!
			fallbackFound = true
			result.Passed = true
			result.Stage = StageDone
			result.Err = nil
			result.Backup = fallbackFile
			result.RestoreDuration = fbResult.RestoreDuration
			result.Checks = fbResult.Checks
			result.SchemaDrift = fbResult.SchemaDrift
			result.PeakMemoryBytes = fbResult.PeakMemoryBytes
			result.DiskFootprintBytes = fbResult.DiskFootprintBytes
			result.IncrementalPatchesApplied = fbResult.IncrementalPatchesApplied
			// The primary attempt may have set these before failing; the
			// verdict now belongs to the fallback backup.
			result.HasRPOCheck = fbResult.HasRPOCheck
			result.MaxRPOLag = fbResult.MaxRPOLag
			result.RPOViolated = fbResult.RPOViolated
			result.FallbackUsed = true
			result.FallbackBackup = fallbackFile
			result.FallbackRPO = file.ModTime.Sub(fallbackFile.ModTime)
			result.FallbackMessage = fmt.Sprintf("Primary backup %s failed (%v); successfully fell back to %s (RPO: %s)",
				filepath.Base(file.Path), primaryErr, filepath.Base(fallbackFile.Path), result.FallbackRPO.Round(time.Second))
			if target.Chaos.Enabled && result.ChaosInjected {
				result.ChaosPassed = true
				result.ChaosMessage = fmt.Sprintf("Chaos drill passed: simulated corruption on %s was resisted; fallback safely restored database from %s (RPO: %s)",
					filepath.Base(file.Path), filepath.Base(fallbackFile.Path), result.FallbackRPO.Round(time.Second))
			}
			break
		}
	}

	if !fallbackFound {
		result.Stage = primaryStage
		result.Err = fmt.Errorf("%w (fallback drill tried %d older backup(s), all failed)", primaryErr, min(len(allFiles)-1, maxDepth))
		if target.Chaos.Enabled && result.ChaosInjected {
			result.ChaosPassed = false
			result.ChaosMessage = fmt.Sprintf("Chaos drill failed: simulated corruption on %s caused failure, and fallback drills on %d older backup(s) all failed",
				filepath.Base(file.Path), min(len(allFiles)-1, maxDepth))
		}
	}
	return
}

func executeSingleDrill(
	ctx context.Context,
	target config.Target,
	file *backup.File,
	baseline int64,
	hasBaseline bool,
	keepOnFailure bool,
	gpgPassphrase string,
	result *Result,
	onProgress ProgressCallback,
) (drillErr error, drillStage Stage) {
	// Age check
	if target.MaxAge > 0 {
		if age := file.Age(time.Now()); age > target.MaxAge {
			return fmt.Errorf("backup is %s old, older than the %s limit", age.Round(time.Minute), target.MaxAge), StageAge
		}
	}

	// Size drift check
	if target.SizeDrift != nil && hasBaseline {
		if exceedsSizeDrift(file.Size, baseline, target.SizeDrift.MaxDecreasePct) {
			return sizeDriftError(file.Size, baseline, target.SizeDrift.MaxDecreasePct), StageSizeDrift
		}
	}

	var runChecks func(context.Context) []check.Result
	var inspectSchema func(context.Context) ([]schema.TableInfo, error)

	sbOpts := sandbox.Options{
		MemoryLimit:    target.MemoryLimit,
		CPUs:           target.CPUs,
		Network:        target.Network,
		ReadOnlyRootfs: target.ReadOnlyRootfs,
	}

	if target.Engine == config.EngineSQLite {
		if onProgress != nil {
			onProgress(target.Name, StageRestore, "", false, false)
		}
		restoreStarted := time.Now()
		path, cleanup, err := sqlitecheck.PrepareWithOptions(ctx, file, gpgPassphrase, target.AgeIdentity)
		if err != nil {
			return err, StageRestore
		}
		defer func() {
			if keepOnFailure && (drillErr != nil || result.Err != nil) {
				result.DebugHint = fmt.Sprintf("sqlite3 %s", path)
				return
			}
			cleanup()
		}()

		if err := sqlitecheck.IntegrityCheck(ctx, path); err != nil {
			return err, StageRestore
		}

		if err := applyIncrementalPatches(ctx, target, nil, path, gpgPassphrase, result); err != nil {
			return err, StageRestore
		}

		result.RestoreDuration = time.Since(restoreStarted)
		if fi, err := os.Stat(path); err == nil {
			result.DiskFootprintBytes = fi.Size()
		}

		runChecks = func(ctx context.Context) []check.Result {
			return sqlitecheck.RunChecks(ctx, path, target.Checks)
		}
		inspectSchema = func(ctx context.Context) ([]schema.TableInfo, error) {
			return schema.Inspect(ctx, target.Engine, func(ctx context.Context, query string) (string, error) {
				cmd := exec.CommandContext(ctx, "sqlite3", path, query)
				out, err := cmd.CombinedOutput()
				return string(out), err
			})
		}
	} else if target.Engine == config.EngineRedis && file.Format == backup.FormatRedisRDB {
		if onProgress != nil {
			onProgress(target.Name, StageSandbox, "", false, false)
		}
		restoreStarted := time.Now()
		rdbDir, cleanupRDB, err := redischeck.PrepareWithOptions(ctx, file, gpgPassphrase, target.AgeIdentity)
		if err != nil {
			return err, StageRestore
		}
		defer cleanupRDB()

		sbOpts.DataDir = rdbDir
		sb, err := sandbox.StartWithOptions(ctx, target.Engine, target.Image, sbOpts)
		if err != nil {
			return err, StageSandbox
		}
		defer func() {
			if (drillErr != nil || result.Err != nil) && sb != nil {
				result.LogsTail = sb.LogsTail(ctx, 50)
			}
			if keepOnFailure && (drillErr != nil || result.Err != nil) {
				result.DebugHint = debugHint(sb)
				return
			}
			sb.Stop()
		}()

		result.RestoreDuration = time.Since(restoreStarted)
		if fp, err := sb.Footprint(ctx); err == nil {
			result.PeakMemoryBytes = fp.PeakMemoryBytes
			result.DiskFootprintBytes = fp.DiskFootprintBytes
		}

		runChecks = func(ctx context.Context) []check.Result {
			return check.RunAll(ctx, sb, target.Engine, target.Checks)
		}
		inspectSchema = func(ctx context.Context) ([]schema.TableInfo, error) {
			return nil, nil // Redis has no relational table schema
		}
	} else {
		if onProgress != nil {
			onProgress(target.Name, StageSandbox, "", false, false)
		}
		sb, err := sandbox.StartWithOptions(ctx, target.Engine, target.Image, sbOpts)
		if err != nil {
			return err, StageSandbox
		}
		defer func() {
			if (drillErr != nil || result.Err != nil) && sb != nil {
				result.LogsTail = sb.LogsTail(ctx, 50)
			}
			if keepOnFailure && (drillErr != nil || result.Err != nil) {
				result.DebugHint = debugHint(sb)
				return
			}
			sb.Stop()
		}()

		if onProgress != nil {
			onProgress(target.Name, StageRestore, "", false, false)
		}
		restoreStarted := time.Now()
		restoreOpts := restore.Options{
			GPGPassphrase: gpgPassphrase,
			AgeIdentity:   target.AgeIdentity,
		}
		if _, err := restore.RunWithOptions(ctx, sb, target.Engine, file, restoreOpts); err != nil {
			return err, StageRestore
		}

		if err := applyIncrementalPatches(ctx, target, sb, "", gpgPassphrase, result); err != nil {
			return err, StageRestore
		}

		result.RestoreDuration = time.Since(restoreStarted)
		if fp, err := sb.Footprint(ctx); err == nil {
			result.PeakMemoryBytes = fp.PeakMemoryBytes
			result.DiskFootprintBytes = fp.DiskFootprintBytes
		}
		runChecks = func(ctx context.Context) []check.Result {
			return check.RunAll(ctx, sb, target.Engine, target.Checks)
		}
		inspectSchema = func(ctx context.Context) ([]schema.TableInfo, error) {
			return schema.Inspect(ctx, target.Engine, func(ctx context.Context, q string) (string, error) {
				return querySandbox(ctx, sb, target.Engine, q)
			})
		}
	}

	if exceedsRTO(result.RestoreDuration, target.MaxRestoreDuration) {
		return rtoError(result.RestoreDuration, target.MaxRestoreDuration), StageRTO
	}

	// Schema drift inspection
	if target.AutoSchemaCheck && inspectSchema != nil {
		if onProgress != nil {
			onProgress(target.Name, StageSchema, "", false, false)
		}
		tables, err := inspectSchema(ctx)
		if err != nil {
			return fmt.Errorf("schema inspection failed: %w", err), StageSchema
		}
		drift := schema.CompareWithNames(tables, target.SchemaBaseline)
		result.SchemaDrift = &drift
		if len(drift.MissingTables) > 0 {
			return fmt.Errorf("schema validation failed: missing expected tables %v", drift.MissingTables), StageSchema
		}
	}

	// Checks
	if onProgress != nil {
		onProgress(target.Name, StageChecks, "", false, false)
	}
	result.Checks = runChecks(ctx)
	for _, c := range result.Checks {
		if c.IsRPOCheck {
			result.HasRPOCheck = true
			if c.RPOLag > result.MaxRPOLag {
				result.MaxRPOLag = c.RPOLag
			}
			if !c.Passed {
				result.RPOViolated = true
			}
		}
		if !c.Passed {
			return fmt.Errorf("check %q failed: %s", c.Name, c.Reason), StageChecks
		}
	}

	return nil, StageDone
}

func querySandbox(ctx context.Context, sb *sandbox.Sandbox, engine config.Engine, q string) (string, error) {
	var args []string
	switch engine {
	case config.EnginePostgres:
		args = []string{
			"psql",
			"--username", sandbox.User(),
			"--dbname", sandbox.DBName(),
			"--tuples-only", "--no-align",
			"--set", "ON_ERROR_STOP=1",
			"--command", q,
		}
	case config.EngineMySQL:
		args = []string{
			"mysql",
			"--user=root",
			"--password=" + sandbox.Password(),
			"--skip-column-names", "--batch",
			"--execute=" + q,
			sandbox.DBName(),
		}
	case config.EngineMongoDB:
		evalCode := fmt.Sprintf("const db = db.getSiblingDB('%s'); print(%s)", sandbox.DBName(), q)
		args = []string{
			"mongosh",
			"--username", sandbox.User(),
			"--password=" + sandbox.Password(),
			"--authenticationDatabase", "admin",
			"--quiet",
			"--eval", evalCode,
		}
	default:
		return "", fmt.Errorf("unsupported engine for schema inspection: %s", engine)
	}
	out, err := sb.Exec(ctx, "", args...)
	if engine == config.EngineMySQL {
		out = check.StripMySQLPasswordWarning(out)
	}
	return out, err
}

func runHook(ctx context.Context, cmdStr string, timeout time.Duration, env map[string]string) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("command timed out after %s: %s", timeout, string(out))
		}
		return fmt.Errorf("%v: %s", err, string(out))
	}
	return nil
}

func hookStatus(passed bool) string {
	if passed {
		return "PASSED"
	}
	return "FAILED"
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func backupPath(f *backup.File) string {
	if f == nil {
		return ""
	}
	return f.Path
}

// debugHint builds a ready-to-paste command for connecting to a kept sandbox.
func debugHint(sb *sandbox.Sandbox) string {
	switch sb.Engine {
	case config.EngineMySQL:
		return fmt.Sprintf("docker exec -it %s mysql --user=root --password=%s %s",
			sb.Name, sandbox.Password(), sandbox.DBName())
	case config.EngineRedis:
		return fmt.Sprintf("docker exec -it %s redis-cli", sb.Name)
	case config.EngineMongoDB:
		return fmt.Sprintf("docker exec -it %s mongosh --username %s --password=%s --authenticationDatabase admin",
			sb.Name, sandbox.User(), sandbox.Password())
	default: // Postgres
		return fmt.Sprintf("docker exec -it %s psql --username %s --dbname %s",
			sb.Name, sandbox.User(), sandbox.DBName())
	}
}

// exceedsRTO reports whether a restore blew through its configured time budget.
func exceedsRTO(actual, limit time.Duration) bool {
	return limit > 0 && actual > limit
}

func rtoError(actual, limit time.Duration) error {
	return fmt.Errorf("restore took %s, longer than the %s RTO limit",
		actual.Round(time.Millisecond), limit)
}

// exceedsSizeDrift reports whether current is a suspicious shrink from baseline.
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

// RunAll verifies every target, continuing past failures.
func RunAll(ctx context.Context, targets []config.Target, st *state.State, parallelism int, keepOnFailure bool, gpgPassphrase string) []Result {
	return RunAllWithProgress(ctx, targets, st, parallelism, keepOnFailure, gpgPassphrase, nil)
}

// RunAllWithProgress verifies every target while reporting stage transitions to onProgress.
func RunAllWithProgress(ctx context.Context, targets []config.Target, st *state.State, parallelism int, keepOnFailure bool, gpgPassphrase string, onProgress ProgressCallback) []Result {
	// Proactively sweep any leftover orphan sandboxes in the background
	go func() {
		_, _ = sandbox.ReapOrphans(context.Background(), 2*time.Hour)
	}()

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

			result := RunWithProgress(ctx, target, baseline, hasBaseline, keepOnFailure, gpgPassphrase, onProgress)
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

// createCorruptedBackupCopy creates a disposable temporary copy of srcPath with
// deliberate byte flips to simulate backup corruption for chaos drill testing.
func createCorruptedBackupCopy(srcPath string, corruptBytes int) (string, error) {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("backup file is empty")
	}

	corrupted := make([]byte, len(data))
	copy(corrupted, data)

	if corruptBytes <= 0 {
		corruptBytes = 64
	}
	n := min(corruptBytes, len(corrupted))
	offset := 0
	if len(corrupted) > 32 {
		offset = 16
	}
	for i := 0; i < n && (offset+i) < len(corrupted); i++ {
		corrupted[offset+i] ^= 0xFF
	}

	tmpFile, err := os.CreateTemp("", "lazarus-chaos-*"+filepath.Ext(srcPath))
	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(corrupted); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	_ = tmpFile.Close()
	return tmpPath, nil
}

func isTerminal(f *os.File) bool {
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

func applyIncrementalPatches(
	ctx context.Context,
	target config.Target,
	sb *sandbox.Sandbox,
	sqlitePath string,
	gpgPassphrase string,
	result *Result,
) error {
	if len(target.IncrementalPatches) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var patchFiles []*backup.File

	for _, pattern := range target.IncrementalPatches {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("invalid incremental patch pattern %q: %w", pattern, err)
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			f, err := backup.Identify(m)
			if err != nil {
				return fmt.Errorf("identify incremental patch %q: %w", m, err)
			}
			patchFiles = append(patchFiles, f)
		}
	}

	// Sort chronologically ascending (oldest first to newest)
	sort.Slice(patchFiles, func(i, j int) bool {
		return patchFiles[i].ModTime.Before(patchFiles[j].ModTime)
	})

	restoreOpts := restore.Options{
		GPGPassphrase: gpgPassphrase,
		AgeIdentity:   target.AgeIdentity,
	}

	for _, patch := range patchFiles {
		patchBase := filepath.Base(patch.Path)
		if target.Engine == config.EngineSQLite {
			cmd := exec.CommandContext(ctx, "sqlite3", sqlitePath, fmt.Sprintf(".read %s", patch.Path))
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("incremental patch %s failed: %w: %s", patchBase, err, strings.TrimSpace(string(out)))
			}
		} else {
			if _, err := restore.ApplyPatch(ctx, sb, target.Engine, patch, restoreOpts); err != nil {
				return fmt.Errorf("incremental patch %s failed: %w", patchBase, err)
			}
		}
		result.IncrementalPatchesApplied = append(result.IncrementalPatchesApplied, patchBase)
	}

	return nil
}

func runRemediation(ctx context.Context, cmdStr string, timeout time.Duration, env map[string]string) RemediationResult {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	res := RemediationResult{
		Triggered: true,
		Command:   cmdStr,
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	out, err := cmd.CombinedOutput()
	res.Duration = time.Since(started)
	res.Output = strings.TrimSpace(string(out))

	if err != nil {
		res.Success = false
		if ctx.Err() == context.DeadlineExceeded {
			res.Error = fmt.Sprintf("remediation timed out after %s", timeout)
		} else {
			res.Error = err.Error()
		}
	} else {
		res.Success = true
	}

	return res
}
