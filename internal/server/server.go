package server

import (
	"encoding/json"
	"net/http"
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
	s.mux.HandleFunc("GET /api/v1/reports", s.handleGetReports)
	s.mux.HandleFunc("GET /api/v1/summary", s.handleGetSummary)
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
