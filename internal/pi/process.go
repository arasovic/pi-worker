package pi

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// toolAllowlist is the approved shell/test-capable tool slice for the v0
// coding worker, per docs/pi-cli-surface.md.
const toolAllowlist = "read,grep,find,ls,edit,write,bash"

// processCloseGrace is how long Close waits for the child to exit on stdin
// EOF before killing it. Tests shorten it.
var processCloseGrace = 5 * time.Second

// Process launches the host pi executable in RPC mode in a workspace with a
// fresh private per-worker session directory and the exact disabling flags
// from docs/pi-cli-surface.md. The child inherits the host environment
// verbatim; credentials never appear in argv. The child runs inside a
// platform lifecycle boundary (process group on Unix, job object on Windows)
// so cancellation, timeout, and forced Close terminate the direct child and
// descendants in that boundary; on Unix an additional sweep terminates
// ordinary descendants that moved to another process group, such as commands
// started by Pi's built-in bash tool. This is best-effort lifecycle
// recovery, not a sandbox: descendants spawned after the pre-close snapshot
// or deliberately reparented before it may escape, and a process spawned
// during the cleanup sweep itself may too. If Pi exits and is reaped before
// cleanup begins, v0 has no safe lineage snapshot and surviving descendants
// may also escape. Wait is always collected.
type Process struct {
	executable string
	workspace  string
	sessionDir string
	name       string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	started bool
	waitCh  chan struct{}
	waitErr error

	cont *childContainment
	// stopCancelWatch stops the context watcher once the child has been
	// reaped; it is nil before Start. The wait path joins an in-flight kill
	// callback before waitCh closes, so Close never releases the
	// containment while terminate is still running.
	stopCancelWatch func() bool

	mu     sync.Mutex
	closed bool
}

// processName derives the per-worker --name from the already-unique private
// session directory. The directory base is fresh and unique per MkdirTemp
// call and contains no secrets, so the name is unique per worker and safe:
// no whitespace, slashes, or credential material.
func processName(sessionDir string) string {
	return "pi-worker-" + strings.TrimPrefix(filepath.Base(sessionDir), "pi-worker-v0-")
}

// NewProcess prepares a fresh private per-worker session directory. The
// executable is resolved at Start time.
func NewProcess(executable, workspace string) (*Process, error) {
	sessionDir, err := os.MkdirTemp("", "pi-worker-v0-*")
	if err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	return &Process{
		executable: executable,
		workspace:  workspace,
		sessionDir: sessionDir,
		name:       processName(sessionDir),
	}, nil
}

// SessionDir returns the private session directory for this worker.
func (p *Process) SessionDir() string {
	return p.sessionDir
}

// argv is the exact writable worker launch from docs/pi-cli-surface.md.
func (p *Process) argv() []string {
	return []string{
		p.executable,
		"--mode", "rpc",
		"--session-dir", p.sessionDir,
		"--name", p.name,
		"--no-context-files",
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-themes",
		"--no-approve",
		"--tools", toolAllowlist,
	}
}

// Start launches the child with the workspace as working directory and the
// inherited host environment, inside the platform lifecycle boundary.
// Cancellation or timeout terminates the direct child, descendants in its
// process group/job, and ordinary descendants that left the group on Unix.
// An already-cancelled or expired context fails Start
// before any child spawns. Every failure after containment creation
// releases the containment and any created pipes exactly once: ownership
// transfers to the Process only on success.
func (p *Process) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		// An already-expired deadline or cancellation must fail before any
		// child spawns: this is the start-failure classification path.
		return err
	}
	if p.started {
		return fmt.Errorf("process already started")
	}
	cont, err := newChildContainment()
	if err != nil {
		return fmt.Errorf("process containment: %w", err)
	}
	argv := p.argv()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = p.workspace
	// cmd.Env stays nil: Go then inherits the host environment without the
	// worker reading, copying, or logging it.
	// cmd.Stderr stays nil: os/exec then connects child fd 2 directly to
	// os.DevNull, whereas a non-nil io.Writer creates an OS pipe and a copy
	// goroutine whose lifetime can be extended by descendant fd inheritance.

	// stdin and stdout own the caller ends of the child pipes created
	// below. release is the single cleanup path for every failure after
	// containment creation: it returns the containment (the Windows job
	// handle) and any created pipes to the OS exactly once, and Close never
	// releases again because ownership transfers only on success.
	var stdin io.WriteCloser
	var stdout io.ReadCloser
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if stdin != nil {
			_ = stdin.Close()
		}
		if stdout != nil {
			_ = stdout.Close()
		}
		_ = cont.close()
	}
	defer func() {
		if !p.started {
			release()
		}
	}()

	if err := cont.preStart(cmd); err != nil {
		return fmt.Errorf("process containment setup: %w", err)
	}
	stdin, err = cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("create stdin pipe: %w", err)
	}
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", p.executable, err)
	}
	if err := cont.assign(cmd.Process); err != nil {
		// Assignment is required before Start may report success. Kill and
		// reap the direct child immediately; the Windows implementation
		// documents its unavoidable post-create assignment window.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("process containment assignment: %w", err)
	}

	p.mu.Lock()
	p.cmd = cmd
	p.stdin = stdin
	p.stdout = stdout
	p.cont = cont
	p.started = true
	p.waitCh = make(chan struct{})
	p.mu.Unlock()

	// Watch the caller's context: cancellation or timeout terminates the
	// direct child and processes still in its lifecycle boundary. Registered after the
	// child is fully bound, so an already-done context still kills the tree.
	// killDone is closed when the callback finishes; the wait path joins the
	// callback through it before waitCh closes, so Close proves the callback
	// is done before releasing the containment and terminate never races
	// close. AfterFunc invokes the callback at most once, so the deferred
	// close of killDone is exactly-once.
	killDone := make(chan struct{})
	p.stopCancelWatch = context.AfterFunc(ctx, func() {
		defer close(killDone)
		p.killTree()
	})
	go func() {
		p.waitErr = cmd.Wait()
		// stop returns true only when the callback can never run; false
		// means it has started or is scheduled, so join it here. Either way
		// waitCh closes only after terminate is provably finished.
		if !p.stopCancelWatch() {
			<-killDone
		}
		close(p.waitCh)
	}()
	return nil
}

// Stdin returns the child stdin writer.
func (p *Process) Stdin() io.WriteCloser {
	return p.stdin
}

// Stdout returns the child stdout reader.
func (p *Process) Stdout() io.Reader {
	return p.stdout
}

// Wait blocks until the child has been reaped. It is always collected: Close
// and every worker path call it exactly once. When it returns, any
// in-flight cancellation kill callback has finished, so the containment can
// be released safely.
func (p *Process) Wait() error {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		return nil
	}
	<-p.waitCh
	return p.waitErr
}

// killTree terminates the child and descendants in the inherited process
// group/job, and on Unix ordinary descendants that moved to another process
// group (identified by a creation-time-verified lineage snapshot). It is the
// primary termination path for cancellation, timeout, and forced Close while
// the child is still alive. On failure it falls back to killing the direct
// child.
func (p *Process) killTree() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if err := p.cont.terminate(p.cmd.Process.Pid); err != nil {
		_ = p.cmd.Process.Kill()
	}
}

// Close closes the child stdin, waits for exit (killing the tree after
// processCloseGrace if needed), then cleans up residual descendants captured
// while the root is still live, releases containment, and removes the session
// directory. A root already reaped before Close has no safe lineage identity
// to snapshot. Close is idempotent and safe to call after a failed Start: a
// process that never started has no containment to release.
func (p *Process) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true

	started := p.started
	var residualDescendants any
	var stdin io.WriteCloser
	var waitCh chan struct{}
	var cont *childContainment
	if started && p.cmd != nil && p.cmd.Process != nil && p.cont != nil {
		// Snapshot only while the child is still potentially live. If it has
		// already been reaped, root ownership is gone and the pid may have
		// been reused.
		select {
		case <-p.waitCh:
		default:
			residualDescendants = p.cont.snapshotDescendants(p.cmd.Process.Pid)
		}
		stdin = p.stdin
		waitCh = p.waitCh
		cont = p.cont
	}
	p.mu.Unlock()

	if started {
		if stdin != nil {
			_ = stdin.Close()
		}
		if waitCh == nil {
			if cont != nil {
				_ = cont.close()
			}
			return os.RemoveAll(p.sessionDir)
		}
		select {
		case <-waitCh:
		case <-time.After(processCloseGrace):
			p.killTree()
			<-waitCh
		}
		// Root exit can obscure detached descendants from a fresh process-table
		// snapshot. Finish by terminating pre-close lineage-verified targets,
		// then release containment.
		if cont != nil {
			cont.terminateDescendants(residualDescendants)
			_ = cont.close()
		}
	}
	return os.RemoveAll(p.sessionDir)
}
