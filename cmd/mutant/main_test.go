package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoLens-Project/golens-mutant/internal/config"
	"github.com/GoLens-Project/golens-mutant/internal/scheduler"
)

// withArgs temporarily replaces os.Args (run() parses them itself).
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	old := os.Args
	os.Args = append([]string{"mutant"}, args...)
	t.Cleanup(func() { os.Args = old })
}

func TestVersionFlag(t *testing.T) {
	withArgs(t, "--version")
	if err := run(); err != nil {
		t.Errorf("run --version = %v", err)
	}
}

func TestGenerateExampleFlag(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config-example.yaml")
	withArgs(t, "--generate-config-example", out)
	if err := run(); err != nil {
		t.Fatalf("run --generate-config-example = %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != config.Example {
		t.Error("generated example does not match config.Example")
	}
}

func TestMissingConfigIsError(t *testing.T) {
	withArgs(t, "--config", filepath.Join(t.TempDir(), "absent.yaml"))
	if err := run(); err == nil {
		t.Error("run with missing config unexpectedly succeeded")
	}
}

func TestNoPackagesIsError(t *testing.T) {
	empty := t.TempDir()
	// The root exists but holds no packages.
	if err := os.MkdirAll(filepath.Join(empty, "libs"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(empty, "config.yaml")
	body := `
mutation:
  target_roots: [` + filepath.Join(empty, "libs") + `]
  file_patterns: ["lib/**/*.dart"]
  tests:
    - pattern: "lib/**"
      tests: [t]
  commands: [x]
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	withArgs(t, "--config", cfgPath)
	err := run()
	if err == nil || !strings.Contains(err.Error(), "no packages found") {
		t.Errorf("run on empty workspace = %v, want no-packages error", err)
	}
}

func TestRunEndToEnd(t *testing.T) {
	base := t.TempDir()
	libs := filepath.Join(base, "libs", "demo", "lib")
	if err := os.MkdirAll(libs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "libs", "demo", "pubspec.yaml"), []byte("name: demo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libs, "a.dart"), []byte("int a() => 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(base, "config.yaml")
	body := `
mutation:
  target_roots: [` + filepath.Join(base, "libs") + `]
  file_patterns: ["lib/**/*.dart"]
  tests:
    - pattern: "lib/**"
      tests: [t]
  commands: ["echo mutating {package}/{file} && exit 1"]
resources:
  spawn_cooldown: 0s
workspace:
  base_dir: ` + filepath.Join(base, ".work") + `
  bootstrap: ""
reports:
  dir: ` + filepath.Join(base, "reports") + `
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	withArgs(t, "--config", cfgPath)
	if err := run(); err != nil {
		t.Fatalf("end-to-end run = %v", err)
	}
	b, err := os.ReadFile(filepath.Join(base, "reports", "resume-state.json"))
	if err != nil {
		t.Fatalf("resume state: %v", err)
	}
	if !strings.Contains(string(b), `"killed"`) {
		t.Errorf("resume state = %s, want a killed result", b)
	}
}

// exemptFixture builds a two-package workspace (demo and other) and returns
// its config path. Both packages have one mapped, runnable file.
func exemptFixture(t *testing.T) (base, cfgPath string) {
	t.Helper()
	base = t.TempDir()
	for _, pkg := range []string{"demo", "other"} {
		lib := filepath.Join(base, "libs", pkg, "lib")
		if err := os.MkdirAll(lib, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "libs", pkg, "pubspec.yaml"), []byte("name: "+pkg), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lib, "a.dart"), []byte("int a() => 1;"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath = filepath.Join(base, "config.yaml")
	body := `
mutation:
  target_roots: [` + filepath.Join(base, "libs") + `]
  file_patterns: ["lib/**/*.dart"]
  tests:
    - pattern: "lib/**"
      tests: [t]
  commands: ["echo mutating {package}/{file} && exit 1"]
resources:
  spawn_cooldown: 0s
workspace:
  base_dir: ` + filepath.Join(base, ".work") + `
  bootstrap: ""
reports:
  dir: ` + filepath.Join(base, "reports") + `
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, cfgPath
}

func TestRunEndToEndExempt(t *testing.T) {
	base, cfgPath := exemptFixture(t)
	// Package names embed the absolute target root, so match on the
	// suffix; "libs/nowhere" matches nothing and must trigger a warning.
	var stderr bytes.Buffer
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = oldStderr })
	withArgs(t, "--config", cfgPath, "--exempt", "**/libs/demo", "--exempt", "libs/nowhere")
	runErr := run()
	w.Close()
	os.Stderr = oldStderr
	if _, err := io.Copy(&stderr, r); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("run --exempt = %v", runErr)
	}
	if s := stderr.String(); !strings.Contains(s, "matched no packages") || !strings.Contains(s, "libs/nowhere") {
		t.Errorf("stderr missing unmatched-pattern warning:\n%s", s)
	}

	// Only "other" ran: its result is recorded, demo's is not.
	b, err := os.ReadFile(filepath.Join(base, "reports", "resume-state.json"))
	if err != nil {
		t.Fatalf("resume state: %v", err)
	}
	if s := string(b); !strings.Contains(s, "libs/other") || strings.Contains(s, "libs/demo") {
		t.Errorf("resume state = %s, want only the other package", s)
	}
	// No report or sandbox was built for the exempt package either.
	for _, dir := range []string{filepath.Join(base, "reports", "packages"), filepath.Join(base, ".work", "cache")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), "demo") {
				t.Errorf("%s contains exempt artifact %s", dir, e.Name())
			}
		}
	}
}

func TestRunEndToEndAllExemptIsError(t *testing.T) {
	_, cfgPath := exemptFixture(t)
	withArgs(t, "--config", cfgPath, "--exempt", "**/libs/*")
	err := run()
	if err == nil || !strings.Contains(err.Error(), "exempted") {
		t.Errorf("run all-exempt = %v, want all-exempted error", err)
	}
}

func TestRenderBar(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	bar := renderBar(w)
	bar(3, 10)
	w.Close()
	buf := make([]byte, 256)
	n, _ := r.Read(buf)
	got := string(buf[:n])
	if !strings.Contains(got, "3/10 files") {
		t.Errorf("bar = %q, want progress counts", got)
	}
	if c := strings.Count(got, "="); c != 9 { // 30-wide bar, 30% filled
		t.Errorf("bar has %d '=' segments, want 9: %q", c, got)
	}

	// Zero total must not divide by zero.
	bar(0, 0)
}

func TestLogFileEvent(t *testing.T) {
	var buf bytes.Buffer
	// Events render in their own location; construct UTC times so the
	// expectation is not timezone-dependent.
	logFileEvent(&buf)(scheduler.FileEvent{
		Time: time.Unix(0, 0).UTC(), Package: "libs/pa", File: "lib/a.dart", Done: 1, Total: 2,
	})
	want := "\r\033[K[1970-01-01T00:00:00Z] started 1/2 libs/pa - lib/a.dart\n"
	if got := buf.String(); got != want {
		t.Errorf("logFileEvent = %q, want %q", got, want)
	}
	buf.Reset()
	logFileEvent(&buf)(scheduler.FileEvent{
		Time: time.Unix(0, 0).UTC(), Package: "libs/pa", File: "lib/a.dart", Done: 2, Total: 2, Finished: true,
	})
	want = "\r\033[K[1970-01-01T00:00:00Z] finished 2/2 libs/pa - lib/a.dart\n"
	if got := buf.String(); got != want {
		t.Errorf("logFileEvent finished = %q, want %q", got, want)
	}
}

// failWriter rejects every write, standing in for a broken console.
type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 0, errors.New("boom") }

// gatedWriter blocks every write until released, standing in for a
// stalled console (Ctrl-S, laggy PTY); writes are recorded once flowing.
type gatedWriter struct {
	mu    sync.Mutex
	block bool
	cond  *sync.Cond
	buf   bytes.Buffer
}

func newGatedWriter() *gatedWriter {
	g := &gatedWriter{block: true}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gatedWriter) release() {
	g.mu.Lock()
	g.block = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	g.mu.Lock()
	for g.block {
		g.cond.Wait()
	}
	g.buf.Write(p)
	g.mu.Unlock()
	return len(p), nil
}

func (g *gatedWriter) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

func TestStreamSink(t *testing.T) {
	var buf bytes.Buffer
	sink := newStreamSink(&lockedWriter{w: &buf}, scheduler.FileEvent{
		Package: "libs/pa", File: "lib/a.dart", Done: 1, Total: 2,
	})
	// Chunks are not line-aligned; '\n' terminates lines and no tag may
	// land mid-line. ('\r' handling has its own cases below.)
	for _, chunk := range []string{"par", "tial\nne", "xt\n"} {
		if n, err := sink.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil", chunk, n, err, len(chunk))
		}
	}
	sink.Write([]byte("trailing"))
	sink.Flush()

	const tag = "] started 1/2 libs/pa - lib/a.dart "
	for _, line := range []string{"partial", "next", "trailing"} {
		if !strings.Contains(buf.String(), tag+line+"\n") {
			t.Errorf("stream output missing line %q:\n%s", line, buf.String())
		}
	}
	if c := strings.Count(buf.String(), "libs/pa - lib/a.dart"); c != 3 {
		t.Errorf("tag appears %d times, want exactly one per line (3):\n%s", c, buf.String())
	}

	// Spinner redraws (bare '\r', no '\n') overwrite the line: they
	// collapse to the final frame, not one tagged line per frame.
	// Flush drains the queue before the assertions read buf.
	buf.Reset()
	sink.Write([]byte("frame1\rframe2\rframe3\n"))
	sink.Flush()
	if strings.Contains(buf.String(), "frame1") || strings.Contains(buf.String(), "frame2") {
		t.Errorf("spinner frames not collapsed:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), tag+"frame3\n") {
		t.Errorf("final spinner frame missing:\n%s", buf.String())
	}

	// An unterminated line wiped by a '\r' is gone entirely.
	buf.Reset()
	sink.Write([]byte("ne"))
	sink.Write([]byte("xt\rspin\n"))
	sink.Flush()
	if strings.Contains(buf.String(), "next") {
		t.Errorf("overwritten line content leaked:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), tag+"spin\n") {
		t.Errorf("line after overwrite missing:\n%s", buf.String())
	}

	// CRLF counts as one terminator, whole or split across writes.
	buf.Reset()
	sink.Write([]byte("whole\r\ndef\n"))
	sink.Write([]byte("abc\r"))
	sink.Write([]byte("\n"))
	sink.Flush()
	for _, line := range []string{"whole", "def", "abc"} {
		if !strings.Contains(buf.String(), tag+line+"\n") {
			t.Errorf("CRLF line %q missing:\n%s", line, buf.String())
		}
	}
	if strings.Count(buf.String(), "libs/pa - lib/a.dart") != 3 {
		t.Errorf("CRLF emitted extra lines:\n%s", buf.String())
	}

	// Flush resolves a trailing '\r' exactly once — no spurious empty
	// line, and Close stops the drainer.
	buf.Reset()
	sink.Write([]byte("done\r"))
	sink.Flush()
	if c := strings.Count(buf.String(), tag+"done\n"); c != 1 {
		t.Errorf("trailing-'\\r' flush emitted %d lines, want 1:\n%s", c, buf.String())
	}
	sink.Close()

	// A failing console must not surface through Write (it would
	// misclassify the mutation result via cmd.Wait).
	sink2 := newStreamSink(&lockedWriter{w: failWriter{}}, scheduler.FileEvent{
		Package: "libs/pa", File: "lib/a.dart", Done: 1, Total: 2,
	})
	if n, err := sink2.Write([]byte("x\n")); err != nil || n != 2 {
		t.Errorf("Write on failing console = %d, %v; want 2, nil", n, err)
	}
	sink2.Flush()
	sink2.Close()
}

func TestStreamSinkDropsWhenConsoleStalled(t *testing.T) {
	gate := newGatedWriter()
	sink := newStreamSinkCap(&lockedWriter{w: gate}, scheduler.FileEvent{
		Package: "libs/pa", File: "lib/a.dart", Done: 1, Total: 2,
	}, 2)
	// The drainer is stuck inside the first write; with a queue of two,
	// everything past the third line must drop instead of blocking the
	// copy goroutine.
	for i := range 20 {
		chunk := fmt.Sprintf("line%d\n", i)
		if n, err := sink.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil — console stall must not block Write", chunk, n, err, len(chunk))
		}
	}
	gate.release()
	sink.Flush() // must not deadlock
	sink.Close()

	out := gate.String()
	if c := strings.Count(out, "- lib/a.dart line"); c > 3 {
		t.Errorf("%d lines reached the stalled console, want ≤ 3 (1 in flight + 2 queued):\n%s", c, out)
	}
	if !strings.Contains(out, "stream lines dropped") {
		t.Errorf("no drop summary after stalled console:\n%s", out)
	}
}
