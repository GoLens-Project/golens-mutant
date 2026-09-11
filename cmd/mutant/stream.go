package main

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoLens-Project/golens-mutant/internal/scheduler"
)

// lockedWriter serializes every terminal write (progress bar, notices,
// per-file event lines, live command output) so concurrent workers never
// interleave mid-line.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// filePrefix renders the shared per-file line pattern:
// [timestamp] state x/y package - file. Event lines end here; streamed
// command output appends after a space.
func filePrefix(ts time.Time, state string, done, total int, pkg, file string) string {
	return fmt.Sprintf("[%s] %s %d/%d %s - %s",
		ts.Format(time.RFC3339), state, done, total, pkg, file)
}

// streamSink tees one file's raw command output onto the console as
// tagged, line-atomic writes using the same pattern as the per-file
// event lines: [timestamp] started x/y package - file, then the output.
//
// A stalled console (Ctrl-S, laggy PTY) must never stall the child
// process — the per-command deadline would then misclassify the file as
// "timeout" — so Write only appends to a bounded queue; a dedicated
// drainer goroutine performs the terminal writes. When the queue is
// full the line is dropped and counted, with one summary note per
// flush: streaming is best-effort by design.
//
// Lines terminate at '\n' (or "\r\n", counted once). A bare '\r' is an
// overwrite: it resets the current line, collapsing spinner redraws to
// their final frame instead of one tagged line per frame.
type streamSink struct {
	w       *lockedWriter
	ev      scheduler.FileEvent // started event: package, file, x/y context
	queue   chan func()
	dropped atomic.Int64

	// mu guards the fields below; Write is called from exec's stdout and
	// stderr copy goroutines concurrently.
	mu     sync.Mutex
	part   []byte // trailing partial line (ends with '\r' when crHeld)
	crHeld bool   // part ends with a '\r' that may yet prove to be "\r\n"
	closed bool   // Close was called; further lines are dropped
}

// streamQueueCap bounds queued lines per file before dropping.
const streamQueueCap = 1024

func newStreamSink(w *lockedWriter, e scheduler.FileEvent) *streamSink {
	return newStreamSinkCap(w, e, streamQueueCap)
}

func newStreamSinkCap(w *lockedWriter, e scheduler.FileEvent, queueCap int) *streamSink {
	s := &streamSink{w: w, ev: e, queue: make(chan func(), queueCap)}
	go func() {
		for f := range s.queue {
			f()
		}
	}()
	return s
}

// Write scans p in place and queues each complete line; only the
// trailing partial is buffered. It never blocks on the console and
// never fails (an error would reach cmd.Wait through io.MultiWriter and
// misclassify the file).
func (s *streamSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	if s.crHeld {
		if len(p) > 0 && p[0] == '\n' {
			// join copies: the drainer reads the line while Write may
			// keep appending to part's backing array.
			s.emitLocked(s.join(nil, bytes.TrimSuffix(s.part, []byte{'\r'})))
			s.part = s.part[:0]
			start = 1
		} else {
			s.part = s.part[:0] // bare '\r': the redraw starts over
		}
		s.crHeld = false
	}
	for start < len(p) {
		i := bytes.IndexAny(p[start:], "\r\n")
		if i < 0 {
			s.part = append(s.part, p[start:]...)
			return len(p), nil
		}
		i += start
		switch {
		case p[i] == '\n':
			s.emitLocked(s.join(s.part, p[start:i]))
			s.part = s.part[:0]
			start = i + 1
		case i+1 == len(p):
			// Trailing '\r': CRLF or overwrite is undecided until the
			// next chunk (or Flush).
			s.part = append(s.part, p[start:i]...)
			s.part = append(s.part, '\r')
			s.crHeld = true
			return len(p), nil
		case p[i+1] == '\n': // CRLF: one terminator
			s.emitLocked(s.join(s.part, p[start:i]))
			s.part = s.part[:0]
			start = i + 2
		default: // bare '\r': overwrite resets the line
			s.part = s.part[:0]
			start = i + 1
		}
	}
	return len(p), nil
}

// join concatenates the carried partial and the in-place line segment.
func (s *streamSink) join(head, tail []byte) []byte {
	line := make([]byte, 0, len(head)+len(tail))
	line = append(line, head...)
	return append(line, tail...)
}

// Flush emits the trailing partial line, then drains the queue so every
// line enqueued so far has hit the console before the next command's
// output starts. It runs after a command completes (never on the copy
// path), so blocking here delays only this worker.
func (s *streamSink) Flush() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	line := s.join(nil, s.part)
	if s.crHeld {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		s.crHeld = false
	}
	s.part = nil
	if len(line) > 0 {
		s.emitLocked(line)
	}
	s.mu.Unlock()

	done := make(chan struct{})
	s.queue <- func() { close(done) } // blocking: post-command context
	<-done

	// Report dropped lines after the barrier: the queue is empty here,
	// so this enqueue cannot itself be dropped.
	if n := s.dropped.Swap(0); n > 0 {
		s.mu.Lock()
		if !s.closed {
			s.queue <- s.writeLine(fmt.Appendf(nil, "… %d stream lines dropped (console stalled)", n))
		}
		s.mu.Unlock()
	}
}

// Close drains any remaining queued lines, then stops the drainer
// goroutine. It is called once the file's command chain has finished
// (after the final Flush).
func (s *streamSink) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	// No further sends can arrive (every sender checks closed under
	// mu), so this barrier runs after the last queued line, and closing
	// the queue afterwards is race-free.
	done := make(chan struct{})
	s.queue <- func() { close(done) }
	<-done
	close(s.queue)
}

// emitLocked queues one complete line; the caller holds mu. A full
// queue drops the line — the copy goroutine must never wait on the
// console.
func (s *streamSink) emitLocked(line []byte) {
	if s.closed {
		return
	}
	select {
	case s.queue <- s.writeLine(line):
	default:
		s.dropped.Add(1)
	}
}

func (s *streamSink) writeLine(line []byte) func() {
	return func() {
		fmt.Fprintf(s.w, "\r\033[K%s %s\n",
			filePrefix(time.Now(), "started", s.ev.Done, s.ev.Total, s.ev.Package, s.ev.File), line)
	}
}
