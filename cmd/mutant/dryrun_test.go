package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoLens-Project/golens-mutant/internal/config"
	"github.com/GoLens-Project/golens-mutant/internal/discover"
	"github.com/GoLens-Project/golens-mutant/internal/report"
	"github.com/GoLens-Project/golens-mutant/internal/workspace"
)

// dryFixture builds a two-package workspace — core: one mapped file and
// one unmapped; net: one mapped file — and returns a loader that writes
// a config with the given extra YAML.
func dryFixture(t *testing.T) (dir string, load func(extra string) string) {
	t.Helper()
	dir = t.TempDir()
	write := func(rel string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("src"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("libs/core/pubspec.yaml")
	write("libs/core/lib/a.dart")
	write("libs/core/lib/generated.dart")
	write("libs/net/pubspec.yaml")
	write("libs/net/lib/net.dart")

	return dir, func(extra string) string {
		// The base config carries no reports: section — callers that
		// need one provide it via extra (YAML rejects duplicate keys,
		// so a base block plus a caller block cannot coexist).
		if extra == "" {
			extra = "reports:\n  dir: " + filepath.Join(dir, "reports") + "\n"
		}
		cfgBody := `
mutation:
  target_roots: [` + filepath.Join(dir, "libs") + `]
  file_patterns: ["lib/**/*.dart"]
  tests:
    - pattern: "lib/a.dart"
      tests: ["test/core_test.dart"]
    - pattern: "lib/net*.dart"
      tests: ["test/net_test.dart", "test/integ_test.dart"]
  commands:
    - "mutate {abs_file} && runtests {tests}"
workspace:
  base_dir: ` + filepath.Join(dir, ".work") + `
  bootstrap: "pub get in {target_dir}"
` + extra
		p := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(p, []byte(cfgBody), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
}

func TestRenderPlan(t *testing.T) {
	_, load := dryFixture(t)
	cfg, err := config.Load(load(""))
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := discover.Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("packages = %d, want 2", len(pkgs))
	}
	ws, err := workspace.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// core/a.dart already has a result: the plan must show it as
	// recorded and pick net.dart as the command example.
	var net *discover.Package
	for _, p := range pkgs {
		if strings.HasSuffix(p.Name, "net") {
			net = p
		}
	}
	recorded := map[string]map[string]string{
		pkgs[0].Name: {"lib/a.dart": "killed"},
	}

	out := renderPlan(cfg, pkgs, ws, recorded)
	for _, want := range []string{
		"2 package(s), 2 runnable file(s), 1 skipped",
		"test/core_test.dart",
		"test/net_test.dart test/integ_test.dart",
		"skipped (no tests mapping)",
		"[recorded: killed — resume skips]",
		"commands (example:",
		"pub get in",
		"max workers:",
		"resume: true (1 file(s) already recorded)",
		"sandboxes:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "mutate "+ws.SandboxPath(net.Name)+"/lib/net.dart") {
		t.Errorf("plan example not interpolated for net.dart:\n%s", out)
	}
}

func TestRenderPlanExempt(t *testing.T) {
	_, load := dryFixture(t)
	cfg, err := config.Load(load(""))
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := discover.Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Exempt core: its mapped file must show as skipped (exempt), not
	// runnable, and net.dart must become the command example.
	for _, p := range pkgs {
		if strings.HasSuffix(p.Name, "core") {
			p.Exempt = true
		}
	}
	ws, err := workspace.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}

	out := renderPlan(cfg, pkgs, ws, nil)
	for _, want := range []string{
		"2 package(s), 1 runnable file(s), 2 skipped (2 exempt)",
		"[exempt]",
		"skipped (exempt)",
		"commands (example:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q:\n%s", want, out)
		}
	}
	// The command example must come from net, not the exempt package.
	var example string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "commands (example:") {
			example = line
		}
	}
	if !strings.Contains(example, "net") {
		t.Errorf("command example %q not from net package:\n%s", example, out)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	t.Run("json backend", func(t *testing.T) {
		dir, load := dryFixture(t)
		withArgs(t, "--config", load(""), "--dry-run")
		if err := run(); err != nil {
			t.Fatalf("dry run = %v", err)
		}
		// No sandboxes, no reports tree at all — a dry run must not
		// even create directories.
		if _, err := os.Stat(filepath.Join(dir, ".work")); !os.IsNotExist(err) {
			t.Error("dry run created sandboxes")
		}
		if _, err := os.Stat(filepath.Join(dir, "reports")); !os.IsNotExist(err) {
			t.Error("dry run created the reports directory")
		}
	})

	t.Run("sqlite backend", func(t *testing.T) {
		dir, load := dryFixture(t)
		db := filepath.Join(dir, "mutant.db")
		withArgs(t, "--config", load(`
reports:
  storage: sqlite
  sqlite_path: `+db+`
`), "--dry-run")
		if err := run(); err != nil {
			t.Fatalf("dry run = %v", err)
		}
		if _, err := os.Stat(db); !os.IsNotExist(err) {
			t.Error("dry run created the sqlite database")
		}
		if _, err := os.Stat(filepath.Join(dir, ".work")); !os.IsNotExist(err) {
			t.Error("dry run created sandboxes")
		}
	})
}

func TestDryRunOverSeededSQLite(t *testing.T) {
	dir, load := dryFixture(t)
	db := filepath.Join(dir, "mutant.db")
	cfgPath := load(`
reports:
  storage: sqlite
  sqlite_path: ` + db + `
`)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := report.NewSQLiteStore(db, filepath.Join(dir, "logs"))
	if err != nil {
		t.Skipf("sqlite unavailable: %v", err)
	}
	pkgs, err := discover.Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(report.FileResult{
		Package: pkgs[0].Name, File: "lib/a.dart", Result: "killed", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	before, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	withArgs(t, "--config", cfgPath, "--dry-run")
	if err := run(); err != nil {
		t.Fatalf("dry run over seeded sqlite = %v", err)
	}
	after, _ := os.ReadFile(db)
	if !bytes.Equal(before, after) {
		t.Error("dry run modified the sqlite database")
	}
	if _, err := os.Stat(filepath.Join(dir, ".work")); !os.IsNotExist(err) {
		t.Error("dry run created sandboxes")
	}
}

func TestDryRunReadsExistingResumeState(t *testing.T) {
	_, load := dryFixture(t)
	cfgPath := load("")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// Seed recorded state through a real store, then dry-run over it.
	store, err := report.NewJSONStore(cfg.Reports.Dir)
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := discover.Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(report.FileResult{
		Package: pkgs[0].Name, File: "lib/a.dart", Result: "killed", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	withArgs(t, "--config", cfgPath, "--dry-run")
	if err := run(); err != nil {
		t.Fatalf("dry run over recorded state = %v", err)
	}
	state, err := report.ReadResume(cfg.Reports.Storage, cfg.Reports.Dir, cfg.SQLitePath())
	if err != nil {
		t.Fatal(err)
	}
	if state[pkgs[0].Name]["lib/a.dart"] != "killed" {
		t.Errorf("ReadResume = %v, want recorded killed result", state)
	}
}
