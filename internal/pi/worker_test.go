package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// happyPathScript drives fakepi through the full worker lifecycle.
func happyPathScript(finalText string) *script.Script {
	return &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"get_state": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium","isStreaming":false}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"agent_start"}`)},
			{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"The answer is 42."}]}}`)},
			{Event: json.RawMessage(`{"type":"turn_end","message":{},"toolResults":[]}`)},
			{Event: json.RawMessage(`{"type":"agent_end","messages":[],"willRetry":false}`)},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"` + finalText + `"}`)}},
		},
	}}
}

func TestWorkerIDZeroDefaultsToOne(t *testing.T) {
	// Direct callers leave the identity zero; it must label worker 1. The
	// controller passes explicit identities 1..N through unchanged.
	for id, want := range map[int]int{0: 1, -3: 1, 1: 1, 2: 2, 3: 3} {
		if got := workerID(id); got != want {
			t.Fatalf("workerID(%d) = %d, want %d", id, got, want)
		}
	}
}

func TestWorkerDebugHeartbeatCoversSlowSetupAndStopsBeforeFinalStatus(t *testing.T) {
	scriptConfig := happyPathScript("slow setup answer")
	scriptConfig.Triggers["get_available_models"] = []script.Step{
		{SleepMS: 250},
		{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
	}
	requestLog := setupFakePiEnv(t, scriptConfig)

	clock := &fakeClock{t: time.Unix(0, 0)}
	writes := make(chan string, 32)
	writer := &notifyingWriter{writes: writes}
	sink := newDebugSinkWithClock(writer, clock.t, clock.now)
	timerReady := make(chan *deterministicHeartbeatTimer, 1)
	timer := &deterministicHeartbeatTimer{
		ticks:  make(chan time.Time, 4),
		resets: make(chan time.Duration, 32),
		stops:  make(chan struct{}, 1),
	}
	sink.newHeartbeatTimer = func(time.Duration) debugTimer {
		timerReady <- timer
		return timer
	}

	done := make(chan WorkerResult, 1)
	go func() {
		done <- New(fakePiBin).Run(context.Background(), WorkerRequest{
			Model:     "acme/m-1",
			Prompt:    "go",
			Workspace: t.TempDir(),
			Debug:     sink,
		})
	}()

	select {
	case <-timerReady:
	case <-time.After(time.Second):
		t.Fatal("lifecycle heartbeat did not start after process start")
	}
	_ = waitRequestLog(t, requestLog, 1)
	// The heartbeat reports only when a full interval of fake time has
	// passed with no visible worker line. The worker runs on the real
	// scheduler, so it can log a line (rpc completions, phases, prompt
	// events) at the same fake instant as our tick but before the loop
	// reads lastActivity; the loop then consumes that tick without
	// reporting and re-arms the timer for the remainder. The loop
	// publishes every re-arm on the timer reset channel, so drive the
	// heartbeat exactly like its own timer: advance the clock by one
	// interval, tick, and whenever the loop re-arms without a line,
	// advance again and retry, until it observes a genuinely silent
	// interval. A tick dropped to recent activity is therefore never the
	// last one.
	clock.advance(debugHeartbeatInterval)
	foundHeartbeat := false
	deadline := time.After(time.Second)
	for !foundHeartbeat {
		select {
		case timer.ticks <- clock.now():
		default:
			// The loop is behind on ticks it has not consumed yet; those
			// will produce the report, so do not pile more into the buffer.
		}
		select {
		case line := <-writes:
			if strings.Contains(line, "phase=waiting-for-pi") {
				foundHeartbeat = true
			}
		case <-timer.resets:
			clock.advance(debugHeartbeatInterval)
		case <-deadline:
			t.Fatal("no lifecycle heartbeat during slow setup RPC")
		}
	}

	result := <-done
	// The heartbeat must have been emitted during the slow setup, before
	// the prompt request: the first waiting-for-pi line must precede the
	// prompt RPC's first line in the sink's serialized debug output.
	// Checking the request log in flight races the worker's real-time
	// progress against this goroutine's read; buffer order under the
	// sink's write lock is authoritative and cannot flake on scheduling.
	allBodies := debugBodies(t, writer.buf.String())
	firstHeartbeat := slices.IndexFunc(allBodies, func(body string) bool {
		return strings.Contains(body, "phase=waiting-for-pi")
	})
	firstPrompt := slices.IndexFunc(allBodies, func(body string) bool {
		return strings.Contains(body, "rpc=prompt")
	})
	if firstHeartbeat < 0 {
		t.Fatalf("no lifecycle heartbeat during slow setup RPC")
	}
	if firstPrompt >= 0 && firstHeartbeat >= firstPrompt {
		t.Fatalf("heartbeat arrived after prompt instead of during setup: heartbeat at line %d, prompt at line %d (of %d)", firstHeartbeat, firstPrompt, len(allBodies))
	}
	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	bodies := debugBodies(t, writer.buf.String())
	if !strings.HasPrefix(bodies[len(bodies)-1], "worker=1 status=completed total=") {
		t.Fatalf("final status was not the last debug line: %q", bodies[len(bodies)-1])
	}
	before := len(bodies)
	for {
		select {
		case <-writes:
		default:
			goto drained
		}
	}

drained:
	clock.advance(debugHeartbeatInterval)
	select {
	case timer.ticks <- clock.now():
	default:
		// The heartbeat loop is stopped and joined by now; the tick is
		// only a probe that it emits nothing, so a full tick buffer must
		// not wedge the test.
	}
	select {
	case line := <-writes:
		t.Fatalf("debug line emitted after final status: %q", line)
	case <-time.After(50 * time.Millisecond):
	}
	if got := len(debugBodies(t, writer.buf.String())); got != before {
		t.Fatalf("debug output changed after final status: %d lines, want %d", got, before)
	}
}

func TestWorkerDebugLinesCarryRequestedIdentity(t *testing.T) {
	// The worker identity in the request labels every debug line; it is
	// operational metadata only and never appears in results.
	setupFakePiEnv(t, happyPathScript("identity answer"))
	var debugOut bytes.Buffer
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
		WorkerID:  2,
		Debug:     NewDebugSink(&debugOut),
	})
	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	out := debugOut.String()
	if !strings.Contains(out, "worker=2 phase=starting provider=acme model=m-1") {
		t.Fatalf("debug missing identity-2 starting line:\n%s", out)
	}
	if !strings.Contains(out, "worker=2 status=completed total=") {
		t.Fatalf("debug missing identity-2 completion line:\n%s", out)
	}
	if strings.Contains(out, "worker=1 ") {
		t.Fatalf("identity-2 run emitted worker=1 lines:\n%s", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Count(line, "worker=") != 1 {
			t.Fatalf("line %q carries %d worker labels", line, strings.Count(line, "worker="))
		}
	}
}

func TestWorkerCompletesAndRetrievesFinalText(t *testing.T) {
	logPath := setupFakePiEnv(t, happyPathScript("The answer is 42."))
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "solve it",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if result.Model != "acme/m-1" {
		t.Fatalf("model = %q", result.Model)
	}
	if result.Explanation != "The answer is 42." {
		t.Fatalf("explanation = %q", result.Explanation)
	}
	if result.Error != "" {
		t.Fatalf("error = %q", result.Error)
	}

	types := waitRequestLog(t, logPath, 5)
	want := []string{"get_available_models", "set_model", "get_state", "prompt", "get_last_assistant_text"}
	if !slices.Equal(types, want) {
		t.Fatalf("request log = %v, want %v", types, want)
	}
	if slices.Contains(types, "bash") {
		t.Fatalf("worker emitted a direct rpc bash request")
	}
}

func TestWorkerOmittedThinkingReportsConfirmedDefault(t *testing.T) {
	logPath := setupFakePiEnv(t, happyPathScript("default answer"))
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusCompleted || result.ThinkingLevel != ThinkingMedium {
		t.Fatalf("result = %#v, want completed medium", result)
	}
	if result.RequestedThinkingLevel != "" || result.ThinkingFallback || result.Warning != "" {
		t.Fatalf("omitted thinking metadata = %#v", result)
	}
	want := []string{"get_available_models", "set_model", "get_state", "prompt", "get_last_assistant_text"}
	if got := waitRequestLog(t, logPath, len(want)); !slices.Equal(got, want) {
		t.Fatalf("request log = %v, want %v", got, want)
	}
}

func TestWorkerRejectsMismatchedActiveModelConfirmation(t *testing.T) {
	scriptConfig := happyPathScript("must not be returned")
	scriptConfig.Triggers["get_state"] = []script.Step{{Response: &script.Response{
		Success: true,
		Data:    json.RawMessage(`{"model":{"provider":"acme","id":"m-2"},"thinkingLevel":"medium"}`),
	}}}
	logPath := setupFakePiEnv(t, scriptConfig)

	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusError || result.Error != "protocol error: get_state confirmed a different active model" {
		t.Fatalf("result = %#v, want exact active-model confirmation protocol error", result)
	}
	types := waitRequestLog(t, logPath, 3)
	if slices.Contains(types, "prompt") {
		t.Fatalf("request log = %v; mismatched active model must stop before prompt", types)
	}
}

func TestWorkerExplicitThinkingAppliesAndConfirms(t *testing.T) {
	scriptConfig := happyPathScript("max answer")
	scriptConfig.TriggerSequences = map[string][][]script.Step{
		"get_state": {
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium"}`)}}},
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"max"}`)}}},
		},
	}
	scriptConfig.Triggers["get_available_thinking_levels"] = []script.Step{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"levels":["off","medium","max"]}`)}}}
	scriptConfig.Triggers["set_thinking_level"] = []script.Step{{Response: &script.Response{Success: true}}}
	logPath := setupFakePiEnv(t, scriptConfig)

	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:         "acme/m-1",
		ThinkingLevel: ThinkingMax,
		Prompt:        "go",
		Workspace:     t.TempDir(),
	})

	if result.Status != StatusCompleted || result.RequestedThinkingLevel != ThinkingMax || result.ThinkingLevel != ThinkingMax {
		t.Fatalf("result = %#v, want completed explicit max", result)
	}
	if result.ThinkingFallback || result.Warning != "" {
		t.Fatalf("explicit max unexpectedly fell back: %#v", result)
	}
	want := []string{"get_available_models", "set_model", "get_state", "get_available_thinking_levels", "set_thinking_level", "get_state", "prompt", "get_last_assistant_text"}
	if got := waitRequestLog(t, logPath, len(want)); !slices.Equal(got, want) {
		t.Fatalf("request log = %v, want %v", got, want)
	}
}

func TestWorkerThinkingUnavailableFallsBackToConfirmedDefault(t *testing.T) {
	scriptConfig := happyPathScript("fallback answer")
	scriptConfig.Triggers["get_available_thinking_levels"] = []script.Step{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"levels":["off","medium"]}`)}}}
	logPath := setupFakePiEnv(t, scriptConfig)

	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:         "acme/m-1",
		ThinkingLevel: ThinkingMax,
		Prompt:        "go",
		Workspace:     t.TempDir(),
	})

	if result.Status != StatusCompleted || result.ThinkingLevel != ThinkingMedium || !result.ThinkingFallback {
		t.Fatalf("result = %#v, want completed fallback to medium", result)
	}
	wantWarning := "requested thinking=max unavailable; continuing with Pi default thinking=medium"
	if result.Warning != wantWarning {
		t.Fatalf("warning = %q, want %q", result.Warning, wantWarning)
	}
	want := []string{"get_available_models", "set_model", "get_state", "get_available_thinking_levels", "prompt", "get_last_assistant_text"}
	if got := waitRequestLog(t, logPath, len(want)); !slices.Equal(got, want) {
		t.Fatalf("request log = %v, want %v", got, want)
	}
}

func TestWorkerThinkingRejectionFallsBackOnlyWhenDefaultUnchanged(t *testing.T) {
	tests := []struct {
		name       string
		finalLevel string
		wantStatus string
		wantPrompt bool
	}{
		{name: "unchanged default", finalLevel: "medium", wantStatus: StatusCompleted, wantPrompt: true},
		{name: "changed after rejection", finalLevel: "high", wantStatus: StatusError, wantPrompt: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scriptConfig := happyPathScript("answer")
			scriptConfig.TriggerSequences = map[string][][]script.Step{
				"get_state": {
					{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium"}`)}}},
					{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"` + test.finalLevel + `"}`)}}},
				},
			}
			scriptConfig.Triggers["get_available_thinking_levels"] = []script.Step{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"levels":["medium","max"]}`)}}}
			scriptConfig.Triggers["set_thinking_level"] = []script.Step{{Response: &script.Response{Success: false, Error: "SECRET-REJECTION"}}}
			logPath := setupFakePiEnv(t, scriptConfig)

			result := New(fakePiBin).Run(context.Background(), WorkerRequest{
				Model:         "acme/m-1",
				ThinkingLevel: ThinkingMax,
				Prompt:        "go",
				Workspace:     t.TempDir(),
			})

			if result.Status != test.wantStatus {
				t.Fatalf("result = %#v, want status %q", result, test.wantStatus)
			}
			if strings.Contains(result.Warning+result.Error, "SECRET-REJECTION") {
				t.Fatalf("result leaked upstream rejection: %#v", result)
			}
			if test.wantPrompt {
				if !result.ThinkingFallback || result.ThinkingLevel != ThinkingMedium {
					t.Fatalf("result = %#v, want fallback to medium", result)
				}
			} else if result.ThinkingFallback {
				t.Fatalf("mismatched state must not be marked as fallback: %#v", result)
			}
			types := waitRequestLog(t, logPath, 6)
			if slices.Contains(types, "prompt") != test.wantPrompt {
				t.Fatalf("request log = %v, wantPrompt=%v", types, test.wantPrompt)
			}
		})
	}
}

func TestWorkerThinkingConfirmationMismatchStopsBeforePrompt(t *testing.T) {
	scriptConfig := happyPathScript("must not be returned")
	scriptConfig.TriggerSequences = map[string][][]script.Step{
		"get_state": {
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium"}`)}}},
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"high"}`)}}},
		},
	}
	scriptConfig.Triggers["get_available_thinking_levels"] = []script.Step{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"levels":["medium","max"]}`)}}}
	scriptConfig.Triggers["set_thinking_level"] = []script.Step{{Response: &script.Response{Success: true}}}
	logPath := setupFakePiEnv(t, scriptConfig)

	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:         "acme/m-1",
		ThinkingLevel: ThinkingMax,
		Prompt:        "go",
		Workspace:     t.TempDir(),
	})

	if result.Status != StatusError || result.ThinkingFallback {
		t.Fatalf("result = %#v, want hard confirmation failure", result)
	}
	types := waitRequestLog(t, logPath, 6)
	if slices.Contains(types, "prompt") {
		t.Fatalf("request log = %v; confirmation mismatch must stop before prompt", types)
	}
}

func TestWorkerThinkingProtocolFailureDoesNotFallbackOrPrompt(t *testing.T) {
	scriptConfig := happyPathScript("must not be returned")
	scriptConfig.Triggers["get_available_thinking_levels"] = []script.Step{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"levels":["ultra"]}`)}}}
	logPath := setupFakePiEnv(t, scriptConfig)

	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:         "acme/m-1",
		ThinkingLevel: ThinkingMax,
		Prompt:        "go",
		Workspace:     t.TempDir(),
	})

	if result.Status != StatusError || result.ThinkingFallback || result.Warning != "" {
		t.Fatalf("result = %#v, want hard protocol failure", result)
	}
	types := waitRequestLog(t, logPath, 4)
	if slices.Contains(types, "prompt") {
		t.Fatalf("request log = %v; malformed thinking catalog must stop before prompt", types)
	}
}

func TestWorkerTaskFailureRetainsConfirmedThinkingMetadata(t *testing.T) {
	scriptConfig := happyPathScript("unused")
	scriptConfig.TriggerSequences = map[string][][]script.Step{
		"get_state": {
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium"}`)}}},
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"max"}`)}}},
		},
	}
	scriptConfig.Triggers["get_available_thinking_levels"] = []script.Step{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"levels":["medium","max"]}`)}}}
	scriptConfig.Triggers["set_thinking_level"] = []script.Step{{Response: &script.Response{Success: true}}}
	scriptConfig.Triggers["prompt"] = []script.Step{{Response: &script.Response{Success: false, Error: "task rejected"}}}
	setupFakePiEnv(t, scriptConfig)

	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:         "acme/m-1",
		ThinkingLevel: ThinkingMax,
		Prompt:        "go",
		Workspace:     t.TempDir(),
	})

	if result.Status != StatusFailed || result.RequestedThinkingLevel != ThinkingMax || result.ThinkingLevel != ThinkingMax {
		t.Fatalf("result = %#v, want failed task with confirmed max metadata", result)
	}
}

func TestWorkerWaitsForSettledPastAgentEnd(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"agent_end","messages":[],"willRetry":false}`)},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"after settle"}`)}},
		},
	}}
	setupFakePiEnv(t, script)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})
	if result.Status != StatusCompleted || result.Explanation != "after settle" {
		t.Fatalf("result = %#v", result)
	}
}

func TestWorkerModelUnavailableNeverFallsBack(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"other","id":"x"}]}`)}},
		},
	}}
	logPath := setupFakePiEnv(t, script)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusUnavailable {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "acme/m-1") || !strings.Contains(result.Error, "no fallback") {
		t.Fatalf("error = %q", result.Error)
	}
	types := waitRequestLog(t, logPath, 1)
	if !slices.Equal(types, []string{"get_available_models"}) {
		t.Fatalf("request log = %v; worker must stop before set_model/prompt", types)
	}
}

func TestWorkerModelUnavailableIsCatalogAnswerNotNameFormatAnswer(t *testing.T) {
	// An invented name is refused by the catalog-membership check, not by
	// a name-format rule: a colon in the id is not a format error, so a
	// colon-carrying name that the catalog does not offer must fail with
	// the not-in-the-available-catalog answer (exit 3 path), never an
	// invalid-model-selector format error (exit 5 path). The name-format
	// rule accepts the shape, and the catalog is the authority.
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
	}}
	logPath := setupFakePiEnv(t, script)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1:free",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable; error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "not in the available catalog") {
		t.Fatalf("error = %q, want the catalog-membership answer", result.Error)
	}
	if strings.Contains(result.Error, "invalid model selector") {
		t.Fatalf("error = %q, want no name-format answer", result.Error)
	}
	types := waitRequestLog(t, logPath, 1)
	if !slices.Equal(types, []string{"get_available_models"}) {
		t.Fatalf("request log = %v; worker must stop before set_model/prompt", types)
	}
}

func TestWorkerRunsCatalogColonIdEntry(t *testing.T) {
	// A routing-provider catalog offers "acme/m-1:free" next to
	// "acme/m-1". The name is in the catalog, so the worker must split it
	// at the first slash and request exactly the pair the catalog reports:
	// the id is opaque content, not a thinking suffix to strip or refuse.
	scriptConfig := happyPathScript("colon id answer")
	scriptConfig.Triggers["get_available_models"] = []script.Step{
		{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"},{"provider":"acme","id":"m-1:free"}]}`)}},
	}
	scriptConfig.Triggers["set_model"] = []script.Step{
		{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1:free"}`)}},
	}
	scriptConfig.Triggers["get_state"] = []script.Step{
		{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1:free"},"thinkingLevel":"medium"}`)}},
	}
	logPath := setupFakePiEnv(t, scriptConfig)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1:free",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if result.Explanation != "colon id answer" {
		t.Fatalf("explanation = %q", result.Explanation)
	}
	if result.Model != "acme/m-1:free" {
		t.Fatalf("model = %q", result.Model)
	}
	want := []string{"get_available_models", "set_model", "get_state", "prompt", "get_last_assistant_text"}
	if got := waitRequestLog(t, logPath, len(want)); !slices.Equal(got, want) {
		t.Fatalf("request log = %v, want %v", got, want)
	}
}

func TestWorkerInvalidCatalogContainerIsProtocolError(t *testing.T) {
	// A success response with a missing data container is a protocol
	// violation (exit 9 path), not a model-unavailable/readiness failure
	// (exit 3 path).
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true}},
		},
	}}
	setupFakePiEnv(t, script)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})
	if result.Status != StatusError {
		t.Fatalf("status = %q, want error; error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "protocol error") {
		t.Fatalf("error = %q, want protocol classification", result.Error)
	}
}

func TestWorkerExplicitEmptyCatalogIsModelUnavailable(t *testing.T) {
	// An explicitly present empty models array is a valid catalog: the
	// requested model is simply unavailable (exit 3 path), never a
	// protocol error.
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[]}`)}},
		},
	}}
	setupFakePiEnv(t, script)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})
	if result.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable; error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "not in the available catalog") {
		t.Fatalf("error = %q", result.Error)
	}
}

func TestWorkerCatalogReadinessFailure(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: false, Error: "authentication required"}},
		},
	}}
	setupFakePiEnv(t, script)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})
	if result.Status != StatusUnavailable {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "authentication required") {
		t.Fatalf("error = %q", result.Error)
	}
}

func TestWorkerSetModelFailureIsReadiness(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: false, Error: "provider not configured"}},
		},
	}}
	setupFakePiEnv(t, script)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})
	if result.Status != StatusUnavailable {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "provider not configured") {
		t.Fatalf("error = %q", result.Error)
	}
}

func TestWorkerPromptRejectedIsTaskFailure(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: false, Error: "prompt rejected"}},
		},
	}}
	setupFakePiEnv(t, script)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "prompt rejected") {
		t.Fatalf("error = %q", result.Error)
	}
}

func TestWorkerNoFinalTextIsTaskFailure(t *testing.T) {
	// An explicit text:null means no assistant text exists; the worker
	// reports a task failure (exit 5 path), not a protocol error.

	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":null}`)}},
		},
	}}
	setupFakePiEnv(t, script)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "final text") {
		t.Fatalf("error = %q", result.Error)
	}
}

func TestWorkerOmittedTextFieldIsTaskFailure(t *testing.T) {
	// Observed Pi 0.84.1 behavior: when no assistant text exists, the
	// server serializes the undefined value as an omitted text key
	// (data:{}), not as text:null. A model error that settles without
	// content must classify as a task failure (exit 5 path), never as a
	// protocol violation (exit 9 path).
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{}`)}},
		},
	}}
	setupFakePiEnv(t, script)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want failed; error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "final text") {
		t.Fatalf("error = %q", result.Error)
	}
}

// TestWorkerAssistantErrorWithTextRemainsFailed drives a settled assistant
// error that also emitted text. The text is evidence from the failed turn,
// not a final explanation, and the upstream errorMessage is never projected.
func TestWorkerAssistantErrorWithTextRemainsFailed(t *testing.T) {
	const upstreamSecret = "UPSTREAM-ERROR-SECRET-7a2f"
	scriptConfig := happyPathScript("partial evidence")
	scriptConfig.Triggers["prompt"] = []script.Step{
		{Response: &script.Response{Success: true}},
		{Event: json.RawMessage(`{"type":"message_start","message":{"role":"assistant","content":[]}}`)},
		{Event: json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"partial evidence"}}`)},
		{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"partial evidence"}],"stopReason":"error","errorMessage":"` + upstreamSecret + `"}}`)},
		{Event: json.RawMessage(`{"type":"agent_end","messages":[],"willRetry":false}`)},
		{Event: json.RawMessage(`{"type":"agent_settled"}`)},
	}
	setupFakePiEnv(t, scriptConfig)
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want failed; result = %#v", result.Status, result)
	}
	if result.Error != "upstream/model turn ended with an error" {
		t.Fatalf("error = %q, want stable upstream/model wording", result.Error)
	}
	if result.Explanation != "" || result.PartialExplanation != "partial evidence" {
		t.Fatalf("result = %#v, want partial evidence without final explanation", result)
	}
	if strings.Contains(result.Error, upstreamSecret) {
		t.Fatalf("error leaked errorMessage: %q", result.Error)
	}
}

func TestWorkerPartialExplanationUsesOneSharedUTF8ByteBudget(t *testing.T) {
	// These are many individually legal deltas, not one oversized frame. The
	// first assistant message and the newer in-flight message together exceed
	// MaxFrameBytes, so the result seam must still return bounded valid UTF-8.
	chunk := strings.Repeat("界", 4096) // exactly 12 KiB of UTF-8
	firstDeltas := MaxFrameBytes / (2 * len(chunk))
	currentDeltas := MaxFrameBytes/len(chunk) + 1
	want := strings.Repeat("界", (MaxFrameBytes-2)/utf8.RuneLen('界'))
	promptSteps := []script.Step{
		{Response: &script.Response{Success: true}},
		{Event: json.RawMessage(`{"type":"message_start","message":{"role":"assistant","content":[]}}`)},
	}
	for i := 0; i < firstDeltas; i++ {
		data, err := json.Marshal(map[string]any{
			"type":                  "message_update",
			"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": chunk},
		})
		if err != nil {
			t.Fatalf("marshal first delta %d: %v", i, err)
		}
		promptSteps = append(promptSteps, script.Step{Event: data})
	}
	promptSteps = append(promptSteps, script.Step{Event: json.RawMessage(`{"type":"message_start","message":{"role":"toolResult"}}`)})
	for i := 0; i < currentDeltas; i++ {
		data, err := json.Marshal(map[string]any{
			"type":                  "message_update",
			"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": chunk},
		})
		if err != nil {
			t.Fatalf("marshal current delta %d: %v", i, err)
		}
		promptSteps = append(promptSteps, script.Step{Event: data})
	}
	// Do not emit agent_settled: EOF returns through withThinking, making
	// this exercise the worker/result seam rather than final text. Exit only
	// after all deltas have flushed so the result is deterministic and quick.
	promptSteps = append(promptSteps, script.Step{Exit: true})
	scriptConfig := happyPathScript("unused")
	scriptConfig.Triggers["prompt"] = promptSteps
	setupFakePiEnv(t, scriptConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := New(fakePiBin).Run(ctx, WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusError {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if result.Explanation != "" {
		t.Fatalf("explanation = %q, want absent on interrupted run", result.Explanation)
	}
	if len(result.PartialExplanation) != len(want) {
		t.Fatalf("partialExplanation = %d bytes, want exact bounded size %d", len(result.PartialExplanation), len(want))
	}
	if result.PartialExplanation != want {
		t.Fatal("partialExplanation did not preserve the most recent UTF-8 suffix")
	}
	if len(result.PartialExplanation) > MaxFrameBytes || !utf8.ValidString(result.PartialExplanation) {
		t.Fatalf("partialExplanation violates the %d-byte UTF-8 bound", MaxFrameBytes)
	}
}

func TestWorkerTimeoutCleansUpProcessAndSession(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{SleepMS: 10000},
		},
	}}
	setupFakePiEnv(t, script)
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	result := New(fakePiBin).Run(ctx, WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusTimedOut {
		t.Fatalf("status = %q, want timed-out; error = %q", result.Status, result.Error)
	}
	if result.Error == "" {
		t.Fatalf("timed-out result must carry an error message")
	}
	if _, err := os.Stat(sessionDirFromMeta(t, metaPath)); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after timeout: %v", err)
	}
}

func TestWorkerCancellationCleansUp(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{SleepMS: 10000},
		},
	}}
	setupFakePiEnv(t, script)
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(150*time.Millisecond, cancel)
	defer timer.Stop()
	result := New(fakePiBin).Run(ctx, WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled; error = %q", result.Status, result.Error)
	}
	if _, err := os.Stat(sessionDirFromMeta(t, metaPath)); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after cancellation: %v", err)
	}
}

func TestWorkerAlreadyExpiredDeadlineIsTimedOutWithoutLaunchingPi(t *testing.T) {
	// An already-expired deadline must yield StatusTimedOut (exit 7 path)
	// and must not launch the host executable at all. Regression: the
	// pre-launch ctx.Err() check used to map DeadlineExceeded to cancelled.
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	result := New(fakePiBin).Run(ctx, WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusTimedOut {
		t.Fatalf("status = %q, want timed-out; error = %q", result.Status, result.Error)
	}
	if result.Error == "" {
		t.Fatalf("timed-out result must carry an error message")
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("pi was launched for an already-expired run")
	}
}

// deadlineBeforeStartContext is a context whose first Err() call (the
// worker's pre-launch check) reports nil while its Done channel is already
// closed. The child launch then fails deterministically with
// DeadlineExceeded before spawning anything, exercising the start-failure
// classification without sleeping.
type deadlineBeforeStartContext struct {
	context.Context
	mu    sync.Mutex
	calls int
}

func (c *deadlineBeforeStartContext) Done() <-chan struct{} {
	closed := make(chan struct{})
	close(closed)
	return closed
}

func (c *deadlineBeforeStartContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= 2 {
		return context.DeadlineExceeded
	}
	return nil
}

func TestWorkerStartFailureFromDeadlineExceededIsTimedOut(t *testing.T) {
	// A start failure caused by DeadlineExceeded must yield StatusTimedOut
	// (exit 7 path), not cancelled or unavailable. Regression: the
	// proc.Start error path used to map DeadlineExceeded to cancelled.
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)
	ctx := &deadlineBeforeStartContext{Context: context.Background()}
	result := New(fakePiBin).Run(ctx, WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusTimedOut {
		t.Fatalf("status = %q, want timed-out; error = %q", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "timed out") {
		t.Fatalf("error = %q, want timed-out detail", result.Error)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("pi was launched for a start that failed with DeadlineExceeded")
	}
}

func TestWorkerAlreadyCancelledContextIsCancelledWithoutLaunchingPi(t *testing.T) {
	// An already-cancelled context must yield StatusCancelled (exit 8
	// path) and must not launch the host executable at all. Explicit
	// cancellation stays cancelled even after the regression fix.
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := New(fakePiBin).Run(ctx, WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled; error = %q", result.Status, result.Error)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("pi was launched for an already-cancelled run")
	}
}

func TestWorkerRejectsInvalidInputWithoutLaunchingPi(t *testing.T) {
	worker := New(fakePiBin)
	workspace := t.TempDir()
	tests := []struct {
		name string
		req  WorkerRequest
	}{
		{"empty prompt", WorkerRequest{Model: "acme/m-1", Workspace: workspace}},
		{"empty model", WorkerRequest{Prompt: "go", Workspace: workspace}},
		{"empty workspace", WorkerRequest{Model: "acme/m-1", Prompt: "go"}},
		{"malformed model", WorkerRequest{Model: "acme", Prompt: "go", Workspace: workspace}},
		{"model without id", WorkerRequest{Model: "acme/", Prompt: "go", Workspace: workspace}},
		{"model without provider", WorkerRequest{Model: "/m-1", Prompt: "go", Workspace: workspace}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := worker.Run(context.Background(), test.req)
			if result.Status != StatusFailed {
				t.Fatalf("status = %q, want failed; error = %q", result.Status, result.Error)
			}
			if result.Error == "" {
				t.Fatalf("error must not be empty")
			}
		})
	}
}
