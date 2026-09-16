# golens-mutant

[![ci](https://github.com/GoLens-Project/golens-mutant/actions/workflows/ci.yml/badge.svg)](https://github.com/GoLens-Project/golens-mutant/actions/workflows/ci.yml)
[![coverage](https://raw.githubusercontent.com/GoLens-Project/golens-mutant/coverage/coverage.svg)](https://github.com/GoLens-Project/golens-mutant/actions/workflows/ci.yml)

`mutant` is a Go CLI that orchestrates mutation testing — or any
file-mutation-based test runner — across a workspace of libs and
packages. It is completely agnostic to the underlying tool: every command
it executes is defined as a template in a YAML config file.

It schedules work around your machine's live CPU and memory headroom,
isolates every package in a cached sandbox, kills or retries runaway
steps, persists per-file results as they complete, and can resume an
interrupted session exactly where it stopped.

## Install

```sh
go install github.com/GoLens-Project/golens-mutant/cmd/mutant@latest
```

Prebuilt binaries for linux/darwin/windows (amd64 + arm64) are attached
to each [release](https://github.com/GoLens-Project/golens-mutant/releases).

## Quick start

```sh
# 1. Generate a fully documented example config
mutant --generate-config-example config.yaml

# 2. Edit it: point target_roots at your workspace, wire your mutation
#    tool into mutation.commands, map files to their tests.

# 3. Preview what would run — no sandboxes, no commands, nothing written
mutant --config config.yaml --dry-run

# 4. Run
mutant --config config.yaml
```

The run prints a progress bar on stderr, a per-file log line — timestamp,
state, in-package progress, package and file path —

```
[2026-09-10T14:32:01+02:00] started 3/12 libs/core - lib/api/client.dart
[2026-09-10T14:32:04+02:00] finished 3/12 libs/core - lib/api/client.dart
```

With `reports.show_logs: true`, live command output is interleaved using
the same pattern, tagged with the file it belongs to:

```
[2026-09-10T14:32:02+02:00] started 3/12 libs/core - lib/api/client.dart 00:03 +1: All tests passed!
```

Notices for resource events (worker scale-down, package aborts) and a
human-readable summary close out the run:

```
Mutation run finished in 4m12s
  files:    128
  killed:   97
  survived: 22
  other:    4 timeout, 5 skipped
  mutation score: 81.5%
  libs/core                                  64 files, score 84.3%
  libs/ui                                    64 files, score 78.7%
```

## Dry run

`--dry-run` prints the full session plan and exits — the safe first step
for a new config, before any sandbox is built or command executed:

```sh
mutant --config config.yaml --dry-run
```

```
Dry run — 4 package(s), 41 runnable file(s), 7 skipped

  libs/core
    lib/api/client.dart            test/api_test.dart
    lib/api/mock.dart              test/api_test.dart
    lib/generated/proto.dart       skipped (no tests mapping)
    ...
  libs/network
    lib/net/http.dart              test/net_test.dart test/integ_test.dart
    lib/net/old.dart               test/net_test.dart [recorded: killed — resume skips]
    ...

  commands (example: libs/core/lib/api/client.dart):
    mutation_test -- /abs/.work/cache/libs__core/lib/api/client.dart
    flutter test --coverage test/api_test.dart
    bootstrap: flutter pub get in /abs/.work/cache/libs__core

  settings:
    max workers: 8 (gradual ramp-up)
    cooldown: 2s, scale_down_after: 30s
    timeout: 5m per command (continue)
    resume: true (1 file(s) already recorded)
    storage: json in reports
    sandboxes: /abs/.work/cache
```

What it tells you before anything runs:

- **Discovery** — every package and file that would be mutated
- **Test mappings** — which selectors each file's `{tests}` resolves to,
  and which files are skipped (no `mutation.tests` match) — the most
  common config mistake, visible before it wastes a run
- **Resume state** — files already recorded are marked with their prior
  result and excluded from the run
- **Real commands** — the interpolated chain (and bootstrap) for the
  first file that would execute, with actual sandbox paths
- **Effective settings** — worker ceiling, cooldown, timeout policy,
  storage backend, sandbox root

A dry run is strictly read-only: no monitor, no sandbox directories, no
report writes, and the SQLite backend (if configured) is opened
read-only — never created or migrated.

## How it works

1. **Discovery** — walks the configured target roots; every directory
   containing the package marker (`pubspec.yaml` by default) becomes a
   package, its files filtered through include/exclude globs and mapped
   to test selectors.
2. **Resource guard** — a monitor samples CPU (instant usage, or
   normalized load average) and free RAM. New work only starts while
   both are within your thresholds; work already in flight always
   finishes.
3. **Scheduling** — each work unit is one file mutation. Worker slots
   claim a package's sandbox exclusively, restore the target file
   pristine, run the command chain, classify the outcome, then observe a
   cooldown before taking more work. Ramp-up is gradual (or calculated),
   and sustained memory pressure scales worker capacity down — with
   recovery once resources free up. A timed-out command is asked to
   terminate first (SIGTERM) and hard-killed after the configurable
   `mutation.timeout.kill_grace` (10s default) — a cooperative engine
   can use that window to restore the file it was mutating.
4. **Sandbox cache** — the first slot to reach a package bootstraps it
   (source sync + your dependency command); later slots reuse the
   prepared copy. A step killed before its engine could restore the
   mutated file marks the sandbox dirty, so the next checkout re-syncs
   the whole package from source (verified by content, no re-bootstrap)
   — a leaked mutant can never poison a later step's sanity run.
5. **Reports** — every completed file is persisted immediately
   (JSON files, or SQLite) with raw command logs kept verbatim, so an
   interrupted run resumes with at most one file lost. Set
   `reports.show_logs: true` to also stream each command's output live
   to the terminal while it runs, prefixed with its package and file.

## Command templates

Macros interpolated at execution time:

| Macro | Expands to |
|---|---|
| `{file}` | package-relative path of the mutated file |
| `{abs_file}` | absolute path of the file inside its sandbox |
| `{tests}` | test selectors from the `mutation.tests` mapping |
| `{target_dir}` | absolute path of the package's sandbox |
| `{package}` | package name (e.g. `libs/core`) |

Results are classified from the command outcome: ordered
stdout/stderr regex rules first, then an exit-code map (single codes or
ranges like `"2-5"`), defaulting to `0 → survived`, anything else →
`killed`. Timeouts and per-process memory-budget kills are classified
automatically.

## Resuming

Resume is on by default. Per-file results are written after every
completed file; re-running the same config skips everything already
recorded:

```sh
mutant --config config.yaml   # interrupted…
mutant --config config.yaml   # …picks up where it stopped
```

Disable with `resume: {enabled: false}`.

## Exempting packages

`--exempt` skips whole packages entirely — no sandboxes, no commands,
no report or resume records — for ad-hoc runs without touching the config:

```sh
mutant --config config.yaml --exempt 'libs/generated/**' --exempt packages/legacy_proto
```

Patterns are globs matched against package names (`libs/core`,
`apps/libs/pa`, … — the same syntax as `mutation.file_exclude_patterns`);
the flag is repeatable. Exempted packages stay visible in `--dry-run`,
listed as `[exempt]` with their files marked `skipped (exempt)`, so you
can confirm a pattern matched what you intended. A pattern that matches
no package prints a warning — usually a typo.

## Selecting a package or file

`--package` and `--file` narrow a run without touching the config — for
one quick package or a single stubborn file:

```sh
mutant --config config.yaml --package libs/core
mutant --config config.yaml --file 'libs/core/lib/api/client.dart'
```

Both use the same glob syntax as `--exempt`, matched against package
names and `package/file` paths respectively (for `--file`, join the
package name from the log line or `--dry-run` heading with the
package-relative file path shown beneath it). Combine them to pin a file inside a
package; a selector that matches nothing is an error, and `--dry-run`
shows exactly what a selection would run.

## Development

```sh
make test        # full suite with the race detector
make cover       # coverage with a per-function report (coverage.txt)
make cover-html  # coverage as an HTML report in the browser
make ci          # fmt-check + vet + test, the same checks CI runs
make smoke       # build the CLI and run it against a toy workspace
make install     # go install ./cmd/mutant into GOBIN
```

Releases are automated and driven by **branch names**: PR branches must
be prefixed with a semver label — `patch/…`, `minor/…`, or `major/…`
(breaking). CI rejects unlabeled branches, and on merge the release
workflow reads the labels of every merged PR (highest wins), tags, and
publishes on every push to `master`. Direct pushes fall back to
conventional commit subjects (`fix:`, `feat:`, `feat!:` /
`BREAKING CHANGE`). Each release also commits the new version into
`internal/version/VERSION`, which the binary embeds — so `mutant
--version` reports the true release even for `go install` builds, which
run without ldflags stamping.
