// Command mutant orchestrates mutation testing (or any file-mutation
// based test runner) across a workspace of libs/packages, driven entirely
// by a YAML config (D1).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GoLens-Project/golens-mutant/internal/config"
	"github.com/GoLens-Project/golens-mutant/internal/discover"
	"github.com/GoLens-Project/golens-mutant/internal/monitor"
	"github.com/GoLens-Project/golens-mutant/internal/report"
	"github.com/GoLens-Project/golens-mutant/internal/scheduler"
	"github.com/GoLens-Project/golens-mutant/internal/workspace"
)

// version is stamped at build time via
// -ldflags "-X main.version=…" (D17).
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mutant:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("mutant", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to the YAML configuration file")
	genExample := fs.String("generate-config-example", "", "write a fully documented config example to this path and exit")
	dryRun := fs.Bool("dry-run", false, "print the session plan (packages, test mappings, commands) without running anything")
	showVersion := fs.Bool("version", false, "print version and exit")
	var exemptPatterns []string
	fs.Func("exempt", "glob pattern of package name(s) to skip entirely, e.g. --exempt 'libs/generated/**' (repeatable)",
		func(v string) error {
			exemptPatterns = append(exemptPatterns, v)
			return nil
		})
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	if *showVersion {
		fmt.Println("mutant", version)
		return nil
	}
	if *genExample != "" {
		return os.WriteFile(*genExample, []byte(config.Example), 0o644)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Ctrl-C / SIGTERM: stop between files; per-file resume state is
	// already persisted (D8), so the next run picks up here.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pkgs, err := discover.Discover(cfg)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	if len(pkgs) == 0 {
		return fmt.Errorf("no packages found under %v (looked for %q markers)",
			cfg.Mutation.TargetRoots, cfg.Mutation.PackageMarker)
	}
	if unmatched := discover.MarkExempt(pkgs, exemptPatterns); len(unmatched) > 0 {
		fmt.Fprintf(os.Stderr, "mutant: warning: --exempt pattern(s) matched no packages: %s\n",
			strings.Join(unmatched, ", "))
	}

	mon := monitor.New(cfg)

	ws, err := workspace.NewManager(cfg)
	if err != nil {
		return err
	}

	// Dry run: read-only from here. Resume state is loaded through the
	// side-effect-free ReadResume (a store would create reports dirs or
	// the SQLite database), and nothing is executed or persisted.
	if *dryRun {
		var recorded map[string]map[string]string
		if cfg.ResumeEnabled() {
			recorded, err = report.ReadResume(cfg.Reports.Storage, cfg.Reports.Dir, cfg.SQLitePath())
			if err != nil {
				return fmt.Errorf("resume state: %w", err)
			}
		}
		fmt.Print(renderPlan(cfg, pkgs, ws, recorded))
		return nil
	}

	go mon.Run(ctx)

	var store report.Store
	if cfg.Reports.Storage == "sqlite" {
		store, err = report.NewSQLiteStore(cfg.SQLitePath(), cfg.Reports.Dir+"/logs")
	} else {
		store, err = report.NewJSONStore(cfg.Reports.Dir)
	}
	if err != nil {
		return err
	}
	defer store.Close()

	// Load resume state BEFORE building the scheduler: the scheduler
	// persists skip records when Run starts, and recording before this
	// load would clobber the previous session's results (F1).
	var resumed map[string]map[string]string
	if cfg.ResumeEnabled() {
		resumed, err = store.Resume()
		if err != nil {
			return fmt.Errorf("resume state: %w", err)
		}
	}

	// Exempted packages never reach the scheduler: no sandboxes, no
	// skip records, no resume entries — as if they were never discovered.
	runnable := pkgs[:0]
	for _, p := range pkgs {
		if !p.Exempt {
			runnable = append(runnable, p)
		}
	}
	if len(runnable) == 0 {
		return fmt.Errorf("all %d package(s) exempted by --exempt — nothing to run", len(pkgs))
	}

	// All terminal output shares one locked writer so live-streamed
	// command output never interleaves with notices or the progress bar.
	lw := &lockedWriter{w: os.Stderr}
	opts := scheduler.Options{
		Notify:   func(msg string) { fmt.Fprintf(lw, "\r\033[K· %s\n", msg) },
		Progress: renderBar(lw),
		FileLog:  logFileEvent(lw),
	}
	if cfg.ShowLogs() {
		opts.Stream = func(e scheduler.FileEvent) io.Writer {
			return newStreamSink(lw, e)
		}
	}
	sched, err := scheduler.New(cfg, mon, ws, store, runnable, opts)
	if err != nil {
		return err
	}
	if resumed != nil {
		sched.SkipDone(resumed)
	}

	started := time.Now()
	runErr := sched.Run(ctx)
	sum, err := store.Summarize(started, time.Now())
	if err != nil {
		// Report both: why the run stopped (if it did) and why no final
		// report could be written.
		return errors.Join(runErr, err)
	}
	fmt.Fprint(os.Stderr, "\r\033[K")
	fmt.Print(report.Render(sum))
	return runErr
}

// logFileEvent returns the per-file log callback: one line per started/
// finished file — timestamp, in-package progress, file path, state —
// printed above the progress bar.
func logFileEvent(w io.Writer) func(scheduler.FileEvent) {
	return func(e scheduler.FileEvent) {
		state := "started"
		if e.Finished {
			state = "finished"
		}
		fmt.Fprintf(w, "\r\033[K%s\n",
			filePrefix(e.Time, state, e.Done, e.Total, e.Package, e.File))
	}
}

// renderBar returns a progress callback drawing a simple ANSI bar with
// done/total counts (D12).
func renderBar(w io.Writer) func(done, total int) {
	return func(done, total int) {
		const width = 30
		filled := 0
		if total > 0 {
			filled = width * done / total
		}
		fmt.Fprintf(w, "\r\033[K[%s%s] %d/%d files",
			strings.Repeat("=", filled),
			strings.Repeat(" ", width-filled),
			done, total)
	}
}
