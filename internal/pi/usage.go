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
// EventHandler, summing the usage of every assistant message. The
// message_update subtypes observed on the wire against Pi 0.84.4 are
// thinking_start, thinking_delta, thinking_end, text_start, text_delta,
// text_end, toolcall_start, toolcall_delta, and toolcall_end — that is
// what has been observed, not a closed set — and no "done" or "error"
// subtype is ever forwarded by the RPC transport. Numbers appear on the
// end frame of each content block (thinking_end, toolcall_end, text_end),
// and a message can carry more than one such frame; each carries the
// message's cumulative usage so far, so the latest one reported is the
// message's figure and summing the frames would double-count what was
// already cumulative. The accumulator therefore names no subtype, so a
// subtype not yet observed cannot break it. It remembers each message's
// latest reported usage and commits it once, on the message_start and
// message_end boundaries, so a message is counted exactly once whether
// its end frame arrives or not. The client calls OnEvent from its single
// driving goroutine, matching the client's single-flight contract, so the
// accumulator needs no locking.
type usageAccumulator struct {
	total   Usage
	pending *Usage
}

// OnEvent tracks one message-boundary or usage frame. It never returns an
// error: per the EventHandler contract an error is a protocol violation
// that fails the whole client, and a measurement problem must never fail
// a run that otherwise worked. On message_start and message_end the
// message's latest reported usage is committed into the total and
// forgotten, so a message whose end never arrives (a timed-out or
// cancelled run) still contributes what it reported. A message_update
// whose usage is missing, null, unparseable, or negative is skipped
// silently — a malformed frame is a reported omission, never a failure —
// and a frame with numbers replaces whatever this message reported
// before: the last reported usage is the message's final figure.
func (a *usageAccumulator) OnEvent(event Event) error {
	switch event.Type {
	case "message_start", "message_end":
		a.commit()
	case "message_update":
		var frame struct {
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(event.Raw, &frame); err != nil {
			return nil
		}
		if len(frame.Usage) == 0 || isJSONNull(frame.Usage) {
			return nil
		}
		var usage Usage
		if err := json.Unmarshal(frame.Usage, &usage); err != nil {
			return nil
		}
		if usageNegative(usage) {
			return nil
		}
		a.pending = &usage
	}
	return nil
}

// commit folds the pending message usage into the running total and
// forgets it. The message boundary events call it on both sides of a
// message, and snapshot calls it once more before returning, so a message
// with no observed end still contributes exactly once.
func (a *usageAccumulator) commit() {
	if a.pending == nil {
		return
	}
	a.add(*a.pending)
	a.pending = nil
}

// usageNegative reports whether any field of a reported usage is a
// negative number. A negative count or cost is not a measurement.
func usageNegative(usage Usage) bool {
	if usage.Input < 0 || usage.Output < 0 || usage.CacheRead < 0 || usage.CacheWrite < 0 || usage.TotalTokens < 0 {
		return true
	}
	if usage.CacheWrite1h != nil && *usage.CacheWrite1h < 0 {
		return true
	}
	if usage.Reasoning != nil && *usage.Reasoning < 0 {
		return true
	}
	return usage.Cost.Input < 0 || usage.Cost.Output < 0 || usage.Cost.CacheRead < 0 ||
		usage.Cost.CacheWrite < 0 || usage.Cost.Total < 0
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

// isZero reports whether every accumulated figure is zero: all token
// counts, all cost channels, and the optional fields absent or zero.
func (u Usage) isZero() bool {
	return u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 &&
		u.TotalTokens == 0 &&
		(u.CacheWrite1h == nil || *u.CacheWrite1h == 0) &&
		(u.Reasoning == nil || *u.Reasoning == 0) &&
		u.Cost.Input == 0 && u.Cost.Output == 0 && u.Cost.CacheRead == 0 &&
		u.Cost.CacheWrite == 0 && u.Cost.Total == 0
}

// snapshot returns the summed usage, or nil when nothing was measured.
// Nil means "never measured, or only zeros reported": a completed run
// cannot genuinely consume zero tokens — a prompt was sent — so an
// all-zero total means the provider reported nothing, and the field must
// be absent rather than claim a free run. The optional fields stay
// absent unless some message reported them.
func (a *usageAccumulator) snapshot() *Usage {
	a.commit()
	if a.total.isZero() {
		return nil
	}
	total := a.total
	return &total
}
