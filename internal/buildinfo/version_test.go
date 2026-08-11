package buildinfo

import "testing"

func withBuildInfo(t *testing.T, version, commit, buildDate string) {
	t.Helper()
	oldVersion, oldCommit, oldBuildDate := Version, Commit, BuildDate
	Version, Commit, BuildDate = version, commit, buildDate
	t.Cleanup(func() {
		Version = oldVersion
		Commit = oldCommit
		BuildDate = oldBuildDate
	})
}

func TestCurrentReturnsSourceIdentity(t *testing.T) {
	withBuildInfo(t, "dev", "unknown", "unknown")
	if got := Current(); got != (Info{Version: "dev", Commit: "unknown", BuildDate: "unknown"}) {
		t.Fatalf("got = %#v, want version=dev commit=unknown buildDate=unknown", got)
	}
}

func TestCurrentReturnsValueCopy(t *testing.T) {
	withBuildInfo(t, "v0.1.0", "0123456789abcdef0123456789abcdef01234567", "2026-08-11T00:00:00Z")
	got := Current()
	got.Version = "mutated"
	if Version == "mutated" {
		t.Fatalf("Current() returned alias to global state")
	}
}

func TestInfoStringSourceBuild(t *testing.T) {
	info := Info{Version: "dev", Commit: "unknown", BuildDate: "unknown"}
	if got := info.String(); got != "dev" {
		t.Fatalf("info.String() = %q, want %q", got, "dev")
	}
}

func TestVersionStringForInjectedMetadata(t *testing.T) {
	info := Info{
		Version:   "v0.1.0",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildDate: "2026-08-11T00:00:00Z",
	}
	const want = "v0.1.0 (commit 0123456789abcdef0123456789abcdef01234567, built 2026-08-11T00:00:00Z)"
	if got := info.String(); got != want {
		t.Fatalf("info.String() = %q, want %q", got, want)
	}
}
