// Lazarus verifies that database backups can actually be restored, by
// restoring them into a throwaway container and asserting the data is really
// there. Exit code 0 means every target restored and passed its checks.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"lazarus/internal/config"
	"lazarus/internal/notify"
	"lazarus/internal/report"
	"lazarus/internal/state"
	"lazarus/internal/verify"
)

func main() {
	configPath := flag.String("config", "lazarus.yml", "path to the config file")
	targetName := flag.String("target", "", "verify only this target (default: all)")
	asJSON := flag.Bool("json", false, "machine-readable output")
	keepOnFailure := flag.Bool("keep-on-failure", false, "keep a failing target's sandbox container (or SQLite temp file) instead of tearing it down, for manual inspection")
	flag.Parse()

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

func filterTarget(targets []config.Target, name string) ([]config.Target, error) {
	for _, t := range targets {
		if t.Name == name {
			return []config.Target{t}, nil
		}
	}
	return nil, fmt.Errorf("no target named %q in config", name)
}
