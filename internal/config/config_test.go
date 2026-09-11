package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfig = `
mutation:
  target_roots: [libs]
  tests:
    - pattern: "lib/**"
      tests: ["test/all_test.dart"]
  commands:
    - "mutation_test -- {abs_file} && run_tests {tests}"
`

func TestLoadMinimal(t *testing.T) {
	c, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Mutation.Commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(c.Mutation.Commands))
	}
	// Defaults.
	if c.Mutation.PackageMarker != "pubspec.yaml" {
		t.Errorf("package_marker default = %q", c.Mutation.PackageMarker)
	}
	if c.Mutation.Timeout.OnTimeout != "continue" {
		t.Errorf("on_timeout default = %q", c.Mutation.Timeout.OnTimeout)
	}
	if c.Resources.CPUMetric != "instant" {
		t.Errorf("cpu_metric default = %q", c.Resources.CPUMetric)
	}
	if c.Cooldown() != 2*time.Second {
		t.Errorf("cooldown default = %v", c.Cooldown())
	}
	if c.Timeout() != 5*time.Minute {
		t.Errorf("timeout default = %v", c.Timeout())
	}
	if !c.ScaleDownEnabled() {
		t.Error("scale_down default = false, want true")
	}
	if !c.ResumeEnabled() {
		t.Error("resume default = false, want true")
	}
	if c.Scheduling.Order != "discovery" || c.Scheduling.RampUp != "gradual" {
		t.Errorf("scheduling defaults = %q/%q", c.Scheduling.Order, c.Scheduling.RampUp)
	}
	if c.Reports.Storage != "json" || c.Reports.Dir != "reports" {
		t.Errorf("reports defaults = %q/%q", c.Reports.Storage, c.Reports.Dir)
	}
}

func TestLoadExampleIsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(Example), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("example config invalid: %v", err)
	}
	if sel, ok := c.TestsFor("lib/src/core/foo.dart"); !ok || len(sel) != 1 || sel[0] != "test/core_test.dart" {
		t.Errorf("TestsFor(core) = %v, %v", sel, ok)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no roots", "mutation:\n  commands: [x]\n", "target_roots"},
		{"no commands", "mutation:\n  target_roots: [libs]\n", "commands"},
		{"bad fallback", minimalConfig + "\n  tests_fallback: sometimes\n", "tests_fallback"},
		{"all without all_tests", minimalConfig + "\n  tests_fallback: all\n", "all_tests"},
		{"bad cpu metric", minimalConfig + "\nresources:\n  cpu_metric: vibes\n", "cpu_metric"},
		{"bad ordering", minimalConfig + "\nscheduling:\n  order: random\n", "order"},
		{"bad storage", minimalConfig + "\nreports:\n  storage: csv\n", "storage"},
		{"bad exit key", minimalConfig + "\nresults:\n  by_exit_code:\n    x: killed\n", "by_exit_code"},
		{"bad result class", minimalConfig + "\nresults:\n  by_exit_code:\n    \"7\": vaporized\n", "result class"},
		{"tests empty pattern", `
mutation:
  target_roots: [libs]
  commands: [x]
  tests:
    - tests: [t]
`, "pattern is required"},
		{"tests no selectors", `
mutation:
  target_roots: [libs]
  commands: [x]
  tests:
    - pattern: "lib/**"
`, "selector"},
		{"rule empty pattern", minimalConfig + "\nresults:\n  rules:\n    - result: killed\n", "pattern is required"},
		{"rule bad regex", minimalConfig + `
results:
  rules:
    - pattern: "["
      result: killed
`, "bad pattern"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestInterpolate(t *testing.T) {
	got := Interpolate(`tool run {file} --tests {tests} --dir {target_dir} --pkg {package} --abs {abs_file} --keep {other}`, Vars{
		File:      "lib/a.dart",
		AbsFile:   "/w/lib/a.dart",
		Tests:     "t1 t2",
		TargetDir: "/w",
		Package:   "libs/foo",
	})
	want := `tool run lib/a.dart --tests t1 t2 --dir /w --pkg libs/foo --abs /w/lib/a.dart --keep {other}`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestTestsFor(t *testing.T) {
	c, err := Load(writeConfig(t, minimalConfig+`
  tests_fallback: all
  all_tests: "test"
`))
	if err != nil {
		t.Fatal(err)
	}
	if sel, ok := c.TestsFor("lib/x.dart"); !ok || sel[0] != "test/all_test.dart" {
		t.Errorf("mapped = %v %v", sel, ok)
	}
	if sel, ok := c.TestsFor("bin/x.dart"); !ok || sel[0] != "test" {
		t.Errorf("fallback = %v %v", sel, ok)
	}
}

func TestClassify(t *testing.T) {
	c, err := Load(writeConfig(t, minimalConfig+`
results:
  by_exit_code:
    "0": survived
    "1": killed
    "2-5": build_error
  rules:
    - pattern: "mutant survived"
      result: survived
`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		exit     int
		out      string
		timedOut bool
		oom      bool
		want     string
	}{
		{"exit0", 0, "", false, false, ResultSurvived},
		{"exit1", 1, "", false, false, ResultKilled},
		{"exit2", 2, "", false, false, ResultBuildError},
		{"exit5", 5, "", false, false, ResultBuildError},
		{"exit9-default", 9, "", false, false, ResultKilled},
		{"rule-wins", 1, "mutant survived", false, false, ResultSurvived},
		{"timeout", 1, "", true, false, ResultTimeout},
		{"oom", 1, "", false, true, ResultOOM},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Classify(tc.exit, tc.out, tc.timedOut, tc.oom); got != tc.want {
				t.Errorf("Classify(%d, %q, %v, %v) = %q, want %q",
					tc.exit, tc.out, tc.timedOut, tc.oom, got, tc.want)
			}
		})
	}
}

func TestGetters(t *testing.T) {
	c, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ScaleDownAfter(); got != 30*time.Second {
		t.Errorf("ScaleDownAfter default = %v, want 30s", got)
	}
	if got := c.SQLitePath(); got == "" {
		t.Error("SQLitePath default is empty")
	}
	c2, err := Load(writeConfig(t, minimalConfig+`
resources:
  scale_down_after: 5m
reports:
  sqlite_path: /tmp/x.db
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.ScaleDownAfter(); got != 5*time.Minute {
		t.Errorf("ScaleDownAfter = %v, want 5m", got)
	}
	if got := c2.SQLitePath(); got != "/tmp/x.db" {
		t.Errorf("SQLitePath = %q", got)
	}
}

func TestParseExitKey(t *testing.T) {
	cases := []struct {
		in      string
		lo, hi  int
		wantErr bool
	}{
		{"0", 0, 0, false},
		{"2-5", 2, 5, false},
		{"x", 0, 0, true},   // not numeric
		{"x-y", 0, 0, true}, // non-numeric range
		{"5-2", 0, 0, true}, // lo > hi
		{"-3", 0, 0, true},  // negative
		{"3-", 0, 0, true},  // dangling range
	}
	for _, tc := range cases {
		lo, hi, err := parseExitKey(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseExitKey(%q) unexpectedly ok (%d-%d)", tc.in, lo, hi)
			}
			continue
		}
		if err != nil || lo != tc.lo || hi != tc.hi {
			t.Errorf("parseExitKey(%q) = %d-%d, %v; want %d-%d", tc.in, lo, hi, err, tc.lo, tc.hi)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"2s": 2 * time.Second, "5m": 5 * time.Minute, "1h30m": 90 * time.Minute,
		"0": 0, "3": 3 * time.Second, "1.5s": 1500 * time.Millisecond,
	} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) unexpectedly ok", bad)
		}
	}
}
