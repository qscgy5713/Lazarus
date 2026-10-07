package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A control plane restart forgets every worker (its registry is in-memory);
// the next heartbeat's 404 must trigger a re-registration.
func TestWorkerHeartbeatReRegistersAfterControlPlaneRestart(t *testing.T) {
	var mu sync.Mutex
	registered := false
	var registers, heartbeats int
	var lastAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		lastAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/api/v1/workers/register":
			registers++
			registered = true
		case "/api/v1/workers/heartbeat":
			heartbeats++
			if !registered {
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}))
	defer srv.Close()

	wc := &workerClient{baseURL: srv.URL, token: "tok", client: srv.Client(), id: "w1", register: map[string]any{"id": "w1"}}
	wc.heartbeat(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if registers != 1 || heartbeats != 2 {
		t.Fatalf("registers=%d heartbeats=%d, want 1 and 2 (404 -> register -> retry)", registers, heartbeats)
	}
	if lastAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", lastAuth)
	}
}

func TestWorkerPollSendsTargetNamesAndRejectsErrors(t *testing.T) {
	var got struct {
		Targets []string `json:"targets"`
	}
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"has_task":true,"target_name":"orders"}`))
	}))
	defer srv.Close()

	wc := &workerClient{baseURL: srv.URL, client: &http.Client{Timeout: time.Second}, id: "w1"}
	name, err := wc.poll(context.Background(), nil, []string{"orders", "billing"})
	if err != nil || name != "orders" {
		t.Fatalf("poll = %q, %v", name, err)
	}
	if len(got.Targets) != 2 {
		t.Errorf("poll body targets = %v, want both names", got.Targets)
	}

	status = http.StatusForbidden
	if _, err := wc.poll(context.Background(), nil, nil); err == nil {
		t.Error("expected error for 403 poll response")
	}
}
