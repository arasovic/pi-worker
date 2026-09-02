package pi

import (
	"strings"
	"unicode"
)

// ExactModelSelector returns the exact provider/model selector for valid
// catalog components. Components are deliberately opaque apart from the
// separators and whitespace that would make the selector ambiguous.
//
// The provider may not contain a slash but the id may: a catalog reached
// through a routing provider reports ids that carry their own upstream
// provider, so the pair is a routing name plus "upstream/model". Splitting on
// the first slash keeps every selector unambiguous, which is only true while
// exactly one side may contain one.
func ExactModelSelector(provider, id string) (string, bool) {
	if provider == "" || id == "" || strings.ContainsAny(provider, "/:") || strings.Contains(id, ":") {
		return "", false
	}
	if strings.IndexFunc(provider, unicode.IsSpace) >= 0 || strings.IndexFunc(id, unicode.IsSpace) >= 0 {
		return "", false
	}
	return provider + "/" + id, true
}
