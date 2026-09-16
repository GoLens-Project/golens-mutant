package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// selectFixture builds a two-package workspace — demo with two mapped
// files, other with one — and returns its config path. Package names
// embed the absolute target root, so selectors glob on the /libs/ suffix.
func selectFixture(t *testing.T) (base, cfgPath string) {
	t.Helper()
	base = t.TempDir()
	for pkg, files := range map[string][]string{
		"demo":  {"a.dart", "b.dart"},
		"other": {"a.dart"},
	} {
		lib := filepath.Join(base, "libs", pkg, "lib")
		if err := os.MkdirAll(lib, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "libs", pkg, "pubspec.yaml"), []byte("name: "+pkg), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(lib, f), []byte("int x() => 1;"), 0o644); err != nil {
				t.Fatal(err)
			}
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

// resumeState reads and parses the session's resume-state.json
// (package → file → result).
func resumeState(t *testing.T, base string) map[string]map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(base, "reports", "resume-state.json"))
	if err != nil {
		t.Fatalf("resume state: %v", err)
	}
	var state map[string]map[string]string
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatalf("resume state: %v", err)
	}
	return state
}

// assertNoPackageArtifacts fails if any report or sandbox artifact
// mentions pkg — unselected work must leave nothing behind (the same
// checks TestRunEndToEndExempt applies to the exempt package).
func assertNoPackageArtifacts(t *testing.T, base, pkg string) {
	t.Helper()
	for _, dir := range []string{filepath.Join(base, "reports", "packages"), filepath.Join(base, ".work", "cache")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), pkg) {
				t.Errorf("%s contains %s artifact %s", dir, pkg, e.Name())
			}
		}
	}
}

func TestRunEndToEndPackageSelect(t *testing.T) {
	base, cfgPath := selectFixture(t)
	withArgs(t, "--config", cfgPath, "--package", "**/libs/demo")
	if err := run(); err != nil {
		t.Fatalf("run --package = %v", err)
	}
	// Only demo ran: both its files, nothing from other.
	state := resumeState(t, base)
	if len(state) != 1 || state[filepath.Join(base, "libs", "demo")] == nil {
		t.Fatalf("resume state = %v, want only the demo package", state)
	}
	got := state[filepath.Join(base, "libs", "demo")]
	if len(got) != 2 || got["lib/a.dart"] != "killed" || got["lib/b.dart"] != "killed" {
		t.Errorf("demo files = %v, want lib/a.dart and lib/b.dart killed", got)
	}
	// Unselected work never ran: no report or sandbox for other either.
	assertNoPackageArtifacts(t, base, "other")
}

func TestRunEndToEndFileSelect(t *testing.T) {
	base, cfgPath := selectFixture(t)
	withArgs(t, "--config", cfgPath, "--file", "**/libs/demo/lib/b.dart")
	if err := run(); err != nil {
		t.Fatalf("run --file = %v", err)
	}
	state := resumeState(t, base)
	if len(state) != 1 || state[filepath.Join(base, "libs", "demo")] == nil {
		t.Fatalf("resume state = %v, want only the demo package", state)
	}
	got := state[filepath.Join(base, "libs", "demo")]
	if len(got) != 1 || got["lib/b.dart"] != "killed" {
		t.Errorf("demo files = %v, want only lib/b.dart killed", got)
	}
	assertNoPackageArtifacts(t, base, "other")
	// demo ran, but only for the selected file: its package report must
	// not mention the unselected a.dart.
	entries, err := os.ReadDir(filepath.Join(base, "reports", "packages"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.Contains(entries[0].Name(), "demo") {
		t.Fatalf("package reports = %v, want only demo's", entries)
	}
	b, err := os.ReadFile(filepath.Join(base, "reports", "packages", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "lib/a.dart") {
		t.Errorf("demo report records the unselected file:\n%s", b)
	}
}

// TestDryRunSelection pins that --dry-run shows exactly what a selection
// would run: the plan header and package list reflect only the selection,
// and nothing is persisted.
func TestDryRunSelection(t *testing.T) {
	base, cfgPath := selectFixture(t)
	var stdout bytes.Buffer
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })
	withArgs(t, "--config", cfgPath, "--dry-run", "--package", "**/libs/demo")
	runErr := run()
	w.Close()
	os.Stdout = oldStdout
	if _, err := io.Copy(&stdout, r); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("dry run --package = %v", runErr)
	}
	out := stdout.String()
	// The fixture holds two packages with three mapped files; only
	// demo's two survive the selection.
	if !strings.Contains(out, "Dry run — 1 package(s), 2 runnable file(s), 0 skipped") {
		t.Errorf("plan header does not reflect the selection:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(base, "libs", "demo")) {
		t.Errorf("plan omits the selected package:\n%s", out)
	}
	if strings.Contains(out, filepath.Join(base, "libs", "other")) {
		t.Errorf("plan lists the unselected package:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(base, ".work")); !os.IsNotExist(err) {
		t.Error("dry run created sandboxes")
	}
	if _, err := os.Stat(filepath.Join(base, "reports")); !os.IsNotExist(err) {
		t.Error("dry run created the reports directory")
	}
}

func TestSelectorMatchingNothingIsError(t *testing.T) {
	base, cfgPath := selectFixture(t)
	// Select runs before the store, so the error path must be free of
	// side effects — reports/ is never created.
	assertNoReports := func(t *testing.T) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(base, "reports")); !os.IsNotExist(err) {
			t.Error("no-match error created the reports directory")
		}
	}
	t.Run("package", func(t *testing.T) {
		withArgs(t, "--config", cfgPath, "--package", "libs/nowhere")
		err := run()
		if err == nil || !strings.Contains(err.Error(), "matched no discovered package") {
			t.Errorf("run --package libs/nowhere = %v, want no-match error", err)
		}
		assertNoReports(t)
	})
	t.Run("file", func(t *testing.T) {
		withArgs(t, "--config", cfgPath, "--file", "**/lib/absent.dart")
		err := run()
		if err == nil || !strings.Contains(err.Error(), "matched no discovered file") {
			t.Errorf("run --file absent = %v, want no-match error", err)
		}
		assertNoReports(t)
	})
}
