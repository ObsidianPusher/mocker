package monitor

import "time"

// ResourceSnapshot reports CPU and memory usage at a point in time.
type ResourceSnapshot struct {
	CPUPercent  float64   `json:"cpu_percent"`
	MemoryBytes uint64    `json:"memory_bytes"`
	Timestamp   time.Time `json:"timestamp"`
}

// ComputeSnapshot returns a ResourceSnapshot plus updated prev counters.
func ComputeSnapshot(prevProc, prevTotal uint64, current JiffiesSnapshot, currentTotal uint64) (ResourceSnapshot, uint64, uint64) {
	cpu := 0.0
	if currentTotal > prevTotal && current.Process >= prevProc {
		cpu = (float64(current.Process-prevProc) / float64(currentTotal-prevTotal)) * 100
	}
	return ResourceSnapshot{
		CPUPercent:  clampPercent(cpu),
		MemoryBytes: current.RSS,
		Timestamp:   time.Now(),
	}, current.Process, currentTotal
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
