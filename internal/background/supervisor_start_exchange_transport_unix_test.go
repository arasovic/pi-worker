//go:build darwin || linux

package background

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/run"
)

// oversizedAcceptedReplyRequest makes the prepared Snapshot substantially
// larger than an os.Pipe buffer. The request remains valid, but its accepted
// projection contains the maximum task count, a full prompt projection, and
// enough data metadata to make the reply write block on every supported Unix
// target covered by this file.
func oversizedAcceptedReplyRequest(t *testing.T) (supervisorStartRequest, string, string) {
	t.Helper()
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	base := req.tasks[0]
	base.Prompt = strings.Repeat("x", 4096)
	base.Data = make([]run.DataFile, 64)
	for i := range base.Data {
		base.Data[i] = run.DataFile{
			Path:    "in/oversized-" + string(rune('a'+i%26)) + strings.Repeat("p", 20000) + ".bin",
			Content: []byte{byte(i)},
		}
	}
	req.tasks = make([]run.Task, run.MaxTasks)
	for i := range req.tasks {
		task := base
		task.Writes.Paths = []string{"out-" + string(rune('a'+i)) + ".txt"}
		task.Data = make([]run.DataFile, len(base.Data))
		for j, file := range base.Data {
			file.Path = "in/task-" + string(rune('a'+i)) + "-" + string(rune('a'+j)) + strings.Repeat("p", 20000) + ".bin"
			task.Data[j] = file
		}
		req.tasks[i] = task
	}
	req.maxModelWorkers = len(req.tasks)
	return req, backgroundRoot, admissionRoot
}

// sendStartRequestAsync starts the request write after the exchange reader is
// running. The oversized valid request can itself exceed pipe capacity.
func sendStartRequestAsync(t *testing.T, fx *startExchangeFixture, req supervisorStartRequest) <-chan error {
	t.Helper()
	payload, err := encodeSupervisorStartRequest(req)
	if err != nil {
		t.Fatalf("encode start request: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- writeFrame(fx.requestWriter, payload, privateFrameLimit)
	}()
	return done
}

func waitStartRequestWrite(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write start request frame: %v", err)
		}
	case <-time.After(startExchangeTimeout):
		t.Fatal("request write did not finish within the bounded wait")
	}
}

func setStartExchangeReadDeadline(t *testing.T, reader *os.File) {
	t.Helper()
	if err := reader.SetReadDeadline(time.Now().Add(startExchangeTimeout)); err != nil {
		t.Fatalf("set response read deadline: %v", err)
	}
}

// TestSupervisorStartExchangeAcceptedReplyZeroByteWriteFailureRollsBack
// closes the parent response read end before the accepted reply starts. The
// accepted write therefore consumes zero bytes, allowing the child to make a
// rejection attempt; the transport failures must remain joined with the
// rollback outcome and the durable preparation must be gone.
func TestSupervisorStartExchangeAcceptedReplyZeroByteWriteFailureRollsBack(t *testing.T) {
	fx := newStartExchangeFixture(t)
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	sendStartRequest(t, fx, req)
	if err := fx.responseReader.Close(); err != nil {
		t.Fatalf("close parent response read end: %v", err)
	}

	out := waitExchange(t, startExchange(fx))
	if out.result.accepted || out.result.preparation != nil {
		t.Fatalf("zero-byte accepted write returned accepted=%v preparation=%v", out.result.accepted, out.result.preparation)
	}
	if out.err == nil || !errors.Is(out.err, syscall.EPIPE) {
		t.Fatalf("zero-byte accepted write did not preserve the transport cause: %v", out.err)
	}
	if !strings.Contains(out.err.Error(), "write rejection frame") {
		t.Fatalf("error %q does not prove that a rejection write was attempted", out.err)
	}
	if _, err := os.Stat(filepath.Join(backgroundRoot, req.runID, "snapshot.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("accepted snapshot survived zero-byte transport rollback: %v", err)
	}
	requireDirEmpty(t, backgroundRoot)
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets survived zero-byte transport rollback: %+v", st.Tickets)
	}
}

// TestSupervisorStartExchangeAcceptedReplyPartialWriteFailure uses a reply
// larger than the real pipe capacity. It reads a small positive prefix and
// closes the parent end while the accepted write is blocked. The preparation
// rolls back, the transport cause is returned, and no rejection is attempted
// after the accepted frame has already reached the wire partially.
func TestSupervisorStartExchangeAcceptedReplyPartialWriteFailure(t *testing.T) {
	fx := newStartExchangeFixture(t)
	req, backgroundRoot, admissionRoot := oversizedAcceptedReplyRequest(t)
	done := startExchange(fx)
	requestDone := sendStartRequestAsync(t, fx, req)
	waitStartRequestWrite(t, requestDone)

	setStartExchangeReadDeadline(t, fx.responseReader)
	prefix := make([]byte, 8)
	n, err := io.ReadFull(fx.responseReader, prefix)
	if err != nil {
		t.Fatalf("read positive accepted-reply prefix: %v", err)
	}
	if n <= 0 {
		t.Fatal("accepted-reply prefix was empty")
	}
	announced := binary.BigEndian.Uint32(prefix[:4])
	if announced <= uint32(n-4) {
		t.Fatalf("accepted reply payload did not exceed the prefix: announced=%d prefixPayload=%d", announced, n-4)
	}
	if err := fx.responseReader.Close(); err != nil {
		t.Fatalf("close parent response read end after prefix: %v", err)
	}

	out := waitExchange(t, done)
	if out.result.accepted || out.result.preparation != nil {
		t.Fatalf("partial accepted write returned accepted=%v preparation=%v", out.result.accepted, out.result.preparation)
	}
	if out.err == nil || !errors.Is(out.err, syscall.EPIPE) {
		t.Fatalf("partial accepted write did not return its transport cause: %v", out.err)
	}
	if strings.Contains(out.err.Error(), "write rejection frame") {
		t.Fatalf("partial accepted write attempted/appended a rejection: %v", out.err)
	}
	requireDirEmpty(t, backgroundRoot)
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets survived partial transport rollback: %+v", st.Tickets)
	}
}

// TestSupervisorStartExchangeAcceptedReplyCloseDiagnosticPreservesAccepted
// closes the child request reader inside the injected preparation closure.
// The accepted reply is fully read before the exchange returns, so acceptance
// remains final even though deferred transport cleanup reports that close.
func TestSupervisorStartExchangeAcceptedReplyCloseDiagnosticPreservesAccepted(t *testing.T) {
	fx := newStartExchangeFixture(t)
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	sendStartRequest(t, fx, req)

	done := startExchangeWithPrepare(fx, func(got supervisorStartRequest) (*supervisorPreparation, error) {
		prep, err := prepareSupervisorStart(got)
		if err != nil {
			return nil, err
		}
		if err := fx.pipes.requestReader.Close(); err != nil {
			return nil, err
		}
		return prep, nil
	})
	setStartExchangeReadDeadline(t, fx.responseReader)
	_, reply := readStartReply(t, fx)
	if !reply.accepted() {
		t.Fatalf("reply rejected the accepted exchange: %q", reply.reason)
	}

	out := waitExchange(t, done)
	if !out.result.accepted || out.result.preparation == nil {
		t.Fatalf("close diagnostic changed acceptance: accepted=%v preparation=%v", out.result.accepted, out.result.preparation)
	}
	if out.err == nil || !strings.Contains(out.err.Error(), "close request fd") {
		t.Fatalf("accepted result lost the request-reader close diagnostic: %v", out.err)
	}
	if !reflect.DeepEqual(out.result.request, req) {
		t.Fatalf("accepted result did not retain the complete request:\n got: %+v\nwant: %+v", out.result.request, req)
	}
	if !equalSnapshots(reply.snapshot, out.result.preparation.snapshot) || len(out.result.preparation.tickets) != len(req.tasks) {
		t.Fatalf("accepted result did not retain the complete preparation: snapshotEqual=%v tickets=%d wantTickets=%d", equalSnapshots(reply.snapshot, out.result.preparation.snapshot), len(out.result.preparation.tickets), len(req.tasks))
	}
	if _, err := os.Stat(filepath.Join(backgroundRoot, req.runID, "snapshot.json")); err != nil {
		t.Fatalf("accepted snapshot disappeared before caller rollback: %v", err)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != len(req.tasks) {
		t.Fatalf("tickets disappeared before caller rollback: %+v", st.Tickets)
	}
	if err := out.result.preparation.rollback(); err != nil {
		t.Fatalf("explicit rollback of accepted preparation: %v", err)
	}
	requireDirEmpty(t, backgroundRoot)
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets left after explicit rollback: %+v", st.Tickets)
	}
}

// TestSupervisorStartExchangeByteCounterCountsOnlyBytesAcceptedByPipe
// checks both successful and zero-byte real os.Pipe writes. The counter must
// use the n returned by the underlying file, never len(p) or an attempted
// write's size.
func TestSupervisorStartExchangeByteCounterCountsOnlyBytesAcceptedByPipe(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})

	counter := &supervisorStartByteCounter{w: writer}
	want := []byte("accepted")
	n, err := counter.Write(want)
	if err != nil {
		t.Fatalf("write accepted bytes: %v", err)
	}
	if n != len(want) || counter.n != int64(n) {
		t.Fatalf("counter after accepted write = n:%d total:%d, want n:%d", n, counter.n, len(want))
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read accepted bytes: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("pipe bytes = %q, want %q", got, want)
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	before := counter.n
	n, err = counter.Write([]byte("not accepted"))
	if err == nil || !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("closed-pipe write returned n=%d err=%v, want EPIPE", n, err)
	}
	if n != 0 || counter.n != before {
		t.Fatalf("counter recorded bytes not accepted by pipe: n=%d total=%d before=%d", n, counter.n, before)
	}
}
