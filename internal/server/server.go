package server

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr             string
	APIKey           string
	StateFile        string
	OverdueThreshold time.Duration
	DemoMode         bool
}

type Server struct {
	cfg   Config
	store *Store
	mux   *http.ServeMux
}

func New(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	store := NewStore(cfg.StateFile, cfg.OverdueThreshold)
	if cfg.DemoMode {
		store.SeedDemoData()
	}
	s := &Server{
		cfg:   cfg,
		store: store,
		mux:   http.NewServeMux(),
	}
	s.routes()
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
