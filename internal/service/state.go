package service

import (
	"time"

	"github.com/xumaojie/mocker/internal/monitor"
)

// ServiceStatus reports runtime state for a service instance.
type ServiceStatus struct {
	ID            string                   `json:"id"`
	Name          string                   `json:"name"`
	Executable    string                   `json:"executable"`
	Args          []string                 `json:"args"`
	Healthy       bool                     `json:"healthy"`
	Running       bool                     `json:"running"`
	PID           int                      `json:"pid"`
	Port          int                      `json:"port"`
	PortOpen      bool                     `json:"port_open"`
	StartedAt     *time.Time               `json:"started_at,omitempty"`
	Restarts      int                      `json:"restarts"`
	LastExitError string                   `json:"last_exit_error,omitempty"`
	LogPath       string                   `json:"log_path"`
	Metrics       monitor.ResourceSnapshot `json:"metrics"`
}
