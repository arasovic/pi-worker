package piversion

import "testing"

func TestClassifyVersion(t *testing.T) {
	tests := []struct {
		name   string
		output string
		status Status
	}{
		{name: "verified", output: "0.84.2\n", status: StatusVerified},
		{name: "unverified release", output: "0.99.0", status: StatusUnverified},
		{name: "unverified prerelease", output: "1.2.3-alpha.1+build.5", status: StatusUnverified},
		{name: "empty", output: "", status: StatusInvalid},
		{name: "prefixed text", output: "pi 0.84.2", status: StatusInvalid},
		{name: "missing patch", output: "1.2", status: StatusInvalid},
		{name: "leading zero", output: "01.2.3", status: StatusInvalid},
		{name: "empty prerelease identifier", output: "1.2.3-alpha..1", status: StatusInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Classify(test.output)
			if got.Status != test.status {
				t.Fatalf("status = %q, want %q", got.Status, test.status)
			}
		})
	}
}

func TestClassifyTrimsOnlyOuterWhitespace(t *testing.T) {
	got := Classify("\t0.84.2 \n")
	if got.Status != StatusVerified || got.Version != "0.84.2" {
		t.Fatalf("classification = %#v, want verified 0.84.2", got)
	}

	if got := Classify("0.84.2\nextra"); got.Status != StatusInvalid {
		t.Fatalf("multi-line output status = %q, want invalid", got.Status)
	}
}
