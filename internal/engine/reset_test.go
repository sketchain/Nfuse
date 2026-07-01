package engine

import (
	"testing"
	"time"
)

func TestNextReset(t *testing.T) {
	loc := time.UTC
	tests := []struct {
		name   string
		last   time.Time
		anchor int
		want   time.Time
	}{
		{
			name:   "same month, anchor ahead",
			last:   time.Date(2026, 3, 5, 10, 0, 0, 0, loc),
			anchor: 15,
			want:   time.Date(2026, 3, 15, 0, 0, 0, 0, loc),
		},
		{
			name:   "anchor already passed rolls to next month",
			last:   time.Date(2026, 3, 20, 10, 0, 0, 0, loc),
			anchor: 15,
			want:   time.Date(2026, 4, 15, 0, 0, 0, 0, loc),
		},
		{
			name:   "anchor equals last day rolls forward",
			last:   time.Date(2026, 3, 15, 0, 0, 0, 0, loc),
			anchor: 15,
			want:   time.Date(2026, 4, 15, 0, 0, 0, 0, loc),
		},
		{
			name:   "december rolls to january",
			last:   time.Date(2026, 12, 20, 0, 0, 0, 0, loc),
			anchor: 1,
			want:   time.Date(2027, 1, 1, 0, 0, 0, 0, loc),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextReset(tc.last, tc.anchor); !got.Equal(tc.want) {
				t.Errorf("nextReset(%v, %d) = %v, want %v", tc.last, tc.anchor, got, tc.want)
			}
		})
	}
}
