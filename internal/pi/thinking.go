package pi

import "fmt"

// ThinkingLevel is one Pi 0.84.1 reasoning-effort value.
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

// ParseThinkingLevel accepts only the exact Pi 0.84.1 thinking vocabulary.
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
