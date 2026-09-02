package pi

import "strings"

// ExactModelSelector returns the exact provider/model selector for a
// catalog pair, or reports that the pair cannot be named. The selector rule
// is structural only: a selector is one string that must split back into a
// non-empty provider and a non-empty id at its first slash, and nothing
// about either half is inspected beyond that. An id may carry a slash, a
// colon, whitespace, or anything else the catalog reports. The one
// asymmetry is deliberate: the provider is whatever precedes the first
// slash, so a provider that itself contains a slash can never be named,
// because the first slash always separates — which is exactly what keeps
// every selector unambiguous. Whether a name is usable is the catalog's
// answer, not this rule's: a name this rule accepts still has to be an
// entry the catalog offers.
func ExactModelSelector(provider, id string) (string, bool) {
	if provider == "" || id == "" || strings.Contains(provider, "/") {
		return "", false
	}
	return provider + "/" + id, true
}
