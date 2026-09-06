//go:build darwin || linux

package background

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"
)

// testExe resolves the running binary path for process-spawning tests.
func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// TestRoleProcessValidationErrors ensures empty executable and invalid role
// fail before any process or pipe is created.
func TestRoleProcessValidationErrors(t *testing.T) {
	_, err := startRoleProcess("", roleSupervisor)
	if err == nil {
		t.Fatal("expected error for empty executable")
	}
	_, err = startRoleProcess(testExe(t), role("bogus"))
	if err == nil {
		t.Fatal("expected error for invalid role")
	}
}

// TestRoleProcessSupervisorEcho exercises the full supervisor echo lifecycle:
// frame I/O, idempotent CloseRequest, concurrent Wait, and state assertions.
func TestRoleProcessSupervisorEcho(t *testing.T) {
	exe := testExe(t)
	canary := make([]byte, 64)
	for i := range canary {
		canary[i] = byte(i)
	}
	canary[0] = 0   // contains NUL
	canary[1] = '\n' // contains newline

	p, err := startRoleProcess(exe, roleSupervisor)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if got := p.cmd.Args; len(got) != 2 || got[0] != exe || got[1] != string(roleSupervisor) {
		t.Fatalf("cmd.Args = %q, want [%q, %q]", got, exe, roleSupervisor)
	}
	if p.cmd.Env != nil {
		t.Fatalf("cmd.Env = %v, want nil", p.cmd.Env)
	}

	const nCanaries = 5
	done := make(chan []byte, nCanaries)
	for i := 0; i < nCanaries; i++ {
		go func(n int) {
			err := p.Send(canary)
			if err != nil {
				t.Errorf("Send #%d canary: %v", n, err)
				return
			}
			b, rerr := p.Receive()
			if rerr != nil {
				t.Errorf("Receive #%d canary: %v", n, rerr)
				return
			}
			select {
			case done <- b:
			default:
				t.Error("done channel full")
			}
		}(i)
	}
	for i := 0; i < nCanaries; i++ {
		select {
		case b := <-done:
			if len(b) != len(canary) {
				t.Errorf("#%d canary len=%d, want %d", i, len(b), len(canary))
			} else if string(b) != string(canary) {
				t.Errorf("#%d canary mismatch", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("receive canary #%d timeout", i)
		}
	}

	frame1 := []byte{1, 2, 3}
	frame2 := []byte{4, 5}
	if err := p.Send(frame1); err != nil {
		t.Fatalf("Send frame1: %v", err)
	}
	if b, err := p.Receive(); err != nil || string(b) != string(frame1) {
		t.Fatalf("Receive frame1: got %v/%v, want %v", b, err, frame1)
	}
	if err := p.Send(frame2); err != nil {
		t.Fatalf("Send frame2: %v", err)
	}
	if b, err := p.Receive(); err != nil || string(b) != string(frame2) {
		t.Fatalf("Receive frame2: got %v/%v, want %v", b, err, frame2)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()

	err1 := p.CloseRequest()
	if err1 != nil {
		t.Logf("first CloseRequest: %v", err1)
	}
	err2 := p.CloseRequest()
	if err2 != err1 {
		t.Fatalf("second CloseRequest(%v) != first(%v)", err2, err1)
	}

	if _, err := p.Receive(); err != io.EOF {
		t.Fatalf("Receive after CloseRequest: got %v, want io.EOF", err)
	}

	ctx, cancel := context.WithTimeout(closeCtx, 5*time.Second)
	defer cancel()

	resultsCh := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() { resultsCh <- p.Wait() }()
	}
	var errs []error
	for i := 0; i < 5; i++ {
		select {
		case e := <-resultsCh:
			errs = append(errs, e)
		case <-ctx.Done():
			t.Fatalf("concurrent Wait timeout at %d", i)
		}
	}
	for i, e := range errs {
		if e != nil {
			t.Errorf("Wait[%d]: %v", i, e)
		}
	}

	ps := p.cmd.ProcessState
	if ps == nil {
		t.Fatal("cmd.ProcessState is nil, want non-nil")
	}
	if !ps.Exited() {
		t.Fatal("ProcessState.ExitCode() wanted exited")
	}

	repeatedErr := p.Wait()
	if repeatedErr != nil {
		t.Errorf("repeated Wait: %v", repeatedErr)
	}
	if cerr := p.Close(); cerr != nil {
		t.Errorf("subsequent Close: %v", cerr)
	}
}

// TestRoleProcessRoundTripLargePayload verifies a single 1 MiB binary round-trip,
// then CloseRequest, Receive EOF, and Wait success.
func TestRoleProcessRoundTripLargePayload(t *testing.T) {
	size := 1 << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i & 0xff)
	}

	p, err := startRoleProcess(testExe(t), roleSupervisor)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if err := p.Send(payload); err != nil {
		t.Fatalf("Send 1 MiB: %v", err)
	}

	resp, err := p.Receive()
	if err != nil {
		t.Fatalf("Receive 1 MiB: %v", err)
	}
	if len(resp) != size {
		t.Fatalf("received %d bytes, want %d", len(resp), size)
	}
	if !bytes.Equal(resp, payload) {
		t.Fatalf("1 MiB payload mismatch")
	}

	if err := p.CloseRequest(); err != nil {
		t.Logf("CloseRequest: %v", err)
	}
	if _, err := p.Receive(); err != io.EOF {
		t.Fatalf("Receive after CloseRequest: got %v, want io.EOF", err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
