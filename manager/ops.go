package manager

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
)

const (
	// maxLogLines is the ring kept per agent; a line is cut at maxLogLine
	// characters. Commands are logged without their environment.
	maxLogLines = 200
	maxLogLine  = 400
	// viewLogLines is how much of the ring the page gets.
	viewLogLines = 60
)

// DefaultStall is how long an operation may show no activity (no output, no
// bytes) before it is ended. It is a stall window, never a total time cap: a
// first download of an agent can be half a gigabyte.
const DefaultStall = 10 * time.Minute

// operation is one install, update or other action on one agent.
type operation struct {
	kind    string
	started time.Time

	mu      sync.Mutex
	ended   time.Time
	running bool
	err     string
	log     []string
	partial string

	cancel  context.CancelFunc
	stall   time.Duration
	timer   *time.Timer
	stalled atomic.Bool
	// canceled is set when the member pressed Cancel.
	canceled atomic.Bool
}

func newOperation(kind string, stall time.Duration, cancel context.CancelFunc, now time.Time) *operation {
	op := &operation{kind: kind, started: now, running: true, cancel: cancel, stall: stall}
	if stall > 0 {
		op.timer = time.AfterFunc(stall, func() {
			op.stalled.Store(true)
			cancel()
		})
	}
	return op
}

// touch records activity: the stall window starts over.
func (o *operation) touch() {
	if o.timer != nil {
		o.timer.Reset(o.stall)
	}
}

func (o *operation) addLine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	if len(line) > maxLogLine {
		line = line[:maxLogLine] + "…"
	}
	o.mu.Lock()
	o.log = append(o.log, line)
	if len(o.log) > maxLogLines {
		o.log = o.log[len(o.log)-maxLogLines:]
	}
	o.mu.Unlock()
	o.touch()
}

func (o *operation) logf(format string, args ...any) { o.addLine(fmt.Sprintf(format, args...)) }

// finish ends the operation with err (nil = done).
func (o *operation) finish(err error, now time.Time) {
	if o.timer != nil {
		o.timer.Stop()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.running = false
	o.ended = now
	switch {
	case err == nil:
		o.err = ""
	case o.stalled.Load():
		o.err = fmt.Sprintf("stopped: no activity for %s", o.stall.Round(time.Second))
	case o.canceled.Load():
		o.err = "cancelled"
	default:
		o.err = err.Error()
	}
	if o.err != "" {
		o.log = append(o.log, "failed: "+o.err)
	}
}

// OpView is an operation as the page sees it.
type OpView struct {
	Kind    string    `json:"kind"`
	Running bool      `json:"running"`
	Error   string    `json:"error,omitempty"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
	Log     []string  `json:"log,omitempty"`
}

func (o *operation) view() *OpView {
	o.mu.Lock()
	defer o.mu.Unlock()
	log := o.log
	if len(log) > viewLogLines {
		log = log[len(log)-viewLogLines:]
	}
	return &OpView{Kind: o.kind, Running: o.running, Error: o.err, Started: o.started, Ended: o.ended, Log: append([]string(nil), log...)}
}

// lineWriter turns a command's output into log lines.
type lineWriter struct {
	op  *operation
	buf bytes.Buffer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		i := bytes.IndexAny(w.buf.Bytes(), "\r\n")
		if i < 0 {
			break
		}
		w.op.addLine(string(w.buf.Next(i + 1)))
	}
	w.op.touch() // bytes without a newline are activity too
	return len(p), nil
}

func (w *lineWriter) flush() { w.op.addLine(w.buf.String()); w.buf.Reset() }

// run executes a command for an operation: no input, output streamed to the
// log, the whole process group ended when the operation is cancelled or
// stalls.
func (s *Service) run(ctx context.Context, op *operation, name string, args ...string) error {
	return s.runEnv(ctx, op, s.commandEnv(), name, args...)
}

// runEnv is run with an environment of its own: the command is logged, its
// output is the operation's log (each line counts as activity), and it is
// stopped, process group and all, when the operation is cancelled.
func (s *Service) runEnv(ctx context.Context, op *operation, env []string, name string, args ...string) error {
	op.logf("$ %s %s", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
	lw := &lineWriter{op: op}
	cmd.Stdout, cmd.Stderr = lw, lw
	cmd.Env = env
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	lw.flush()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("%s exited with status %d", name, ee.ExitCode())
		}
		return err
	}
	return nil
}
