//go:build darwin || linux

package background

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/run"
)

// startExchangeTimeout bounds one child-side supervisor start handshake in
// the tests below. A correct accepted exchange finishes in milliseconds
// because it never waits for starter EOF or for a second request frame;
// five seconds is generous headroom that still fails fast on a regression.
const startExchangeTimeout = 5 * time.Second

// startExchangeFixture is one real-pipe supervisor start exchange viewed
// from the starter side. pipes carries the child-side transport ends that
// receiveSupervisorStart takes ownership of — the request reader on fd 3
// and the response writer on fd 4; there is no ownership reader (fd 5)
// because the supervisor role never receives one. requestWriter and
// responseReader are the parent-side ends of the same two pipes and are
// exactly the handles a starter holds across the handshake: the exchange
// must never touch them, and the starter closes its request writer only
// after the reply arrives. Later exchange tests reuse this fixture; Cleanup
// closes every end the exchange has not already closed.
type startExchangeFixture struct {
	pipes          *childRolePipes
	requestWriter  *os.File // starter -> child request pipe write end
	responseReader *os.File // child -> starter response pipe read end
}

// newStartExchangeFixture creates one real os.Pipe per direction and wraps
// the child-side ends in the childRolePipes handed to the exchange.
func newStartExchangeFixture(t *testing.T) *startExchangeFixture {
	t.Helper()
	requestReader, requestWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create request pipe: %v", err)
	}
	responseReader, responseWriter, err := os.Pipe()
	if err != nil {
		requestReader.Close()
		requestWriter.Close()
		t.Fatalf("create response pipe: %v", err)
	}
	fx := &startExchangeFixture{
		pipes:          &childRolePipes{requestReader: requestReader, responseWriter: responseWriter},
		requestWriter:  requestWriter,
		responseReader: responseReader,
	}
	t.Cleanup(fx.close)
	return fx
}

// close releases every fixture end still open, ignoring errors: the child
// ends are normally already closed by the exchange and the parent ends by
// the test itself.
func (fx *startExchangeFixture) close() {
	if fx.pipes != nil {
		if fx.pipes.requestReader != nil {
			fx.pipes.requestReader.Close()
		}
		if fx.pipes.responseWriter != nil {
			fx.pipes.responseWriter.Close()
		}
	}
	if fx.requestWriter != nil {
		fx.requestWriter.Close()
	}
	if fx.responseReader != nil {
		fx.responseReader.Close()
	}
}

// exchangeStartRequest returns one valid start request for a full accepted
// exchange: fresh temporary background and admission roots so the real
// preparation is durable and observable, a task prompt well over 4 KB, and
// the fixed ordered binary data of validStartRequest on the first task.
// The roots are returned for direct disk assertions.
func exchangeStartRequest(t *testing.T) (req supervisorStartRequest, backgroundRoot, admissionRoot string) {
	t.Helper()
	req = validStartRequest()
	big := bigUnicodePrompt()
	if len(big) <= 4096 {
		t.Fatalf("test prompt must exceed 4 KB, got %d bytes", len(big))
	}
	req.tasks[0].Prompt = big
	req.backgroundRoot = t.TempDir()
	req.admissionRoot = t.TempDir()
	return req, req.backgroundRoot, req.admissionRoot
}

// sendStartRequest encodes req and writes exactly one request frame to the
// starter request writer, which stays open: a correct child answers
// without ever seeing starter EOF.
func sendStartRequest(t *testing.T, fx *startExchangeFixture, req supervisorStartRequest) {
	t.Helper()
	payload, err := encodeSupervisorStartRequest(req)
	if err != nil {
		t.Fatalf("encode start request: %v", err)
	}
	if err := writeFrame(fx.requestWriter, payload, privateFrameLimit); err != nil {
		t.Fatalf("write start request frame: %v", err)
	}
}

// exchangeOutcome carries one return of the child-side exchange across the
// goroutine boundary.
type exchangeOutcome struct {
	result supervisorStartResult
	err    error
}

// startExchange runs receiveSupervisorStart over the fixture's child ends
// on a fresh goroutine and returns the channel that receives the outcome.
func startExchange(fx *startExchangeFixture) <-chan exchangeOutcome {
	done := make(chan exchangeOutcome, 1)
	go func() {
		result, err := receiveSupervisorStart(fx.pipes)
		done <- exchangeOutcome{result: result, err: err}
	}()
	return done
}

// waitExchange blocks on done no longer than startExchangeTimeout. The
// tests never close the starter request writer, so an exchange that waits
// for EOF or for a second frame times out here and fails the test.
func waitExchange(t *testing.T, done <-chan exchangeOutcome) exchangeOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(startExchangeTimeout):
		t.Fatal("exchange did not finish within the bounded wait: the handshake must not wait for starter EOF or a second request frame")
		return exchangeOutcome{} // unreachable
	}
}

// readStartReply reads exactly one reply frame from the starter response
// reader and returns its raw bytes together with the strict decode, so
// tests can assert on both the wire document and its meaning. The
// startExchangeTimeout read deadline is armed here through
// setStartExchangeReadDeadline before the frame is read, so every success
// and rejection test that reads the reply before awaiting the exchange
// outcome is bounded without a deadline at each call site.
func readStartReply(t *testing.T, fx *startExchangeFixture) ([]byte, supervisorStartReply) {
	t.Helper()
	setStartExchangeReadDeadline(t, fx.responseReader)
	payload, err := readFrame(fx.responseReader, privateFrameLimit)
	if err != nil {
		t.Fatalf("read reply frame: %v", err)
	}
	reply, err := decodeSupervisorStartReply(payload)
	if err != nil {
		t.Fatalf("decode reply frame: %v", err)
	}
	return payload, reply
}

// equalSnapshots reports whether two snapshots are identical at the
// fidelity the wire can express: empty per-worker Writes and Data slices
// normalize to nil before the comparison.
func equalSnapshots(a, b Snapshot) bool {
	return reflect.DeepEqual(normalizeEmptyWorkerSlices(a), normalizeEmptyWorkerSlices(b))
}

// requireNoDataBytesLeak fails when any document contains the standard
// base64 encoding of a carried data file's raw content. The snapshot
// projection records path, byte count, and digest only, so the raw bytes
// of binary task material must never appear in a snapshot document.
func requireNoDataBytesLeak(t *testing.T, docs ...[]byte) {
	t.Helper()
	leaks := []string{
		base64.StdEncoding.EncodeToString([]byte{0x00, 0x01, 0xff, 0xfe}),
		base64.StdEncoding.EncodeToString([]byte{0xde, 0xad, 0xbe, 0xef}),
	}
	for _, doc := range docs {
		for _, leak := range leaks {
			if bytes.Contains(doc, []byte(leak)) {
				t.Fatalf("snapshot document leaks raw data bytes: base64 %q present", leak)
			}
		}
	}
}

// TestSupervisorStartExchangeAcceptedCompletesBeforeRequestWriterClose
// runs one accepted exchange while the starter keeps its request writer
// open. The bounded wait fails if the child waits for starter EOF or for a
// second frame; afterwards the child's own transport ends are closed, the
// parent ends remain caller-owned, and the one reply frame is followed by
// clean EOF.
func TestSupervisorStartExchangeAcceptedCompletesBeforeRequestWriterClose(t *testing.T) {
	fx := newStartExchangeFixture(t)
	req, _, _ := exchangeStartRequest(t)

	sendStartRequest(t, fx, req)
	out := waitExchange(t, startExchange(fx))
	if out.err != nil {
		t.Fatalf("receiveSupervisorStart: %v", out.err)
	}
	if !out.result.accepted {
		t.Fatal("accepted exchange reported a non-accepted result")
	}
	if out.result.preparation == nil {
		t.Fatal("accepted result carries no preparation")
	}

	// The starter request writer was still open when the exchange
	// finished; the starter now receives exactly one complete accepted
	// frame.
	_, reply := readStartReply(t, fx)
	if !reply.accepted() {
		t.Fatalf("starter received a rejection %q for an accepted exchange", reply.reason)
	}

	// The child request reader and response writer were closed on return.
	if fx.pipes.requestReader != nil || fx.pipes.responseWriter != nil {
		t.Fatalf("child transport ends still open after the exchange returned: %+v", fx.pipes)
	}
	if _, err := readFrame(fx.responseReader, privateFrameLimit); !errors.Is(err, io.EOF) {
		t.Fatalf("read after the reply frame returned %v, want EOF: the child response writer must be closed", err)
	}

	// The parent ends remain caller-owned: each closes cleanly exactly
	// once here, which an end already closed by the exchange would refuse.
	if err := fx.requestWriter.Close(); err != nil {
		t.Fatalf("starter request writer is no longer caller-owned: %v", err)
	}
	if err := fx.responseReader.Close(); err != nil {
		t.Fatalf("starter response reader is no longer caller-owned: %v", err)
	}

	// Leave no durable artifacts behind.
	if err := out.result.preparation.rollback(); err != nil {
		t.Fatalf("cleanup rollback: %v", err)
	}
}

// TestSupervisorStartExchangeAcceptedReplyAndDecodedRequest verifies the
// accepted reply decodes and the result carries accepted=true, a nonnil
// preparation, and the complete decoded request: the >4 KB prompt, the
// ordered binary data, the verify argv, and every setting of the original
// request.
func TestSupervisorStartExchangeAcceptedReplyAndDecodedRequest(t *testing.T) {
	fx := newStartExchangeFixture(t)
	req, _, _ := exchangeStartRequest(t)
	big := req.tasks[0].Prompt

	sendStartRequest(t, fx, req)
	done := startExchange(fx)

	// The starter side decodes the one accepted reply.
	_, reply := readStartReply(t, fx)
	if !reply.accepted() {
		t.Fatalf("reply rejected the accepted exchange: %q", reply.reason)
	}
	if reply.snapshot.RunID != req.runID || reply.snapshot.State != RunAccepted {
		t.Fatalf("reply snapshot identity mismatch: runId=%q state=%q", reply.snapshot.RunID, reply.snapshot.State)
	}

	out := waitExchange(t, done)
	if out.err != nil {
		t.Fatalf("receiveSupervisorStart: %v", out.err)
	}
	if !out.result.accepted {
		t.Fatal("accepted exchange reported a non-accepted result")
	}
	if out.result.preparation == nil {
		t.Fatal("accepted result carries no preparation")
	}

	// The decoded request is the complete request; on mismatch, print both
	// sides as wire documents for diagnosis.
	if !reflect.DeepEqual(out.result.request, req) {
		got, gerr := encodeSupervisorStartRequest(out.result.request)
		want, werr := encodeSupervisorStartRequest(req)
		if gerr == nil && werr == nil {
			t.Fatalf("decoded request mismatch:\n got: %s\nwant: %s", got, want)
		}
		t.Fatalf("decoded request mismatch:\n got: %+v\nwant: %+v", out.result.request, req)
	}

	// Spot checks make the completeness visible: the full >4 KB prompt and
	// the ordered binary data survived, and the request settings decoded.
	decoded := out.result.request
	if decoded.tasks[0].Prompt != big {
		t.Fatalf("decoded prompt differs from the sent >4 KB prompt")
	}
	if len(decoded.tasks[0].Prompt) <= 4096 {
		t.Fatalf("decoded prompt lost bytes: %d", len(decoded.tasks[0].Prompt))
	}
	if !bytes.Equal(decoded.tasks[0].Data[0].Content, []byte{0x00, 0x01, 0xff, 0xfe}) ||
		!bytes.Equal(decoded.tasks[0].Data[1].Content, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("decoded binary data differs from the sent content: %+v", decoded.tasks[0].Data)
	}
	if !reflect.DeepEqual(decoded.verify, []string{"go", "test", "./..."}) {
		t.Fatalf("decoded verify argv mismatch: %q", decoded.verify)
	}
	if decoded.executionTimeout != req.executionTimeout ||
		decoded.backgroundRoot != req.backgroundRoot ||
		decoded.admissionRoot != req.admissionRoot ||
		decoded.maxModelWorkers != req.maxModelWorkers ||
		decoded.worktree == nil || *decoded.worktree != *req.worktree ||
		decoded.piExecutable != req.piExecutable ||
		!decoded.debug {
		t.Fatalf("decoded request settings mismatch: %+v", decoded)
	}

	// The reply snapshot is the prepared snapshot, not a re-derivation.
	if !equalSnapshots(reply.snapshot, out.result.preparation.snapshot) {
		t.Fatalf("reply snapshot differs from the prepared snapshot:\n got: %+v\nwant: %+v",
			reply.snapshot, out.result.preparation.snapshot)
	}

	if err := out.result.preparation.rollback(); err != nil {
		t.Fatalf("cleanup rollback: %v", err)
	}
}

// TestSupervisorStartExchangeDurabilityPrecedesReplyAndProjectionOnly
// verifies the ordering contract of an accepted exchange and the bounded
// projection. The reply frame is read to completion before the child
// handshake is awaited: because the frame is written only after the
// accepted Snapshot and admission tickets are durable, both artifacts must
// already be observable at that moment. The reply and the persisted
// Snapshot then carry only the bounded prompt projection and data
// metadata, never the full prompt or the raw data bytes, while the decoded
// result request still carries the full material.
func TestSupervisorStartExchangeDurabilityPrecedesReplyAndProjectionOnly(t *testing.T) {
	fx := newStartExchangeFixture(t)
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	big := req.tasks[0].Prompt
	data := req.tasks[0].Data[0]

	sendStartRequest(t, fx, req)
	done := startExchange(fx)

	// Complete reply frame on the starter side: the accepted Snapshot and
	// the ticket batch were durable before the reply frame was written, so
	// they must be observable here, before the handshake is awaited.
	replyPayload, reply := readStartReply(t, fx)
	if !reply.accepted() {
		t.Fatalf("reply rejected the accepted exchange: %q", reply.reason)
	}
	snapPath := filepath.Join(backgroundRoot, req.runID, "snapshot.json")
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("accepted snapshot not durable by the time the reply frame was complete: %v", err)
	}
	st := readAdmissionState(t, admissionRoot)
	if len(st.Tickets) != len(req.tasks) {
		t.Fatalf("durable tickets = %d, want %d at reply-frame completion: %+v", len(st.Tickets), len(req.tasks), st.Tickets)
	}
	for i, tk := range st.Tickets {
		if tk.RunID != req.runID || tk.WorkerID != i+1 || tk.State != "queued" {
			t.Errorf("ticket[%d] mismatch at reply-frame completion: %+v", i, tk)
		}
	}

	out := waitExchange(t, done)
	if out.err != nil {
		t.Fatalf("receiveSupervisorStart: %v", out.err)
	}
	if !out.result.accepted || out.result.preparation == nil {
		t.Fatalf("accepted exchange returned accepted=%v preparation=%v", out.result.accepted, out.result.preparation)
	}
	prep := out.result.preparation

	// The in-memory request is untouched by the projection: the full
	// >4 KB prompt and the raw data bytes arrived over the wire.
	if out.result.request.tasks[0].Prompt != big {
		t.Fatal("decoded request lost the full >4 KB prompt")
	}
	if !bytes.Equal(out.result.request.tasks[0].Data[0].Content, data.Content) {
		t.Fatal("decoded request lost the raw data bytes")
	}

	// Every worker task in the prepared snapshot is exactly the bounded
	// projection: prompt capped at 4096 bytes with the truncated flag, and
	// data reduced to path/byteCount/sha256 metadata.
	wantProjection := run.ProjectTasks(req.tasks)
	for i, worker := range prep.snapshot.Workers {
		if !reflect.DeepEqual(worker.Task, wantProjection[i]) {
			t.Fatalf("snapshot worker %d task is not the bounded projection:\n got: %+v\nwant: %+v", i+1, worker.Task, wantProjection[i])
		}
	}
	first := prep.snapshot.Workers[0].Task
	if !first.PromptTruncated || len(first.Prompt) > 4096 || !strings.HasPrefix(big, first.Prompt) {
		t.Fatalf("snapshot prompt is not the bounded truncated prefix: truncated=%v bytes=%d", first.PromptTruncated, len(first.Prompt))
	}
	sum := sha256.Sum256(data.Content)
	if got := first.Data[0]; got.Path != data.Path || got.Bytes != len(data.Content) || got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("snapshot data is not pure metadata of the carried content: %+v", got)
	}

	// The reply and the persisted Snapshot both carry exactly the prepared
	// projection-only snapshot, and neither document leaks the raw data
	// bytes.
	disk, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	requireNoDataBytesLeak(t, replyPayload, disk)
	loaded, err := prep.store.Load(req.runID)
	if err != nil {
		t.Fatalf("reload accepted snapshot from disk: %v", err)
	}
	if !equalSnapshots(reply.snapshot, prep.snapshot) {
		t.Fatalf("reply snapshot differs from the prepared snapshot:\n got: %+v\nwant: %+v", reply.snapshot, prep.snapshot)
	}
	if !equalSnapshots(loaded, prep.snapshot) {
		t.Fatalf("persisted snapshot differs from the prepared snapshot:\n got: %+v\nwant: %+v", loaded, prep.snapshot)
	}
}

// TestSupervisorStartExchangeResultRollbackRemovesArtifacts verifies that
// the preparation carried by an accepted result can be rolled back
// explicitly: the persisted Snapshot and its run directory disappear and
// every durable ticket is cancelled.
func TestSupervisorStartExchangeResultRollbackRemovesArtifacts(t *testing.T) {
	fx := newStartExchangeFixture(t)
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)

	sendStartRequest(t, fx, req)
	out := waitExchange(t, startExchange(fx))
	if out.err != nil {
		t.Fatalf("receiveSupervisorStart: %v", out.err)
	}
	if !out.result.accepted || out.result.preparation == nil {
		t.Fatalf("accepted exchange returned accepted=%v preparation=%v", out.result.accepted, out.result.preparation)
	}

	// Artifacts exist before the explicit rollback.
	if _, err := os.Stat(filepath.Join(backgroundRoot, req.runID, "snapshot.json")); err != nil {
		t.Fatalf("snapshot missing before rollback: %v", err)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != len(req.tasks) {
		t.Fatalf("tickets before rollback = %d, want %d", len(st.Tickets), len(req.tasks))
	}

	if err := out.result.preparation.rollback(); err != nil {
		t.Fatalf("rollback of the accepted result preparation: %v", err)
	}

	// The run directory is gone and every ticket was cancelled.
	requireDirEmpty(t, backgroundRoot)
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets left after rollback: %+v", st.Tickets)
	}
}

// TestSupervisorStartExchangeNilPipesAndMissingEndsReturnErrors verifies
// that nil pipes and pipes without transport ends return errors instead of
// panicking, and never report acceptance.
func TestSupervisorStartExchangeNilPipesAndMissingEndsReturnErrors(t *testing.T) {
	t.Run("nil pipes", func(t *testing.T) {
		result, err := receiveSupervisorStart(nil)
		if err == nil {
			t.Fatal("receiveSupervisorStart(nil) returned no error")
		}
		if !strings.Contains(err.Error(), "pipes must not be nil") {
			t.Fatalf("error %q does not name the nil pipes", err)
		}
		if result.accepted {
			t.Fatal("nil pipes reported an accepted result")
		}
	})

	t.Run("missing pipe ends", func(t *testing.T) {
		result, err := receiveSupervisorStart(&childRolePipes{})
		if err == nil {
			t.Fatal("receiveSupervisorStart with nil transport ends returned no error")
		}
		if !strings.Contains(err.Error(), "read") {
			t.Fatalf("error %q does not report the failed request read", err)
		}
		if result.accepted {
			t.Fatal("missing ends reported an accepted result")
		}
	})
}
