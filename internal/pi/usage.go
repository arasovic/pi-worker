package pi

import (
	"encoding/json"
)

// Usage mirrors Pi's per-message usage measurement field for field. It is
// a named passthrough: the token counts and dollar figures are Pi's own
// numbers, copied without conversion, and pi-worker derives no price of
// its own. Cost is in US dollars as Pi computed it. Input, Output,
// CacheRead, CacheWrite, TotalTokens, and Cost are always present;
// CacheWrite1h and Reasoning are present only when some message reported
// them.
type Usage struct {
	Input        int       `json:"input"`
	Output       int       `json:"output"`
	CacheRead    int       `json:"cacheRead"`
	CacheWrite   int       `json:"cacheWrite"`
	CacheWrite1h *int      `json:"cacheWrite1h,omitempty"`
	Reasoning    *int      `json:"reasoning,omitempty"`
	TotalTokens  int       `json:"totalTokens"`
	Cost         UsageCost `json:"cost"`
}

// UsageCost mirrors Pi's cost sub-object: the per-channel and total dollar
// figures Pi computed, passed through unchanged.
type UsageCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// usageAccumulator is the worker's usage measurement. It implements
// EventHandler, summing the usage of every assistant message whose
// assistantMessageEvent.type is "done" or "error". Every frame of a
// message carries that message's cumulative usage so far, so reading any
// earlier frame would double-count: the terminal frame alone carries the
// message's final usage and is the only one read. The client calls
// OnEvent from its single driving goroutine, matching the client's
// single-flight contract, so the accumulator needs no locking.
type usageAccumulator struct {
	total Usage
	seen  bool
}

// OnEvent sums one terminal message frame's usage. It never returns an
// error: per the EventHandler contract an error is a protocol violation
// that fails the whole client, and a measurement problem must never fail
// a run that otherwise worked. A frame that is not a message_update, is
// not terminal, or carries missing, null, or unparseable usage is skipped
// silently; a malformed frame is a reported omission, never a failure.
func (a *usageAccumulator) OnEvent(event Event) error {
	if event.Type != "message_update" {
		return nil
	}
	var frame struct {
		Usage                 json.RawMessage `json:"usage"`
		AssistantMessageEvent struct {
			Type string `json:"type"`
		} `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal(event.Raw, &frame); err != nil {
		return nil
	}
	switch frame.AssistantMessageEvent.Type {
	case "done", "error":
	default:
		return nil
	}
	if len(frame.Usage) == 0 || isJSONNull(frame.Usage) {
		return nil
	}
	var usage Usage
	if err := json.Unmarshal(frame.Usage, &usage); err != nil {
		return nil
	}
	a.add(usage)
	a.seen = true
	return nil
}

// add sums one message's reported usage into the running total. The
// optional fields are summed only when a message reported them, so an
// absent optional field stays absent — never a zero pi-worker invented.
func (a *usageAccumulator) add(usage Usage) {
	a.total.Input += usage.Input
	a.total.Output += usage.Output
	a.total.CacheRead += usage.CacheRead
	a.total.CacheWrite += usage.CacheWrite
	a.total.TotalTokens += usage.TotalTokens
	a.total.Cost.Input += usage.Cost.Input
	a.total.Cost.Output += usage.Cost.Output
	a.total.Cost.CacheRead += usage.Cost.CacheRead
	a.total.Cost.CacheWrite += usage.Cost.CacheWrite
	a.total.Cost.Total += usage.Cost.Total
	if usage.CacheWrite1h != nil {
		if a.total.CacheWrite1h == nil {
			value := *usage.CacheWrite1h
			a.total.CacheWrite1h = &value
		} else {
			*a.total.CacheWrite1h += *usage.CacheWrite1h
		}
	}
	if usage.Reasoning != nil {
		if a.total.Reasoning == nil {
			value := *usage.Reasoning
			a.total.Reasoning = &value
		} else {
			*a.total.Reasoning += *usage.Reasoning
		}
	}
}

// snapshot returns the summed usage, or nil when no terminal frame was
// observed. Nil means "never measured"; a non-nil all-zero value means
// "measured, and it was free". The two claims stay distinguishable.
func (a *usageAccumulator) snapshot() *Usage {
	if !a.seen {
		return nil
	}
	total := a.total
	return &total
}
