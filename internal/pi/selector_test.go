package pi

import "testing"

// A routing provider reports ids that carry their own upstream provider, so an
// id may contain a slash while a provider may not. That asymmetry is what
// keeps a selector unambiguous: the first slash is always the separator, so
// "a/b/c" can only ever mean provider "a" and id "b/c". Allowing a slash on
// both sides would make the same string mean two different models.
func TestExactModelSelectorSlashRules(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		id       string
		want     string
	}{
		{name: "plain", provider: "acme", id: "model", want: "acme/model"},
		{name: "routing prefix in id", provider: "acme", id: "upstream/model", want: "acme/upstream/model"},
		{name: "two prefixes in id", provider: "acme", id: "a/b/model", want: "acme/a/b/model"},
		{name: "slash in provider", provider: "ac/me", id: "model", want: ""},
		{name: "slash in both", provider: "ac/me", id: "up/model", want: ""},
		{name: "colon in id", provider: "acme", id: "model:thinking", want: ""},
		{name: "space in id", provider: "acme", id: "up/mo del", want: ""},
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
