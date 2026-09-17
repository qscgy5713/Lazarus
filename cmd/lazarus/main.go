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
	"lazarus/internal/report"
	"lazarus/internal/verify"
)

func main() {
	configPath := flag.String("config", "lazarus.yml", "path to the config file")
	targetName := flag.String("target", "", "verify only this target (default: all)")
	asJSON := flag.Bool("json", false, "machine-readable output")
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

	results := verify.RunAll(ctx, targets)

	var allPassed bool
	if *asJSON {
		allPassed = report.JSON(os.Stdout, results)
	} else {
		allPassed = report.Text(os.Stdout, results)
	}

	if !allPassed {
		os.Exit(1)
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
