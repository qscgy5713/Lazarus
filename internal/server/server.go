package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lazarus/internal/certpdf"
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
	cfg           Config
	store         *Store
	mux           *http.ServeMux
	httpSrv       *http.Server
	httpSrvMu     sync.Mutex
	startTime     time.Time
	alertMu       sync.Mutex
	lastAlerted   map[string]time.Time
	alertStop     chan struct{}
	alertDone     chan struct{}
	streamClients map[chan []byte]struct{}
	streamMu      sync.RWMutex
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
		cfg:           cfg,
		store:         store,
		mux:           http.NewServeMux(),
		startTime:     time.Now(),
		lastAlerted:   make(map[string]time.Time),
		alertStop:     make(chan struct{}),
		alertDone:     make(chan struct{}),
		streamClients: make(map[chan []byte]struct{}),
	}
	s.routes()
	if cfg.AlertWebhookURL != "" {
		s.startAlertWorker()
	} else {
		close(s.alertDone)
	}
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) ListenAndServe() error {
	s.httpSrvMu.Lock()
	s.httpSrv = &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.httpSrvMu.Unlock()
	return s.httpSrv.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server and stops background alert workers.
func (s *Server) Shutdown(ctx context.Context) error {
	select {
	case <-s.alertStop:
	default:
		close(s.alertStop)
	}

	select {
	case <-s.alertDone:
	case <-ctx.Done():
	}

	s.httpSrvMu.Lock()
	srv := s.httpSrv
	s.httpSrvMu.Unlock()

	if srv != nil {
		return srv.Shutdown(ctx)
	}
	return nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /", s.handleDashboard)
	s.mux.HandleFunc("POST /api/v1/reports", s.handlePostReport)
	s.mux.HandleFunc("GET /api/v1/targets", s.handleGetTargets)
	s.mux.HandleFunc("POST /api/v1/targets/{name}/mute", s.handleMuteTarget)
	s.mux.HandleFunc("POST /api/v1/targets/{name}/unmute", s.handleUnmuteTarget)
	s.mux.HandleFunc("POST /api/v1/targets/{name}/trigger", s.handleTriggerTarget)
	s.mux.HandleFunc("GET /api/v1/reports", s.handleGetReports)
	s.mux.HandleFunc("GET /api/v1/summary", s.handleGetSummary)
	s.mux.HandleFunc("GET /api/v1/export/csv", s.handleExportCSV)
	s.mux.HandleFunc("GET /api/v1/export/certificate.pdf", s.handleExportPDF)
	s.mux.HandleFunc("GET /api/v1/stream", s.handleStream)
}

func (s *Server) checkAuth(r *http.Request) bool {
	if s.cfg.APIKey == "" {
		return true
	}
	expected := []byte(s.cfg.APIKey)

	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := []byte(strings.TrimPrefix(auth, "Bearer "))
		if subtle.ConstantTimeCompare(token, expected) == 1 {
			return true
		}
	}
	for _, header := range []string{"X-Lazarus-Key", "X-API-Key"} {
		if val := r.Header.Get(header); val != "" {
			if subtle.ConstantTimeCompare([]byte(val), expected) == 1 {
				return true
			}
		}
	}
	return false
}

func (s *Server) checkStreamAuth(r *http.Request) bool {
	if s.checkAuth(r) {
		return true
	}
	if s.cfg.APIKey == "" {
		return true
	}
	key := r.URL.Query().Get("api_key")
	if key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.APIKey)) == 1 {
		return true
	}
	return false
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	summary := s.store.GetSummary()
	uptime := time.Since(s.startTime).Round(time.Second).Seconds()

	resp := map[string]any{
		"status":          "ok",
		"uptime_seconds":  uptime,
		"targets_tracked": summary.TotalTargets,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Verify memory store is accessible
	_ = s.store.GetSummary()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
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

	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
	var rep InboundReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	s.store.RecordReport(rep)
	s.Broadcast("drill_report", rep)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","message":"report recorded"}`))
}

func (s *Server) handleGetTargets(w http.ResponseWriter, r *http.Request) {
	tag := r.URL.Query().Get("tag")
	targets := s.store.GetTargetsFiltered(tag)
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
	if err := s.store.SetTargetMuted(name, true); err != nil {
		http.Error(w, `{"error":"target not found"}`, http.StatusNotFound)
		return
	}
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
	if err := s.store.SetTargetMuted(name, false); err != nil {
		http.Error(w, `{"error":"target not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","message":"target unmuted"}`))
}

func (s *Server) handleTriggerTarget(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, `{"error":"target name required"}`, http.StatusBadRequest)
		return
	}
	if err := s.store.TriggerTarget(name); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
		return
	}
	s.Broadcast("drill_triggered", map[string]string{"target": name})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "triggered",
		"target":  name,
		"message": "drill triggered successfully",
	})
}

func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	targetFilter := r.URL.Query().Get("target")
	statusFilter := r.URL.Query().Get("status")
	tagFilter := r.URL.Query().Get("tag")
	history := s.store.GetAuditHistory(targetFilter, statusFilter, tagFilter, 1000)

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	filename := fmt.Sprintf("lazarus-audit-%s.csv", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)

	writer := csv.NewWriter(w)
	defer writer.Flush()

	_ = writer.Write([]string{
		"Target",
		"Tags",
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
			sanitizeCSVField(h.Target),
			sanitizeCSVField(strings.Join(h.Tags, "; ")),
			h.DrilledAt.Format(time.RFC3339),
			passedStr,
			sanitizeCSVField(h.Stage),
			sanitizeCSVField(h.BackupPath),
			sanitizeCSVField(h.BackupSize),
			strconv.FormatInt(h.RestoreMs, 10),
			strconv.FormatInt(h.TotalDuration, 10),
			strconv.Itoa(h.ChecksTotal),
			strconv.Itoa(h.ChecksPassed),
			strconv.Itoa(h.ChecksFailed),
			sanitizeCSVField(h.Error),
		})
	}
}

func (s *Server) handleExportPDF(w http.ResponseWriter, r *http.Request) {
	tagFilter := r.URL.Query().Get("tag")
	targets := s.store.GetTargetsFiltered(tagFilter)
	summary := s.store.GetSummary()

	var targetAudits []certpdf.TargetAudit
	for _, t := range targets {
		restoreDurationStr := "-"
		if t.LastRestoreMs > 0 {
			restoreDurationStr = fmt.Sprintf("%dms", t.LastRestoreMs)
		}
		backupSizeStr := t.LastBackupSize
		if backupSizeStr == "" {
			backupSizeStr = "-"
		}
		checksPassed := 0
		for _, c := range t.LastChecks {
			if c.Passed {
				checksPassed++
			}
		}

		verdict := "PASS"
		if t.Status == StatusMuted {
			verdict = "MUTED"
		} else if t.Status == StatusOverdue {
			verdict = "OVERDUE"
		} else if !t.LastPassed {
			verdict = "FAIL"
		} else if !t.SLAPassed {
			verdict = "SLA MISS"
		}

		targetAudits = append(targetAudits, certpdf.TargetAudit{
			Name:            t.Name,
			Passed:          t.LastPassed,
			Verdict:         verdict,
			Stage:           t.LastStage,
			RestoreDuration: restoreDurationStr,
			BackupSize:      backupSizeStr,
			BackupAge:       t.LastBackupAge,
			ChecksPassed:    checksPassed,
			ChecksTotal:     len(t.LastChecks),
			Error:           t.LastError,
		})
	}

	host := "lazarus-control-plane"
	if len(targets) > 0 && targets[0].LastHostname != "" {
		host = targets[0].LastHostname
	}

	eligible := summary.TotalTargets - summary.Muted
	certData := certpdf.CertificateData{
		Hostname:      host,
		GeneratedAt:   time.Now().UTC(),
		TotalTargets:  summary.TotalTargets,
		PassedTargets: summary.Healthy,
		Eligible:      eligible,
		SLAPercentage: summary.SLAPercentage,
		Targets:       targetAudits,
	}

	pdfData := certpdf.Generate(certData)

	filename := fmt.Sprintf("lazarus-audit-certificate-%s.pdf", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", strconv.Itoa(len(pdfData)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdfData)
}

// sanitizeCSVField prevents CSV Formula Injection when opened in Excel/Sheets.
func sanitizeCSVField(val string) string {
	if len(val) > 0 && (val[0] == '=' || val[0] == '+' || val[0] == '-' || val[0] == '@' || val[0] == '\t' || val[0] == '\r') {
		return "'" + val
	}
	return val
}

func (s *Server) startAlertWorker() {
	ticker := time.NewTicker(s.cfg.AlertInterval)
	go func() {
		defer close(s.alertDone)
		defer ticker.Stop()
		for {
			select {
			case <-s.alertStop:
				return
			case <-ticker.C:
				s.CheckAndSendAlerts()
			}
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
		if t.Status == StatusHealthy {
			s.alertMu.Lock()
			delete(s.lastAlerted, t.Name)
			s.alertMu.Unlock()
			continue
		}

		if t.Status != StatusOverdue || t.Muted {
			continue
		}

		s.alertMu.Lock()
		last, alerted := s.lastAlerted[t.Name]
		if alerted && now.Sub(last) < 4*time.Hour {
			s.alertMu.Unlock()
			continue
		}
		s.alertMu.Unlock()

		msg := fmt.Sprintf("⚠️ [Lazarus Dead Man's Snitch] Target %q is OVERDUE! Last successful drill was at %s.",
			t.Name, t.LastDrilledAt.Format(time.RFC3339))

		var body []byte
		switch strings.ToLower(s.cfg.AlertFormat) {
		case "discord":
			body, _ = json.Marshal(map[string]string{"content": msg})
		default: // slack, teams, generic, etc.
			body, _ = json.Marshal(map[string]string{"text": msg})
		}

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

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.alertMu.Lock()
			s.lastAlerted[t.Name] = now
			s.alertMu.Unlock()
		} else {
			log.Printf("lazarus-server: alert delivery for target %q returned status %d", t.Name, resp.StatusCode)
		}
	}
}

// Broadcast sends an SSE event to all connected live clients.
func (s *Server) Broadcast(eventType string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, string(data)))

	s.streamMu.RLock()
	defer s.streamMu.RUnlock()

	for ch := range s.streamClients {
		select {
		case ch <- msg:
		default:
			// Client buffer full, skip
		}
	}
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if !s.checkStreamAuth(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming unsupported"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	msgCh := make(chan []byte, 32)
	s.streamMu.Lock()
	s.streamClients[msgCh] = struct{}{}
	s.streamMu.Unlock()

	defer func() {
		s.streamMu.Lock()
		delete(s.streamClients, msgCh)
		s.streamMu.Unlock()
	}()

	// Send initial connected ping
	initData, _ := json.Marshal(map[string]any{"status": "connected", "time": time.Now().UTC()})
	fmt.Fprintf(w, "event: connected\ndata: %s\n\n", initData)
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-msgCh:
			_, _ = w.Write(msg)
			flusher.Flush()
		case <-ticker.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}
