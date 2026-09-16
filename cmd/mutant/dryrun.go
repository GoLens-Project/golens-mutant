package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/GoLens-Project/golens-mutant/internal/config"
	"github.com/GoLens-Project/golens-mutant/internal/discover"
	"github.com/GoLens-Project/golens-mutant/internal/workspace"
)

// renderPlan produces the --dry-run output: the full session plan —
// packages, per-file test mappings, skips, resume state, and the
// interpolated command chain — without executing or persisting anything.
func renderPlan(cfg *config.Config, pkgs []*discover.Package, ws *workspace.Manager, recorded map[string]map[string]string) string {
	var b strings.Builder
	runnable, skipped, alreadyDone, exempt := 0, 0, 0, 0
	for _, p := range pkgs {
		for _, f := range p.Files {
			switch {
			case p.Exempt:
				exempt++
				skipped++
			case !f.Run:
				skipped++
			default:
				runnable++
				if recorded[p.Name][f.Rel] != "" {
					alreadyDone++
				}
			}
		}
	}

	fmt.Fprintf(&b, "Dry run — %d package(s), %d runnable file(s), %d skipped",
		len(pkgs), runnable, skipped)
	if exempt > 0 {
		fmt.Fprintf(&b, " (%d exempt)", exempt)
	}
	fmt.Fprintf(&b, "\n\n")

	for _, p := range pkgs {
		if p.Exempt {
			fmt.Fprintf(&b, "  %s [exempt]\n", p.Name)
		} else {
			fmt.Fprintf(&b, "  %s\n", p.Name)
		}
		for _, f := range p.Files {
			switch {
			case p.Exempt:
				fmt.Fprintf(&b, "    %-50s skipped (exempt)\n", f.Rel)
			case !f.Run:
				fmt.Fprintf(&b, "    %-50s skipped (no tests mapping)\n", f.Rel)
			case recorded[p.Name][f.Rel] != "":
				fmt.Fprintf(&b, "    %-50s %s [recorded: %s — resume skips]\n",
					f.Rel, strings.Join(f.Tests, " "), recorded[p.Name][f.Rel])
			default:
				fmt.Fprintf(&b, "    %-50s %s\n", f.Rel, strings.Join(f.Tests, " "))
			}
		}
	}

	// Interpolated commands, using the first runnable file as the
	// example.
printed:
	for _, p := range pkgs {
		if p.Exempt {
			continue
		}
		for _, f := range p.Files {
			if !f.Run || recorded[p.Name][f.Rel] != "" {
				continue
			}
			targetDir := ws.SandboxPath(p.Name)
			vars := config.Vars{
				File:      f.Rel,
				AbsFile:   filepath.Join(targetDir, filepath.FromSlash(f.Rel)),
				Tests:     strings.Join(f.Tests, " "),
				TargetDir: targetDir,
				Package:   p.Name,
			}
			fmt.Fprintf(&b, "\n  commands (example: %s/%s):\n", p.Name, f.Rel)
			for _, tmpl := range cfg.Mutation.Commands {
				fmt.Fprintf(&b, "    %s\n", config.Interpolate(tmpl, vars))
			}
			if cfg.Workspace.Bootstrap != "" {
				bootVars := config.Vars{TargetDir: targetDir, Package: p.Name}
				fmt.Fprintf(&b, "    bootstrap: %s\n", config.Interpolate(cfg.Workspace.Bootstrap, bootVars))
			}
			break printed
		}
	}

	fmt.Fprintf(&b, "\n  settings:\n")
	fmt.Fprintf(&b, "    max workers: %d (%s ramp-up)\n", cfg.MaxWorkers(), cfg.Scheduling.RampUp)
	switch n := cfg.ConcurrentPackages(); {
	case n == 1:
		fmt.Fprintf(&b, "    packages: sequential (one at a time)\n")
	case n > 1:
		fmt.Fprintf(&b, "    packages: up to %d concurrent\n", n)
	default:
		fmt.Fprintf(&b, "    packages: unlimited concurrency\n")
	}
	fmt.Fprintf(&b, "    cooldown: %s, scale_down_after: %s\n",
		cfg.Resources.SpawnCooldown, cfg.Resources.ScaleDownAfter)
	fmt.Fprintf(&b, "    timeout: %s per command (%s)\n",
		cfg.Mutation.Timeout.Duration, cfg.Mutation.Timeout.OnTimeout)
	fmt.Fprintf(&b, "    resume: %v", cfg.ResumeEnabled())
	if cfg.ResumeEnabled() {
		fmt.Fprintf(&b, " (%d file(s) already recorded)", alreadyDone)
	}
	fmt.Fprintln(&b)
	if cfg.Scheduling.WorkersAfterCache {
		fmt.Fprintf(&b, "    workers_after_cache: on\n")
	}
	fmt.Fprintf(&b, "    storage: %s in %s\n", cfg.Reports.Storage, cfg.Reports.Dir)
	fmt.Fprintf(&b, "    sandboxes: %s\n", ws.CacheRoot())
	return b.String()
}
