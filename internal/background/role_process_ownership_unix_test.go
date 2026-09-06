//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestRoleProcessWorkerHostOwnership verifies the worker-host lifecycle:
// the child receives fd 3 (request reader), fd 4 (response writer), and
// fd 5 (ownership reader).  Closing the ownership write end signals the
// child to exit; Receive returns io.EOF and Wait yields nil once the
// child has been reaped.
func TestRoleProcessWorkerHostOwnership(t *testing.T) {
	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// Three extra files: request reader, response writer, ownership reader.
	if got := len(p.cmd.ExtraFiles); got != 3 {
		t.Fatalf("ExtraFiles length = %d, want 3", got)
	}
	if p.ownershipWriter == nil {
		t.Fatal("ownershipWriter is nil, want non-nil")
	}

	// Idempotent CloseOwnership: second call returns the same nil.
	if err1 := p.CloseOwnership(); err1 != nil {
		t.Fatalf("first CloseOwnership: %v", err1)
	}
	if err2 := p.CloseOwnership(); err2 != nil {
		t.Fatalf("second CloseOwnership: %v", err2)
	}

	// Child exits when the ownership pipe gets EOF; Receive returns
	// io.EOF and Wait returns nil.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	receiveErrCh := make(chan error, 1)
	go func() { receiveErrCh <- func() error { _, err := p.Receive(); return err }() }()

	waitCh := make(chan error, 1)
	go func() { waitCh <- p.Wait() }()

	var recvErr, waitErr error
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.Done():
			t.Fatalf("operation timeout at iteration %d", i+1)
		case recvErr = <-receiveErrCh:
		case waitErr = <-waitCh:
		}
	}
	if recvErr != io.EOF {
		t.Fatalf("Receive: got %v, want io.EOF", recvErr)
	}
	if waitErr != nil {
		t.Fatalf("Wait: got %v, want nil", waitErr)
	}

	assertRoleProcessReaped(t, p)
}

// TestRoleProcessSupervisorLifecycle exercises the supervisor flow:
// two extra files only, ownershipWriter is nil, one Send/Receive
// round-trip, CloseRequest produces io.EOF on Receive, Wait returns nil.
func TestRoleProcessSupervisorLifecycle(t *testing.T) {
	p, err := startRoleProcess(testExe(t), roleSupervisor)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// Two extra files: request reader, response writer.
	if got := len(p.cmd.ExtraFiles); got != 2 {
		t.Fatalf("ExtraFiles length = %d, want 2", got)
	}
	if p.ownershipWriter != nil {
		t.Fatal("ownershipWriter is non-nil, want nil for supervisor")
	}

	// Idempotent CloseOwnership on supervisor returns nil.
	if e1 := p.CloseOwnership(); e1 != nil {
		t.Fatalf("first CloseOwnership: %v", e1)
	}
	if e2 := p.CloseOwnership(); e2 != nil {
		t.Fatalf("second CloseOwnership: %v", e2)
	}

	payload := []byte("supervisor-test-payload")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sendDone := make(chan error, 1)
	go func() { sendDone <- p.Send(payload) }()

	var sendErr error
	select {
	case <-ctx.Done():
		t.Fatal("Send timed out")
	case sendErr = <-sendDone:
	}
	if sendErr != nil {
		t.Fatalf("Send: got %v, want nil", sendErr)
	}

	recvDone := make(chan []byte, 1)
	receiveErrCh := make(chan error, 1)
	go func() {
		b, err := p.Receive()
		receiveErrCh <- err
		if err == nil && b != nil {
			recvDone <- b
		}
	}()

	var resp []byte
	select {
	case <-ctx.Done():
		t.Fatal("Receive timed out")
	case resp = <-recvDone:
	}
	if string(resp) != string(payload) {
		t.Fatalf("got %q, want %q", resp, payload)
	}

	// CloseRequest then expect io.EOF on next Receive.
	closeErr := p.CloseRequest()
	if closeErr != nil {
		t.Logf("CloseRequest: %v", closeErr)
	}

	eofCh := make(chan error, 1)
	go func() { eofCh <- func() error { _, err := p.Receive(); return err }() }()

	var eofErr error
	select {
	case <-ctx.Done():
		t.Fatal("Receive after CloseRequest timed out")
	case eofErr = <-eofCh:
	}
	if eofErr != io.EOF {
		t.Fatalf("Receive after CloseRequest: got %v, want io.EOF", eofErr)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- p.Wait() }()

	var waitErr error
	select {
	case <-ctx.Done():
		t.Fatal("Wait timed out")
	case waitErr = <-waitCh:
	}
	if waitErr != nil {
		t.Fatalf("Wait: got %v, want nil", waitErr)
	}
}

// TestRoleProcessParentClosesChildSideOwnershipReader confirms the invariant:
// after Start the parent's copy of the ownership reader (the child-side fd
// passed via ExtraFiles[2]) is closed.  Stat is used as the probe so an
// accidentally-live descriptor fails immediately instead of blocking on Read.
func TestRoleProcessParentClosesChildSideOwnershipReader(t *testing.T) {
	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// Three extra files: request reader, response writer, ownership reader.
	if got := len(p.cmd.ExtraFiles); got != 3 {
		t.Fatalf("ExtraFiles length = %d, want 3", got)
	}

	// Probe ExtraFiles[2] — the exact *os.File given to cmd.Start — to
	// confirm the parent closed its child-side copy after Start.
	if _, err := p.cmd.ExtraFiles[2].Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("ExtraFiles[2] after Start: expected os.ErrClosed, got %v", err)
	}

	if err := p.CloseOwnership(); err != nil {
		t.Fatalf("CloseOwnership: %v", err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	assertRoleProcessReaped(t, p)
}

// TestRoleProcessWorkerHostOwnershipByteMismatch verifies that writing
// even a single byte through the ownership pipe causes the worker-host
// child to abort with exit code 97, pinning the EOF-only ownership
// protocol.
func TestRoleProcessWorkerHostOwnershipByteMismatch(t *testing.T) {
	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if got := len(p.cmd.ExtraFiles); got != 3 {
		t.Fatalf("ExtraFiles length = %d, want 3", got)
	}
	if p.ownershipWriter == nil {
		t.Fatal("ownershipWriter is nil, want non-nil")
	}

	// Write exactly one byte into the ownership pipe; child must abort.
	if _, err := p.ownershipWriter.Write([]byte{0xab}); err != nil {
		t.Fatalf("ownership write: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	waitCh := make(chan error, 1)
	go func() { waitCh <- p.Wait() }()

	var waitErr error
	select {
	case <-ctx.Done():
		t.Fatal("Wait timed out")
	case waitErr = <-waitCh:
	}

	var ee *exec.ExitError
	if !errors.As(waitErr, &ee) {
		t.Fatalf("Wait: got %T(%v), want *exec.ExitError(code 97)", waitErr, waitErr)
	}
	if ee.ExitCode() != 97 {
		t.Fatalf("ExitCode = %d, want 97", ee.ExitCode())
	}

	// After child is reaped, Close must be safe.
	if cerr := p.Close(); cerr != nil {
		t.Errorf("Close after exited child: %v", cerr)
	}

	assertRoleProcessReaped(t, p)
}
