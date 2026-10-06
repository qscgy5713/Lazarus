package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Addr             string
	APIKey           string
	StateFile        string
	OverdueThreshold time.Duration
	DemoMode         bool
	AlertWebhookURL  string
	AlertFormat      string
	AlertInterval    time.Duration
}

type Server struct {
	cfg         Config
	store       *Store
	mux         *http.ServeMux
	alertMu     sync.Mutex
	lastAlerted map[string]time.Time
}

func New(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.AlertInterval <= 0 {
		cfg.AlertInterval = 10 * time.Minute
	}
	if cfg.AlertFormat == "" {
		cfg.AlertFormat = "slack"
	}
	store := NewStore(cfg.StateFile, cfg.OverdueThreshold)
	if cfg.DemoMode {
		store.SeedDemoData()
	}
	s := &Server{
		cfg:         cfg,
		store:       store,
		mux:         http.NewServeMux(),
		lastAlerted: make(map[string]time.Time),
	}
	s.routes()
	if cfg.AlertWebhookURL != "" {
		s.startAlertWorker()
	}
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(s.cfg.Addr, s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /", s.handleDashboard)
	s.mux.HandleFunc("POST /api/v1/reports", s.handlePostReport)
	s.mux.HandleFunc("GET /api/v1/targets", s.handleGetTargets)
	s.mux.HandleFunc("POST /api/v1/targets/{name}/mute", s.handleMuteTarget)
	s.mux.HandleFunc("POST /api/v1/targets/{name}/unmute", s.handleUnmuteTarget)
	s.mux.HandleFunc("GET /api/v1/reports", s.handleGetReports)
	s.mux.HandleFunc("GET /api/v1/summary", s.handleGetSummary)
	s.mux.HandleFunc("GET /api/v1/export/csv", s.handleExportCSV)
}

func (s *Server) checkAuth(r *http.Request) bool {
	if s.cfg.APIKey == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") && strings.TrimPrefix(auth, "Bearer ") == s.cfg.APIKey {
		return true
	}
	if r.Header.Get("X-Lazarus-Key") == s.cfg.APIKey || r.Header.Get("X-API-Key") == s.cfg.APIKey {
		return true
	}
	return false
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) handlePostReport(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var rep InboundReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	s.store.RecordReport(rep)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","message":"report recorded"}`))
}

func (s *Server) handleGetTargets(w http.ResponseWriter, r *http.Request) {
	targets := s.store.GetTargets()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(targets)
}

func (s *Server) handleGetReports(w http.ResponseWriter, r *http.Request) {
	reports := s.store.GetRecentReports()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reports)
}

func (s *Server) handleGetSummary(w http.ResponseWriter, r *http.Request) {
	summary := s.store.GetSummary()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(summary)
}

func (s *Server) handleMuteTarget(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, `{"error":"target name required"}`, http.StatusBadRequest)
		return
	}
	_ = s.store.SetTargetMuted(name, true)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","message":"target muted"}`))
}

func (s *Server) handleUnmuteTarget(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, `{"error":"target name required"}`, http.StatusBadRequest)
		return
	}
	_ = s.store.SetTargetMuted(name, false)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","message":"target unmuted"}`))
}

func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	targetFilter := r.URL.Query().Get("target")
	statusFilter := r.URL.Query().Get("status")
	history := s.store.GetAuditHistory(targetFilter, statusFilter, 1000)

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	filename := fmt.Sprintf("lazarus-audit-%s.csv", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)

	writer := csv.NewWriter(w)
	defer writer.Flush()

	_ = writer.Write([]string{
		"Target",
		"DrilledAt",
		"Passed",
		"Stage",
		"BackupPath",
		"BackupSize",
		"RestoreDurationMs",
		"TotalDurationMs",
		"ChecksTotal",
		"ChecksPassed",
		"ChecksFailed",
		"Error",
	})

	for _, h := range history {
		passedStr := "false"
		if h.Passed {
			passedStr = "true"
		}
		_ = writer.Write([]string{
			h.Target,
			h.DrilledAt.Format(time.RFC3339),
			passedStr,
			h.Stage,
			h.BackupPath,
			h.BackupSize,
			strconv.FormatInt(h.RestoreMs, 10),
			strconv.FormatInt(h.TotalDuration, 10),
			strconv.Itoa(h.ChecksTotal),
			strconv.Itoa(h.ChecksPassed),
			strconv.Itoa(h.ChecksFailed),
			h.Error,
		})
	}
}

func (s *Server) startAlertWorker() {
	ticker := time.NewTicker(s.cfg.AlertInterval)
	go func() {
		for range ticker.C {
			s.CheckAndSendAlerts()
		}
	}()
}

// CheckAndSendAlerts scans tracked targets and dispatches webhook notifications for overdue targets.
func (s *Server) CheckAndSendAlerts() {
	if s.cfg.AlertWebhookURL == "" {
		return
	}

	targets := s.store.GetTargets()
	now := time.Now().UTC()

	for _, t := range targets {
		if t.Status != StatusOverdue || t.Muted {
			continue
		}

		s.alertMu.Lock()
		last, alerted := s.lastAlerted[t.Name]
		if alerted && now.Sub(last) < 4*time.Hour {
			s.alertMu.Unlock()
			continue
		}
		s.lastAlerted[t.Name] = now
		s.alertMu.Unlock()

		msg := fmt.Sprintf("⚠️ [Lazarus Dead Man's Snitch] Target %q is OVERDUE! Last successful drill was at %s.",
			t.Name, t.LastDrilledAt.Format(time.RFC3339))

		payload := map[string]string{"text": msg}
		body, _ := json.Marshal(payload)

		req, err := http.NewRequest(http.MethodPost, s.cfg.AlertWebhookURL, bytes.NewReader(body))
		if err != nil {
			log.Printf("lazarus-server: failed to create alert request: %v", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("lazarus-server: alert delivery failed for target %q: %v", t.Name, err)
			continue
		}
		_ = resp.Body.Close()
	}
}
