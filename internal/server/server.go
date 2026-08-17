package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xumaojie/mocker/internal/config"
	"github.com/xumaojie/mocker/internal/fs"
	"github.com/xumaojie/mocker/internal/monitor"
	"github.com/xumaojie/mocker/internal/service"
)

//go:embed web/ui/*
var uiAssets embed.FS

// FileEntry describes metadata returned to the UI.
type FileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Server serves management APIs and the embedded UI.
type Server struct {
	manager    *service.Manager
	fs         *fs.Manager
	addr       string
	server     *http.Server
	configPath string
	config     config.Config
	configMu   sync.RWMutex
	auth       *authManager
	stopOnce   sync.Once
}

// NewServer builds the control plane listening on addr.
func NewServer(manager *service.Manager, fsMgr *fs.Manager, addr string, cfg *config.Config, configPath string) *Server {
	ensureAuthDefaults(cfg, configPath)
	srv := &Server{
		manager:    manager,
		fs:         fsMgr,
		addr:       addr,
		configPath: configPath,
		config:     *cfg,
		auth:       newAuthManager(cfg.Auth.Username, cfg.Auth.PasswordHash),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", srv.handleLogin)
	mux.HandleFunc("/api/auth/logout", srv.handleLogout)
	mux.HandleFunc("/api/auth/me", srv.handleMe)
	mux.HandleFunc("/api/auth/password", srv.handlePassword)
	mux.HandleFunc("/api/system/http", srv.handleHTTPConfig)
	mux.HandleFunc("/api/roots", srv.handleRoots)
	mux.HandleFunc("/api/services", srv.handleServiceList)
	mux.HandleFunc("/api/services/", srv.handleServiceAction)
	mux.HandleFunc("/api/state", srv.handleState)
	mux.HandleFunc("/api/config", srv.handleConfig)
	mux.HandleFunc("/api/metrics", srv.handleMetrics)
	mux.HandleFunc("/api/logs/", srv.handleLog)
	mux.HandleFunc("/api/files", srv.handleListFiles)
	mux.HandleFunc("/api/files/content", srv.handleFileContent)
	mux.HandleFunc("/", srv.fileHandler())

	srv.server = &http.Server{
		Addr:    addr,
		Handler: loggingMiddleware(srv.authMiddleware(mux)),
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	return srv
}

// Serve runs the management HTTP server.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.server.Shutdown(context.Background())
	}()
	go s.memoryGuardLoop(ctx)
	s.configMu.RLock()
	cfg := s.config
	s.configMu.RUnlock()
	return s.server.ListenAndServeTLS(cfg.HTTP.CertFile, cfg.HTTP.KeyFile)
}

func (s *Server) handleRoots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	respondJSON(w, struct {
		Roots []string `json:"roots"`
	}{Roots: s.fs.Roots()})
}

func (s *Server) handleServiceList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	respondJSON(w, struct {
		Services []service.ServiceStatus `json:"services"`
		Metrics  service.MetricsSnapshot `json:"metrics"`
	}{
		Services: s.manager.ListServices(),
		Metrics:  s.manager.GetMetrics(),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.auth.Enabled() {
		respondJSON(w, map[string]string{"status": "disabled"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if !s.auth.Verify(strings.TrimSpace(req.Username), req.Password) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token, expiry, err := s.auth.CreateSession()
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "mocker_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiry,
	})
	respondJSON(w, map[string]string{
		"status":   "ok",
		"username": s.auth.Username(),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cookie, _ := r.Cookie("mocker_session")
	if cookie != nil {
		s.auth.RevokeSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "mocker_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	respondJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.auth.Enabled() {
		respondJSON(w, map[string]interface{}{
			"username": "",
			"enabled":  false,
		})
		return
	}
	cookie, _ := r.Cookie("mocker_session")
	if cookie == nil || !s.auth.ValidateSession(cookie.Value) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	respondJSON(w, map[string]interface{}{
		"username": s.auth.Username(),
		"enabled":  true,
	})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.auth.Enabled() {
		http.Error(w, "auth disabled", http.StatusBadRequest)
		return
	}
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	req.Current = strings.TrimSpace(req.Current)
	req.New = strings.TrimSpace(req.New)
	if req.New == "" || len(req.New) < 6 {
		http.Error(w, "password too short", http.StatusBadRequest)
		return
	}
	if !s.auth.Verify(s.auth.Username(), req.Current) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	hash, err := hashPassword(req.New)
	if err != nil {
		http.Error(w, "hash error", http.StatusInternalServerError)
		return
	}
	s.configMu.Lock()
	cfg := s.config
	cfg.Auth.PasswordHash = hash
	s.config = cfg
	s.configMu.Unlock()

	if err := config.Save(s.configPath, &cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auth.UpdateCredentials(cfg.Auth.Username, cfg.Auth.PasswordHash)
	token, expiry, err := s.auth.CreateSession()
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "mocker_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiry,
	})
	respondJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleHTTPConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Address       string `json:"address"`
		CertFile      string `json:"cert_file"`
		KeyFile       string `json:"key_file"`
		MemoryLimitMB int    `json:"memory_limit_mb"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if req.MemoryLimitMB == 0 {
		req.MemoryLimitMB = config.DefaultMemoryLimitMB
	}
	if err := config.ValidateMemoryLimitMB(req.MemoryLimitMB); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.configMu.RLock()
	nextCfg := s.config
	s.configMu.RUnlock()
	nextCfg.HTTP.Address = req.Address
	nextCfg.HTTP.CertFile = req.CertFile
	nextCfg.HTTP.KeyFile = req.KeyFile
	nextCfg.System.MemoryLimitMB = req.MemoryLimitMB
	if err := config.NormalizeHTTPConfig(&nextCfg.HTTP); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := tls.LoadX509KeyPair(nextCfg.HTTP.CertFile, nextCfg.HTTP.KeyFile); err != nil {
		http.Error(w, fmt.Sprintf("load certificate: %v", err), http.StatusBadRequest)
		return
	}
	if err := config.Save(s.configPath, &nextCfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.configMu.Lock()
	s.config = nextCfg
	s.configMu.Unlock()
	respondJSON(w, map[string]interface{}{
		"status":           "ok",
		"restart_required": true,
		"address":          nextCfg.HTTP.Address,
		"cert_file":        nextCfg.HTTP.CertFile,
		"key_file":         nextCfg.HTTP.KeyFile,
		"memory_limit_mb":  req.MemoryLimitMB,
	})
}

func (s *Server) handleServiceAction(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/api/services/")
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]
	switch action {
	case "start":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.manager.StartService(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		respondJSON(w, map[string]string{"status": "starting"})
	case "stop":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.manager.StopService(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		respondJSON(w, map[string]string{"status": "stopping"})
	case "restart":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.manager.RestartService(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		respondJSON(w, map[string]string{"status": "restarting"})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/api/logs/")
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	lines := 200
	if l := r.URL.Query().Get("lines"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			lines = n
		}
	}
	logContent, err := s.manager.TailLog(rel, lines)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	respondJSON(w, map[string]string{"log": logContent})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	respondJSON(w, s.manager.GetMetrics())
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	respondJSON(w, s.currentStateView())
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		respondJSON(w, s.currentStateView())
	case http.MethodPost:
		var req configUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		services, err := s.buildServiceConfigs(req.Services)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.configMu.RLock()
		currentCfg := s.config
		s.configMu.RUnlock()
		nextCfg := currentCfg
		nextCfg.Services = services
		if err := config.Save(s.configPath, &nextCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.manager.SyncServices(services); err != nil {
			_ = config.Save(s.configPath, &currentCfg)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.configMu.Lock()
		s.config = nextCfg
		s.configMu.Unlock()
		respondJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query().Get("path")
	if query == "" {
		entries := make([]FileEntry, 0, len(s.fs.Roots()))
		for _, root := range s.fs.Roots() {
			entries = append(entries, FileEntry{Name: filepath.Base(root), Path: root, IsDir: true})
		}
		respondJSON(w, entries)
		return
	}
	abs, err := s.fs.Resolve(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !info.IsDir() {
		respondJSON(w, []FileEntry{{Name: info.Name(), Path: abs, IsDir: false, Size: info.Size(), ModTime: info.ModTime()}})
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	response := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		response = append(response, FileEntry{
			Name:    entry.Name(),
			Path:    filepath.Join(abs, entry.Name()),
			IsDir:   entry.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	respondJSON(w, response)
}

func (s *Server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	abs, err := s.fs.Resolve(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(abs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if !isTextFileContent(data) {
			http.Error(w, "only UTF-8 text files are supported for editing", http.StatusUnsupportedMediaType)
			return
		}
		resp := map[string]interface{}{
			"path":      abs,
			"size":      len(data),
			"is_binary": false,
			"content":   string(data),
			"encoding":  "utf-8",
		}
		respondJSON(w, resp)
	case http.MethodPut:
		var req struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Encoding != "" && req.Encoding != "utf-8" {
			http.Error(w, "only utf-8 encoding is supported", http.StatusBadRequest)
			return
		}
		existing, readErr := os.ReadFile(abs)
		if readErr == nil && !isTextFileContent(existing) {
			http.Error(w, "editing binary files is not supported", http.StatusUnsupportedMediaType)
			return
		}
		if readErr != nil && !os.IsNotExist(readErr) {
			http.Error(w, readErr.Error(), http.StatusInternalServerError)
			return
		}
		payload := []byte(req.Content)
		if !isTextFileContent(payload) {
			http.Error(w, "only UTF-8 text content is supported", http.StatusUnsupportedMediaType)
			return
		}
		if err := os.WriteFile(abs, payload, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		respondJSON(w, map[string]string{"status": "written"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func isTextFileContent(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	return utf8.Valid(data)
}

func (s *Server) fileHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" || path == "" {
			path = "index.html"
		} else {
			path = strings.TrimPrefix(path, "/")
		}
		file, err := uiAssets.Open(filepath.Join("web/ui", path))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()

		if ext := filepath.Ext(path); ext != "" {
			w.Header().Set("Content-Type", mime.TypeByExtension(ext))
		}
		_, _ = io.Copy(w, file)
	}
}

func respondJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

type serviceView struct {
	Config       config.ServiceConfig  `json:"config"`
	Status       service.ServiceStatus `json:"status"`
	Root         string                `json:"root"`
	RelativePath string                `json:"relative_path"`
}

type stateView struct {
	Services []serviceView           `json:"services"`
	Metrics  service.MetricsSnapshot `json:"metrics"`
	Roots    []string                `json:"roots"`
	HTTP     config.HTTPConfig       `json:"http"`
	System   config.SystemConfig     `json:"system"`
}

func (s *Server) currentStateView() stateView {
	s.configMu.RLock()
	cfg := s.config
	s.configMu.RUnlock()

	statuses := s.manager.ListServices()
	statusMap := make(map[string]service.ServiceStatus, len(statuses))
	for _, st := range statuses {
		statusMap[st.ID] = st
	}

	views := make([]serviceView, 0, len(cfg.Services))
	for _, svc := range cfg.Services {
		view := serviceView{
			Config: svc,
			Status: statusMap[svc.ID],
		}
		view.Root, view.RelativePath = s.splitRootAndRelative(svc.Path)
		if view.Root == "" && view.Status.Executable != "" {
			view.Root, view.RelativePath = s.splitRootAndRelative(view.Status.Executable)
		}
		views = append(views, view)
		statusMap[svc.ID] = view.Status
	}

	return stateView{
		Services: views,
		Metrics:  s.manager.GetMetrics(),
		Roots:    s.fs.Roots(),
		HTTP:     cfg.HTTP,
		System:   cfg.System,
	}
}

func (s *Server) memoryGuardLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	pid := os.Getpid()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.configMu.RLock()
		limitMB := s.config.System.MemoryLimitMB
		s.configMu.RUnlock()
		if limitMB <= 0 {
			limitMB = config.DefaultMemoryLimitMB
		}
		snap, err := monitor.ReadProcessJiffies(pid)
		if err != nil {
			continue
		}
		limitBytes := uint64(limitMB) * 1024 * 1024
		if snap.RSS <= limitBytes {
			continue
		}
		log.Printf("[system] mocker memory limit exceeded: rss=%d bytes, limit=%d bytes", snap.RSS, limitBytes)
		s.stopOnce.Do(func() {
			go func() {
				s.manager.Stop()
				_ = s.server.Shutdown(context.Background())
				time.Sleep(200 * time.Millisecond)
				os.Exit(137)
			}()
		})
		return
	}
}

type configUpdateRequest struct {
	Services []configUpdateService `json:"services"`
}

type configUpdateService struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Root          string   `json:"root"`
	Path          string   `json:"path"`
	Args          []string `json:"args"`
	AutoStart     bool     `json:"auto_start"`
	RestartOnExit bool     `json:"restart_on_exit"`
}

func (s *Server) buildServiceConfigs(items []configUpdateService) ([]config.ServiceConfig, error) {
	roots := s.fs.Roots()
	if len(roots) == 0 {
		return nil, fmt.Errorf("no roots configured")
	}
	allowed := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		allowed[root] = struct{}{}
	}

	services := make([]config.ServiceConfig, 0, len(items))
	for i, item := range items {
		if item.ID == "" {
			return nil, fmt.Errorf("service at index %d missing id", i)
		}
		root := item.Root
		if root == "" {
			root = roots[0]
		}
		if _, ok := allowed[root]; !ok {
			return nil, fmt.Errorf("service %s root %s not allowed", item.ID, root)
		}
		if item.Path == "" {
			return nil, fmt.Errorf("service %s missing path", item.ID)
		}
		execPath := buildExecPath(root, item.Path)
		svc := config.ServiceConfig{
			ID:            item.ID,
			Name:          item.Name,
			Path:          execPath,
			Args:          append([]string(nil), item.Args...),
			AutoStart:     item.AutoStart,
			RestartOnExit: item.RestartOnExit,
		}
		if err := config.NormalizeServiceConfig(&svc); err != nil {
			return nil, err
		}
		services = append(services, svc)
	}
	return services, nil
}

func (s *Server) splitRootAndRelative(path string) (string, string) {
	cleaned := filepath.Clean(path)
	for _, root := range s.fs.Roots() {
		rel, err := filepath.Rel(root, cleaned)
		if err != nil {
			continue
		}
		if rel == "." {
			return root, ""
		}
		if !strings.HasPrefix(rel, "..") {
			return root, rel
		}
	}
	return "", cleaned
}

func buildExecPath(root, rel string) string {
	trimmed := strings.TrimSpace(rel)
	if trimmed == "" {
		return filepath.Clean(root)
	}
	if filepath.IsAbs(trimmed) {
		return filepath.Clean(trimmed)
	}
	return filepath.Clean(filepath.Join(root, trimmed))
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/") {
			if path == "/api/auth/login" || path == "/api/auth/me" {
				next.ServeHTTP(w, r)
				return
			}
			cookie, _ := r.Cookie("mocker_session")
			if cookie == nil || !s.auth.ValidateSession(cookie.Value) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func ensureAuthDefaults(cfg *config.Config, configPath string) {
	changed := false
	if cfg.Auth.Username == "" {
		cfg.Auth.Username = "admin"
		changed = true
	}
	if cfg.Auth.PasswordHash == "" {
		hash, err := hashPassword("admin")
		if err != nil {
			log.Printf("[auth] default password hash error: %v", err)
		} else {
			cfg.Auth.PasswordHash = hash
			changed = true
		}
	}
	if changed && configPath != "" {
		if err := config.Save(configPath, cfg); err != nil {
			log.Printf("[auth] persist auth defaults failed: %v", err)
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		duration := time.Since(start)
		if sw.status >= 500 || duration > 2*time.Second {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, duration)
		}
	})
}
