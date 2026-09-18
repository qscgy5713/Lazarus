// Lazarus verifies that database backups can actually be restored, by
// restoring them into a throwaway container and asserting the data is really
// there. Exit code 0 means every target restored and passed its checks.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"lazarus/internal/config"
	"lazarus/internal/notify"
	"lazarus/internal/report"
	"lazarus/internal/state"
	"lazarus/internal/verify"
)

// version is set at build time via -ldflags "-X main.version=...". goreleaser
// does this for every released binary; a `go build` run by hand leaves it at
// "dev", which is the right answer for a binary that isn't a tagged release.
var version = "dev"

func main() {
	configPath := flag.String("config", "lazarus.yml", "path to the config file")
	targetName := flag.String("target", "", "verify only this target (default: all)")
	asJSON := flag.Bool("json", false, "machine-readable output")
	keepOnFailure := flag.Bool("keep-on-failure", false, "keep a failing target's sandbox container (or SQLite temp file) instead of tearing it down, for manual inspection")
	checkConfig := flag.Bool("check-config", false, "validate the config file and exit, without fetching, restoring, or touching Docker")
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

	if *checkConfig {
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

	results := verify.RunAll(ctx, targets, st, cfg.Parallelism, *keepOnFailure)

	if err := st.Save(cfg.StateFile); err != nil {
		fmt.Fprintf(os.Stderr, "lazarus: WARNING: could not save state file %q: %v\n", cfg.StateFile, err)
	}

	var allPassed bool
	if *asJSON {
		allPassed = report.JSON(os.Stdout, results)
	} else {
		allPassed = report.Text(os.Stdout, results)
	}

	sendNotification(ctx, cfg.Notify, results)

	if !allPassed {
		os.Exit(1)
	}
}

func sendNotification(ctx context.Context, cfg config.Notify, results []verify.Result) {
	notifier := notify.New(cfg.WebhookURL, notify.Format(cfg.Format), notify.When(cfg.When))
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
	Name                    string  `json:"name"`
	Engine                  string  `json:"engine"`
	Path                    string  `json:"path"`
	FetchCommand            string  `json:"fetch_command,omitempty"`
	FetchTimeoutMs          int64   `json:"fetch_timeout_ms,omitempty"`
	Image                   string  `json:"image,omitempty"`
	MaxAgeMs                int64   `json:"max_age_ms,omitempty"`
	MaxRestoreDurationMs    int64   `json:"max_restore_duration_ms,omitempty"`
	SizeDriftMaxDecreasePct float64 `json:"size_drift_max_decrease_pct,omitempty"`
	Checks                  int     `json:"checks"`
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
			Engine:               string(t.Engine),
			Path:                 t.Path,
			FetchCommand:         t.FetchCommand,
			FetchTimeoutMs:       t.FetchTimeout.Milliseconds(),
			Image:                t.Image,
			MaxAgeMs:             t.MaxAge.Milliseconds(),
			MaxRestoreDurationMs: t.MaxRestoreDuration.Milliseconds(),
			Checks:               len(t.Checks),
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
