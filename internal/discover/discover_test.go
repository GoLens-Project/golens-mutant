package discover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoLens-Project/golens-mutant/internal/config"
)

// tree builds:
//
//	root/
//	  pubspec.yaml          ← root itself is a package
//	  lib/root.dart         (no tests mapping → skipped file)
//	  lib/gen.g.dart        (excluded)
//	  core/
//	    pubspec.yaml        ← nested package
//	    lib/core.dart       (mapped)
//	    lib/skipme.dart     (unmapped → skipped)
//	  plain/                ← not a package
//	    lib/lost.dart
func tree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	files := map[string]string{
		"pubspec.yaml":         "",
		"lib/root.dart":        "",
		"lib/gen.g.dart":       "",
		"core/pubspec.yaml":    "",
		"core/lib/core.dart":   "",
		"core/lib/skipme.dart": "",
		"plain/lib/lost.dart":  "",
	}
	for rel, body := range files {
		full := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func loadConfig(t *testing.T, body string) *config.Config {
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

func TestDiscoverNestedPackagesAndFilters(t *testing.T) {
	base := tree(t)
	c := loadConfig(t, `
mutation:
  target_roots: [.]
  package_marker: pubspec.yaml
  file_patterns: ["lib/**/*.dart"]
  file_exclude_patterns: ["**/*.g.dart"]
  tests:
    - pattern: "lib/core.dart"
      tests: ["test/core_test.dart"]
  commands: [x]
`)
	// Point the target root at the fixture.
	c.Mutation.TargetRoots = []string{base}

	pkgs, err := Discover(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("packages = %d, want 2 (root + core): %+v", len(pkgs), pkgs)
	}

	byName := map[string]*Package{}
	for _, p := range pkgs {
		byName[p.Name] = p
	}
	// Names embed the configured root path (F4).
	root, core := byName[base], byName[base+"/core"]
	if root == nil || core == nil {
		t.Fatalf("missing packages: %v", byName)
	}

	// Root package: root.dart unmapped (skipped), gen.g.dart excluded.
	// plain/lib/lost.dart belongs to no package.
	if len(root.Files) != 1 || root.Files[0].Rel != "lib/root.dart" || root.Files[0].Run {
		t.Errorf("root files = %+v", root.Files)
	}
	// Core package: core.dart mapped+runnable, skipme.dart unmapped.
	if len(core.Files) != 2 {
		t.Fatalf("core files = %+v", core.Files)
	}
	var runnable []File
	for _, f := range core.Files {
		if f.Run {
			runnable = append(runnable, f)
		}
	}
	if len(runnable) != 1 || runnable[0].Rel != "lib/core.dart" {
		t.Errorf("runnable = %+v", runnable)
	}
	if len(runnable[0].Tests) != 1 || runnable[0].Tests[0] != "test/core_test.dart" {
		t.Errorf("tests = %+v", runnable[0].Tests)
	}
	if got := core.RunnableFiles(); len(got) != 1 {
		t.Errorf("RunnableFiles = %+v", got)
	}
}

func TestMarkExempt(t *testing.T) {
	pkgs := []*Package{
		{Name: "libs/core"},
		{Name: "libs/generated/api"},
		{Name: "apps/tool"},
	}

	// Empty patterns: no-op, no unmatched.
	if unmatched := MarkExempt(pkgs, nil); unmatched != nil {
		t.Errorf("unmatched = %v, want nil", unmatched)
	}
	for _, p := range pkgs {
		if p.Exempt {
			t.Errorf("%s marked exempt with no patterns", p.Name)
		}
	}

	unmatched := MarkExempt(pkgs, []string{
		"libs/core",             // exact name
		"libs/generated/**",     // glob
		"packages/legacy_proto", // matches nothing
	})
	if !pkgs[0].Exempt || !pkgs[1].Exempt {
		t.Errorf("libs/core, libs/generated/api not exempt: %+v", pkgs)
	}
	if pkgs[2].Exempt {
		t.Errorf("apps/tool wrongly exempt")
	}
	if len(unmatched) != 1 || unmatched[0] != "packages/legacy_proto" {
		t.Errorf("unmatched = %v, want [packages/legacy_proto]", unmatched)
	}

	// Marking again is idempotent; the already-matched pattern is not
	// reported as unmatched on a second pass over the same queue.
	if unmatched := MarkExempt(pkgs[:2], []string{"libs/**"}); unmatched != nil {
		t.Errorf("unmatched = %v, want nil", unmatched)
	}
}

// selectPackages builds a three-package queue: core with a mapped and an
// unmapped (skipped) file, net and tool with one mapped file each.
func selectPackages() []*Package {
	return []*Package{
		{Name: "libs/core", Files: []File{
			{Rel: "lib/a.dart", Tests: []string{"core_t"}, Run: true},
			{Rel: "lib/skip.dart"},
		}},
		{Name: "libs/net", Files: []File{
			{Rel: "lib/net.dart", Tests: []string{"net_t"}, Run: true},
		}},
		{Name: "apps/tool", Files: []File{
			{Rel: "lib/tool.dart", Tests: []string{"tool_t"}, Run: true},
		}},
	}
}

func TestSelect(t *testing.T) {
	t.Run("empty selectors keep everything", func(t *testing.T) {
		got, err := Select(selectPackages(), "", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Errorf("packages = %d, want all 3 kept", len(got))
		}
	})

	t.Run("package selector keeps matching packages", func(t *testing.T) {
		got, err := Select(selectPackages(), "libs/**", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Name != "libs/core" || got[1].Name != "libs/net" {
			t.Errorf("packages = %v, want libs/core and libs/net", names(got))
		}
		// Package filtering keeps the packages whole.
		if len(got[0].Files) != 2 {
			t.Errorf("libs/core files = %+v, want untouched", got[0].Files)
		}
	})

	t.Run("package selector matching nothing is an error", func(t *testing.T) {
		_, err := Select(selectPackages(), "libs/nowhere", "")
		if err == nil {
			t.Fatal("want error for unmatched --package")
		}
		for _, want := range []string{`--package "libs/nowhere"`, "have: libs/core, libs/net, apps/tool"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})

	t.Run("file selector narrows files and drops empty packages", func(t *testing.T) {
		pkgs := selectPackages()
		got, err := Select(pkgs, "", "libs/core/lib/a.dart")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "libs/core" {
			t.Fatalf("packages = %v, want only libs/core", names(got))
		}
		if len(got[0].Files) != 1 || got[0].Files[0].Rel != "lib/a.dart" {
			t.Errorf("files = %+v, want only lib/a.dart", got[0].Files)
		}
		// The selection must not cull the input queue.
		if len(pkgs[0].Files) != 2 {
			t.Errorf("input package mutated: %+v", pkgs[0].Files)
		}
	})

	t.Run("file selector glob spans packages and keeps skipped files", func(t *testing.T) {
		got, err := Select(selectPackages(), "", "**/lib/*.dart")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("packages = %v, want all 3 (each has a match)", names(got))
		}
		// core keeps both its files, unmapped one included, flags intact.
		if len(got[0].Files) != 2 || got[0].Files[1].Run {
			t.Errorf("libs/core files = %+v, want a.dart + skip.dart (Run false)", got[0].Files)
		}
	})

	t.Run("file selector matching nothing is an error", func(t *testing.T) {
		_, err := Select(selectPackages(), "", "libs/core/lib/absent.dart")
		if err == nil {
			t.Fatal("want error for unmatched --file")
		}
		for _, want := range []string{`--file "libs/core/lib/absent.dart"`, "in: libs/core, libs/net, apps/tool"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})

	t.Run("package and file selectors combine", func(t *testing.T) {
		got, err := Select(selectPackages(), "libs/**", "**/lib/net.dart")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "libs/net" || len(got[0].Files) != 1 {
			t.Errorf("selection = %v %v, want libs/net with net.dart only", names(got), got[0].Files)
		}
		// After the package filter, the file error lists only the remaining
		// packages.
		_, err = Select(selectPackages(), "libs/**", "apps/**")
		if err == nil || !strings.Contains(err.Error(), "in: libs/core, libs/net") {
			t.Errorf("error = %v, want --file miss listing only libs/*", err)
		}
	})
}

func TestDiscoverOverlappingRootsDedupes(t *testing.T) {
	base := t.TempDir()
	write := func(rel string) {
		t.Helper()
		full := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("libs/pa/pubspec.yaml")
	write("libs/pa/lib/a.dart")

	c := loadConfig(t, `
mutation:
  target_roots: [.]
  file_patterns: ["lib/**/*.dart"]
  tests:
    - pattern: "lib/**"
      tests: [t]
  commands: [x]
`)
	// Overlapping roots: the parent and the child.
	c.Mutation.TargetRoots = []string{base, filepath.Join(base, "libs")}

	pkgs, err := Discover(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("packages = %d, want 1 (deduped): %+v", len(pkgs), pkgs)
	}
	// The file must be claimed exactly once (first root wins).
	if len(pkgs[0].Files) != 1 || pkgs[0].Files[0].Rel != "lib/a.dart" {
		t.Errorf("files = %+v, want exactly one lib/a.dart", pkgs[0].Files)
	}
}

func TestDiscoverSameNamedRootsDontCollide(t *testing.T) {
	base := t.TempDir()
	for _, root := range []string{"apps/libs/pa", "tools/libs/pa"} {
		for _, rel := range []string{"pubspec.yaml", "lib/a.dart"} {
			full := filepath.Join(base, root, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := loadConfig(t, `
mutation:
  target_roots: [.]
  file_patterns: ["lib/**/*.dart"]
  tests:
    - pattern: "lib/**"
      tests: [t]
  commands: [x]
`)
	c.Mutation.TargetRoots = []string{filepath.Join(base, "apps/libs"), filepath.Join(base, "tools/libs")}

	pkgs, err := Discover(c)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, p := range pkgs {
		names[p.Name]++
		if len(p.Files) != 1 {
			t.Errorf("%s files = %+v", p.Name, p.Files)
		}
	}
	if len(names) != 2 {
		t.Errorf("package names = %v, want 2 distinct", names)
	}
	for name, n := range names {
		if n != 1 {
			t.Errorf("name %q used %d times (collision)", name, n)
		}
	}
}

func TestPkgName(t *testing.T) {
	cases := []struct {
		root, absRoot, pkgDir, want string
	}{
		// Relative root: name embeds the configured root path.
		{"libs", "/abs/libs", "/abs/libs", "libs"},
		{"libs", "/abs/libs", "/abs/libs/core", "libs/core"},
		{"apps/libs", "/abs/apps/libs", "/abs/apps/libs/pa", "apps/libs/pa"},
		// Root ".": the path relative to the root as-is.
		{".", "/abs", "/abs/libs", "libs"},
	}
	for _, tc := range cases {
		if got := pkgName(tc.root, tc.absRoot, tc.pkgDir); got != tc.want {
			t.Errorf("pkgName(%q, %q, %q) = %q, want %q", tc.root, tc.absRoot, tc.pkgDir, got, tc.want)
		}
	}
}

func TestDiscoverOrdering(t *testing.T) {
	base := t.TempDir()
	// aaa has 1 runnable file, zzz has 3: lexical (discovery) order puts
	// aaa first, largest_first puts zzz first.
	pkgs := map[string]int{"aaa": 1, "zzz": 3}
	for name, n := range pkgs {
		dir := filepath.Join(base, name, "lib")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, name, "pubspec.yaml"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))+".dart"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfgBody := `
mutation:
  target_roots: [.]
  file_patterns: ["lib/**/*.dart"]
  tests:
    - pattern: "lib/**"
      tests: [t]
  commands: [x]
`
	c := loadConfig(t, cfgBody)
	c.Mutation.TargetRoots = []string{base}

	got, err := Discover(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("packages = %d, want 2", len(got))
	}
	if filepath.Base(got[0].Dir) != "aaa" {
		t.Errorf("discovery order: first = %s, want aaa", got[0].Name)
	}

	c.Scheduling.Order = "largest_first"
	got, err = Discover(c)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got[0].Dir) != "zzz" {
		t.Errorf("largest_first order: first = %s, want big", got[0].Name)
	}
}
