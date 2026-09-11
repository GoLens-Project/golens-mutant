package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoLens-Project/golens-mutant/internal/config"
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
