// Package config loads, validates, and interpolates the YAML configuration
// that drives every aspect of the orchestrator. The CLI itself is agnostic
// to the underlying mutation engine: all commands it executes come from
// templates defined here (D1).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Result classes a mutation run can be classified into.
const (
	ResultKilled     = "killed"      // tests failed → mutant detected
	ResultSurvived   = "survived"    // tests passed → mutant undetected
	ResultBuildError = "build_error" // tool could not compile/run the target
	ResultTimeout    = "timeout"     // killed after the per-command timeout
	ResultOOM        = "oom"         // killed for exceeding the memory budget
	ResultError      = "error"       // could not run / classify
)

// Config is the root of the YAML configuration file.
type Config struct {
	Mutation   MutationConfig   `yaml:"mutation"`
	Resources  ResourcesConfig  `yaml:"resources"`
	Scheduling SchedulingConfig `yaml:"scheduling"`
	Workspace  WorkspaceConfig  `yaml:"workspace"`
	Reports    ReportsConfig    `yaml:"reports"`
	Results    ResultsConfig    `yaml:"results"`
	Resume     ResumeConfig     `yaml:"resume"`

	compileOnce   sync.Once
	compiledRules *compiledResults
}

// MutationConfig defines what to mutate and how to execute the toolchain.
// All commands are templates; see Interpolate for the macro variables.
type MutationConfig struct {
	// TargetRoots are directories scanned for packages (a package is any
	// directory containing the PackageMarker file).
	TargetRoots []string `yaml:"target_roots"`

	// PackageMarker names the file whose presence defines a package
	// boundary (e.g. "pubspec.yaml", "go.mod", "Cargo.toml").
	PackageMarker string `yaml:"package_marker"`

	// FilePatterns are globs (relative to each package) selecting the
	// files to mutate.
	FilePatterns []string `yaml:"file_patterns"`

	// FileExcludePatterns are globs rejecting files matched by
	// FilePatterns.
	FileExcludePatterns []string `yaml:"file_exclude_patterns"`

	// Tests maps file globs to test selectors (D13). The list is
	// ordered: the first pattern matching a file wins.
	Tests []TestMapping `yaml:"tests"`

	// TestsFallback: "skip" (default) — files without a mapping are
	// skipped; "all" — {tests} expands to the configured AllTests
	// selector.
	TestsFallback string `yaml:"tests_fallback"`

	// AllTests is the selector substituted for {tests} when
	// TestsFallback is "all" and a file has no explicit mapping.
	AllTests string `yaml:"all_tests"`

	// Commands run sequentially per file mutation, in order. Each entry
	// is "executable arg1 arg2 ..." with macro variables interpolated.
	Commands []string `yaml:"commands"`

	// Timeout applies to each command independently (D14).
	Timeout TimeoutConfig `yaml:"timeout"`
}

// TestMapping binds a file glob to test selectors (D13).
type TestMapping struct {
	// Pattern is a glob matched against the package-relative file path.
	Pattern string `yaml:"pattern"`
	// Tests is the selector list substituted for {tests} (joined by
	// spaces).
	Tests []string `yaml:"tests"`
}

// TimeoutConfig controls per-command timeout behavior (D14).
type TimeoutConfig struct {
	// Duration is a Go duration string (e.g. "5m"). Default "5m";
	// "0" disables the timeout.
	Duration string `yaml:"duration"`

	// OnTimeout: "continue" (default — record the file as timed out and
	// keep going) or "abort_package" (drop the package's remaining files;
	// resumable via D8).
	OnTimeout string `yaml:"on_timeout"`

	// KillGrace is how long a timed-out command's process group gets
	// between SIGTERM and SIGKILL, so a cooperative engine can restore
	// the file it was mutating. Default "10s" (Docker's stop grace);
	// "0" escalates immediately.
	KillGrace string `yaml:"kill_grace"`
}

// ResourcesConfig defines the dynamic resource guard thresholds (D2, D5).
type ResourcesConfig struct {
	// MinFreeRAMMB is the minimum free physical RAM, in megabytes,
	// required to spawn a new worker.
	MinFreeRAMMB int `yaml:"min_free_ram_mb"`

	// MaxCPUPercent is the ceiling for the CPU metric.
	MaxCPUPercent int `yaml:"max_cpu_percent"`

	// CPUMetric selects the CPU gauge: "instant" (default) or "load_avg"
	// (1-minute load average normalized by core count, expressed in
	// percent) (D5).
	CPUMetric string `yaml:"cpu_metric"`

	// MaxWorkers caps concurrent worker slots. Zero means
	// runtime.NumCPU() (D11).
	MaxWorkers int `yaml:"max_workers"`

	// MemoryBudgetPerStepMB, when non-zero, is a per-process RSS budget
	// in megabytes: a worker process exceeding it is killed and its file
	// recorded as "oom".
	MemoryBudgetPerStepMB int `yaml:"memory_budget_per_step_mb"`

	// SpawnCooldown is the pause after a slot completes a file before
	// re-checking resources (D3). Defaults to "2s".
	SpawnCooldown string `yaml:"spawn_cooldown"`

	// ScaleDown enables dynamic worker-capacity scale-down under
	// sustained low memory (D7). Default: enabled.
	ScaleDown *bool `yaml:"scale_down"`

	// ScaleDownAfter is how long the resource gate must stay closed
	// before worker capacity is scaled down. Defaults to "30s".
	ScaleDownAfter string `yaml:"scale_down_after"`
}

// SchedulingConfig controls queue ordering and worker ramp-up.
type SchedulingConfig struct {
	// Order: "discovery" (default, D10) or "largest_first" (D6).
	Order string `yaml:"order"`

	// RampUp: "gradual" (default — one new worker per completion cycle,
	// gated by resources) or "calculated" (start all workers up front)
	// (D15).
	RampUp string `yaml:"ramp_up"`

	// WorkersAfterCache: when true, no new workers spawn until every
	// package's sandbox has been bootstrapped once (D16).
	WorkersAfterCache bool `yaml:"workers_after_cache"`

	// ConcurrentPackages caps how many packages may be in flight at
	// once. Nil (unset) means 1: packages run strictly sequentially —
	// every file of a package completes before the next package starts.
	// 0 lifts the limit entirely.
	ConcurrentPackages *int `yaml:"concurrent_packages"`
}

// WorkspaceConfig defines the sandbox engine settings (D16).
type WorkspaceConfig struct {
	// BaseDir is the root holding sandboxes and the package cache.
	BaseDir string `yaml:"base_dir"`

	// CacheDir holds prepared per-package sandboxes, relative to BaseDir.
	CacheDir string `yaml:"cache_dir"`

	// Bootstrap is the dependency/setup command run once per package to
	// prepare its sandbox (e.g. "flutter pub get"). Templates may use
	// {target_dir}.
	Bootstrap string `yaml:"bootstrap"`

	// SyncExcludePatterns are glob patterns skipped when copying source
	// files into sandboxes (matched against package-relative paths;
	// a pattern applies to any path segment it matches).
	SyncExcludePatterns []string `yaml:"sync_exclude_patterns"`
}

// ReportsConfig controls output destinations and storage (D9).
type ReportsConfig struct {
	// Dir holds per-package JSON reports, the aggregated summary, and
	// raw logs.
	Dir string `yaml:"dir"`

	// Storage: "json" (default) or "sqlite" (D9).
	Storage string `yaml:"storage"`

	// SQLitePath overrides the default SQLite location
	// (~/.config/golens-mutant/mutant.db).
	SQLitePath string `yaml:"sqlite_path"`

	// ShowLogs live-streams each command's combined stdout/stderr to the
	// terminal while it runs (prefixed per file), in addition to the
	// buffered per-file logs under Dir/logs. Default: false.
	ShowLogs bool `yaml:"show_logs"`
}

// ResultsConfig maps raw command outcomes to mutation-result classes.
// This is the tool-specific knowledge the otherwise agnostic CLI needs.
type ResultsConfig struct {
	// ByExitCode maps an exit code to a result class. Keys may be a
	// single code ("0", "1") or a range ("2-5"). Unlisted codes fall
	// back to the default: 0 → survived, anything else → killed.
	ByExitCode map[string]string `yaml:"by_exit_code"`

	// Rules are ordered combined-output (stdout+stderr) matchers
	// evaluated before ByExitCode: the first matching rule wins.
	Rules []ResultRule `yaml:"rules"`
}

// ResultRule classifies a run whose combined output matches Pattern.
type ResultRule struct {
	// Pattern is a regular expression matched against combined
	// stdout+stderr.
	Pattern string `yaml:"pattern"`

	// Result is the class assigned on match (see the Result* constants).
	Result string `yaml:"result"`
}

// ResumeConfig controls interrupted-session recovery (D8).
type ResumeConfig struct {
	// Enabled: persist per-file state after every completed file so an
	// interrupted run picks up where it stopped. Default: true.
	Enabled *bool `yaml:"enabled"`
}

// exitRange is one parsed by_exit_code key with its result class.
type exitRange struct {
	lo, hi int
	class  string
}

// compiledResults caches the parsed classification rules so Classify is
// deterministic (ranges sorted, not map order) and regexes compile once.
type compiledResults struct {
	ranges []exitRange // sorted by lo, then hi
	rules  []compiledRule
}

type compiledRule struct {
	re     *regexp.Regexp
	result string
}

// classification returns the compiled rules, building them on first use.
func (c *Config) classification() *compiledResults {
	c.compileOnce.Do(func() {
		cr := &compiledResults{}
		for key, class := range c.Results.ByExitCode {
			if lo, hi, err := parseExitKey(key); err == nil {
				cr.ranges = append(cr.ranges, exitRange{lo, hi, class})
			}
		}
		sort.Slice(cr.ranges, func(i, j int) bool {
			if cr.ranges[i].lo != cr.ranges[j].lo {
				return cr.ranges[i].lo < cr.ranges[j].lo
			}
			return cr.ranges[i].hi < cr.ranges[j].hi
		})
		for _, rule := range c.Results.Rules {
			if re, err := regexp.Compile(rule.Pattern); err == nil {
				cr.rules = append(cr.rules, compiledRule{re, rule.Result})
			}
		}
		c.compiledRules = cr
	})
	return c.compiledRules
}

// Load reads, parses, applies defaults to, and validates the config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ApplyDefaults fills every unset field with its documented default.
func (c *Config) ApplyDefaults() {
	m := &c.Mutation
	if m.PackageMarker == "" {
		m.PackageMarker = "pubspec.yaml"
	}
	if len(m.FilePatterns) == 0 {
		m.FilePatterns = []string{"lib/**/*.dart"}
	}
	if m.TestsFallback == "" {
		m.TestsFallback = "skip"
	}
	if m.Timeout.Duration == "" {
		m.Timeout.Duration = "5m"
	}
	if m.Timeout.OnTimeout == "" {
		m.Timeout.OnTimeout = "continue"
	}
	if m.Timeout.KillGrace == "" {
		m.Timeout.KillGrace = "10s"
	}
	r := &c.Resources
	if r.CPUMetric == "" {
		r.CPUMetric = "instant"
	}
	if r.SpawnCooldown == "" {
		r.SpawnCooldown = "2s"
	}
	if r.ScaleDownAfter == "" {
		r.ScaleDownAfter = "30s"
	}
	if r.ScaleDown == nil {
		t := true
		r.ScaleDown = &t
	}
	if c.Scheduling.Order == "" {
		c.Scheduling.Order = "discovery"
	}
	if c.Scheduling.RampUp == "" {
		c.Scheduling.RampUp = "gradual"
	}
	w := &c.Workspace
	if w.BaseDir == "" {
		w.BaseDir = ".work"
	}
	if w.CacheDir == "" {
		w.CacheDir = "cache"
	}
	rep := &c.Reports
	if rep.Dir == "" {
		rep.Dir = "reports"
	}
	if rep.Storage == "" {
		rep.Storage = "json"
	}
	if c.Resume.Enabled == nil {
		t := true
		c.Resume.Enabled = &t
	}
}

// Validate reports any structurally invalid configuration.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if len(c.Mutation.TargetRoots) == 0 {
		add("mutation.target_roots: at least one target root is required")
	}
	if len(c.Mutation.Commands) == 0 {
		add("mutation.commands: at least one command template is required")
	}
	switch c.Mutation.TestsFallback {
	case "skip", "all":
	default:
		add(`mutation.tests_fallback: must be "skip" or "all" (got %q)`, c.Mutation.TestsFallback)
	}
	if c.Mutation.TestsFallback == "all" && c.Mutation.AllTests == "" {
		add(`mutation.all_tests: required when tests_fallback is "all"`)
	}
	switch c.Mutation.Timeout.OnTimeout {
	case "continue", "abort_package":
	default:
		add(`mutation.timeout.on_timeout: must be "continue" or "abort_package" (got %q)`, c.Mutation.Timeout.OnTimeout)
	}
	if _, err := ParseDuration(c.Mutation.Timeout.Duration); err != nil {
		add("mutation.timeout.duration: %v", err)
	}
	if _, err := ParseDuration(c.Mutation.Timeout.KillGrace); err != nil {
		add("mutation.timeout.kill_grace: %v", err)
	}
	switch c.Resources.CPUMetric {
	case "instant", "load_avg":
	default:
		add(`resources.cpu_metric: must be "instant" or "load_avg" (got %q)`, c.Resources.CPUMetric)
	}
	if _, err := ParseDuration(c.Resources.SpawnCooldown); err != nil {
		add("resources.spawn_cooldown: %v", err)
	}
	if _, err := ParseDuration(c.Resources.ScaleDownAfter); err != nil {
		add("resources.scale_down_after: %v", err)
	}
	switch c.Scheduling.Order {
	case "discovery", "largest_first":
	default:
		add(`scheduling.order: must be "discovery" or "largest_first" (got %q)`, c.Scheduling.Order)
	}
	switch c.Scheduling.RampUp {
	case "gradual", "calculated":
	default:
		add(`scheduling.ramp_up: must be "gradual" or "calculated" (got %q)`, c.Scheduling.RampUp)
	}
	if p := c.Scheduling.ConcurrentPackages; p != nil && *p < 0 {
		add(`scheduling.concurrent_packages: must be >= 0 (0 = no limit)`)
	}
	if s := c.Reports.Storage; s != "json" && s != "sqlite" {
		add(`reports.storage: must be "json" or "sqlite" (got %q)`, s)
	}
	for i, tm := range c.Mutation.Tests {
		if tm.Pattern == "" {
			add("mutation.tests[%d]: pattern is required", i)
		}
		if len(tm.Tests) == 0 {
			add("mutation.tests[%d]: at least one test selector is required", i)
		}
	}
	for key, class := range c.Results.ByExitCode {
		if _, _, err := parseExitKey(key); err != nil {
			add("results.by_exit_code: %v", err)
		}
		if !ValidResult(class) {
			add("results.by_exit_code: unknown result class %q", class)
		}
	}
	for i, rule := range c.Results.Rules {
		if rule.Pattern == "" {
			add("results.rules[%d]: pattern is required", i)
		} else if _, err := regexp.Compile(rule.Pattern); err != nil {
			add("results.rules[%d]: bad pattern: %v", i, err)
		}
		if !ValidResult(rule.Result) {
			add("results.rules[%d]: unknown result class %q", i, rule.Result)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// ValidResult reports whether class is a known result class.
func ValidResult(class string) bool {
	switch class {
	case ResultKilled, ResultSurvived, ResultBuildError, ResultTimeout, ResultOOM, ResultError:
		return true
	}
	return false
}

// Timeout returns the parsed per-command timeout (zero disables it).
func (c *Config) Timeout() time.Duration {
	d, _ := ParseDuration(c.Mutation.Timeout.Duration)
	return d
}

// KillGrace returns the parsed SIGTERM-to-SIGKILL grace period for
// timed-out commands (zero escalates immediately).
func (c *Config) KillGrace() time.Duration {
	d, _ := ParseDuration(c.Mutation.Timeout.KillGrace)
	return d
}

// Cooldown returns the parsed worker spawn cooldown.
func (c *Config) Cooldown() time.Duration {
	d, _ := ParseDuration(c.Resources.SpawnCooldown)
	return d
}

// ScaleDownAfter returns how long sustained pressure must last before
// worker capacity is reduced (D7).
func (c *Config) ScaleDownAfter() time.Duration {
	d, _ := ParseDuration(c.Resources.ScaleDownAfter)
	return d
}

// MaxWorkers returns the effective worker ceiling: the configured value,
// or runtime.NumCPU() when unset (D11).
func (c *Config) MaxWorkers() int {
	if w := c.Resources.MaxWorkers; w > 0 {
		return w
	}
	return runtime.NumCPU()
}

// ScaleDownEnabled reports whether dynamic scale-down is on (D7).
func (c *Config) ScaleDownEnabled() bool { return *c.Resources.ScaleDown }

// ConcurrentPackages returns the package-concurrency limit. The default
// (unset) is 1 — packages run strictly one at a time; 0 means no limit.
func (c *Config) ConcurrentPackages() int {
	if p := c.Scheduling.ConcurrentPackages; p != nil {
		return *p
	}
	return 1
}

// ResumeEnabled reports whether resume state is persisted (D8).
func (c *Config) ResumeEnabled() bool { return *c.Resume.Enabled }

// ShowLogs reports whether command output is streamed live to the
// terminal while it runs (D9).
func (c *Config) ShowLogs() bool { return c.Reports.ShowLogs }

// SQLitePath returns the configured or default SQLite database path (D9).
func (c *Config) SQLitePath() string {
	if c.Reports.SQLitePath != "" {
		return c.Reports.SQLitePath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "golens-mutant.db"
	}
	return filepath.Join(home, ".config", "golens-mutant", "mutant.db")
}

// TestsFor resolves the {tests} selectors for a package-relative file
// (D13). It returns the selectors and whether the file should run at all.
func (c *Config) TestsFor(file string) ([]string, bool) {
	for _, tm := range c.Mutation.Tests {
		if MatchGlob(tm.Pattern, file) {
			return tm.Tests, true
		}
	}
	if c.Mutation.TestsFallback == "all" {
		return []string{c.Mutation.AllTests}, true
	}
	return nil, false
}

// Classify maps a command outcome to a result class. Output rules run
// first (in configured order); exit-code ranges are then evaluated
// deterministically (sorted by range start, not map iteration order);
// unlisted codes default to 0 → survived, anything else → killed.
func (c *Config) Classify(exitCode int, output string, timedOut, oomKilled bool) string {
	if timedOut {
		return ResultTimeout
	}
	if oomKilled {
		return ResultOOM
	}
	cr := c.classification()
	for _, rule := range cr.rules {
		if rule.re.MatchString(output) {
			return rule.result
		}
	}
	for _, r := range cr.ranges {
		if exitCode >= r.lo && exitCode <= r.hi {
			return r.class
		}
	}
	if exitCode == 0 {
		return ResultSurvived
	}
	return ResultKilled
}

func parseExitKey(key string) (lo, hi int, err error) {
	if i := strings.IndexByte(key, '-'); i > 0 {
		if _, err = fmt.Sscanf(key, "%d-%d", &lo, &hi); err != nil {
			return 0, 0, fmt.Errorf("bad exit-code key %q", key)
		}
	} else if _, err = fmt.Sscanf(key, "%d", &lo); err != nil {
		return 0, 0, fmt.Errorf("bad exit-code key %q", key)
	} else {
		hi = lo
	}
	if lo > hi || lo < 0 {
		return 0, 0, fmt.Errorf("bad exit-code range %q", key)
	}
	return lo, hi, nil
}

// ParseDuration parses a Go duration string ("2s", "5m", "1h30m"); a bare
// number is read as seconds.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	var secs float64
	if _, err := fmt.Sscanf(s, "%g", &secs); err != nil {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// Vars carries the per-file macro values interpolated into command
// templates at execution time.
type Vars struct {
	File      string // package-relative path of the mutated file
	AbsFile   string // absolute path of the file inside the sandbox
	Tests     string // space-joined selectors from the Tests mapping
	TargetDir string // absolute package sandbox directory
	Package   string // package name (target-root-relative directory)
}

// Interpolate expands the macro variables in a command template. Anything
// that is not a known macro is left untouched so shell syntax, flags, and
// tool-specific placeholders survive.
func Interpolate(tmpl string, v Vars) string {
	return strings.NewReplacer(
		"{file}", v.File,
		"{abs_file}", v.AbsFile,
		"{tests}", v.Tests,
		"{target_dir}", v.TargetDir,
		"{package}", v.Package,
	).Replace(tmpl)
}
