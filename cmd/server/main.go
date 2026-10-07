package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lazarus/internal/server"
)

var version = "dev"

func main() {
	addr := flag.String("addr", getEnvOrDefault("ADDR", ":8080"), "HTTP listen address (e.g. :8080)")
	apiKey := flag.String("api-key", os.Getenv("SERVER_API_KEY"), "Optional API key for webhook authentication (acts as the admin key when -admin-key is unset)")
	adminKey := flag.String("admin-key", os.Getenv("ADMIN_KEY"), "RBAC admin key: trigger, mute, report ingestion and worker registration")
	viewerKey := flag.String("viewer-key", os.Getenv("VIEWER_KEY"), "RBAC read-only viewer key: dashboard, reports and audit exports")
	stateFile := flag.String("state", getEnvOrDefault("STATE_FILE", "lazarus-server.json"), "Path to persist target states")
	overdueStr := flag.String("overdue", getEnvOrDefault("OVERDUE_THRESHOLD", "26h"), "Threshold after which a target is marked Overdue")
	demoMode := flag.Bool("demo", os.Getenv("DEMO_MODE") == "true" || os.Getenv("DEMO_MODE") == "1", "Seed demo drill records on startup")
	alertWebhook := flag.String("alert-webhook", os.Getenv("ALERT_WEBHOOK_URL"), "Webhook URL for Dead Man's Snitch overdue alerts")
	alertFormat := flag.String("alert-format", getEnvOrDefault("ALERT_FORMAT", "slack"), "Alert format: slack, discord, teams")
	alertIntervalStr := flag.String("alert-interval", getEnvOrDefault("ALERT_INTERVAL", "10m"), "Interval for checking overdue alerts")
	usersFile := flag.String("users-file", os.Getenv("USERS_FILE"), "YAML file of named admin/viewer keys for per-user audit attribution")
	historyLimit := flag.Int("history-limit", 1000, "drill history records retained per target")
	retentionStr := flag.String("retention", getEnvOrDefault("RETENTION", "9600h"), "drop drill history and audit events older than this (default 400 days)")
	leaseStr := flag.String("lease-timeout", getEnvOrDefault("LEASE_TIMEOUT", "2h"), "requeue a worker-claimed drill not reported within this long")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("lazarus-server", version)
		return
	}

	if port := os.Getenv("PORT"); port != "" && *addr == ":8080" {
		*addr = ":" + port
	}

	overdueThreshold, err := time.ParseDuration(*overdueStr)
	if err != nil {
		log.Fatalf("invalid overdue duration %q: %v", *overdueStr, err)
	}

	alertInterval, err := time.ParseDuration(*alertIntervalStr)
	if err != nil {
		log.Fatalf("invalid alert interval %q: %v", *alertIntervalStr, err)
	}

	retention, err := time.ParseDuration(*retentionStr)
	if err != nil {
		log.Fatalf("invalid retention %q: %v", *retentionStr, err)
	}
	leaseTimeout, err := time.ParseDuration(*leaseStr)
	if err != nil {
		log.Fatalf("invalid lease timeout %q: %v", *leaseStr, err)
	}
	var users []server.User
	if *usersFile != "" {
		if users, err = server.LoadUsers(*usersFile); err != nil {
			log.Fatalf("%v", err)
		}
	}

	srv := server.New(server.Config{
		Addr:             *addr,
		APIKey:           *apiKey,
		AdminKey:         *adminKey,
		ViewerKey:        *viewerKey,
		StateFile:        *stateFile,
		OverdueThreshold: overdueThreshold,
		DemoMode:         *demoMode,
		AlertWebhookURL:  *alertWebhook,
		AlertFormat:      *alertFormat,
		AlertInterval:    alertInterval,
		Users:            users,
		HistoryLimit:     *historyLimit,
		Retention:        retention,
		LeaseTimeout:     leaseTimeout,
	})

	fmt.Println("==========================================================")
	fmt.Printf("⚡ Lazarus Control Plane — Disaster Recovery Dashboard (%s)\n", version)
	displayURL := *addr
	if strings.HasPrefix(displayURL, ":") {
		displayURL = "localhost" + displayURL
	}
	fmt.Printf("   Web Dashboard : http://%s\n", displayURL)
	fmt.Printf("   Report Webhook: http://%s/api/v1/reports\n", displayURL)
	fmt.Printf("   State File    : %s\n", *stateFile)
	if *demoMode {
		fmt.Println("   Demo Mode     : Active (Preloaded sample drill records)")
	}
	if *apiKey != "" || *adminKey != "" || *viewerKey != "" || len(users) > 0 {
		fmt.Printf("   Authentication: Enabled (%d named user(s); Bearer / X-Lazarus-Key / session cookie)\n", len(users))
	} else {
		fmt.Println("   Authentication: Open (No API Key set)")
	}
	fmt.Println("==========================================================")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			log.Fatalf("server failed: %v", err)
		}
	case <-ctx.Done():
		fmt.Println("\nReceived shutdown signal, shutting down Lazarus Control Plane gracefully...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
		fmt.Println("Server stopped.")
	}
}

func getEnvOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
