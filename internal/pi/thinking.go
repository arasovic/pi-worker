package pi

import (
	"fmt"
	"slices"
)

// ThinkingLevel is one Pi reasoning-effort value.
type ThinkingLevel string

const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
	ThinkingMax     ThinkingLevel = "max"
)

// thinkingLevelOrder lists Pi's thinking vocabulary from least to most reasoning.
var thinkingLevelOrder = []ThinkingLevel{ThinkingOff, ThinkingMinimal, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh, ThinkingMax}

// ParseThinkingLevel accepts only the exact Pi thinking vocabulary.
func ParseThinkingLevel(value string) (ThinkingLevel, bool) {
	level := ThinkingLevel(value)
	switch level {
	case ThinkingOff, ThinkingMinimal, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh, ThinkingMax:
		return level, true
	default:
		return "", false
	}
}

// SessionState is the narrow Pi state projection required to confirm the
// active exact model and effective thinking level.
type SessionState struct {
	Model         ModelProjection
	ThinkingLevel ThinkingLevel
}

// ThinkingLevelRejectedError reports a well-formed Pi rejection of an exact
// thinking level. Worker policy may recover only by retaining the previously
// confirmed Pi default; protocol and transport failures use different errors.
type ThinkingLevelRejectedError struct {
	Level ThinkingLevel
}

func (e *ThinkingLevelRejectedError) Error() string {
	return fmt.Sprintf("thinking level rejected: %s", e.Level)
}

type thinkingOutcome struct {
	requested ThinkingLevel
	effective ThinkingLevel
	fallback  bool
	warning   string
}

func (o thinkingOutcome) apply(result WorkerResult) WorkerResult {
	result.RequestedThinkingLevel = o.requested
	result.ThinkingLevel = o.effective
	result.ThinkingFallback = o.fallback
	result.Warning = o.warning
	return result
}

func thinkingLevelsContain(levels []ThinkingLevel, requested ThinkingLevel) bool {
	return slices.Contains(levels, requested)
}

// lowestThinkingLevel returns the least-reasoning level among levels, by
// thinkingLevelOrder rather than by the order Pi listed them, or false when
// levels holds none of the vocabulary.
func lowestThinkingLevel(levels []ThinkingLevel) (ThinkingLevel, bool) {
	for _, level := range thinkingLevelOrder {
		if slices.Contains(levels, level) {
			return level, true
		}
	}
	return "", false
}

func thinkingFallbackWarning(requested, effective ThinkingLevel, reason string) string {
	return fmt.Sprintf("requested thinking=%s %s; continuing with Pi default thinking=%s", requested, reason, effective)
}

func validateStateModel(state SessionState, provider, id string) error {
	if state.Model.Provider != provider || state.Model.ID != id {
		return newProtocolError("get_state confirmed a different active model")
	}
	return nil
}
