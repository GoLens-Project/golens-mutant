// Package scheduler orchestrates worker slots over the package queue:
// each work unit is one file mutation. A slot claims its package's
// sandbox exclusively (D16), restores the file pristine, runs the
// configured command chain, classifies the outcome, records it, cools
// down (D3), and only takes more work while the resource guard allows
// (D2, D4).
package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoLens-Project/golens-mutant/internal/config"
	"github.com/GoLens-Project/golens-mutant/internal/discover"
	"github.com/GoLens-Project/golens-mutant/internal/monitor"
	"github.com/GoLens-Project/golens-mutant/internal/report"
	"github.com/GoLens-Project/golens-mutant/internal/workspace"
)

// Options carries the scheduler's output hooks (progress bar, notices).
type Options struct {
	// Notify receives human-readable events (scale-down, ramp-up).
	Notify func(msg string)

	// Progress is called after every completed file.
	Progress func(done, total int)

	// FileLog, when set, receives a started event when a file's command
	// chain begins and a finished event when its result is recorded.
	FileLog func(e FileEvent)
}

// FileEvent is one per-file execution log line.
type FileEvent struct {
	Time     time.Time
	Package  string // package name ({package} macro)
	File     string // package-relative path ({file} macro)
	Done     int    // files resolved in this package, including this one on finish
	Total    int    // files this package will run this session
	Finished bool
}

// Scheduler runs one mutation session.
type Scheduler struct {
	cfg   *config.Config
	mon   *monitor.Monitor
	ws    *workspace.Manager
	store report.Store
	opt   Options

	mu     sync.Mutex
	states []*pkgState // in queue order

	// skipped are unmapped files to report as "skipped"; they are
	// persisted only at Run start, after the caller has loaded resume
	// state (F1: recording in New would clobber it).
	skipped []report.FileResult

	// resolved marks files that already carry a recorded result (from a
	// previous session), so their earlier class is never overwritten by
	// a fresh skip record (F1).
	resolved map[string]bool

	// total counts every runnable file in the session's queue, including
	// ones already recorded by earlier sessions; done counts files
	// resolved this session (executed or resumed), so done reaches total.
	total int
	done  int

	capacity int // current worker ceiling (scale-down lowers it)
	maxCap   int // configured ceiling (recovery target, D7)
	active   int // live workers
}

type pkgState struct {
	pkg     *discover.Package
	pending []discover.File
	aborted bool

	// total/done drive the per-package x/y of FileEvent log lines. A
	// package's sandbox is exclusive, so its files resolve one at a time
	// and done counts up in queue order.
	total int
	done  int
}

// New prepares a scheduler. It does not persist anything: unmapped-file
// skips are recorded at Run start, after the caller has loaded resume
// state and called SkipDone (D8, F1).
func New(cfg *config.Config, mon *monitor.Monitor, ws *workspace.Manager, store report.Store, pkgs []*discover.Package, opt Options) (*Scheduler, error) {
	maxWorkers := cfg.MaxWorkers()
	s := &Scheduler{
		cfg:      cfg,
		mon:      mon,
		ws:       ws,
		store:    store,
		opt:      opt,
		states:   make([]*pkgState, 0, len(pkgs)),
		resolved: map[string]bool{},
		capacity: maxWorkers,
		maxCap:   maxWorkers,
	}
	for _, p := range pkgs {
		st := &pkgState{pkg: p}
		for _, f := range p.Files {
			if !f.Run {
				// Unmapped files are reported as skipped, never run.
				s.skipped = append(s.skipped, report.FileResult{
					Package: p.Name, File: f.Rel, Result: "skipped",
					StartedAt: time.Now(),
				})
				continue
			}
			st.pending = append(st.pending, f)
			s.total++
		}
		st.total = len(st.pending)
		s.states = append(s.states, st)
	}
	return s, nil
}

// Total reports how many files this session expects to run.
func (s *Scheduler) Total() int { return s.total }

// SkipDone removes files already recorded by a previous session (D8) and
// marks them resolved so their recorded class is never overwritten.
func (s *Scheduler) SkipDone(done map[string]map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Every recorded file keeps its earlier result — including unmapped
	// files whose fresh skip record would otherwise overwrite it.
	for pkg, files := range done {
		for file := range files {
			s.resolved[pkg+"\x00"+file] = true
		}
	}
	for _, st := range s.states {
		kept := st.pending[:0]
		for _, f := range st.pending {
			if done[st.pkg.Name][f.Rel] != "" {
				s.done++
			} else {
				kept = append(kept, f)
			}
		}
		st.pending = kept
		st.total = len(kept)
	}
}

// Run drives the session to completion: workers are spawned per the
// ramp-up mode (D15), gated by the resource guard (D2) and the sandbox
// cache setting (D16), with scale-down under sustained pressure (D7) and
// recovery once headroom returns.
func (s *Scheduler) Run(ctx context.Context) error {
	// Persist unmapped-file skips now — after resume state was loaded —
	// so earlier results survive (F1). Files that already carry a result
	// from a previous session keep it.
	for _, r := range s.skipped {
		if s.resolved[r.Package+"\x00"+r.File] {
			continue
		}
		if err := s.store.Record(r); err != nil {
			return fmt.Errorf("record skips: %w", err)
		}
	}

	var wg sync.WaitGroup
	spawn := func() {
		s.mu.Lock()
		s.active++
		s.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				s.mu.Lock()
				s.active--
				s.mu.Unlock()
			}()
			s.worker(ctx)
		}()
	}

	initial := 1
	if s.cfg.Scheduling.RampUp == "calculated" {
		s.mu.Lock()
		initial = s.capacity
		s.mu.Unlock()
	}
	for range initial {
		spawn()
	}

	// Controller: gradual ramp-up, scale-down, and recovery. Capacity
	// steps are throttled to one per scale_down_after interval: sustained
	// pressure degrades capacity a notch at a time instead of collapsing
	// it to one worker within a few ticks of the threshold crossing.
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	stepEvery := s.cfg.ScaleDownAfter()
	var lastStep time.Time
	for {
		if ctx.Err() != nil {
			break
		}
		s.mu.Lock()
		finished := s.pendingCountLocked() == 0
		s.mu.Unlock()
		if finished {
			break
		}

		now := time.Now()
		closed := s.mon.ClosedFor()
		if s.cfg.ScaleDownEnabled() && closed >= stepEvery && now.Sub(lastStep) >= stepEvery {
			s.mu.Lock()
			if s.capacity > 1 {
				s.capacity--
				s.notifyf("resource pressure %.0fs: scaling worker capacity down to %d", closed.Seconds(), s.capacity)
			}
			s.mu.Unlock()
			lastStep = now
		} else if open := s.mon.OpenFor(); s.cfg.ScaleDownEnabled() && s.capacity < s.maxCap && open >= stepEvery && now.Sub(lastStep) >= stepEvery {
			// Recovery (D7): sustained headroom earns one capacity step
			// back toward the configured ceiling.
			s.mu.Lock()
			if s.capacity < s.maxCap {
				s.capacity++
				s.notifyf("resources recovered (%.0fs headroom): scaling worker capacity up to %d", open.Seconds(), s.capacity)
			}
			s.mu.Unlock()
			lastStep = now
		} else if s.mon.Allowed() {
			// Ramp-up / recovery: one new worker per tick while the
			// gate is open (D15 gradual; recovery after D7 scale-down).
			s.mu.Lock()
			underCap := s.active < s.capacity
			s.mu.Unlock()
			if underCap && s.cacheReadyForMoreWorkers() {
				spawn()
			}
		}

		<-tick.C
	}

	wg.Wait()
	return ctx.Err()
}

// cacheReadyForMoreWorkers enforces workers_after_cache (D16): beyond
// the first worker, new spawns wait until every package is bootstrapped
// or otherwise resolved — no pending files (all resumed), aborted, or
// already prepared (F8: counting only bootstraps could never be
// satisfied under resume).
func (s *Scheduler) cacheReadyForMoreWorkers() bool {
	if !s.cfg.Scheduling.WorkersAfterCache {
		return true
	}
	s.mu.Lock()
	active := s.active
	resolved := 0
	for _, st := range s.states {
		if st.aborted || len(st.pending) == 0 || s.ws.IsPrepared(st.pkg.Name) {
			resolved++
		}
	}
	total := len(s.states)
	s.mu.Unlock()
	if active <= 1 {
		return true
	}
	return resolved >= total
}

// fileStarted emits the per-file started log event for a claimed file
// (x = files resolved so far + this one).
func (s *Scheduler) fileStarted(st *pkgState, file string) {
	if s.opt.FileLog == nil {
		return
	}
	s.mu.Lock()
	x, y := st.done+1, st.total
	s.mu.Unlock()
	s.opt.FileLog(FileEvent{Time: time.Now(), Package: st.pkg.Name, File: file, Done: x, Total: y})
}

// fileFinished counts the file toward its package and emits the finished
// log event.
func (s *Scheduler) fileFinished(st *pkgState, file string) {
	s.mu.Lock()
	st.done++
	x, y := st.done, st.total
	s.mu.Unlock()
	if s.opt.FileLog == nil {
		return
	}
	s.opt.FileLog(FileEvent{Time: time.Now(), Package: st.pkg.Name, File: file, Done: x, Total: y, Finished: true})
}

// notifyf emits an event line if a Notify hook is set.
func (s *Scheduler) notifyf(format string, args ...any) string {
	msg := fmt.Sprintf(format, args...)
	if s.opt.Notify != nil {
		s.opt.Notify(msg)
	}
	return msg
}

func (s *Scheduler) pendingCountLocked() int {
	n := 0
	for _, st := range s.states {
		if !st.aborted {
			n += len(st.pending)
		}
	}
	return n
}

// worker is one slot's main loop.
func (s *Scheduler) worker(ctx context.Context) {
	ranOne := false
	for {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		if s.active > s.capacity {
			s.mu.Unlock()
			return // scaled down (D7)
		}
		s.mu.Unlock()

		st, sbx, file, ok := s.nextWork(ctx)
		if !ok {
			s.mu.Lock()
			more := s.pendingCountLocked() > 0
			s.mu.Unlock()
			if !more {
				return
			}
			// Work exists but every eligible sandbox is busy (D16):
			// back off briefly.
			if !sleepCtx(ctx, 50*time.Millisecond) {
				return
			}
			continue
		}

		// Cooldown after the previous file, then gate on resources
		// (D3, D4): the current file always finishes; only the next
		// claim waits.
		if ranOne {
			if !sleepCtx(ctx, s.cfg.Cooldown()) {
				sbx.Release()
				s.requeue(st, file)
				return
			}
		}
		for !s.mon.Allowed() {
			if !sleepCtx(ctx, 200*time.Millisecond) {
				sbx.Release()
				s.requeue(st, file)
				return
			}
		}

		s.runFile(ctx, st, sbx, file)
		sbx.Release()
		ranOne = true
	}
}

// requeue puts an unclaimed file back at the head of its package.
func (s *Scheduler) requeue(st *pkgState, file discover.File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.pending = append([]discover.File{file}, st.pending...)
}

// nextWork claims the next runnable file whose package sandbox is free
// (D16), preserving queue order. ok=false means nothing is claimable
// right now. ctx bounds bootstrap: a canceled session never records a
// spurious error for a half-prepared sandbox (F2).
func (s *Scheduler) nextWork(ctx context.Context) (*pkgState, *workspace.Sandbox, discover.File, bool) {
	s.mu.Lock()
	snapshot := make([]*pkgState, len(s.states))
	copy(snapshot, s.states)
	s.mu.Unlock()

	for _, st := range snapshot {
		if ctx.Err() != nil {
			return nil, nil, discover.File{}, false
		}
		s.mu.Lock()
		if st.aborted || len(st.pending) == 0 {
			s.mu.Unlock()
			continue
		}
		file := st.pending[0]
		s.mu.Unlock()

		sbx, ok, err := s.ws.TryAcquire(ctx, st.pkg)
		if err != nil {
			if ctx.Err() != nil {
				// Session canceled mid-bootstrap: leave the file
				// unrecorded so resume re-runs it (D21).
				return nil, nil, discover.File{}, false
			}
			// Bootstrap/sync failure: record as error and drop the file —
			// but only if the head re-check confirms this worker still
			// owns the pop. If another worker claimed the same head in
			// between, that worker owns the file now; recording here
			// would double-count it (done > total).
			s.mu.Lock()
			if len(st.pending) == 0 || st.pending[0].Rel != file.Rel {
				s.mu.Unlock()
				continue
			}
			st.pending = st.pending[1:]
			s.mu.Unlock()
			s.record(report.FileResult{
				Package: st.pkg.Name, File: file.Rel, Result: config.ResultError,
				StartedAt: time.Now(),
			})
			s.fileFinished(st, file.Rel)
			s.notifyf("package %s: sandbox preparation failed: %v", st.pkg.Name, err)
			continue
		}
		if !ok {
			continue // sandbox busy; try the next package
		}

		s.mu.Lock()
		if st.aborted || len(st.pending) == 0 || st.pending[0].Rel != file.Rel {
			// State changed while acquiring; put it back.
			s.mu.Unlock()
			sbx.Release()
			s.requeue(st, file)
			continue
		}
		st.pending = st.pending[1:]
		s.mu.Unlock()
		return st, sbx, file, true
	}
	return nil, nil, discover.File{}, false
}

// runFile restores the file pristine, runs the command chain, classifies
// and records the outcome (D14 abort-package included).
func (s *Scheduler) runFile(ctx context.Context, st *pkgState, sbx *workspace.Sandbox, file discover.File) {
	start := time.Now()
	s.fileStarted(st, file.Rel)
	if err := sbx.RestorePristine(file.Rel); err != nil {
		s.record(report.FileResult{
			Package: st.pkg.Name, File: file.Rel, Result: config.ResultError,
			StartedAt: start,
		})
		s.fileFinished(st, file.Rel)
		s.notifyf("%s/%s: pristine restore failed: %v", st.pkg.Name, file.Rel, err)
		return
	}

	vars := config.Vars{
		File:      file.Rel,
		AbsFile:   sbx.FilePath(file.Rel),
		Tests:     strings.Join(file.Tests, " "),
		TargetDir: sbx.Dir,
		Package:   st.pkg.Name,
	}
	exit, output, timedOut, oom, runErr := s.runCommands(ctx, sbx, vars)

	// An interrupted run (Ctrl-C) must not persist a fabricated result:
	// the file stays unrecorded and re-runs on resume (D21, F2).
	if ctx.Err() != nil {
		return
	}

	result := s.cfg.Classify(exit, output, timedOut, oom)
	if runErr != nil && !timedOut && !oom {
		result = config.ResultError
	}
	s.record(report.FileResult{
		Package:    st.pkg.Name,
		File:       file.Rel,
		Result:     result,
		ExitCode:   exit,
		DurationMS: time.Since(start).Milliseconds(),
		StartedAt:  start,
	})
	s.fileFinished(st, file.Rel)
	_ = s.store.Log(report.FileResult{Package: st.pkg.Name, File: file.Rel}, output)

	if timedOut && s.cfg.Mutation.Timeout.OnTimeout == "abort_package" {
		s.mu.Lock()
		st.aborted = true
		dropped := len(st.pending)
		st.pending = nil
		s.mu.Unlock()
		s.notifyf("package %s aborted after timeout (%d files dropped, resumable)", st.pkg.Name, dropped)
	}
}

// runCommands executes the configured command chain sequentially in the
// sandbox, stopping at the first failure. It enforces the per-command
// timeout (D14) and the per-process memory budget, killing the command's
// whole process group so timeouts never orphan the toolchain (F5).
func (s *Scheduler) runCommands(ctx context.Context, sbx *workspace.Sandbox, vars config.Vars) (exit int, output string, timedOut, oom bool, err error) {
	var buf bytes.Buffer
	for _, tmpl := range s.cfg.Mutation.Commands {
		if ctx.Err() != nil {
			return exit, buf.String(), false, false, ctx.Err()
		}
		cmdline := strings.TrimSpace(config.Interpolate(tmpl, vars))
		if cmdline == "" {
			continue
		}

		cmdCtx := ctx
		var cancel context.CancelFunc
		if t := s.cfg.Timeout(); t > 0 {
			cmdCtx, cancel = context.WithTimeout(ctx, t)
		}
		cmd := exec.Command("sh", "-c", cmdline)
		setNewPGroup(cmd)
		cmd.Dir = sbx.Dir
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out

		if serr := cmd.Start(); serr != nil {
			if cancel != nil {
				cancel()
			}
			return exit, buf.String(), false, false, serr
		}
		// Kill the whole process group on timeout or session cancel.
		finished := make(chan struct{})
		go func() {
			select {
			case <-cmdCtx.Done():
				killGroup(cmd)
			case <-finished:
			}
		}()

		// Memory budget watcher (per step, whole process tree).
		var oomKilled atomic.Bool
		stopRSS := make(chan struct{})
		if budget := s.cfg.Resources.MemoryBudgetPerStepMB; budget > 0 {
			watchRSS(cmd, budget, stopRSS, &oomKilled)
		}

		waitErr := cmd.Wait()
		close(finished)
		close(stopRSS)
		buf.Write(out.Bytes())
		if cancel != nil {
			cancel()
		}

		switch {
		case cmdCtx.Err() == context.DeadlineExceeded:
			return exit, buf.String(), true, false, nil
		case ctx.Err() != nil:
			return exit, buf.String(), false, false, ctx.Err()
		case oomKilled.Load():
			return exit, buf.String(), false, true, nil
		}
		if waitErr != nil {
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) {
				exit = ee.ExitCode()
			} else {
				return exit, buf.String(), false, false, waitErr
			}
		}
		if exit != 0 {
			return exit, buf.String(), false, false, nil
		}
	}
	return exit, buf.String(), false, false, nil
}

// watchRSS kills cmd's process group when its tree's resident memory
// exceeds budgetMB. oomKilled is set only when the kill fired.
func watchRSS(cmd *exec.Cmd, budgetMB int, stop <-chan struct{}, oomKilled *atomic.Bool) {
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if cmd.Process == nil || cmd.Process.Pid <= 0 {
					continue
				}
				if groupRSS(int32(cmd.Process.Pid)) > uint64(budgetMB)<<20 {
					if oomKilled.CompareAndSwap(false, true) {
						killGroup(cmd)
					}
					return
				}
			}
		}
	}()
}

func (s *Scheduler) record(r report.FileResult) {
	if err := s.store.Record(r); err != nil {
		s.notifyf("report write failed: %v", err)
	}
	s.mu.Lock()
	s.done++
	done, total := s.done, s.total
	s.mu.Unlock()
	if s.opt.Progress != nil {
		s.opt.Progress(done, total)
	}
}

// Done reports how many files completed this session.
func (s *Scheduler) Done() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
