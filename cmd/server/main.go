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
	apiKey := flag.String("api-key", os.Getenv("SERVER_API_KEY"), "Optional API key for webhook authentication")
	stateFile := flag.String("state", getEnvOrDefault("STATE_FILE", "lazarus-server.json"), "Path to persist target states")
	overdueStr := flag.String("overdue", getEnvOrDefault("OVERDUE_THRESHOLD", "26h"), "Threshold after which a target is marked Overdue")
	demoMode := flag.Bool("demo", os.Getenv("DEMO_MODE") == "true" || os.Getenv("DEMO_MODE") == "1", "Seed demo drill records on startup")
	alertWebhook := flag.String("alert-webhook", os.Getenv("ALERT_WEBHOOK_URL"), "Webhook URL for Dead Man's Snitch overdue alerts")
	alertFormat := flag.String("alert-format", getEnvOrDefault("ALERT_FORMAT", "slack"), "Alert format: slack, discord, teams")
	alertIntervalStr := flag.String("alert-interval", getEnvOrDefault("ALERT_INTERVAL", "10m"), "Interval for checking overdue alerts")
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

	srv := server.New(server.Config{
		Addr:             *addr,
		APIKey:           *apiKey,
		StateFile:        *stateFile,
		OverdueThreshold: overdueThreshold,
		DemoMode:         *demoMode,
		AlertWebhookURL:  *alertWebhook,
		AlertFormat:      *alertFormat,
		AlertInterval:    alertInterval,
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
	if *apiKey != "" {
		fmt.Println("   Authentication: Enabled (X-Lazarus-Key / Bearer Token)")
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
