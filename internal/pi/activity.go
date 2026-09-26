package pi

import (
	"encoding/json"
	"time"
)

// activityInterval is the fixed silence bound between two activity reports
// when no tool starts. A tool start is already bounded by a model round
// trip and is always reported; every other event is reported at most once
// per interval. The value is a plain constant: there is no flag, config
// key, or tunable for it.
const activityInterval = 10 * time.Second

// activityAccumulator is the worker's liveness measurement. It implements
// EventHandler and reports a best-effort Activity while the run is in
// flight so a caller can tell a worker that is making progress from one
// that is stuck.
//
// On every Pi event it stamps LastEventAt. On a tool_execution_start it
// counts the call and projects the tool name through the fixed allowlist,
// then reports immediately: a tool start is bounded by a model round trip,
// and the reader must see the tool that is running now. Every other event
// is reported at most once per activityInterval — the first report when
// none was made yet, then one as soon as the interval has elapsed. There
// is no timer and no goroutine: the decision is made on the event path.
//
// The accumulator never returns an error: activity is a report, and a
// measurement problem must never fail a run that otherwise worked. A nil
// observer makes it do nothing observable. It is called from the client's
// single driving goroutine, matching the client's single-flight contract,
// so it needs no locking.
type activityAccumulator struct {
	workerID int
	observe  ActivityObserver
	// now is the clock seam; production passes time.Now, tests drive it.
	now      func() time.Time
	lastCall time.Time
	activity Activity
}

// OnEvent observes one Pi event, updates the activity it carries, and
// reports it under the tool-start / interval rule described on the type.
// It never returns an error.
func (a *activityAccumulator) OnEvent(event Event) error {
	now := a.now().UTC()
	a.activity.LastEventAt = now
	if event.Type == "tool_execution_start" {
		var projection struct {
			ToolName string `json:"toolName"`
		}
		// Malformed and unknown fields are opaque: a bad projection simply
		// reports the fixed unknown tool name. The raw name never travels.
		_ = json.Unmarshal(event.Raw, &projection)
		a.activity.ToolCalls++
		a.activity.LastTool = toolName(projection.ToolName)
		a.report(now)
		return nil
	}
	if a.lastCall.IsZero() || now.Sub(a.lastCall) >= activityInterval {
		a.report(now)
	}
	return nil
}

// report hands the current activity to the observer and records the call
// instant. A nil observer is a no-op: the handler then does nothing
// observable, though the retained activity still advances.
func (a *activityAccumulator) report(now time.Time) {
	if a.observe != nil {
		a.observe(a.workerID, a.activity)
	}
	a.lastCall = now
}
