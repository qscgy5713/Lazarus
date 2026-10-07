package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// User is a named API key. Exactly one of Key (plaintext) or KeySHA256 (hex
// SHA-256 of the key, so the file never holds the secret itself) is set.
type User struct {
	Name      string   `yaml:"name" json:"name"`
	Role      UserRole `yaml:"role" json:"role"`
	Key       string   `yaml:"key" json:"-"`
	KeySHA256 string   `yaml:"key_sha256" json:"-"`
}

// Principal is who a request authenticated as.
type Principal struct {
	Name string
	Role UserRole
}

const (
	sessionCookie = "lazarus_session"
	// csrfHeader must accompany cookie-authenticated state-changing requests.
	// A custom header can't be set cross-site without a CORS preflight, which
	// this server never grants.
	csrfHeader = "X-Lazarus-CSRF"
)

// LoadUsers reads a YAML (or JSON, a YAML subset) file of the form:
//
//	users:
//	  - name: alice
//	    role: admin
//	    key_sha256: <hex sha256 of alice's key>
//	  - name: auditor
//	    role: viewer
//	    key: some-plaintext-key
func LoadUsers(path string) ([]User, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read users file: %w", err)
	}
	var doc struct {
		Users []User `yaml:"users"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse users file: %w", err)
	}
	seen := map[string]bool{}
	for i, u := range doc.Users {
		if u.Name == "" {
			return nil, fmt.Errorf("users[%d]: name is required", i)
		}
		if seen[u.Name] {
			return nil, fmt.Errorf("users: duplicate name %q", u.Name)
		}
		seen[u.Name] = true
		if u.Role != RoleAdmin && u.Role != RoleViewer {
			return nil, fmt.Errorf("user %q: role must be admin or viewer", u.Name)
		}
		if (u.Key == "") == (u.KeySHA256 == "") {
			return nil, fmt.Errorf("user %q: set exactly one of key or key_sha256", u.Name)
		}
		if u.KeySHA256 != "" {
			b, err := hex.DecodeString(u.KeySHA256)
			if err != nil || len(b) != sha256.Size {
				return nil, fmt.Errorf("user %q: key_sha256 must be 64 hex characters", u.Name)
			}
			doc.Users[i].KeySHA256 = strings.ToLower(u.KeySHA256)
		}
	}
	return doc.Users, nil
}

// adminKey is the explicit admin key, falling back to the legacy APIKey.
func (s *Server) adminKey() string {
	if s.cfg.AdminKey != "" {
		return s.cfg.AdminKey
	}
	return s.cfg.APIKey
}

// authRequired reports whether any credential is configured. With none,
// every caller is treated as admin for backward compatibility.
func (s *Server) authRequired() bool {
	return s.adminKey() != "" || s.cfg.ViewerKey != "" || len(s.cfg.Users) > 0
}

// principalForKey resolves a presented API key.
func (s *Server) principalForKey(token string) (Principal, bool) {
	if token == "" {
		return Principal{}, false
	}
	if k := s.adminKey(); k != "" && subtle.ConstantTimeCompare([]byte(token), []byte(k)) == 1 {
		return Principal{Name: "admin-key", Role: RoleAdmin}, true
	}
	if k := s.cfg.ViewerKey; k != "" && subtle.ConstantTimeCompare([]byte(token), []byte(k)) == 1 {
		return Principal{Name: "viewer-key", Role: RoleViewer}, true
	}
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	for _, u := range s.cfg.Users {
		if u.Key != "" && subtle.ConstantTimeCompare([]byte(token), []byte(u.Key)) == 1 {
			return Principal{Name: u.Name, Role: u.Role}, true
		}
		if u.KeySHA256 != "" && subtle.ConstantTimeCompare([]byte(digest), []byte(u.KeySHA256)) == 1 {
			return Principal{Name: u.Name, Role: u.Role}, true
		}
	}
	return Principal{}, false
}

// authResult is the outcome of authenticating a request.
type authResult struct {
	principal Principal
	ok        bool
	// presented is true when the request carried some credential (valid or
	// not); only those count toward the brute-force limiter.
	presented bool
	viaCookie bool
}

func (s *Server) authenticate(r *http.Request) authResult {
	if !s.authRequired() {
		return authResult{principal: Principal{Name: "anonymous", Role: RoleAdmin}, ok: true}
	}
	if token := bearerOrHeaderToken(r); token != "" {
		p, ok := s.principalForKey(token)
		return authResult{principal: p, ok: ok, presented: true}
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		p, ok := s.sessions.get(c.Value)
		// A stale cookie (e.g. after a control plane restart) is not a
		// guessing attempt — session IDs are 256-bit random — and counting it
		// would lock users out of signing back in.
		return authResult{principal: p, ok: ok, viaCookie: true}
	}
	// Kept for backward compatibility with scripts; the web UI uses the
	// session cookie so keys stay out of URLs and access logs.
	if q := r.URL.Query().Get("api_key"); q != "" {
		p, ok := s.principalForKey(q)
		return authResult{principal: p, ok: ok, presented: true}
	}
	return authResult{}
}

func bearerOrHeaderToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	for _, header := range []string{"X-Lazarus-Key", "X-API-Key"} {
		if val := r.Header.Get(header); val != "" {
			return val
		}
	}
	return ""
}

// authorize authenticates r and checks it holds at least minRole, writing
// the error response itself when it doesn't.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, minRole UserRole) (Principal, bool) {
	ip := clientIP(r)
	if s.limiter.blocked(ip) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, `{"error":"too many failed authentication attempts"}`, http.StatusTooManyRequests)
		return Principal{}, false
	}

	res := s.authenticate(r)
	if !res.ok {
		if res.presented {
			s.limiter.fail(ip)
		}
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return Principal{}, false
	}
	if res.viaCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
		http.Error(w, `{"error":"missing `+csrfHeader+` header"}`, http.StatusForbidden)
		return Principal{}, false
	}
	if res.principal.Role == RoleAdmin || (minRole == RoleViewer && res.principal.Role == RoleViewer) {
		return res.principal, true
	}
	http.Error(w, `{"error":"forbidden: admin role required"}`, http.StatusForbidden)
	return Principal{}, false
}

func (s *Server) requireRole(w http.ResponseWriter, r *http.Request, minRole UserRole) bool {
	_, ok := s.authorize(w, r, minRole)
	return ok
}

// clientIP is the TCP peer address. X-Forwarded-For is deliberately not
// trusted: anyone could spoof it to dodge the limiter.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sessionStore holds browser sessions created by /api/v1/auth/login. They
// live in memory: a control plane restart just means signing in again.
type sessionStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	sessions map[string]sessionEntry
}

type sessionEntry struct {
	principal Principal
	expires   time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, sessions: make(map[string]sessionEntry)}
}

func (ss *sessionStore) create(p Principal) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	now := time.Now()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for k, e := range ss.sessions {
		if now.After(e.expires) {
			delete(ss.sessions, k)
		}
	}
	ss.sessions[id] = sessionEntry{principal: p, expires: now.Add(ss.ttl)}
	return id, nil
}

func (ss *sessionStore) get(id string) (Principal, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	e, ok := ss.sessions[id]
	if !ok || time.Now().After(e.expires) {
		delete(ss.sessions, id)
		return Principal{}, false
	}
	return e.principal, true
}

func (ss *sessionStore) delete(id string) {
	ss.mu.Lock()
	delete(ss.sessions, id)
	ss.mu.Unlock()
}

// authLimiter blocks an IP for one window after max failed attempts within
// a window.
type authLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	state  map[string]*limitEntry
}

type limitEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

func newAuthLimiter(max int, window time.Duration) *authLimiter {
	return &authLimiter{max: max, window: window, state: make(map[string]*limitEntry)}
}

func (l *authLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.state[ip]
	return ok && time.Now().Before(e.blockedUntil)
}

func (l *authLimiter) fail(ip string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.state) > 10000 {
		for k, e := range l.state {
			if now.Sub(e.windowStart) > l.window && now.After(e.blockedUntil) {
				delete(l.state, k)
			}
		}
	}
	e, ok := l.state[ip]
	if !ok || now.Sub(e.windowStart) > l.window {
		e = &limitEntry{windowStart: now}
		l.state[ip] = e
	}
	e.failures++
	if e.failures >= l.max {
		e.blockedUntil = now.Add(l.window)
		e.failures = 0
		e.windowStart = now
	}
}
