package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var serverBin string

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "lazarus-server-test-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	serverBin = filepath.Join(tmpDir, "lazarus-server")
	build := exec.Command("go", "build", "-o", serverBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		_ = os.RemoveAll(tmpDir)
		fmt.Fprintf(os.Stderr, "build lazarus-server: %v: %s\n", err, out)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(tmpDir)
	os.Exit(code)
}

func TestVersionFlag(t *testing.T) {
	cmd := exec.Command(serverBin, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--version failed: %v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), "lazarus-server ") {
		t.Errorf("output = %q, want prefix 'lazarus-server '", string(out))
	}
}

func TestInvalidDurationFlagFails(t *testing.T) {
	cmd := exec.Command(serverBin, "--overdue", "not-a-duration")
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected failure on invalid duration, got exit 0")
	}
	if !strings.Contains(errBuf.String(), "invalid overdue duration") {
		t.Errorf("stderr = %q, want mention of invalid overdue duration", errBuf.String())
	}
}

func TestServerStartupAndGracefulShutdown(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "server-state.json")

	// Dynamically acquire an available port to avoid conflicts
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cmd := exec.Command(serverBin, "--addr", addr, "--state", stateFile)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	// Poll healthz until server is ready
	healthzURL := "http://" + addr + "/healthz"
	client := &http.Client{Timeout: 300 * time.Millisecond}
	ready := false
	for i := 0; i < 40; i++ {
		time.Sleep(50 * time.Millisecond)
		resp, err := client.Get(healthzURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			ready = true
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}

	if !ready {
		t.Fatalf("server failed to become ready at %s within 2s, output:\n%s", healthzURL, outBuf.String())
	}

	// Send Interrupt signal to test graceful shutdown
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("failed to send interrupt signal: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exited with error on signal: %v, output:\n%s", err, outBuf.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("server did not shut down gracefully within 3s, output:\n%s", outBuf.String())
	}

	if !strings.Contains(outBuf.String(), "shutting down Lazarus Control Plane gracefully") {
		t.Errorf("stdout missing graceful shutdown message, got:\n%s", outBuf.String())
	}
}
