# mutant: killed steps leak mutated files into the shared sandbox

Component: scheduling/kill path and sandbox restore
Version observed: mutant dev (2026-09-11 run)
Impact: 230 of 311 file results misclassified as build_error in a single session

## Summary

When a step is killed on timeout, mutation_test (and any engine that mutates
source files in place and restores them only on normal exit) never restores
its target file. The mutant stays in the package sandbox. mutant's
restore-before-run only restores the file the CURRENT step targets, so a
mutant leaked into any other file poisons every later step in that package:
their unmodified-code sanity run fails to compile, and they get classified
as build_error.

## Reproduction (real run, Arete UI)

Run: make mutant-ui, single package (ui), 311 files, config
ui/scripts/mutant.yaml. Engine command chain per file:

1. printf one-file XML (mutation_test step config)
2. mutation_test -b -f none {target_dir}/step.xml

Sequence of events:

1. Step for lib/app/shell_drawer.dart (211 mutations) hit the 30m timeout
   and was killed. The engine had mutated the file in place; no restore ran.
2. The sandbox copy was left containing a boolean-negation mutant:
   repo source:   label: (familyName != null && familyName.isNotEmpty)
   sandbox copy:  label: (!(familyName != null )&& familyName.isNotEmpty)
3. Every later step whose test selectors import the app shell (100 steps)
   failed its sanity run with a compile error in shell_drawer.dart and was
   recorded as build_error.
4. Same pattern for lib/features/planning/presentation/plan_view.dart
   (161 mutations, timed out, leaked !(reason != null )&&), poisoning
   another 106 steps.

Net: 206 of 230 build_error results were contamination, not real build
failures. The two contamination sources are themselves in the timeout list,
so their own verdicts are unknown.

Evidence available in the Arete repo under reports/:
- reports/packages/ui.json (per-file results)
- reports/logs/ui/*.log (raw engine output; the leaked mutants are visible
  verbatim in the compile errors of unrelated files' logs)

## Root cause analysis

- The engine cannot clean up after SIGKILL; only the orchestrator can.
- mutant treats the sandbox as clean after a kill because restoration is
  scoped to the next step's target file only.
- The fixed per-command timeout (30m) made kills routine rather than
  exceptional: 13 of 14 timeouts were large files (40-211 mutations at
  roughly 10-20s per suite run) that could not fit in 30m on an idle
  machine, so the buggy path fired early and repeatedly.

## Suggested fixes

1. On kill or timeout, mark the package sandbox dirty and restore the
   pristine content of EVERY file ever used as a mutation target in that
   package before the sandbox is reused (checksum-verify rather than
   assume). Cheaper alternative: re-sync the whole package.
2. Soften the kill: SIGTERM first with a grace period so a cooperative
   engine can restore, SIGKILL only after the grace period expires.
3. Scale the timeout by work size (e.g. mutation count discovered by the
   engine, or a per-file estimate) instead of one flat duration, so large
   files do not routinely hit the kill path.
4. Optional hardening: after any killed step, record which file was being
   mutated in the resume state so a later manual inspection can verify the
   sandbox healed.

## Related anomaly (separate, lower priority)

lib/features/virtues/presentation/widgets/virtue_score_tile.dart recorded a
timeout with only 4 mutations, implying a hung suite rather than slow
arithmetic; not a mutant defect, but worth a diagnostic hook to distinguish
"huge" from "stuck" timeouts in reports.
