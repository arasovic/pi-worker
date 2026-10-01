package pi

import "testing"

func TestNearestThinkingLevel(t *testing.T) {
	tests := []struct {
		name      string
		levels    []ThinkingLevel
		requested ThinkingLevel
		want      ThinkingLevel
		wantOK    bool
	}{
		{
			name:      "nearest below wins by vocabulary order not list order",
			levels:    []ThinkingLevel{ThinkingHigh, ThinkingLow, ThinkingXHigh},
			requested: ThinkingMax,
			want:      ThinkingXHigh,
			wantOK:    true,
		},
		{
			name:      "above when nothing is below",
			levels:    []ThinkingLevel{ThinkingHigh, ThinkingLow},
			requested: ThinkingMinimal,
			want:      ThinkingLow,
			wantOK:    true,
		},
		{
			name:      "lower beats higher",
			levels:    []ThinkingLevel{ThinkingHigh, ThinkingMax},
			requested: ThinkingXHigh,
			want:      ThinkingHigh,
			wantOK:    true,
		},
		{
			name:      "empty list is not found",
			levels:    nil,
			requested: ThinkingMedium,
			want:      "",
			wantOK:    false,
		},
		{
			name:      "non-vocabulary list is not found",
			levels:    []ThinkingLevel{"bogus"},
			requested: ThinkingMedium,
			want:      "",
			wantOK:    false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := nearestThinkingLevel(test.levels, test.requested)
			if got != test.want || ok != test.wantOK {
				t.Fatalf("nearestThinkingLevel(%v, %q) = (%q, %v), want (%q, %v)", test.levels, test.requested, got, ok, test.want, test.wantOK)
			}
		})
	}
}
