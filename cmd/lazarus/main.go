// Lazarus verifies that database backups can actually be restored, by
// restoring them into a throwaway container and asserting the data is really
// there. Exit code 0 means every target restored and passed its checks.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"lazarus/internal/backup"
	"lazarus/internal/config"
	"lazarus/internal/inspect"
	"lazarus/internal/notify"
	"lazarus/internal/report"
	"lazarus/internal/state"
	"lazarus/internal/ui"
	"lazarus/internal/verify"
)

// version is set at build time via -ldflags "-X main.version=...". goreleaser
// does this for every released binary; a `go build` run by hand leaves it at
// "dev", which is the right answer for a binary that isn't a tagged release.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "inspect":
			inspectCommand(os.Args[2:])
			return
		case "init":
			initCommand(os.Args[2:])
			return
		case "export":
			exportCommand(os.Args[2:])
			return
		}
	}
	configPath := flag.String("config", "lazarus.yml", "path to the config file")
	targetName := flag.String("target", "", "verify only this target (default: all)")
	tagFilter := flag.String("tag", "", "verify only targets matching this tag")
	asJSON := flag.Bool("json", false, "machine-readable output")
	quiet := flag.Bool("quiet", false, "only print failing targets and the final tally (ignored with --json, which is already machine-readable)")
	liveProgress := flag.Bool("live", true, "display real-time multi-target progress in terminal (auto-disabled if non-TTY or with --json/--quiet)")
	keepOnFailure := flag.Bool("keep-on-failure", false, "keep a failing target's sandbox container (or SQLite temp file) instead of tearing it down, for manual inspection")
	checkConfig := flag.Bool("check-config", false, "validate the config file and exit, without fetching, restoring, or touching Docker")
	dryRun := flag.Bool("dry-run", false, "alias for --check-config")
	chaos := flag.Bool("chaos", false, "run chaos engineering drill: deliberately corrupt primary backup to verify fallback recovery resilience")
	outputHTML := flag.String("output-html", "", "path to write standalone HTML disaster recovery audit report")
	daemon := flag.Bool("daemon", false, "run continuously as a daemon periodic runner")
	interval := flag.Duration("interval", 0, "interval between verification runs in daemon mode (e.g. 1h, 30m)")
	workerMode := flag.Bool("worker", false, "run as a distributed worker polling drill tasks from control plane")
	controlPlaneURL := flag.String("control-plane", "", "control plane base URL for worker registration (e.g. http://localhost:8080)")
	workerID := flag.String("worker-id", "", "unique identifier for this worker (defaults to runner-<hostname>-<pid>)")
	workerToken := flag.String("worker-token", "", "authentication bearer token for control plane API")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("lazarus", version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lazarus: %v\n", err)
		os.Exit(2)
	}

	targets := cfg.Targets
	if *targetName != "" {
		targets, err = filterTarget(cfg.Targets, *targetName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lazarus: %v\n", err)
			os.Exit(2)
		}
	}
	if *tagFilter != "" {
		targets, err = filterByTag(targets, *tagFilter)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lazarus: %v\n", err)
			os.Exit(2)
		}
	}

	if *chaos {
		for i := range targets {
			targets[i].Chaos.Enabled = true
			if targets[i].Chaos.CorruptBytes <= 0 {
				targets[i].Chaos.CorruptBytes = 64
			}
		}
	}

	if *checkConfig || *dryRun {
		// config.Load already ran every syntax/logic check it has (unique
		// names, valid engine, exactly-one check expectation, and so on) —
		// getting this far at all means the config is valid. What's left is
		// showing the defaults it resolved, since those are otherwise only
		// visible once something actually runs.
		if *asJSON {
			printConfigSummaryJSON(os.Stdout, cfg, targets)
		} else {
			printConfigSummary(os.Stdout, cfg, targets)
		}
		return
	}

	// Ctrl-C has to reach the verify run so its deferred sandbox teardown
	// gets a chance to run — otherwise an interrupted check leaves a
	// container behind.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := state.Load(cfg.StateFile)
	if err != nil {
		// The state file only backs the size-drift check, an overlay on top
		// of the actual restore-and-check verdict — losing it shouldn't stop
		// a run, just its ability to compare against history this time.
		fmt.Fprintf(os.Stderr, "lazarus: WARNING: could not load state file %q, size-drift baselines reset: %v\n", cfg.StateFile, err)
		st = state.New()
	}

	if *daemon && *workerMode {
		fmt.Fprintln(os.Stderr, "lazarus: --daemon and --worker are mutually exclusive")
		os.Exit(2)
	}

	if *daemon {
		runDaemon(ctx, cfg, targets, st, *keepOnFailure, *asJSON, *quiet, *liveProgress, *outputHTML, *interval)
		return
	}

	if *workerMode {
		cpURL := *controlPlaneURL
		if cpURL == "" && cfg.Notify.DashboardURL != "" {
			cpURL = cfg.Notify.DashboardURL
		}
		if cpURL == "" {
			fmt.Fprintf(os.Stderr, "lazarus: --worker mode requires --control-plane=<URL> (or notify.dashboard_url in config)\n")
			os.Exit(2)
		}
		runWorker(ctx, cfg, targets, st, *keepOnFailure, *asJSON, *quiet, *liveProgress, *outputHTML, cpURL, *workerID, *workerToken, *interval)
		return
	}

	allPassed := runOnce(ctx, cfg, targets, st, *keepOnFailure, *asJSON, *quiet, *liveProgress, *outputHTML)
	if !allPassed {
		os.Exit(1)
	}
}

func runWorker(ctx context.Context, cfg *config.Config, targets []config.Target, st *state.State, keepOnFailure, asJSON, quiet, live bool, outputHTML, cpURL, wID, token string, pollInterval time.Duration) {
	if pollInterval <= 0 {
		pollInterval = 3 * time.Second
	}
	if token == "" {
		// The worker usually talks to the same control plane it reports to.
		token = cfg.Notify.APIKey
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "runner-host"
	}
	if wID == "" {
		wID = fmt.Sprintf("runner-%s-%d", hostname, os.Getpid())
	}

	// Collect unique tags and the exact target names this worker can run.
	tagMap := make(map[string]bool)
	targetNames := make([]string, 0, len(targets))
	for _, t := range targets {
		targetNames = append(targetNames, t.Name)
		for _, tag := range t.Tags {
			tagMap[tag] = true
		}
	}
	var tags []string
	for tag := range tagMap {
		tags = append(tags, tag)
	}

	wc := &workerClient{
		baseURL: strings.TrimRight(cpURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 10 * time.Second},
		register: map[string]any{
			"id":       wID,
			"hostname": hostname,
			"version":  version,
			"tags":     tags,
		},
		id: wID,
	}

	if err := wc.registerWorker(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "lazarus worker: WARNING: failed to register with control plane (will retry on heartbeat): %v\n", err)
	} else {
		fmt.Printf("lazarus worker: registered as %q (host: %s, tags: %v)\n", wID, hostname, tags)
	}
	fmt.Printf("lazarus worker: polling control plane %s every %s (Ctrl+C to stop)\n", wc.baseURL, pollInterval)

	// Heartbeats run on their own goroutine: a drill can take far longer than
	// the control plane's 60s offline cutoff, and the poll loop below is
	// blocked for the whole drill.
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		hbTicker := time.NewTicker(10 * time.Second)
		defer hbTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-hbTicker.C:
				wc.heartbeat(ctx)
			}
		}
	}()

	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			<-hbDone
			fmt.Println("\nlazarus worker: shutting down, unregistering...")
			wc.setTask("offline", "")
			wc.heartbeat(context.Background())
			return

		case <-pollTicker.C:
			name, err := wc.poll(ctx, tags, targetNames)
			if err != nil {
				if ctx.Err() == nil {
					fmt.Fprintf(os.Stderr, "lazarus worker: poll failed: %v\n", err)
				}
				continue
			}
			if name == "" {
				continue
			}

			var matched *config.Target
			for i := range targets {
				if targets[i].Name == name {
					matched = &targets[i]
					break
				}
			}
			if matched == nil {
				fmt.Fprintf(os.Stderr, "lazarus worker: claimed target %q but not found in local config\n", name)
				continue
			}

			fmt.Printf("[%s] lazarus worker: claimed task -> executing drill for %q\n", time.Now().Format("15:04:05"), matched.Name)
			wc.setTask("busy", matched.Name)
			wc.heartbeat(ctx)

			runOnce(ctx, cfg, []config.Target{*matched}, st, keepOnFailure, asJSON, quiet, live, outputHTML)

			wc.setTask("online", "")
			wc.heartbeat(ctx)
		}
	}
}

// workerClient talks to the control plane's worker API.
type workerClient struct {
	baseURL  string
	token    string
	client   *http.Client
	register map[string]any
	id       string

	mu     sync.Mutex
	status string
	task   string
}

func (wc *workerClient) setTask(status, task string) {
	wc.mu.Lock()
	wc.status, wc.task = status, task
	wc.mu.Unlock()
}

func (wc *workerClient) post(ctx context.Context, path string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wc.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if wc.token != "" {
		req.Header.Set("Authorization", "Bearer "+wc.token)
	}
	return wc.client.Do(req)
}

func (wc *workerClient) registerWorker(ctx context.Context) error {
	resp, err := wc.post(ctx, "/api/v1/workers/register", wc.register)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("control plane returned status %d", resp.StatusCode)
	}
	return nil
}

// heartbeat reports the current state, re-registering when the control
// plane no longer knows this worker (its registry is in-memory, so a
// control plane restart forgets every worker).
func (wc *workerClient) heartbeat(ctx context.Context) {
	wc.mu.Lock()
	status, task := wc.status, wc.task
	wc.mu.Unlock()
	if status == "" {
		status = "online"
	}
	payload := map[string]string{"id": wc.id, "status": status, "current_task": task}

	resp, err := wc.post(ctx, "/api/v1/workers/heartbeat", payload)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && status != "offline" {
		if err := wc.registerWorker(ctx); err == nil {
			if resp, err := wc.post(ctx, "/api/v1/workers/heartbeat", payload); err == nil {
				_ = resp.Body.Close()
			}
		}
	}
}

// poll asks for a pending drill and returns the claimed target name, or ""
// when there is nothing to do.
func (wc *workerClient) poll(ctx context.Context, tags, targetNames []string) (string, error) {
	resp, err := wc.post(ctx, "/api/v1/workers/poll", map[string]any{
		"worker_id": wc.id,
		"tags":      tags,
		"targets":   targetNames,
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("control plane returned status %d", resp.StatusCode)
	}
	var pollRes struct {
		HasTask    bool   `json:"has_task"`
		TargetName string `json:"target_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pollRes); err != nil {
		return "", fmt.Errorf("decode poll response: %w", err)
	}
	if !pollRes.HasTask {
		return "", nil
	}
	return pollRes.TargetName, nil
}

func runOnce(ctx context.Context, cfg *config.Config, targets []config.Target, st *state.State, keepOnFailure, asJSON, quiet, live bool, outputHTML string) bool {
	enableLive := live && !asJSON && !quiet && ui.IsTerminal(os.Stdout)
	tracker := ui.NewTracker(os.Stdout, targets, enableLive)
	tracker.Start()

	results := verify.RunAllWithProgress(ctx, targets, st, cfg.Parallelism, keepOnFailure, cfg.GPGPassphrase, tracker.Update)
	tracker.Stop()

	if err := st.Save(cfg.StateFile); err != nil {
		fmt.Fprintf(os.Stderr, "lazarus: WARNING: could not save state file %q: %v\n", cfg.StateFile, err)
	}

	var allPassed bool
	if asJSON {
		allPassed = report.JSON(os.Stdout, results)
	} else {
		allPassed = report.Text(os.Stdout, results, quiet)
	}

	if outputHTML != "" {
		if f, err := os.Create(outputHTML); err == nil {
			_ = report.HTML(f, results, "Lazarus Disaster Recovery Audit Report")
			_ = f.Close()
			if !asJSON && !quiet {
				fmt.Fprintf(os.Stderr, "lazarus: generated HTML audit report at %s\n", outputHTML)
			}
		} else {
			fmt.Fprintf(os.Stderr, "lazarus: WARNING: could not write HTML report %q: %v\n", outputHTML, err)
		}
	}

	sendNotification(ctx, cfg.Notify, results)
	_ = report.WriteGitHubStepSummaryIfPresent(results)
	return allPassed
}

func runDaemon(ctx context.Context, cfg *config.Config, targets []config.Target, st *state.State, keepOnFailure, asJSON, quiet, live bool, outputHTML string, overrideInterval time.Duration) {
	dInterval := overrideInterval
	if dInterval <= 0 {
		// Check target-level intervals
		for _, t := range targets {
			if t.Interval > 0 && (dInterval <= 0 || t.Interval < dInterval) {
				dInterval = t.Interval
			}
		}
	}
	if dInterval <= 0 {
		dInterval = 1 * time.Hour
	}

	fmt.Printf("lazarus: daemon mode active — scheduling %d target(s) every %s (Ctrl+C to stop)\n", len(targets), dInterval)

	// First immediate run
	fmt.Printf("[%s] lazarus: starting initial drill verification cycle...\n", time.Now().Format("2006-01-02 15:04:05"))
	runOnce(ctx, cfg, targets, st, keepOnFailure, asJSON, quiet, live, outputHTML)

	ticker := time.NewTicker(dInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nlazarus: daemon received shutdown signal, gracefully exiting...")
			return
		case t := <-ticker.C:
			fmt.Printf("\n[%s] lazarus: starting scheduled drill verification cycle...\n", t.Format("2006-01-02 15:04:05"))
			runOnce(ctx, cfg, targets, st, keepOnFailure, asJSON, quiet, live, outputHTML)
		}
	}
}

func sendNotification(ctx context.Context, cfg config.Notify, results []verify.Result) {
	notifier := notify.New(cfg.WebhookURL, notify.Format(cfg.Format), notify.When(cfg.When)).
		WithAPIKey(cfg.APIKey).
		WithDashboardURL(cfg.DashboardURL).
		WithSMTP(cfg.SMTP)
	if !notifier.ShouldSend(results) {
		return
	}

	// A failed notification doesn't change the verification verdict, but it
	// must be loud: silently losing the alert would leave the same blind
	// spot this tool exists to close.
	if err := notifier.Send(ctx, results); err != nil {
		fmt.Fprintf(os.Stderr, "lazarus: WARNING: could not deliver notification: %v\n", err)
	}
}

// printConfigSummary reports what --check-config actually confirmed: not
// just "no error", but the values (including resolved defaults like a
// per-engine sandbox image or the 5-minute fetch timeout) that a real run
// would otherwise only reveal once something started fetching or restoring.
func printConfigSummary(w io.Writer, cfg *config.Config, targets []config.Target) {
	fmt.Fprintf(w, "lazarus: config OK — %d target(s), parallelism %d, state file %q\n",
		len(targets), cfg.Parallelism, cfg.StateFile)

	webhook := "not set"
	if cfg.Notify.WebhookURL != "" {
		webhook = "set"
	}
	fmt.Fprintf(w, "notify: format=%s when=%s webhook=%s\n", cfg.Notify.Format, cfg.Notify.When, webhook)

	for _, t := range targets {
		fmt.Fprintf(w, "\n- %s (%s)\n", t.Name, t.Engine)
		fmt.Fprintf(w, "    path: %s\n", t.Path)
		if t.FetchCommand != "" {
			fmt.Fprintf(w, "    fetch_command: %s (timeout %s)\n", t.FetchCommand, t.FetchTimeout)
		}
		if t.S3 != nil {
			fmt.Fprintf(w, "    s3: bucket=%s key=%s (region %s)\n", t.S3.Bucket, t.S3.Key, t.S3.Region)
		}
		if t.CleanupBackup {
			fmt.Fprintf(w, "    cleanup_backup: true\n")
		}
		if t.Image != "" {
			fmt.Fprintf(w, "    image: %s\n", t.Image)
		}
		if t.MaxAge > 0 {
			fmt.Fprintf(w, "    max_age: %s\n", t.MaxAge)
		}
		if t.MaxRestoreDuration > 0 {
			fmt.Fprintf(w, "    max_restore_duration: %s\n", t.MaxRestoreDuration)
		}
		if t.SizeDrift != nil {
			fmt.Fprintf(w, "    size_drift: max_decrease_pct=%.0f%%\n", t.SizeDrift.MaxDecreasePct)
		}
		if len(t.Tags) > 0 {
			fmt.Fprintf(w, "    tags: %s\n", strings.Join(t.Tags, ", "))
		}
		if t.Interval > 0 {
			fmt.Fprintf(w, "    interval: %s\n", t.Interval)
		}
		fmt.Fprintf(w, "    checks: %d\n", len(t.Checks))
	}
}

type configSummaryJSON struct {
	Parallelism int                       `json:"parallelism"`
	StateFile   string                    `json:"state_file"`
	Notify      configSummaryNotifyJSON   `json:"notify"`
	Targets     []configSummaryTargetJSON `json:"targets"`
}

type configSummaryNotifyJSON struct {
	Format     string `json:"format"`
	When       string `json:"when"`
	WebhookSet bool   `json:"webhook_set"`
}

type configSummaryTargetJSON struct {
	Name                    string   `json:"name"`
	Tags                    []string `json:"tags,omitempty"`
	Engine                  string   `json:"engine"`
	Path                    string   `json:"path"`
	FetchCommand            string   `json:"fetch_command,omitempty"`
	FetchTimeoutMs          int64    `json:"fetch_timeout_ms,omitempty"`
	S3Bucket                string   `json:"s3_bucket,omitempty"`
	S3Key                   string   `json:"s3_key,omitempty"`
	CleanupBackup           bool     `json:"cleanup_backup,omitempty"`
	Image                   string   `json:"image,omitempty"`
	MaxAgeMs                int64    `json:"max_age_ms,omitempty"`
	MaxRestoreDurationMs    int64    `json:"max_restore_duration_ms,omitempty"`
	IntervalMs              int64    `json:"interval_ms,omitempty"`
	SizeDriftMaxDecreasePct float64  `json:"size_drift_max_decrease_pct,omitempty"`
	Checks                  int      `json:"checks"`
}

// printConfigSummaryJSON is --check-config's machine-readable counterpart to
// printConfigSummary — --json is documented as "machine-readable output,
// for CI/scripts", and silently falling back to the human-readable text
// whenever it's combined with --check-config would quietly break exactly
// the CI pipelines --check-config is for.
func printConfigSummaryJSON(w io.Writer, cfg *config.Config, targets []config.Target) {
	out := configSummaryJSON{
		Parallelism: cfg.Parallelism,
		StateFile:   cfg.StateFile,
		Notify: configSummaryNotifyJSON{
			Format:     cfg.Notify.Format,
			When:       cfg.Notify.When,
			WebhookSet: cfg.Notify.WebhookURL != "",
		},
		Targets: make([]configSummaryTargetJSON, 0, len(targets)),
	}

	for _, t := range targets {
		jt := configSummaryTargetJSON{
			Name:                 t.Name,
			Tags:                 t.Tags,
			Engine:               string(t.Engine),
			Path:                 t.Path,
			FetchCommand:         t.FetchCommand,
			FetchTimeoutMs:       t.FetchTimeout.Milliseconds(),
			CleanupBackup:        t.CleanupBackup,
			Image:                t.Image,
			MaxAgeMs:             t.MaxAge.Milliseconds(),
			MaxRestoreDurationMs: t.MaxRestoreDuration.Milliseconds(),
			IntervalMs:           t.Interval.Milliseconds(),
			Checks:               len(t.Checks),
		}
		if t.S3 != nil {
			jt.S3Bucket = t.S3.Bucket
			jt.S3Key = t.S3.Key
		}
		if t.SizeDrift != nil {
			jt.SizeDriftMaxDecreasePct = t.SizeDrift.MaxDecreasePct
		}
		out.Targets = append(out.Targets, jt)
	}

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(out)
}

func filterTarget(targets []config.Target, name string) ([]config.Target, error) {
	for _, t := range targets {
		if t.Name == name {
			return []config.Target{t}, nil
		}
	}
	return nil, fmt.Errorf("no target named %q in config", name)
}

func filterByTag(targets []config.Target, tag string) ([]config.Target, error) {
	tag = strings.ToLower(strings.TrimSpace(tag))
	var matched []config.Target
	for _, t := range targets {
		for _, tg := range t.Tags {
			if strings.ToLower(strings.TrimSpace(tg)) == tag {
				matched = append(matched, t)
				break
			}
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("no targets matched tag %q", tag)
	}
	return matched, nil
}

func inspectCommand(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: lazarus inspect <backup-path-or-glob> [--json]")
		os.Exit(2)
	}
	path := args[0]
	asJSON := false
	for _, a := range args[1:] {
		if a == "--json" {
			asJSON = true
		}
	}

	rep, err := inspect.InspectFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lazarus inspect: %v\n", err)
		os.Exit(1)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}

	fmt.Printf("File:        %s\n", rep.Path)
	fmt.Printf("Size:        %s (%d bytes)\n", rep.HumanSize, rep.Size)
	fmt.Printf("Age:         %s (modified %s)\n", rep.Age.Round(time.Minute), rep.ModTime.Format(time.RFC3339))
	fmt.Printf("Format:      %s\n", rep.Format)
	fmt.Printf("Compression: %s (compressed=%t)\n", rep.Compression, rep.Compressed)
	fmt.Printf("Encrypted:   %t\n", rep.Encrypted)
	fmt.Printf("Magic Bytes: %s\n", rep.MagicBytes)
	fmt.Printf("SHA-256:     %s\n", rep.SHA256)
}

func initCommand(args []string) {
	targetFile := "lazarus.yml"
	if len(args) > 0 {
		targetFile = args[0]
	}
	if _, err := os.Stat(targetFile); err == nil {
		fmt.Fprintf(os.Stderr, "lazarus init: file %q already exists. Aborting to prevent overwrite.\n", targetFile)
		os.Exit(1)
	}

	content := `# Lazarus verification configuration
state_file: lazarus-state.json
parallelism: 4

notify:
  webhook_url: "" # or set LAZARUS_WEBHOOK_URL env var
  format: slack
  when: on_failure

targets:
  - name: production-db
    engine: postgres
    path: /backups/postgres/*.sql.gz
    max_age: 26h
    max_restore_duration: 30m
    sla_rto: 15m
    fallback_on_failure: true
    auto_schema_check: true
    checks:
      - name: database has tables
        sql: "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'"
        expect_min: 1
`
	if err := os.WriteFile(targetFile, []byte(content), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "lazarus init: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Created %s successfully! Run `lazarus --config %s` to start disaster recovery drill.\n", targetFile, targetFile)
}

func exportCommand(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	configPath := fs.String("config", "lazarus.yml", "path to lazarus config file")
	format := fs.String("format", "html", "export format: html or md")
	outputPath := fs.String("output", "dr-audit-report.html", "output file path")
	title := fs.String("title", "Lazarus Disaster Recovery Audit Report", "audit report title")
	_ = fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lazarus export: %v\n", err)
		os.Exit(2)
	}

	st, _ := state.Load(cfg.StateFile)
	if st == nil {
		st = state.New()
	}

	var results []verify.Result
	for _, t := range cfg.Targets {
		r := verify.Result{
			Target: t.Name,
			Tags:   t.Tags,
			SLARTO: t.SLARTO,
			Stage:  verify.StageDone,
			Passed: true,
		}
		if ts, ok := st.Get(t.Name); ok && ts.LastSizeBytes > 0 {
			r.Backup = &backup.File{
				Path:    t.Path,
				Size:    ts.LastSizeBytes,
				ModTime: ts.UpdatedAt,
			}
		} else {
			// The state file only records successful runs; without an entry
			// there is no evidence this target ever verified, and an audit
			// report must not claim it did.
			r.Passed = false
			r.Stage = verify.StageLocate
			r.Err = fmt.Errorf("no successful verification recorded in state file %q", cfg.StateFile)
		}
		results = append(results, r)
	}

	f, err := os.Create(*outputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lazarus export: failed to create file %q: %v\n", *outputPath, err)
		os.Exit(1)
	}
	defer f.Close()

	if strings.ToLower(*format) == "html" {
		if err := report.HTML(f, results, *title); err != nil {
			fmt.Fprintf(os.Stderr, "lazarus export: %v\n", err)
			os.Exit(1)
		}
	} else {
		_ = report.StepSummary(f, results)
	}

	fmt.Printf("✓ Lazarus: successfully exported %s disaster recovery audit report to %s\n", *format, *outputPath)
}
