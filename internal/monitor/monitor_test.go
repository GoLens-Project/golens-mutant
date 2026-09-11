package monitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/GoLens-Project/golens-mutant/internal/config"
)

func testConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGateThresholds(t *testing.T) {
	m := New(testConfig(t, `
mutation: {target_roots: [libs], commands: [x]}
resources:
  min_free_ram_mb: 2048
  max_cpu_percent: 90
`))
	m.SetSampler(func() (Sample, error) {
		return Sample{CPUPercent: 50, FreeRAMMB: 4096}, nil
	})
	m.poll()
	if !m.Allowed() {
		t.Error("gate closed with 4GB free / 50% CPU")
	}

	m.SetSampler(func() (Sample, error) {
		return Sample{CPUPercent: 95, FreeRAMMB: 4096}, nil
	})
	m.poll()
	if m.Allowed() {
		t.Error("gate open at 95% CPU")
	}

	m.SetSampler(func() (Sample, error) {
		return Sample{CPUPercent: 50, FreeRAMMB: 1024}, nil
	})
	m.poll()
	if m.Allowed() {
		t.Error("gate open with 1GB free")
	}
}

func TestZeroThresholdDisablesGate(t *testing.T) {
	m := New(testConfig(t, `
mutation: {target_roots: [libs], commands: [x]}
`))
	m.SetSampler(func() (Sample, error) {
		return Sample{CPUPercent: 99.9, FreeRAMMB: 1}, nil
	})
	m.poll()
	if !m.Allowed() {
		t.Error("gate closed though both thresholds disabled")
	}
}

func TestClosedForTracksSustainedPressure(t *testing.T) {
	m := New(testConfig(t, `
mutation: {target_roots: [libs], commands: [x]}
resources: {min_free_ram_mb: 2048}
`))
	var closed atomic.Bool
	m.SetSampler(func() (Sample, error) {
		if closed.Load() {
			return Sample{CPUPercent: 0, FreeRAMMB: 100}, nil
		}
		return Sample{CPUPercent: 0, FreeRAMMB: 8192}, nil
	})
	m.poll()
	if m.ClosedFor() != 0 {
		t.Error("ClosedFor non-zero while gate open")
	}
	closed.Store(true)
	m.poll()
	if m.ClosedFor() <= 0 {
		t.Error("ClosedFor zero right after gate closed")
	}
	time.Sleep(30 * time.Millisecond)
	if m.ClosedFor() < 25*time.Millisecond {
		t.Errorf("ClosedFor = %v, want >= 25ms", m.ClosedFor())
	}
}

func TestRunSamplesContinuously(t *testing.T) {
	m := New(testConfig(t, `
mutation: {target_roots: [libs], commands: [x]}
resources: {min_free_ram_mb: 2048}
`))
	var calls atomic.Int64
	m.SetSampler(func() (Sample, error) {
		calls.Add(1)
		return Sample{CPUPercent: 10, FreeRAMMB: 8192}, nil
	})
	m.SetInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	time.Sleep(60 * time.Millisecond)
	cancel()

	if n := calls.Load(); n < 3 {
		t.Errorf("sampler called %d times in 60ms at 10ms interval, want >= 3", n)
	}
	if _, ok := m.Snapshot(); !ok {
		t.Error("gate closed after healthy samples")
	}
}

// TestF6FailOpenOnSamplerErrors guards F6: a persistently failing
// sampler must fail open with a warning instead of hanging workers on a
// permanently closed gate.
func TestF6FailOpenOnSamplerErrors(t *testing.T) {
	m := New(testConfig(t, `
mutation: {target_roots: [libs], commands: [x]}
resources: {min_free_ram_mb: 2048}
`))
	var warns []string
	m.OnWarn = func(msg string) { warns = append(warns, msg) }
	m.SetSampler(func() (Sample, error) {
		return Sample{}, fmt.Errorf("metrics unavailable")
	})
	for range m.errFailOpen - 1 {
		m.poll()
	}
	if m.Allowed() {
		t.Fatal("gate opened before the fail-open threshold")
	}
	m.poll() // crosses the threshold
	if !m.Allowed() {
		t.Error("gate still closed after persistent sampler failure")
	}
	if !m.Degraded() {
		t.Error("monitor not marked degraded")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "failing open") {
		t.Errorf("warns = %v, want one fail-open notice", warns)
	}
	m.poll() // still failing: no repeated warnings
	if len(warns) != 1 {
		t.Errorf("warns = %v, want the notice emitted once per streak", warns)
	}

	// Recovery: a good sample resumes normal gating.
	m.SetSampler(func() (Sample, error) {
		return Sample{CPUPercent: 0, FreeRAMMB: 100}, nil
	})
	m.poll()
	if m.Allowed() || m.Degraded() {
		t.Error("healthy sample did not resume normal gating")
	}
}

func TestOpenFor(t *testing.T) {
	m := New(testConfig(t, `
mutation: {target_roots: [libs], commands: [x]}
resources: {min_free_ram_mb: 2048}
`))
	healthy := func() (Sample, error) { return Sample{FreeRAMMB: 8192}, nil }
	pressured := func() (Sample, error) { return Sample{FreeRAMMB: 10}, nil }

	m.SetSampler(healthy)
	m.poll()
	if m.OpenFor() <= 0 {
		t.Error("OpenFor = 0 while gate open")
	}
	if m.ClosedFor() != 0 {
		t.Error("ClosedFor non-zero while gate open")
	}

	m.SetSampler(pressured)
	m.poll()
	if m.OpenFor() != 0 {
		t.Error("OpenFor non-zero while gate closed")
	}
	if m.ClosedFor() <= 0 {
		t.Error("ClosedFor = 0 right after closing")
	}
}

func TestSystemSamplerReaderErrors(t *testing.T) {
	restore := func() {
		readRAM = mem.VirtualMemory
		readLoadAvg = load.Avg
		readCPUPercent = func() ([]float64, error) { return cpu.Percent(0, false) }
	}
	defer restore()

	readRAM = func() (*mem.VirtualMemoryStat, error) { return nil, fmt.Errorf("no mem") }
	if _, err := systemSampler("instant")(); err == nil {
		t.Error("RAM read error not surfaced")
	}
	restore()

	readRAM = mem.VirtualMemory
	readLoadAvg = func() (*load.AvgStat, error) { return nil, fmt.Errorf("no load") }
	if _, err := systemSampler("load_avg")(); err == nil {
		t.Error("load read error not surfaced")
	}
	restore()

	readCPUPercent = func() ([]float64, error) { return nil, fmt.Errorf("no cpu") }
	if _, err := systemSampler("instant")(); err == nil {
		t.Error("CPU read error not surfaced")
	}
	readCPUPercent = func() ([]float64, error) { return nil, nil }
	if _, err := systemSampler("instant")(); err == nil {
		t.Error("empty CPU reading not treated as an error")
	}
}

func TestSystemSamplerReal(t *testing.T) {
	// One live gopsutil reading per metric mode; tolerates restricted
	// environments by skipping on error.
	for _, metric := range []string{"instant", "load_avg"} {
		t.Run(metric, func(t *testing.T) {
			s := systemSampler(metric)
			// Two calls: the first primes the instant-mode delta
			// baseline, the second returns a real reading.
			if _, err := s(); err != nil {
				t.Skipf("system sampling unavailable: %v", err)
			}
			sample, err := s()
			if err != nil {
				t.Skipf("system sampling unavailable: %v", err)
			}
			if sample.FreeRAMMB <= 0 {
				t.Errorf("FreeRAMMB = %v, want > 0", sample.FreeRAMMB)
			}
			if sample.CPUPercent < 0 || sample.CPUPercent > 100*float64(max(runtime.NumCPU(), 1)) {
				t.Errorf("CPUPercent = %v, out of range", sample.CPUPercent)
			}
		})
	}
}
