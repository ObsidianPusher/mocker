package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xumaojie/mocker/internal/config"
	"github.com/xumaojie/mocker/internal/fs"
	"github.com/xumaojie/mocker/internal/monitor"
)

var (
	errServiceNotFound = errors.New("service not found")
)

const (
	restartBackoff   = 2 * time.Second
	logTailChunkSize = 64 * 1024
	logCleanupEvery  = 24 * time.Hour
	stopWaitTimeout  = 7 * time.Second
	stopWaitInterval = 50 * time.Millisecond
)

// Manager controls services and their metrics.
type Manager struct {
	fs             *fs.Manager
	services       map[string]*instance
	servicesMu     sync.RWMutex
	metricsMu      sync.RWMutex
	metrics        MetricsSnapshot
	stopCh         chan struct{}
	stopOnce       sync.Once
	lastMonitorLog time.Time
}

type instance struct {
	cfg                config.ServiceConfig
	manager            *Manager
	mu                 sync.RWMutex
	state              ServiceStatus
	loopCtx            context.Context
	loopCancel         context.CancelFunc
	cmd                *exec.Cmd
	running            bool
	logPath            string
	metrics            monitor.ResourceSnapshot
	prevProcessJiffies uint64
	prevSystemJiffies  uint64
}

// MetricsSnapshot exposes the latest resource data for services and aggregate.
type MetricsSnapshot struct {
	Services map[string]monitor.ResourceSnapshot `json:"services"`
	Total    monitor.ResourceSnapshot            `json:"total"`
	System   monitor.SystemMetrics               `json:"system"`
}

// NewManager creates a manager and starts the metrics loop.
func NewManager(fsMgr *fs.Manager, cfg *config.Config) (*Manager, error) {
	if fsMgr == nil {
		return nil, errors.New("fs manager missing")
	}

	mgr := &Manager{
		fs:       fsMgr,
		services: make(map[string]*instance, len(cfg.Services)),
		stopCh:   make(chan struct{}),
		metrics: MetricsSnapshot{
			Services: make(map[string]monitor.ResourceSnapshot),
		},
	}

	if err := mgr.SyncServices(cfg.Services); err != nil {
		return nil, err
	}

	go mgr.metricsLoop()
	go mgr.logCleanupLoop()

	return mgr, nil
}

// Stop shuts down the manager and all running services.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		m.servicesMu.RLock()
		instances := make([]*instance, 0, len(m.services))
		for _, inst := range m.services {
			instances = append(instances, inst)
		}
		m.servicesMu.RUnlock()
		for _, inst := range instances {
			_ = inst.stop()
		}
	})
}

// ListServices returns status snapshots for every configured service.
func (m *Manager) ListServices() []ServiceStatus {
	m.servicesMu.RLock()
	instances := make([]*instance, 0, len(m.services))
	for _, inst := range m.services {
		instances = append(instances, inst)
	}
	m.servicesMu.RUnlock()

	result := make([]ServiceStatus, 0, len(instances))
	for _, inst := range instances {
		result = append(result, inst.status())
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].ID < result[j].ID
		}
		return result[i].Name < result[j].Name
	})
	return result
}

// GetMetrics returns the most recent metrics snapshot.
func (m *Manager) GetMetrics() MetricsSnapshot {
	m.metricsMu.RLock()
	defer m.metricsMu.RUnlock()
	copyServices := make(map[string]monitor.ResourceSnapshot, len(m.metrics.Services))
	for k, v := range m.metrics.Services {
		copyServices[k] = v
	}
	return MetricsSnapshot{
		Services: copyServices,
		Total:    m.metrics.Total,
		System:   m.metrics.System,
	}
}

// StartService requests a service start and returns immediately.
func (m *Manager) StartService(id string) error {
	inst, ok := m.getInstance(id)
	if !ok {
		return errServiceNotFound
	}
	return inst.start()
}

// StopService cancels a running service.
func (m *Manager) StopService(id string) error {
	inst, ok := m.getInstance(id)
	if !ok {
		return errServiceNotFound
	}
	return inst.stop()
}

// RestartService stops and starts a managed service.
func (m *Manager) RestartService(id string) error {
	inst, ok := m.getInstance(id)
	if !ok {
		return errServiceNotFound
	}
	return inst.restart()
}

// TailLog returns the trailing part of the log file for a service.
func (m *Manager) TailLog(id string, lines int) (string, error) {
	inst, ok := m.getInstance(id)
	if !ok {
		return "", errServiceNotFound
	}
	return tailFile(inst.getLogPath(), lines)
}

func (m *Manager) metricsLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	pid := os.Getpid()
	var selfPrev monitor.JiffiesSnapshot
	var selfPrevTotal uint64
	var prevSystem monitor.SystemSnapshot
	var prevNet monitor.NetworkCounters
	var prevNetAt time.Time

	for {
		select {
		case <-ticker.C:
		case <-m.stopCh:
			return
		}

		systemSnap, err := monitor.ReadSystemSnapshot()
		if err != nil {
			m.logMonitorError("read system cpu", err)
			continue
		}

		systemPercent := 0.0
		if prevSystem.Total != 0 && systemSnap.Total > prevSystem.Total {
			totalDelta := systemSnap.Total - prevSystem.Total
			idleDelta := uint64(0)
			if systemSnap.Idle >= prevSystem.Idle {
				idleDelta = systemSnap.Idle - prevSystem.Idle
			}
			if totalDelta > 0 && idleDelta < totalDelta {
				systemPercent = (1.0 - float64(idleDelta)/float64(totalDelta)) * 100
			}
		}
		if systemPercent < 0 {
			systemPercent = 0
		} else if systemPercent > 100 {
			systemPercent = 100
		}
		memInfo, err := monitor.ReadMemoryInfo()
		if err != nil {
			m.logMonitorError("read meminfo", err)
		}
		now := time.Now()
		networkMetrics := monitor.NetworkMetrics{}
		netCounters, err := monitor.ReadNetworkCounters()
		if err != nil {
			m.logMonitorError("read network", err)
		} else {
			if !prevNetAt.IsZero() && now.After(prevNetAt) {
				seconds := now.Sub(prevNetAt).Seconds()
				if seconds > 0 {
					if netCounters.RXBytes >= prevNet.RXBytes {
						networkMetrics.DownloadBytesPerSec = uint64(float64(netCounters.RXBytes-prevNet.RXBytes) / seconds)
					}
					if netCounters.TXBytes >= prevNet.TXBytes {
						networkMetrics.UploadBytesPerSec = uint64(float64(netCounters.TXBytes-prevNet.TXBytes) / seconds)
					}
				}
			}
			prevNet = netCounters
			prevNetAt = now
		}
		aggregateMem := uint64(0)
		m.servicesMu.RLock()
		instances := make([]*instance, 0, len(m.services))
		for _, inst := range m.services {
			instances = append(instances, inst)
		}
		m.servicesMu.RUnlock()
		snapshotServices := make(map[string]monitor.ResourceSnapshot, len(instances))

		for _, inst := range instances {
			snap := inst.updateMetrics(systemSnap.Total)
			snapshotServices[inst.cfg.ID] = snap
			aggregateMem += snap.MemoryBytes
		}

		selfSnap := monitor.ResourceSnapshot{}
		if stat, err := monitor.ReadProcessJiffies(pid); err == nil {
			snap, proc, total := monitor.ComputeSnapshot(selfPrev.Process, selfPrevTotal, stat, systemSnap.Total)
			selfSnap = snap
			selfPrev.Process = proc
			selfPrevTotal = total
		}

		prevSystem = systemSnap

		aggregateMem += selfSnap.MemoryBytes

		m.metricsMu.Lock()
		m.metrics = MetricsSnapshot{
			Services: snapshotServices,
			Total: monitor.ResourceSnapshot{
				CPUPercent:  systemPercent,
				MemoryBytes: aggregateMem,
				Timestamp:   now,
			},
			System: monitor.SystemMetrics{
				CPUPercent: systemPercent,
				Memory:     memInfo,
				Network:    networkMetrics,
			},
		}
		m.metricsMu.Unlock()
	}
}

func (m *Manager) logCleanupLoop() {
	ticker := time.NewTicker(logCleanupEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.cleanupLogs()
		case <-m.stopCh:
			return
		}
	}
}

func (m *Manager) cleanupLogs() {
	m.servicesMu.RLock()
	instances := make([]*instance, 0, len(m.services))
	for _, inst := range m.services {
		instances = append(instances, inst)
	}
	m.servicesMu.RUnlock()

	for _, inst := range instances {
		logPath := inst.getLogPath()
		if logPath == "" {
			continue
		}
		_ = os.Truncate(logPath, 0)
	}
}

func (m *Manager) logMonitorError(action string, err error) {
	if err == nil {
		return
	}
	now := time.Now()
	if !m.lastMonitorLog.IsZero() && now.Sub(m.lastMonitorLog) < 5*time.Minute {
		return
	}
	m.lastMonitorLog = now
	log.Printf("[monitor] %s: %v", action, err)
}

func (inst *instance) status() ServiceStatus {
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	status := inst.state
	status.Metrics = inst.metrics
	return status
}

func (inst *instance) getLogPath() string {
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	if inst.logPath != "" {
		return inst.logPath
	}
	return inst.state.LogPath
}

func (inst *instance) updateMetrics(systemTotal uint64) monitor.ResourceSnapshot {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if !inst.running || inst.state.PID == 0 {
		inst.metrics = monitor.ResourceSnapshot{Timestamp: time.Now()}
		inst.prevProcessJiffies = 0
		inst.prevSystemJiffies = systemTotal
		inst.state.Port = 0
		inst.state.PortOpen = false
		inst.state.Running = false
		inst.state.Healthy = false
		return inst.metrics
	}

	stat, err := monitor.ReadProcessJiffies(inst.state.PID)
	if err != nil {
		inst.metrics = monitor.ResourceSnapshot{Timestamp: time.Now()}
		inst.state.Port = 0
		inst.state.PortOpen = false
		inst.state.Running = false
		inst.state.Healthy = false
		return inst.metrics
	}
	snap, proc, total := monitor.ComputeSnapshot(inst.prevProcessJiffies, inst.prevSystemJiffies, stat, systemTotal)
	inst.prevProcessJiffies = proc
	inst.prevSystemJiffies = total
	inst.metrics = snap
	ports, err := monitor.ReadProcessListeningPorts(inst.state.PID)
	if err == nil && len(ports) > 0 {
		inst.state.Port = ports[0]
		inst.state.PortOpen = true
	} else {
		inst.state.Port = 0
		inst.state.PortOpen = false
	}
	inst.state.Running = true
	inst.state.Healthy = true
	return inst.metrics
}

func (m *Manager) getInstance(id string) (*instance, bool) {
	m.servicesMu.RLock()
	inst, ok := m.services[id]
	m.servicesMu.RUnlock()
	return inst, ok
}

func (m *Manager) newInstance(cfg config.ServiceConfig) *instance {
	status := ServiceStatus{
		ID:         cfg.ID,
		Name:       cfg.Name,
		Executable: cfg.Path,
		Args:       append([]string(nil), cfg.Args...),
		Port:       0,
		LogPath:    "",
	}
	return &instance{
		cfg:     cfg,
		manager: m,
		state:   status,
	}
}

// SyncServices replaces the managed services and applies the new configuration.
func (m *Manager) SyncServices(cfgs []config.ServiceConfig) error {
	newMap := make(map[string]*instance, len(cfgs))
	seen := make(map[string]struct{}, len(cfgs))
	for i, svc := range cfgs {
		if svc.ID == "" {
			return fmt.Errorf("service at index %d missing id", i)
		}
		if svc.Path == "" {
			return fmt.Errorf("service %s missing path", svc.ID)
		}
		if _, exists := seen[svc.ID]; exists {
			return fmt.Errorf("duplicate service id %s", svc.ID)
		}
		seen[svc.ID] = struct{}{}
		newMap[svc.ID] = m.newInstance(svc)
	}

	m.servicesMu.Lock()
	old := m.services
	m.services = newMap
	m.servicesMu.Unlock()

	m.metricsMu.Lock()
	m.metrics.Services = make(map[string]monitor.ResourceSnapshot, len(newMap))
	m.metricsMu.Unlock()

	for _, inst := range old {
		_ = inst.stop()
	}

	for _, inst := range newMap {
		if inst.cfg.AutoStart {
			_ = inst.start()
		}
	}

	return nil
}

func (inst *instance) start() error {
	inst.mu.Lock()
	if inst.loopCtx != nil {
		inst.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	inst.loopCtx = ctx
	inst.loopCancel = cancel
	inst.mu.Unlock()

	go inst.loop()
	return nil
}

func (inst *instance) stop() error {
	inst.mu.Lock()
	cancel := inst.loopCancel
	inst.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (inst *instance) restart() error {
	if err := inst.stop(); err != nil {
		return err
	}
	deadline := time.Now().Add(stopWaitTimeout)
	for {
		inst.mu.RLock()
		stopped := inst.loopCtx == nil
		inst.mu.RUnlock()
		if stopped {
			return inst.start()
		}
		if time.Now().After(deadline) {
			return errors.New("service restart timeout: process is still stopping")
		}
		time.Sleep(stopWaitInterval)
	}
}

func (inst *instance) loop() {
	defer func() {
		inst.mu.Lock()
		inst.loopCtx = nil
		inst.loopCancel = nil
		inst.mu.Unlock()
	}()

	for {
		inst.mu.Lock()
		ctx := inst.loopCtx
		inst.mu.Unlock()

		if ctx == nil {
			return
		}

		err := inst.launchAndMonitor(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[service] %s exited: %v", inst.cfg.ID, err)
		}
		if ctx.Err() != nil || !inst.cfg.RestartOnExit {
			return
		}

		select {
		case <-time.After(restartBackoff):
		case <-ctx.Done():
			return
		}
	}
}

func (inst *instance) launchAndMonitor(ctx context.Context) error {
	execPath, err := inst.resolveExecutable()
	if err != nil {
		return err
	}

	logPath := inst.ensureLogPath(execPath)
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	cmd := exec.CommandContext(ctx, execPath, inst.cfg.Args...)
	cmd.Dir = filepath.Dir(execPath)
	cmd.Stdout = file
	cmd.Stderr = file
	cmd.Env = os.Environ()

	inst.mu.Lock()
	inst.state.Executable = execPath
	inst.state.LogPath = logPath
	inst.state.StartedAt = pointer(time.Now())
	inst.state.Restarts++
	inst.cmd = cmd
	inst.running = false
	inst.mu.Unlock()

	if err := cmd.Start(); err != nil {
		inst.mu.Lock()
		inst.state.LastExitError = err.Error()
		inst.state.PID = 0
		inst.running = false
		inst.mu.Unlock()
		return err
	}

	inst.mu.Lock()
	inst.state.PID = cmd.Process.Pid
	inst.running = true
	inst.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		case err := <-done:
			inst.markStopped(cleanupExitError(err))
			return ctx.Err()
		}
		err := <-done
		inst.markStopped(cleanupExitError(err))
		return ctx.Err()
	case err := <-done:
		inst.markStopped(cleanupExitError(err))
		return err
	}
}

func (inst *instance) markStopped(exitErr string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.state.LastExitError = exitErr
	inst.state.PID = 0
	inst.state.StartedAt = nil
	inst.state.Healthy = false
	inst.state.Running = false
	inst.running = false
}

func (inst *instance) resolveExecutable() (string, error) {
	cleaned := filepath.Clean(inst.cfg.Path)
	if filepath.IsAbs(cleaned) {
		if inst.manager.fs.IsAllowed(cleaned) {
			if _, err := os.Stat(cleaned); err == nil {
				return cleaned, nil
			}
		}
		return "", fmt.Errorf("executable %s not accessible", cleaned)
	}

	for _, root := range inst.manager.fs.Roots() {
		candidate := filepath.Join(root, cleaned)
		if !inst.manager.fs.IsAllowed(candidate) {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %s not found under roots", cleaned)
}

func (inst *instance) ensureLogPath(executable string) string {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.logPath != "" {
		if path, err := filepath.Abs(inst.logPath); err == nil {
			return path
		}
	}
	folder := filepath.Dir(executable)
	logPath := filepath.Join(folder, fmt.Sprintf("%s.log", inst.cfg.ID))
	if inst.manager.fs.IsAllowed(logPath) {
		inst.logPath = logPath
		return logPath
	}
	// Fallback to rooted path inside allowed roots
	fallback := filepath.Join(inst.manager.fs.Roots()[0], fmt.Sprintf("%s.log", inst.cfg.ID))
	inst.logPath = fallback
	return fallback
}

func cleanupExitError(err error) string {
	if err == nil {
		return ""
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return fmt.Sprintf("exit %d", ws.ExitStatus())
		}
		return exitErr.Error()
	}
	return err.Error()
}

func tailFile(path string, lines int) (string, error) {
	if path == "" {
		return "", fmt.Errorf("log path not ready")
	}
	if lines <= 0 {
		lines = 100
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return "", err
	}

	size := stat.Size()
	start := int64(0)
	if size > logTailChunkSize {
		start = size - logTailChunkSize
	}

	buf := make([]byte, size-start)
	if _, err := file.ReadAt(buf, start); err != nil && err != io.EOF {
		return "", err
	}

	linesOut := strings.Split(string(buf), "\n")
	if len(linesOut) > lines {
		linesOut = linesOut[len(linesOut)-lines:]
	}
	if len(linesOut) > 0 && linesOut[0] == "" && start > 0 {
		linesOut = linesOut[1:]
	}

	return strings.Join(linesOut, "\n"), nil
}

func pointer(t time.Time) *time.Time {
	return &t
}
