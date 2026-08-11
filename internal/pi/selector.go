package pi

import (
	"strings"
	"unicode"
)

// ExactModelSelector returns the exact provider/model selector for valid
// catalog components. Components are deliberately opaque apart from the
// separators and whitespace that would make the selector ambiguous.
func ExactModelSelector(provider, id string) (string, bool) {
	if provider == "" || id == "" || strings.ContainsAny(provider, "/:") || strings.ContainsAny(id, "/:") {
		return "", false
	}
	if strings.IndexFunc(provider, unicode.IsSpace) >= 0 || strings.IndexFunc(id, unicode.IsSpace) >= 0 {
		return "", false
	}
	return provider + "/" + id, true
}
