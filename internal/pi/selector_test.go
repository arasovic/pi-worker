package pi

import "testing"

// The selector rule is structural only: one string that splits at its first
// slash into a non-empty provider and a non-empty id. The id's contents are
// never inspected, so a catalog entry whose id carries a slash, a colon, or
// whitespace is nameable — the catalog is the authority on whether a name is
// usable, and 26 of 130 entries in a live routing-provider catalog were
// unreachable purely because their id carried a colon. The one asymmetry is
// deliberate and kept: the provider is whatever precedes the first slash, so
// a catalog entry whose provider itself contains a slash can never be named,
// because the first slash always separates — which is exactly what keeps
// every selector unambiguous.
func TestExactModelSelectorShapeRules(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		id       string
		want     string
	}{
		{name: "plain", provider: "acme", id: "model", want: "acme/model"},
		{name: "routing prefix in id", provider: "acme", id: "upstream/model", want: "acme/upstream/model"},
		{name: "two prefixes in id", provider: "acme", id: "a/b/model", want: "acme/a/b/model"},
		{name: "colon in id", provider: "acme", id: "model:thinking", want: "acme/model:thinking"},
		{name: "space in id", provider: "acme", id: "up/mo del", want: "acme/up/mo del"},
		{name: "slash in provider", provider: "ac/me", id: "model", want: ""},
		{name: "slash in both", provider: "ac/me", id: "up/model", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ExactModelSelector(test.provider, test.id)
			if ok != (test.want != "") {
				t.Fatalf("ExactModelSelector(%q, %q) ok = %v, want %v", test.provider, test.id, ok, test.want != "")
			}
			if got != test.want {
				t.Fatalf("ExactModelSelector(%q, %q) = %q, want %q", test.provider, test.id, got, test.want)
			}
		})
	}
}

// The selector must survive the round trip the worker performs, or a model the
// catalog offers cannot be requested back.
func TestSplitModelSelectorRoundTripsRoutingPrefix(t *testing.T) {
	provider, id, ok := splitModelSelector("acme/upstream/model")
	if !ok {
		t.Fatal("splitModelSelector rejected a selector the catalog can produce")
	}
	if provider != "acme" || id != "upstream/model" {
		t.Fatalf("split = (%q, %q), want (\"acme\", \"upstream/model\")", provider, id)
	}
}
