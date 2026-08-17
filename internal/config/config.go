package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	DefaultMemoryLimitMB = 128
	MinMemoryLimitMB     = 16
	MaxMemoryLimitMB     = 1024 * 1024
	DefaultHTTPSAddress  = "127.0.0.1:8090"
	DefaultCertFile      = "/etc/ssl/certs/server.crt"
	DefaultKeyFile       = "/etc/ssl/private/server.key"
)

var serviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ServiceConfig describes an autostartable service backed by a musl executable.
type ServiceConfig struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Path          string   `json:"path"`
	Args          []string `json:"args"`
	AutoStart     bool     `json:"auto_start"`
	RestartOnExit bool     `json:"restart_on_exit"`
}

// HTTPConfig controls the built-in management server.
type HTTPConfig struct {
	Address  string `json:"address"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type persistTracker struct {
	HTTPAddressMissing  bool
	HTTPCertFileMissing bool
	HTTPKeyFileMissing  bool
}

// AuthConfig controls UI authentication.
type AuthConfig struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// SystemConfig controls runtime limits for mocker itself.
type SystemConfig struct {
	MemoryLimitMB int `json:"memory_limit_mb"`
}

// Config holds the entire application configuration.
type Config struct {
	Roots    []string        `json:"roots"`
	Services []ServiceConfig `json:"services"`
	HTTP     HTTPConfig      `json:"http"`
	Auth     AuthConfig      `json:"auth"`
	System   SystemConfig    `json:"system"`
}

// Load reads YAML from path and returns a parsed config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	tracker := detectPersistDefaults(raw)

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	for i, root := range cfg.Roots {
		cfg.Roots[i] = filepath.Clean(root)
	}
	if err := NormalizeHTTPConfig(&cfg.HTTP); err != nil {
		return nil, err
	}
	for i := range cfg.Services {
		if err := NormalizeServiceConfig(&cfg.Services[i]); err != nil {
			return nil, err
		}
	}

	if cfg.System.MemoryLimitMB == 0 {
		cfg.System.MemoryLimitMB = DefaultMemoryLimitMB
	}
	if err := ValidateMemoryLimitMB(cfg.System.MemoryLimitMB); err != nil {
		return nil, err
	}
	if tracker.needsPersist() {
		if err := Save(path, &cfg); err != nil {
			return nil, fmt.Errorf("persist normalized config %s: %w", path, err)
		}
	}

	return &cfg, nil
}

// Save writes the configuration as pretty JSON to path.
func Save(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func ValidateMemoryLimitMB(limit int) error {
	if limit < MinMemoryLimitMB || limit > MaxMemoryLimitMB {
		return fmt.Errorf("memory_limit_mb must be between %d and %d", MinMemoryLimitMB, MaxMemoryLimitMB)
	}
	return nil
}

// NormalizeServiceConfig trims and validates service configuration in place.
func NormalizeServiceConfig(svc *ServiceConfig) error {
	svc.ID = strings.TrimSpace(svc.ID)
	svc.Name = strings.TrimSpace(svc.Name)
	svc.Path = filepath.Clean(strings.TrimSpace(svc.Path))
	svc.Args = normalizeArgs(svc.Args)
	if svc.ID == "" {
		return fmt.Errorf("service id required")
	}
	if !serviceIDPattern.MatchString(svc.ID) {
		return fmt.Errorf("service %s has invalid id: only letters, digits, dot, underscore, dash are allowed", svc.ID)
	}
	return nil
}

// NormalizeHTTPConfig normalizes HTTPS listener settings.
func NormalizeHTTPConfig(cfg *HTTPConfig) error {
	address, err := NormalizeListenAddress(cfg.Address)
	if err != nil {
		return fmt.Errorf("http.address: %w", err)
	}
	cfg.Address = address
	cfg.CertFile = strings.TrimSpace(cfg.CertFile)
	cfg.KeyFile = strings.TrimSpace(cfg.KeyFile)
	if cfg.CertFile == "" {
		cfg.CertFile = DefaultCertFile
	}
	if cfg.KeyFile == "" {
		cfg.KeyFile = DefaultKeyFile
	}
	return nil
}

// NormalizeListenAddress canonicalizes host:port.
func NormalizeListenAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		address = DefaultHTTPSAddress
	}
	if !strings.Contains(address, ":") {
		if _, err := strconv.Atoi(address); err == nil {
			address = "0.0.0.0:" + address
		} else {
			return "", fmt.Errorf("invalid address")
		}
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("invalid address")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid port")
	}
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func normalizeArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(args))
	for _, arg := range args {
		trimmed := strings.TrimSpace(arg)
		if trimmed == "" {
			continue
		}
		normalized = append(normalized, trimmed)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func detectPersistDefaults(raw map[string]json.RawMessage) persistTracker {
	tracker := persistTracker{}
	httpRaw, ok := raw["http"]
	if !ok {
		tracker.HTTPAddressMissing = true
		tracker.HTTPCertFileMissing = true
		tracker.HTTPKeyFileMissing = true
		return tracker
	}

	var httpObj map[string]json.RawMessage
	if err := json.Unmarshal(httpRaw, &httpObj); err != nil {
		tracker.HTTPAddressMissing = true
		tracker.HTTPCertFileMissing = true
		tracker.HTTPKeyFileMissing = true
		return tracker
	}
	_, tracker.HTTPAddressMissing = httpObj["address"]
	tracker.HTTPAddressMissing = !tracker.HTTPAddressMissing
	_, tracker.HTTPCertFileMissing = httpObj["cert_file"]
	tracker.HTTPCertFileMissing = !tracker.HTTPCertFileMissing
	_, tracker.HTTPKeyFileMissing = httpObj["key_file"]
	tracker.HTTPKeyFileMissing = !tracker.HTTPKeyFileMissing
	return tracker
}

func (p persistTracker) needsPersist() bool {
	return p.HTTPAddressMissing || p.HTTPCertFileMissing || p.HTTPKeyFileMissing
}
