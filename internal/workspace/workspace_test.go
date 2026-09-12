package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoLens-Project/golens-mutant/internal/config"
	"github.com/GoLens-Project/golens-mutant/internal/discover"
)

// fixture builds a source tree and its config:
//
//	src/
//	  libs/pkg_a/  pubspec.yaml, lib/a.dart, build/junk.txt, .dotfile
//	  libs/pkg_b/  pubspec.yaml, lib/b.dart
func fixture(t *testing.T) (cfg *config.Config, pkgs []*discover.Package) {
	t.Helper()
	base := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("libs/pkg_a/pubspec.yaml", "name: a")
	write("libs/pkg_a/lib/a.dart", "original a")
	write("libs/pkg_a/build/junk.txt", "junk")
	write("libs/pkg_a/.dotfile", "dot")
	write("libs/pkg_b/pubspec.yaml", "name: b")
	write("libs/pkg_b/lib/b.dart", "original b")

	cfg = cfgFor(t, base, `
workspace:
  base_dir: `+filepath.Join(base, ".work")+`
  bootstrap: "echo bootstrapped > bootstrap.txt"
  sync_exclude_patterns: [build, .dotfile]
`)
	pkgs, err := discover.Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("packages = %d, want 2", len(pkgs))
	}
	return cfg, pkgs
}

func cfgFor(t *testing.T, base, extra string) *config.Config {
	t.Helper()
	body := `
mutation:
  target_roots: [libs]
  file_patterns: ["lib/**/*.dart"]
  tests:
    - pattern: "lib/**"
      tests: [t]
  commands: [x]
` + extra
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Mutation.TargetRoots = []string{filepath.Join(base, "libs")}
	c.Workspace.BaseDir = filepath.Join(base, ".work")
	return c
}

func pkgByName(pkgs []*discover.Package, name string) *discover.Package {
	for _, p := range pkgs {
		if filepath.Base(p.Dir) == name {
			return p
		}
	}
	return nil
}

func TestSandboxLifecycle(t *testing.T) {
	cfg, pkgs := fixture(t)
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pa := pkgByName(pkgs, "pkg_a")

	s, ok, err := m.TryAcquire(context.Background(), pa)
	if err != nil || !ok || s == nil {
		t.Fatalf("TryAcquire = %v, %v, %v", s, ok, err)
	}

	// Exclusive access: a second acquire of the same package fails.
	if _, ok2, _ := m.TryAcquire(context.Background(), pa); ok2 {
		t.Error("second TryAcquire succeeded while sandbox held")
	}

	// Sources synced, excludes honored, bootstrap ran.
	if b, err := os.ReadFile(filepath.Join(s.Dir, "lib/a.dart")); err != nil || string(b) != "original a" {
		t.Errorf("a.dart = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "build")); !os.IsNotExist(err) {
		t.Error("build/ was synced despite exclude")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".dotfile")); !os.IsNotExist(err) {
		t.Error(".dotfile was synced despite exclude")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "bootstrap.txt")); err != nil {
		t.Errorf("bootstrap did not run: %v", err)
	}

	// Mutate the sandbox file, then RestorePristine must revert it.
	dst := filepath.Join(s.Dir, "lib/a.dart")
	if err := os.WriteFile(dst, []byte("MUTATED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.RestorePristine("lib/a.dart"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "original a" {
		t.Errorf("RestorePristine left %q", b)
	}

	// Release makes it acquirable again without re-bootstrapping.
	s.Release()
	s2, ok, err := m.TryAcquire(context.Background(), pa)
	if err != nil || !ok {
		t.Fatalf("re-acquire failed: %v", err)
	}
	if m.PreparedCount() != 1 {
		t.Errorf("PreparedCount = %d, want 1", m.PreparedCount())
	}
	s2.Release()
}

func TestPersistedSandboxResyncsWithoutBootstrap(t *testing.T) {
	cfg, pkgs := fixture(t)
	m, _ := NewManager(cfg)
	pa := pkgByName(pkgs, "pkg_a")
	s, ok, err := m.TryAcquire(context.Background(), pa)
	if err != nil || !ok {
		t.Fatal(err)
	}
	s.Release()

	// Change the source, mutate the sandbox copy, and start a fresh
	// manager (simulating a resumed session): the persisted sandbox must
	// be re-synced from source.
	srcFile := filepath.Join(pa.Dir, "lib/a.dart")
	if err := os.WriteFile(srcFile, []byte("changed source"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Same byte length as the new source, so only the content compare
	// (not size) can catch the leaked mutation.
	if err := os.WriteFile(filepath.Join(s.Dir, "lib/a.dart"), []byte("MUTATED LEAKED"), 0o644); err != nil {
		t.Fatal(err)
	}

	m2, _ := NewManager(cfg)
	s2, ok, err := m2.TryAcquire(context.Background(), pa)
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer s2.Release()
	if b, _ := os.ReadFile(filepath.Join(s2.Dir, "lib/a.dart")); string(b) != "changed source" {
		t.Errorf("resync left %q, want %q", b, "changed source")
	}
	// bootstrap.txt still present proves the sandbox survived.
	if _, err := os.Stat(filepath.Join(s2.Dir, "bootstrap.txt")); err != nil {
		t.Errorf("bootstrap artifact missing: %v", err)
	}
}

// TestDirtySandboxResyncsOnNextAcquire covers the killed-step path: a
// sandbox marked dirty (engine killed mid-mutation, mutant leaked into a
// file that is not the next step's target) must be fully re-synced from
// source on the next checkout — without re-bootstrapping.
func TestDirtySandboxResyncsOnNextAcquire(t *testing.T) {
	cfg, pkgs := fixture(t)
	m, _ := NewManager(cfg)
	pa := pkgByName(pkgs, "pkg_a")
	s, ok, err := m.TryAcquire(context.Background(), pa)
	if err != nil || !ok {
		t.Fatalf("TryAcquire = %v, %v", s, err)
	}

	// Simulate a killed engine: the leaked mutant sits in a file other
	// than the next step's target, at the same byte length as the source
	// so only the content compare catches it.
	if err := os.WriteFile(filepath.Join(s.Dir, "lib/a.dart"), []byte("MUTATED!OU"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.MarkDirty()
	s.Release()

	// Same manager, same session: the next checkout must re-sync.
	s2, ok, err := m.TryAcquire(context.Background(), pa)
	if err != nil || !ok {
		t.Fatalf("re-acquire = %v, %v", s2, err)
	}
	defer s2.Release()
	if b, _ := os.ReadFile(filepath.Join(s2.Dir, "lib/a.dart")); string(b) != "original a" {
		t.Errorf("dirty re-sync left %q, want %q", b, "original a")
	}
	// No re-bootstrap: the sandbox survives with its artifacts.
	if _, err := os.Stat(filepath.Join(s2.Dir, "bootstrap.txt")); err != nil {
		t.Errorf("bootstrap artifact missing after dirty re-sync: %v", err)
	}
}

func TestSlug(t *testing.T) {
	if got := slug("libs/core"); got != "libs__core" {
		t.Errorf("slug = %q", got)
	}
}

func TestIsPrepared(t *testing.T) {
	cfg, pkgs := fixture(t)
	m, _ := NewManager(cfg)
	pa := pkgByName(pkgs, "pkg_a")
	if m.IsPrepared(pa.Name) {
		t.Error("IsPrepared true before any acquire")
	}
	s, ok, err := m.TryAcquire(context.Background(), pa)
	if err != nil || !ok {
		t.Fatalf("TryAcquire = %v, %v, %v", s, ok, err)
	}
	if !m.IsPrepared(pa.Name) {
		t.Error("IsPrepared false after acquire")
	}
	if m.IsPrepared("never-acquired") {
		t.Error("IsPrepared true for unknown package")
	}
	s.Release()
}

func TestPrepareFailsWhenCacheUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission checks do not apply")
	}
	cfg, pkgs := fixture(t)
	// Pre-create the cache root read-only so sandbox creation fails.
	root := filepath.Join(cfg.Workspace.BaseDir, cfg.Workspace.CacheDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0o755)

	m, _ := NewManager(cfg)
	if _, _, err := m.TryAcquire(context.Background(), pkgByName(pkgs, "pkg_a")); err == nil {
		t.Error("TryAcquire unexpectedly succeeded with unwritable cache root")
	}
}

func TestPrepareFailsOnFailingBootstrap(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "libs", "pa", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "libs", "pa", "pubspec.yaml"), []byte("name: pa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "libs", "pa", "lib", "a.dart"), []byte("src"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := cfgFor(t, base, `
workspace:
  bootstrap: "exit 3"
`)
	pkgs, err := discover.Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := NewManager(cfg)
	_, _, err = m.TryAcquire(context.Background(), pkgByName(pkgs, "pa"))
	if err == nil || !strings.Contains(err.Error(), "exit 3") {
		t.Errorf("TryAcquire err = %v, want bootstrap failure", err)
	}
}

func TestPrepareFailsWhenSourceVanishes(t *testing.T) {
	cfg, pkgs := fixture(t)
	pa := pkgByName(pkgs, "pkg_a")
	// Remove the source tree after discovery: the sync walk must fail.
	if err := os.RemoveAll(pa.Dir); err != nil {
		t.Fatal(err)
	}
	m, _ := NewManager(cfg)
	_, _, err := m.TryAcquire(context.Background(), pa)
	if err == nil {
		t.Error("TryAcquire unexpectedly succeeded with missing source")
	}
}

func TestRestorePristineFailsWhenSourceMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission checks do not apply")
	}
	cfg, pkgs := fixture(t)
	m, _ := NewManager(cfg)
	pa := pkgByName(pkgs, "pkg_a")
	s, ok, err := m.TryAcquire(context.Background(), pa)
	if err != nil || !ok {
		t.Fatalf("TryAcquire = %v, %v, %v", s, ok, err)
	}
	defer s.Release()

	// Unwritable destination directory: the temp-file create must fail.
	if err := os.Chmod(filepath.Join(s.Dir, "lib"), 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(s.Dir, "lib"), 0o755)
	if err := s.RestorePristine("lib/a.dart"); err == nil {
		t.Error("RestorePristine unexpectedly succeeded with unwritable sandbox dir")
	}

	// Missing source file: the open must fail rather than wipe the
	// sandbox copy.
	if err := os.Chmod(filepath.Join(s.Dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(pa.Dir, "lib", "a.dart")); err != nil {
		t.Fatal(err)
	}
	if err := s.RestorePristine("lib/a.dart"); err == nil {
		t.Error("RestorePristine unexpectedly succeeded with missing source")
	}
}

func TestSyncExcludeGlobPatterns(t *testing.T) {
	base := t.TempDir()
	write := func(rel string) {
		t.Helper()
		full := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("libs/pa/pubspec.yaml")
	write("libs/pa/lib/keep.dart")
	write("libs/pa/lib/ign/secret.dart")

	cfg := cfgFor(t, base, `
workspace:
  sync_exclude_patterns: ["lib/ign/**"]
`)
	pkgs, err := discover.Discover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := NewManager(cfg)
	s, ok, err := m.TryAcquire(context.Background(), pkgByName(pkgs, "pa"))
	if err != nil || !ok {
		t.Fatalf("TryAcquire = %v, %v, %v", s, ok, err)
	}
	defer s.Release()
	if _, err := os.Stat(filepath.Join(s.Dir, "lib", "keep.dart")); err != nil {
		t.Errorf("keep.dart not synced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "lib", "ign")); !os.IsNotExist(err) {
		t.Error("lib/ign/ synced despite slash-glob exclude")
	}
}

func TestCopyFileFromDirectoryFails(t *testing.T) {
	// Reading from a directory fails mid-copy, exercising copyFile's
	// temp-file cleanup path.
	dir := t.TempDir()
	if err := copyFile(dir, filepath.Join(dir, "out")); err == nil {
		t.Error("copyFile from a directory unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "out.mutant-tmp")); !os.IsNotExist(err) {
		t.Error("temp file left behind after failed copy")
	}
}

func TestCacheRoot(t *testing.T) {
	cfg, _ := fixture(t)
	m, _ := NewManager(cfg)
	if got := m.CacheRoot(); !strings.HasSuffix(got, filepath.Join(".work", "cache")) {
		t.Errorf("CacheRoot = %q", got)
	}
}
