package runlog

import (
	"testing"
	"time"
)

// TestParseRunIDValid covers well-formed IDs and RunID round-trips.
func TestParseRunIDValid(t *testing.T) {
	tests := []struct {
		name  string
		runID string
		want  time.Time
	}{
		{"basic", "20260830T041530Z-12345", time.Date(2026, 8, 30, 4, 15, 30, 0, time.UTC)},
		{"epoch", "19700101T000000Z-1", time.Unix(0, 0).UTC()},
		{"far future", "20991231T235959Z-9999999", time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC)},
		{"leap year Feb 29", "20240229T120000Z-1", time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)},
		{"century leap Feb 29", "20000229T120000Z-1", time.Date(2000, 2, 29, 12, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRunID(tt.runID)
			if err != nil {
				t.Fatalf("ParseRunID(%q) = %v, want nil", tt.runID, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("parsed = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("round-trip", func(t *testing.T) {
		cases := []time.Time{
			time.Date(2026, 8, 30, 4, 15, 30, 0, time.UTC),
			time.Unix(0, 0).UTC(),
			time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC),
			time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
			time.Date(2000, 2, 29, 12, 0, 0, 0, time.UTC),
		}
		for _, tc := range cases {
			parsed, err := ParseRunID(RunID(tc))
			if err != nil {
				t.Fatalf("ParseRunID(RunID(%v)) = %v, want nil", tc, err)
			}
			if !parsed.Equal(tc) {
				t.Errorf("round-trip: parsed = %v, want %v", parsed, tc)
			}
		}
	})
}

// TestParseRunIDMalformed covers structural defects.
func TestParseRunIDMalformed(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"too short", "20260830T04153"},
		{"prefix 15", "20260830T041530"}, // missing Z
		{"no hyphen", "20260830T041530Z12345"},
		{"double hyphen", "20260830T041530Z--12345"},
		{"dot separator", "20260830T041530.Z-12345"},
		{"empty suffix", "20260830T041530Z-"},
		{"slash", "2026/08/30T041530Z-12345"},
		{"backslash", "2026\\08\\30T041530Z-12345"},
		{"traversal", "..hidden-12345"},
		{"space", "20260830T041530Z- 123"},
		{"plus sign", "20260830T041530Z+12345"},
		{"dot in pid", "20260830T041530Z-12.34"},
		{"non-digit suffix", "20260830T041530Z-abc"},
		{"trailing garbage", "20260830T041530Z-12345abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRunID(tt.input)
			if err == nil {
				t.Fatalf("ParseRunID(%q) = nil, want error", tt.input)
			}
		})
	}
}

// TestParseRunIDPID rejects zero, negative, leading-zero, and overflow PIDs.
func TestParseRunIDPID(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"zero", "20260830T041530Z-0"},
		{"leading zero", "20260830T041530Z-01234"},
		{"overflow", "20260830T041530Z-9999999999999999999999999999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRunID(tt.input)
			if err == nil {
				t.Fatalf("ParseRunID(%q) = nil, want error", tt.input)
			}
		})
	}
}

// TestParseRunIDImpossibleTimestamp rejects unparseable calendar values.
func TestParseRunIDImpossibleTimestamps(t *testing.T) {
	tests := []struct {
		name string
		ts   string
	}{
		{"month 00", "00000000T000000Z"},
		{"month 13", "00001300T000000Z"},
		{"day 00", "00000100T000000Z"},
		{"day 32", "00000132T000000Z"},
		{"hour 24", "00000101T240000Z"},
		{"minute 60", "00000101T006000Z"},
		{"second 60", "00000101T000060Z"},
		{"feb 30", "00000230T000000Z"},
		{"apr 31", "00000431T000000Z"},
		{"sep 31", "00000931T000000Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRunID(tt.ts + "-1")
			if err == nil {
				t.Fatalf("ParseRunID(%q) = nil, want error", tt.ts)
			}
		})
	}
}

// TestParseRunIDLeapYear covers leap-year edge cases.
func TestParseRunIDLeapYear(t *testing.T) {
	ok := []string{"20240229T120000Z-1", "20000229T120000Z-1"}
	fail := []string{"19000229T120000Z-1", "20230229T120000Z-1"}

	for _, id := range ok {
		if _, err := ParseRunID(id); err != nil {
			t.Errorf("ParseRunID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range fail {
		if _, err := ParseRunID(id); err == nil {
			t.Errorf("ParseRunID(%q) = nil, want error", id)
		}
	}
}

// TestValidateRunIDMismatch exercises ValidateRunID at boundary conditions.
func TestValidateRunIDMismatch(t *testing.T) {
	base := time.Date(2026, 8, 30, 4, 15, 30, 0, time.UTC)
	id := base.Format("20060102T150405Z") + "-1"

	tests := []struct {
		name      string
		startedAt time.Time
		wantErr   bool
	}{
		{"exact match", base, false},
		{"off by one second", base.Add(time.Second), true},
		{"off by one minute", base.Add(time.Minute), true},
		{"subsecond above (truncated)", base.Add(999 * time.Millisecond), false},
		{"subsecond below (truncates prev)", base.Add(-999 * time.Microsecond), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRunID(id, tt.startedAt)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	// Structural error propagates.
	if err := ValidateRunID("bad-id", base); err == nil {
		t.Error("ValidateRunID(\"bad-id\", ...) = nil, want error")
	}
}
