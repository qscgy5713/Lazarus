package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"lazarus/internal/server"
)

var version = "dev"

func main() {
	addr := flag.String("addr", getEnvOrDefault("ADDR", ":8080"), "HTTP listen address (e.g. :8080)")
	apiKey := flag.String("api-key", os.Getenv("SERVER_API_KEY"), "Optional API key for webhook authentication")
	stateFile := flag.String("state", getEnvOrDefault("STATE_FILE", "lazarus-server.json"), "Path to persist target states")
	overdueStr := flag.String("overdue", getEnvOrDefault("OVERDUE_THRESHOLD", "26h"), "Threshold after which a target is marked Overdue")
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

	srv := server.New(server.Config{
		Addr:             *addr,
		APIKey:           *apiKey,
		StateFile:        *stateFile,
		OverdueThreshold: overdueThreshold,
	})

	fmt.Println("==========================================================")
	fmt.Printf("⚡ Lazarus Control Plane — Disaster Recovery Dashboard (%s)\n", version)
	fmt.Printf("   Web Dashboard : http://localhost%s\n", *addr)
	fmt.Printf("   Report Webhook: http://localhost%s/api/v1/reports\n", *addr)
	fmt.Printf("   State File    : %s\n", *stateFile)
	if *apiKey != "" {
		fmt.Println("   Authentication: Enabled (X-Lazarus-Key / Bearer Token)")
	} else {
		fmt.Println("   Authentication: Open (No API Key set)")
	}
	fmt.Println("==========================================================")

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func getEnvOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
