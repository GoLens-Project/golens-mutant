// Package monitor samples system CPU and memory metrics and exposes the
// resource guard the scheduler gates worker spawns on (D2, D5).
package monitor

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/GoLens-Project/golens-mutant/internal/config"
)

// Sample is one system-metrics reading.
type Sample struct {
	// CPUPercent is the configured CPU gauge as a percentage: instant
	// usage, or the 1-minute load average normalized by core count
	// (D5).
	CPUPercent float64

	// FreeRAMMB is available physical memory in megabytes.
	FreeRAMMB float64
}

// Sampler produces one metric reading. Swappable in tests.
type Sampler func() (Sample, error)

// Monitor polls a Sampler on an interval and tracks how long the
// resource gate has been continuously closed (for D7 scale-down) or open
// (for D7 recovery).
type Monitor struct {
	metric     string // "instant" | "load_avg"
	maxCPU     float64
	minFreeRAM float64
	interval   time.Duration
	sampler    Sampler

	// OnWarn, when set, receives one-time degradation notices (F6).
	OnWarn func(msg string)

	// errFailOpen is the consecutive-sampler-error count after which the
	// guard fails open instead of hanging the session (F6).
	errFailOpen int

	mu          sync.Mutex
	cur         Sample
	ok          bool
	okSince     time.Time // when the gate last opened (OpenFor)
	closedSince time.Time // when the gate last closed (ClosedFor)
	valid       bool      // at least one gate evaluation happened
	errStreak   int       // consecutive sampler errors
	degraded    bool      // sampler deemed broken; guard disabled
}

// New builds a Monitor from cfg. The default sampler uses gopsutil
// according to cfg's cpu_metric mode.
func New(cfg *config.Config) *Monitor {
	return &Monitor{
		metric:      cfg.Resources.CPUMetric,
		maxCPU:      float64(cfg.Resources.MaxCPUPercent),
		minFreeRAM:  float64(cfg.Resources.MinFreeRAMMB),
		interval:    time.Second,
		sampler:     systemSampler(cfg.Resources.CPUMetric),
		errFailOpen: 5,
	}
}

// SetSampler replaces the metric source (tests).
func (m *Monitor) SetSampler(s Sampler) { m.sampler = s }

// SetInterval replaces the sampling period (tests).
func (m *Monitor) SetInterval(d time.Duration) { m.interval = d }

// Run samples until ctx is done. It always leaves one warm sample so the
// first gate check never sees zero-values; Run should start before the
// scheduler asks Allowed().
func (m *Monitor) Run(ctx context.Context) {
	// Warm up: the first instant-CPU reading is a delta baseline, so
	// sample twice before trusting percentages.
	m.poll()
	m.poll()
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.poll()
		}
	}
}

func (m *Monitor) poll() {
	s, err := m.sampler()
	if err != nil {
		// Transient failures keep the last good sample. A persistently
		// failing sampler (unsupported metric, restricted container)
		// fails open with a warning rather than hanging every worker on
		// a gate that can never be evaluated (F6).
		m.mu.Lock()
		m.errStreak++
		streak, failOpen := m.errStreak, m.errFailOpen
		if streak >= failOpen && !m.degraded {
			m.degraded = true
			m.ok = true
			m.valid = true
			m.okSince = time.Now()
		}
		warn := m.degraded && streak == failOpen
		m.mu.Unlock()
		if warn {
			m.warnf("system metrics unavailable for %d samples (%v): resource guard disabled, failing open", streak, err)
		}
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errStreak = 0
	m.degraded = false
	m.cur = s
	ok := m.gateLocked(s)
	if !m.valid || ok != m.ok {
		m.ok = ok
		if ok {
			m.okSince = now
		} else {
			m.closedSince = now
		}
	}
	if !m.valid {
		// First evaluation anchors whichever timer matches its state.
		if ok {
			m.okSince = now
		} else {
			m.closedSince = now
		}
	}
	m.valid = true
}

func (m *Monitor) warnf(format string, args ...any) {
	if m.OnWarn != nil {
		m.OnWarn(fmt.Sprintf(format, args...))
	}
}

// gateLocked evaluates the thresholds; a zero threshold is disabled.
func (m *Monitor) gateLocked(s Sample) bool {
	if m.minFreeRAM > 0 && s.FreeRAMMB < m.minFreeRAM {
		return false
	}
	if m.maxCPU > 0 && s.CPUPercent > m.maxCPU {
		return false
	}
	return true
}

// Snapshot returns the latest sample and whether spawning is allowed.
func (m *Monitor) Snapshot() (Sample, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur, m.ok
}

// Allowed reports whether a new worker may spawn (D2).
func (m *Monitor) Allowed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ok
}

// ClosedFor reports how long the gate has been continuously closed
// (zero if it is open). Drives D7 scale-down.
func (m *Monitor) ClosedFor() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.valid && !m.ok {
		return time.Since(m.closedSince)
	}
	return 0
}

// OpenFor reports how long the gate has been continuously open (zero if
// it is closed). Drives D7 capacity recovery.
func (m *Monitor) OpenFor() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.valid && m.ok {
		return time.Since(m.okSince)
	}
	return 0
}

// Degraded reports whether the sampler has been failing long enough that
// the guard is disabled (F6).
func (m *Monitor) Degraded() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.degraded
}

// gopsutil readers, stubbed in tests to exercise error paths.
var (
	readRAM        = mem.VirtualMemory
	readLoadAvg    = load.Avg
	readCPUPercent = func() ([]float64, error) { return cpu.Percent(0, false) }
)

// systemSampler builds the gopsutil-backed sampler for the configured
// CPU metric mode (D5).
func systemSampler(metric string) Sampler {
	// Prime the instant-usage delta baseline once.
	if _, err := readCPUPercent(); err != nil {
		_ = err // sampling errors are tolerated; first poll() retries
	}
	return func() (Sample, error) {
		vm, err := readRAM()
		if err != nil {
			return Sample{}, err
		}
		s := Sample{FreeRAMMB: float64(vm.Available) / (1 << 20)}
		switch metric {
		case "load_avg":
			la, err := readLoadAvg()
			if err != nil {
				return Sample{}, err
			}
			cores := max(runtime.NumCPU(), 1)
			s.CPUPercent = la.Load1 / float64(cores) * 100
		default: // "instant"
			pct, err := readCPUPercent()
			if err != nil {
				return Sample{}, err
			}
			if len(pct) == 0 {
				return Sample{}, fmt.Errorf("cpu usage reading was empty")
			}
			s.CPUPercent = pct[0]
		}
		return s, nil
	}
}
