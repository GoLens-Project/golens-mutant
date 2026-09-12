package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoLens-Project/golens-mutant/internal/config"
	"github.com/GoLens-Project/golens-mutant/internal/discover"
	"github.com/GoLens-Project/golens-mutant/internal/monitor"
	"github.com/GoLens-Project/golens-mutant/internal/report"
	"github.com/GoLens-Project/golens-mutant/internal/workspace"
)

type harness struct {
	t     *testing.T
	cfg   *config.Config
	pkgs  []*discover.Package
	mon   *monitor.Monitor
	ws    *workspace.Manager
	store *report.JSONStore
}

// buildFixture creates N packages × M files and a scheduler-ready
// harness. extra YAML is appended to the base config.
func buildFixture(t *testing.T, pkgNames []string, filesPerPkg int, extra string) *harness {
	t.Helper()
	base := t.TempDir()
	for _, name := range pkgNames {
		dir := filepath.Join(base, "libs", name, "lib")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "pubspec.yaml"), []byte("name: "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		for i := range filesPerPkg {
			if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))+".dart"), []byte("src"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	body := `
mutation:
  target_roots: [libs]
  file_patterns: ["lib/**/*.dart"]
  tests:
    - pattern: "lib/**"
      tests: [t]
  commands: ["echo ran {package} {file} > {target_dir}/` + markerName(t) + `2>&1"]
  timeout:
    duration: 5s
resources:
  spawn_cooldown: 0s
  scale_down_after: 200ms
scheduling:
  ramp_up: calculated
workspace:
  base_dir: ` + filepath.Join(base, ".work") + `
  bootstrap: ""
reports:
  dir: ` + filepath.Join(base, "reports") + `
` + extra
	path := filepath.Join(base, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mutation.TargetRoots = []string{filepath.Join(base, "libs")}
	pkgs, err := discover.Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mon := monitor.New(cfg)
	ws, err := workspace.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err := report.NewJSONStore(cfg.Reports.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, cfg: cfg, pkgs: pkgs, mon: mon, ws: ws, store: store}
}

func markerName(t *testing.T) string {
	// Unique marker file per test to avoid template collisions.
	return "marker_" + strings.ReplaceAll(t.Name(), "/", "_") + ".txt"
}

// pkg returns the discovered package name for a fixture package
// (names embed the configured target root path — F4).
func (h *harness) pkg(name string) string {
	for _, p := range h.pkgs {
		if filepath.Base(p.Dir) == name {
			return p.Name
		}
	}
	t := h.t
	t.Fatalf("package %q not found", name)
	return ""
}

// sbxDir returns a package's sandbox directory.
func (h *harness) sbxDir(name string) string {
	return filepath.Join(h.cfg.Workspace.BaseDir, h.cfg.Workspace.CacheDir,
		strings.ReplaceAll(strings.Trim(h.pkg(name), "/"), "/", "__"))
}

func (h *harness) run(t *testing.T, opts Options) *Scheduler {
	t.Helper()
	s, err := New(h.cfg, h.mon, h.ws, h.store, h.pkgs, opts)
	if err != nil {
		t.Fatal(err)
	}
	go h.mon.Run(context.Background())
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRunAllFiles(t *testing.T) {
	h := buildFixture(t, []string{"pa", "pb"}, 3, "")
	s := h.run(t, Options{})
	if s.Done() != 6 {
		t.Fatalf("done = %d, want 6", s.Done())
	}
	state, err := h.store.Resume()
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"pa", "pb"} {
		for i := range 3 {
			file := "lib/" + string(rune('a'+i)) + ".dart"
			if state[h.pkg(pkg)][file] == "" {
				t.Errorf("%s/%s not recorded", pkg, file)
			}
		}
	}
}

func TestFileLogEvents(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 2, "")
	var mu sync.Mutex
	var events []FileEvent
	s := h.run(t, Options{FileLog: func(e FileEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}})
	if s.Done() != 2 {
		t.Fatalf("done = %d, want 2", s.Done())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 4 {
		t.Fatalf("events = %+v, want 4 (start+finish per file)", events)
	}
	// Per-package x/y counts up in queue order: a.dart 1/2, b.dart 2/2,
	// each with a started line before its finished line.
	want := []struct {
		file     string
		done     int
		finished bool
	}{
		{"lib/a.dart", 1, false},
		{"lib/a.dart", 1, true},
		{"lib/b.dart", 2, false},
		{"lib/b.dart", 2, true},
	}
	for i, w := range want {
		e := events[i]
		if e.File != w.file || e.Done != w.done || e.Total != 2 || e.Finished != w.finished {
			t.Errorf("events[%d] = %+v, want %s %d/2 finished=%v", i, e, w.file, w.done, w.finished)
		}
		if e.Package != h.pkg("pa") {
			t.Errorf("events[%d].Package = %q, want %q", i, e.Package, h.pkg("pa"))
		}
		if e.Time.IsZero() {
			t.Errorf("events[%d].Time is zero", i)
		}
	}
}

// captureSink collects one file's streamed output for assertions; it
// records the scheduler's flush/close calls to verify the lifecycle
// contract.
type captureSink struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	events []string // "flush" / "close" in call order
}

func (c *captureSink) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *captureSink) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "flush")
}

func (c *captureSink) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "close")
}

func (c *captureSink) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *captureSink) lifecycle() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

// errSink fails every write, standing in for a broken embedder sink.
type errSink struct{}

func (errSink) Write(p []byte) (int, error) { return 0, errors.New("sink failure") }

func TestStreamsCommandOutput(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 2, "")
	h.cfg.Mutation.Commands = []string{"echo one {file}; echo two >&2; printf partial"}
	var mu sync.Mutex
	sinks := map[string]*captureSink{}
	var started []FileEvent
	s := h.run(t, Options{Stream: func(e FileEvent) io.Writer {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, e)
		c := &captureSink{}
		sinks[e.File] = c
		return c
	}})
	if s.Done() != 2 {
		t.Fatalf("done = %d, want 2", s.Done())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(started) != 2 {
		t.Fatalf("stream hook called %d times, want one per file", len(started))
	}
	for _, e := range started {
		if e.Package != h.pkg("pa") || e.Total != 2 || e.Finished {
			t.Errorf("stream event = %+v, want a started event for %s 2-file package", e, h.pkg("pa"))
		}
	}
	for file, c := range sinks {
		want := "one " + file + "\ntwo\npartial"
		if got := c.String(); got != want {
			t.Errorf("%s: streamed %q, want %q", file, got, want)
		}
		// Lifecycle: at least one flush (trailing partial line emitted),
		// close last.
		events := c.lifecycle()
		if len(events) == 0 || events[len(events)-1] != "close" {
			t.Errorf("%s: lifecycle = %v, want it to end with close", file, events)
		}
		flushed := false
		for _, e := range events {
			flushed = flushed || e == "flush"
		}
		if !flushed {
			t.Errorf("%s: sink never flushed (trailing partial line lost)", file)
		}
		// Tee invariant: the buffered on-disk log is byte-identical to
		// what the stream saw.
		logPath := filepath.Join(h.cfg.Reports.Dir, "logs",
			strings.ReplaceAll(strings.Trim(h.pkg("pa"), "/"), "/", "__"),
			strings.ReplaceAll(file, "/", "__")+".log")
		onDisk, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("%s: read log: %v", file, err)
		}
		if string(onDisk) != c.String() {
			t.Errorf("%s: on-disk log %q != streamed %q", file, onDisk, c.String())
		}
	}
}

func TestStreamSinkErrorsDoNotAffectResults(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 1, "")
	h.cfg.Mutation.Commands = []string{"echo boom"} // exits 0 → survived
	h.run(t, Options{Stream: func(e FileEvent) io.Writer { return errSink{} }})
	state, err := h.store.Resume()
	if err != nil {
		t.Fatal(err)
	}
	if got := state[h.pkg("pa")]["lib/a.dart"]; got != "survived" {
		t.Errorf("result = %q, want survived — sink errors must not reach cmd.Wait", got)
	}
}

func TestExitCodeClassification(t *testing.T) {
	// Commands exit 0 (→ survived by default) for pa, exit 1 (→ killed)
	// for pb, selected by package name.
	h := buildFixture(t, []string{"pa", "pb"}, 1, "")
	h.cfg.Mutation.Commands = []string{"sh -c 'case {package} in *pb) exit 1;; esac'"}
	s := h.run(t, Options{})
	if s.Done() != 2 {
		t.Fatalf("done = %d", s.Done())
	}
	state, _ := h.store.Resume()
	if got := state[h.pkg("pa")]["lib/a.dart"]; got != "survived" {
		t.Errorf("pa = %q, want survived", got)
	}
	if got := state[h.pkg("pb")]["lib/a.dart"]; got != "killed" {
		t.Errorf("pb = %q, want killed", got)
	}
}

func TestTimeoutContinue(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 2, "")
	h.cfg.Mutation.Timeout.Duration = "200ms"
	h.cfg.Mutation.Commands = []string{"sleep 5"}
	s := h.run(t, Options{})
	if s.Done() != 2 {
		t.Fatalf("done = %d, want 2 (continue mode)", s.Done())
	}
	state, _ := h.store.Resume()
	for f, r := range state[h.pkg("pa")] {
		if r != "timeout" {
			t.Errorf("%s = %q, want timeout", f, r)
		}
	}
}

func TestTimeoutAbortPackage(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 3, "")
	h.cfg.Mutation.Timeout.Duration = "200ms"
	h.cfg.Mutation.Timeout.OnTimeout = "abort_package"
	h.cfg.Mutation.Commands = []string{"sleep 5"}
	s := h.run(t, Options{})
	if s.Done() != 1 {
		t.Fatalf("done = %d, want 1 (abort after first)", s.Done())
	}
	state, _ := h.store.Resume()
	if len(state[h.pkg("pa")]) != 1 {
		t.Errorf("recorded = %v, want only the first file", state[h.pkg("pa")])
	}
}

// TestTimeoutDoesNotPoisonLaterSteps guards the killed-step leak: an
// engine timed out mid-mutation leaves its mutant in a file other than
// the next step's target. Without a dirty re-sync, every later step's
// run would see the leaked mutant and misclassify.
func TestTimeoutDoesNotPoisonLaterSteps(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 3, "")
	h.cfg.Mutation.Timeout.Duration = "300ms"
	h.cfg.Mutation.Timeout.KillGrace = "100ms"
	// The first file's "engine" leaks a mutant into lib/b.dart and then
	// hangs past the timeout (killed mid-mutation, restore never runs).
	// Later files' runs probe lib/b.dart: hung again (→ timeout) means
	// the leak survived; a quick exit (→ survived) means the sandbox was
	// re-synced. The .leaked flag makes the leak one-shot so the probe
	// results are attributable.
	h.cfg.Mutation.Commands = []string{
		`if [ ! -f "{target_dir}/.leaked" ]; then echo MUTANT > "{target_dir}/lib/b.dart"; touch "{target_dir}/.leaked"; fi; ` +
			`grep -q MUTANT "{target_dir}/lib/b.dart" && sleep 5 || true`,
	}
	h.run(t, Options{})
	state, _ := h.store.Resume()
	results := state[h.pkg("pa")]
	if got := results["lib/a.dart"]; got != "timeout" {
		t.Errorf("lib/a.dart = %q, want timeout (the killed step)", got)
	}
	// b.dart itself is restored before its own run; c.dart is the real
	// probe: its content check must not see the leaked mutant.
	for _, f := range []string{"lib/b.dart", "lib/c.dart"} {
		if got := results[f]; got != "survived" {
			t.Errorf("%s = %q, want survived — leaked mutant poisoned a later step", f, got)
		}
	}
	// And the sandbox itself must be clean again.
	if b, err := os.ReadFile(filepath.Join(h.sbxDir("pa"), "lib/b.dart")); err != nil || string(b) != "src" {
		t.Errorf("sandbox lib/b.dart = %q, %v, want pristine %q", b, err, "src")
	}
}

func TestSandboxExclusivitySerializesPackage(t *testing.T) {
	// The command logs start/end; concurrent sandbox use would produce
	// two adjacent "start" lines.
	h := buildFixture(t, []string{"pa"}, 4, "")
	h.cfg.Resources.MaxWorkers = 2
	h.cfg.Mutation.Commands = []string{
		"echo start >> " + filepath.Join("{target_dir}", "order.txt") + "; sleep 0.15; echo end >> " + filepath.Join("{target_dir}", "order.txt") + "; true",
	}
	h.run(t, Options{})
	sbx := filepath.Join(h.sbxDir("pa"), "order.txt")
	b, err := os.ReadFile(sbx)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(b))
	if len(lines) != 8 {
		t.Fatalf("order = %v, want 8 entries", lines)
	}
	for i, l := range lines {
		want := "start"
		if i%2 == 1 {
			want = "end"
		}
		if l != want {
			t.Fatalf("order not serialized: %v", lines)
		}
	}
}

func TestResumeSkipsRecordedFiles(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 2, "")
	h.cfg.Mutation.Commands = []string{"echo {file} >> " + filepath.Join("{target_dir}", "ran.txt")}
	// Pre-record one file as done by a previous session.
	if err := h.store.Record(report.FileResult{
		Package: h.pkg("pa"), File: "lib/a.dart", Result: "killed", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s, err := New(h.cfg, h.mon, h.ws, h.store, h.pkgs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := h.store.Resume()
	s.SkipDone(state)
	go h.mon.Run(context.Background())
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The session completes (progress includes the resumed file)…
	if s.Done() != s.Total() {
		t.Errorf("done = %d, total = %d", s.Done(), s.Total())
	}
	// …but only the un-recorded file actually executed.
	b, err := os.ReadFile(filepath.Join(h.sbxDir("pa"), "ran.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "lib/b.dart" {
		t.Errorf("executed = %q, want only lib/b.dart", got)
	}
}

func TestScaleDownAndRecover(t *testing.T) {
	h := buildFixture(t, []string{"pa", "pb", "pc"}, 3, "")
	h.cfg.Resources.MaxWorkers = 2
	h.cfg.Resources.MinFreeRAMMB = 2048
	h.cfg.Resources.ScaleDownAfter = "200ms"
	h.cfg.Mutation.Commands = []string{"sleep 0.1; true"}
	// Rebuild the monitor: it captured the thresholds at fixture-build
	// time, before MinFreeRAMMB was set above.
	h.mon = monitor.New(h.cfg)

	var mu sync.Mutex
	var notices []string
	h.mon.SetSampler(func() (monitor.Sample, error) {
		mu.Lock()
		underPressure := len(notices) == 0 // pressure until first scale-down
		mu.Unlock()
		if underPressure {
			return monitor.Sample{CPUPercent: 0, FreeRAMMB: 10}, nil
		}
		return monitor.Sample{CPUPercent: 0, FreeRAMMB: 8192}, nil
	})
	h.mon.SetInterval(20 * time.Millisecond)
	go h.mon.Run(context.Background())

	s, err := New(h.cfg, h.mon, h.ws, h.store, h.pkgs, Options{
		Notify: func(msg string) {
			mu.Lock()
			notices = append(notices, msg)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Done() != 9 {
		t.Errorf("done = %d, want 9 (queue never aborted)", s.Done())
	}
	mu.Lock()
	defer mu.Unlock()
	scaledDown, recovered := false, false
	for _, n := range notices {
		if strings.Contains(n, "scaling worker capacity down") {
			scaledDown = true
		}
		if strings.Contains(n, "scaling worker capacity up") {
			recovered = true
		}
	}
	if !scaledDown {
		t.Errorf("no scale-down notice among %v", notices)
	}
	// F7: after pressure lifts, capacity must climb back toward
	// max_workers.
	if !recovered {
		t.Errorf("no capacity-recovery notice among %v", notices)
	}
}

// TestF1SecondSessionDoesNotClobberResumeState guards F1: a fresh
// session (scheduler construction + skip recording) must never rewrite
// resume state down to just its own records.
func TestF1SecondSessionDoesNotClobberResumeState(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 2, "")
	// Session 1: complete one file.
	if err := h.store.Record(report.FileResult{
		Package: h.pkg("pa"), File: "lib/a.dart", Result: "killed", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Session 2 starts over the same reports dir, with one unmapped file
	// whose skip record would previously have clobbered the state.
	h.cfg.Mutation.Tests = nil
	var err error
	h.pkgs, err = discover.Discover(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	store2, err := report.NewJSONStore(h.cfg.Reports.Dir)
	if err != nil {
		t.Fatal(err)
	}
	// Load resume state BEFORE constructing the scheduler (as main.go
	// now does) — the same order a real session uses.
	state, err := store2.Resume()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(h.cfg, h.mon, h.ws, store2, h.pkgs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.SkipDone(state)
	go h.mon.Run(context.Background())
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	final, err := store2.Resume()
	if err != nil {
		t.Fatal(err)
	}
	if got := final[h.pkg("pa")]["lib/a.dart"]; got != "killed" {
		t.Errorf("session-1 result = %q, want preserved \"killed\"", got)
	}
	sum, err := store2.Summarize(time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one entry per file: no double-counted skips (F1).
	if sum.Packages[h.pkg("pa")].Total != 2 {
		t.Errorf("package total = %d, want 2 (no duplicates)", sum.Packages[h.pkg("pa")].Total)
	}
}

// TestF2CancelMidRunLeavesFileUnrecorded guards F2: interrupting a run
// mid-file must not persist a fabricated result.
func TestF2CancelMidRunLeavesFileUnrecorded(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 1, "")
	h.cfg.Mutation.Commands = []string{"sleep 1; false"} // would be "killed"

	ctx, cancel := context.WithCancel(context.Background())
	s, err := New(h.cfg, h.mon, h.ws, h.store, h.pkgs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	go h.mon.Run(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond) // mid-file
		cancel()
	}()
	if err := s.Run(ctx); err == nil {
		t.Error("expected context error from canceled run")
	}

	state, err := h.store.Resume()
	if err != nil {
		t.Fatal(err)
	}
	if got := state[h.pkg("pa")]["lib/a.dart"]; got != "" {
		t.Errorf("canceled file recorded as %q, want unrecorded", got)
	}
}

// TestF5TimeoutKillsProcessGroup guards F5: a timeout must kill the
// whole process tree, not just the shell.
func TestF5TimeoutKillsProcessGroup(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 1, "")
	h.cfg.Mutation.Timeout.Duration = "300ms"
	// A grandchild outlives the shell; the group kill must reach it.
	h.cfg.Mutation.Commands = []string{
		"(sleep 0.6; touch " + filepath.Join("{target_dir}", "late.txt") + ") & sleep 5",
	}
	h.run(t, Options{})

	time.Sleep(700 * time.Millisecond) // past the grandchild's own timer
	if _, err := os.Stat(filepath.Join(h.sbxDir("pa"), "late.txt")); !os.IsNotExist(err) {
		t.Error("orphaned grandchild survived the timeout kill")
	}
	state, _ := h.store.Resume()
	if got := state[h.pkg("pa")]["lib/a.dart"]; got != "timeout" {
		t.Errorf("result = %q, want timeout", got)
	}
}

// TestF8WorkersAfterCacheWithResumedPackage guards F8: a package whose
// files are all already recorded still counts toward cache readiness.
func TestF8WorkersAfterCacheWithResumedPackage(t *testing.T) {
	h := buildFixture(t, []string{"pa", "pb"}, 1, "")
	h.cfg.Scheduling.WorkersAfterCache = true
	h.cfg.Resources.MaxWorkers = 2
	// Fully record pa (a previous session completed it).
	if err := h.store.Record(report.FileResult{
		Package: h.pkg("pa"), File: "lib/a.dart", Result: "killed", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	s, err := New(h.cfg, h.mon, h.ws, h.store, h.pkgs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := h.store.Resume()
	s.SkipDone(state)

	// Simulate two active workers and pb prepared: the gate must be open
	// despite pa never being bootstrapped this session.
	sbx, ok, err := h.ws.TryAcquire(context.Background(), h.pkgs[1])
	if err != nil || !ok {
		t.Fatalf("prepare pb: ok=%v err=%v", ok, err)
	}
	sbx.Release()
	s.mu.Lock()
	s.active = 2
	s.mu.Unlock()
	if !s.cacheReadyForMoreWorkers() {
		t.Error("cache gate closed though every package is resolved (prepared or fully resumed)")
	}
	s.mu.Lock()
	s.active = 0
	s.mu.Unlock()
}

// failingStore wraps a Store, failing Record to exercise error handling.
type failingStore struct {
	report.Store
	failNext bool
}

func (f *failingStore) Record(r report.FileResult) error {
	if f.failNext {
		return fmt.Errorf("disk full")
	}
	return f.Store.Record(r)
}

// TestRecordFailureSurfaced covers the scheduler's report-write error
// paths: skip-recording failures abort the run; per-file record
// failures surface as notices without losing the run.
func TestRecordFailureSurfaced(t *testing.T) {
	t.Run("skip recording fails", func(t *testing.T) {
		h := buildFixture(t, []string{"pa"}, 1, "")
		// Unmapped file → a skip record at Run start.
		h.cfg.Mutation.Tests = nil
		var err error
		h.pkgs, err = discover.Discover(h.cfg)
		if err != nil {
			t.Fatal(err)
		}
		fs := &failingStore{Store: h.store, failNext: true}
		s, err := New(h.cfg, h.mon, h.ws, fs, h.pkgs, Options{})
		if err != nil {
			t.Fatal(err)
		}
		go h.mon.Run(context.Background())
		if err := s.Run(context.Background()); err == nil {
			t.Error("Run unexpectedly succeeded when skip recording failed")
		}
	})

	t.Run("file record fails with notice", func(t *testing.T) {
		h := buildFixture(t, []string{"pa"}, 1, "")
		fs := &failingStore{Store: h.store, failNext: true}
		var mu sync.Mutex
		var notices []string
		s, err := New(h.cfg, h.mon, h.ws, fs, h.pkgs, Options{
			Notify: func(msg string) {
				mu.Lock()
				notices = append(notices, msg)
				mu.Unlock()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		go h.mon.Run(context.Background())
		if err := s.Run(context.Background()); err != nil {
			t.Fatalf("Run = %v, want completed run despite record failure", err)
		}
		mu.Lock()
		defer mu.Unlock()
		found := false
		for _, n := range notices {
			if strings.Contains(n, "report write failed") {
				found = true
			}
		}
		if !found {
			t.Errorf("no report-write notice among %v", notices)
		}
	})
}

// TestCooldownCancelRequeues covers the interrupted-cooldown path: a
// worker canceled during its between-files cooldown must requeue the
// claimed file and record nothing for it.
func TestCooldownCancelRequeues(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 2, "")
	h.cfg.Resources.SpawnCooldown = "1s"
	h.cfg.Mutation.Commands = []string{"true"}

	ctx, cancel := context.WithCancel(context.Background())
	s, err := New(h.cfg, h.mon, h.ws, h.store, h.pkgs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	go h.mon.Run(context.Background())
	go func() {
		// First file finishes quickly; the worker then sleeps in its
		// 1s cooldown before file two.
		time.Sleep(400 * time.Millisecond)
		cancel()
	}()
	if err := s.Run(ctx); err == nil {
		t.Error("expected context error from canceled run")
	}
	state, _ := h.store.Resume()
	results := state[h.pkg("pa")]
	if len(results) != 1 {
		t.Errorf("recorded = %v, want exactly the first file (second requeued)", results)
	}
}

// TestBootstrapFailureRecordsError covers nextWork's sandbox-preparation
// failure path: the file is recorded as an error with a notice.
func TestBootstrapFailureRecordsError(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 2, "")
	h.cfg.Workspace.Bootstrap = "exit 3"
	var err error
	h.ws, err = workspace.NewManager(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var notices []string
	s, err := New(h.cfg, h.mon, h.ws, h.store, h.pkgs, Options{
		Notify: func(msg string) {
			mu.Lock()
			notices = append(notices, msg)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go h.mon.Run(context.Background())
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := h.store.Resume()
	for _, r := range state[h.pkg("pa")] {
		if r != "error" {
			t.Errorf("result = %q, want error", r)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, n := range notices {
		if strings.Contains(n, "sandbox preparation failed") {
			found = true
		}
	}
	if !found {
		t.Errorf("no preparation-failure notice among %v", notices)
	}
}

// TestBootstrapFailureNoDoubleCount is the regression test for the
// review blocker: under ramp_up calculated (ungated initial worker
// burst), a failing bootstrap could let two workers record the same head
// file and double-increment the per-package and session counters.
func TestBootstrapFailureNoDoubleCount(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 4, "")
	h.cfg.Workspace.Bootstrap = "exit 3"
	var err error
	h.ws, err = workspace.NewManager(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []FileEvent
	s, err := New(h.cfg, h.mon, h.ws, h.store, h.pkgs, Options{
		FileLog: func(e FileEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go h.mon.Run(context.Background())
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if s.Done() != s.Total() {
		t.Errorf("done = %d, total = %d: counters diverged (double count)", s.Done(), s.Total())
	}
	mu.Lock()
	defer mu.Unlock()
	finished := map[string]int{}
	for _, e := range events {
		if e.Done > e.Total {
			t.Errorf("event %d/%d for %s overflows total", e.Done, e.Total, e.File)
		}
		if e.Finished {
			finished[e.File]++
		}
	}
	for i := range 4 {
		file := "lib/" + string(rune('a'+i)) + ".dart"
		if finished[file] != 1 {
			t.Errorf("%s finished %d times, want exactly 1", file, finished[file])
		}
	}
}

// TestWatchRSSKillsOversizedProcess covers the memory-budget watcher end
// to end: a process tree exceeding its budget is killed and flagged.
func TestWatchRSSKillsOversizedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix shell allocation required")
	}
	cmd := exec.Command("sh", "-c", "var=$(head -c 3000000 /dev/zero); sleep 5")
	setNewPGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var oom atomic.Bool
	stop := make(chan struct{})
	watchRSS(cmd, 2, stop, &oom) // budget: 2 MB

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher never killed the oversized process")
	}
	close(stop)
	if !oom.Load() {
		t.Error("oom flag not set after budget kill")
	}
}

// TestTreeRSSSeesThisProcess covers the process-tree RSS walk.
func TestTreeRSSSeesThisProcess(t *testing.T) {
	if rss := treeRSS(int32(os.Getpid())); rss == 0 {
		t.Error("treeRSS = 0 for a live process")
	}
	if rss := groupRSS(int32(os.Getpid())); rss == 0 {
		t.Error("groupRSS = 0 for a live process")
	}
}

// TestKillGroupWithoutProcess covers the nil-process guard.
func TestKillGroupWithoutProcess(t *testing.T) {
	if err := killGroup(&exec.Cmd{}); err != nil {
		t.Errorf("killGroup on Process-less Cmd = %v, want nil", err)
	}
}

func TestUnmappedFilesRecordedSkipped(t *testing.T) {
	h := buildFixture(t, []string{"pa"}, 1, "")
	// Remove the tests mapping and re-discover: files become unmapped
	// and are recorded as skipped, never executed.
	h.cfg.Mutation.Tests = nil
	var err error
	h.pkgs, err = discover.Discover(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := h.run(t, Options{})
	if s.Total() != 0 || s.Done() != 0 {
		t.Fatalf("total/done = %d/%d, want 0/0", s.Total(), s.Done())
	}
	state, _ := h.store.Resume()
	if got := state[h.pkg("pa")]["lib/a.dart"]; got != "skipped" {
		t.Errorf("unmapped file = %q, want skipped", got)
	}
}
