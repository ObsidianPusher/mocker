package monitor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// JiffiesSnapshot exposes CPU usage snapshot for a single process.
type JiffiesSnapshot struct {
	Process uint64
	RSS     uint64
}

// SystemSnapshot captures total and idle jiffies for the kernel.
type SystemSnapshot struct {
	Total uint64
	Idle  uint64
}

// MemoryInfo exposes basic RAM statistics.
type MemoryInfo struct {
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
	Used      uint64 `json:"used"`
}

// NetworkCounters tracks cumulative network bytes from kernel counters.
type NetworkCounters struct {
	RXBytes uint64
	TXBytes uint64
}

// NetworkMetrics exposes aggregate network throughput.
type NetworkMetrics struct {
	UploadBytesPerSec   uint64 `json:"upload_bytes_per_sec"`
	DownloadBytesPerSec uint64 `json:"download_bytes_per_sec"`
}

// SystemMetrics combines CPU and memory information.
type SystemMetrics struct {
	CPUPercent float64        `json:"cpu_percent"`
	Memory     MemoryInfo     `json:"memory"`
	Network    NetworkMetrics `json:"network"`
}

// ReadSystemSnapshot reads /proc/stat and returns total + idle counters.
func ReadSystemSnapshot() (SystemSnapshot, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return SystemSnapshot{}, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return SystemSnapshot{}, fmt.Errorf("invalid /proc/stat contents")
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return SystemSnapshot{}, fmt.Errorf("unexpected cpu line")
	}

	var total, idle uint64
	for idx, field := range fields[1:] {
		v, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return SystemSnapshot{}, err
		}
		total += v
		if idx == 3 || idx == 4 { // idle + iowait
			idle += v
		}
	}
	return SystemSnapshot{Total: total, Idle: idle}, nil
}

// ReadMemoryInfo reads memory info from /proc/meminfo (same source as htop).
func ReadMemoryInfo() (MemoryInfo, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemoryInfo{}, err
	}
	defer file.Close()

	var total, available uint64
	var free, buffers, cached, reclaimable uint64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, err := parseMemInfoValue(fields)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			total = value
		case "MemAvailable":
			available = value
		case "MemFree":
			free = value
		case "Buffers":
			buffers = value
		case "Cached":
			cached = value
		case "SReclaimable":
			reclaimable = value
		}
		if total != 0 && available != 0 {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return MemoryInfo{}, err
	}
	if total == 0 {
		return MemoryInfo{}, fmt.Errorf("MemTotal missing")
	}
	if available == 0 {
		available = free + buffers + cached + reclaimable
	}
	if available > total {
		available = total
	}
	return MemoryInfo{Total: total, Available: available, Used: total - available}, nil
}

func parseMemInfoValue(fields []string) (uint64, error) {
	if len(fields) < 2 {
		return 0, fmt.Errorf("missing value")
	}
	value, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}
	unit := ""
	if len(fields) >= 3 {
		unit = strings.ToLower(strings.TrimSpace(fields[2]))
	}
	switch unit {
	case "kb", "kib", "k":
		return value * 1024, nil
	case "mb", "mib", "m":
		return value * 1024 * 1024, nil
	case "gb", "gib", "g":
		return value * 1024 * 1024 * 1024, nil
	case "b", "":
		return value, nil
	default:
		return value * 1024, nil
	}
}

// ReadNetworkCounters reads aggregate RX/TX bytes from /proc/net/dev.
// Loopback interface is excluded.
func ReadNetworkCounters() (NetworkCounters, error) {
	return readNetworkCountersFromFile("/proc/net/dev")
}

func readNetworkCountersFromFile(path string) (NetworkCounters, error) {
	file, err := os.Open(path)
	if err != nil {
		return NetworkCounters{}, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNo := 0
	total := NetworkCounters{}
	for scanner.Scan() {
		lineNo++
		// Skip header rows.
		if lineNo <= 2 {
			continue
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "" || iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		// /proc/net/dev has 16 columns after ":".
		if len(fields) < 16 {
			continue
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			continue
		}
		total.RXBytes += rx
		total.TXBytes += tx
	}
	if err := scanner.Err(); err != nil {
		return NetworkCounters{}, err
	}
	return total, nil
}

// cgroup helpers removed to avoid inconsistent overrides across environments.

// ReadProcessListeningPorts returns listening TCP ports for a process.
func ReadProcessListeningPorts(pid int) ([]int, error) {
	inodes, err := readProcessSocketInodes(pid)
	if err != nil {
		return nil, err
	}
	ports := make(map[int]struct{})
	if err := collectListeningPorts("/proc/net/tcp", inodes, ports); err != nil {
		// ignore
	}
	if err := collectListeningPorts("/proc/net/tcp6", inodes, ports); err != nil {
		// ignore
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("no listening ports")
	}
	result := make([]int, 0, len(ports))
	for port := range ports {
		result = append(result, port)
	}
	sort.Ints(result)
	return result, nil
}

func readProcessSocketInodes(pid int) (map[string]struct{}, error) {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, err
	}
	inodes := make(map[string]struct{})
	for _, entry := range entries {
		link, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(link, "socket:[") && strings.HasSuffix(link, "]") {
			inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
			inodes[inode] = struct{}{}
		}
	}
	if len(inodes) == 0 {
		return nil, fmt.Errorf("no socket inodes")
	}
	return inodes, nil
}

func collectListeningPorts(path string, inodes map[string]struct{}, ports map[int]struct{}) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || strings.HasPrefix(fields[0], "sl") {
			continue
		}
		state := fields[3]
		if state != "0A" {
			continue
		}
		inode := fields[9]
		if _, ok := inodes[inode]; !ok {
			continue
		}
		port, err := parseHexPort(fields[1])
		if err != nil {
			continue
		}
		ports[port] = struct{}{}
	}
	return scanner.Err()
}

func parseHexPort(localAddress string) (int, error) {
	parts := strings.Split(localAddress, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid local address")
	}
	val, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return 0, err
	}
	return int(val), nil
}

// ReadProcessJiffies reads utime+stime and rss for given pid.
func ReadProcessJiffies(pid int) (JiffiesSnapshot, error) {
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return JiffiesSnapshot{}, err
	}
	content := string(data)
	idx := strings.LastIndex(content, ")")
	if idx == -1 {
		return JiffiesSnapshot{}, fmt.Errorf("malformed stat file")
	}
	parts := strings.Fields(content[idx+2:])
	if len(parts) < 44 {
		return JiffiesSnapshot{}, fmt.Errorf("stat missing fields")
	}
	utime, err := strconv.ParseUint(parts[11], 10, 64)
	if err != nil {
		return JiffiesSnapshot{}, err
	}
	stime, err := strconv.ParseUint(parts[12], 10, 64)
	if err != nil {
		return JiffiesSnapshot{}, err
	}
	rssPages, err := strconv.ParseUint(parts[21], 10, 64)
	if err != nil {
		return JiffiesSnapshot{}, err
	}
	pageSize := uint64(os.Getpagesize())
	return JiffiesSnapshot{Process: utime + stime, RSS: rssPages * pageSize}, nil
}
