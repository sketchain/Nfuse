package model

import "testing"

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		snap    Snapshot
		wantErr bool
	}{
		{
			name: "ok",
			snap: Snapshot{
				Accounts: []Account{{ID: 1, Name: "a", Tier: TierMonthly, LimitGiB: 1}},
				Ports:    []Port{{ID: 1, AccountID: 1, Start: 80, End: 80}},
			},
		},
		{
			name: "port to unknown account",
			snap: Snapshot{
				Ports: []Port{{ID: 1, AccountID: 99, Start: 80, End: 80}},
			},
			wantErr: true,
		},
		{
			name: "overlapping port across accounts",
			snap: Snapshot{
				Accounts: []Account{{ID: 1, Name: "a", Tier: TierUnlimited}, {ID: 2, Name: "b", Tier: TierUnlimited}},
				Ports:    []Port{{ID: 1, AccountID: 1, Start: 80, End: 80}, {ID: 2, AccountID: 2, Start: 80, End: 80}},
			},
			wantErr: true,
		},
		{
			name: "adjacent ranges are legal",
			snap: Snapshot{
				Accounts: []Account{{ID: 1, Name: "a", Tier: TierUnlimited}, {ID: 2, Name: "b", Tier: TierUnlimited}},
				Ports: []Port{
					{ID: 1, AccountID: 1, Start: 60000, End: 60099},
					{ID: 2, AccountID: 2, Start: 60100, End: 60199},
				},
			},
		},
		{
			name: "overlapping ranges rejected",
			snap: Snapshot{
				Accounts: []Account{{ID: 1, Name: "a", Tier: TierUnlimited}, {ID: 2, Name: "b", Tier: TierUnlimited}},
				Ports: []Port{
					{ID: 1, AccountID: 1, Start: 60000, End: 60099},
					{ID: 2, AccountID: 2, Start: 60099, End: 60199},
				},
			},
			wantErr: true,
		},
		{
			name: "range contained in single-port overlap rejected",
			snap: Snapshot{
				Accounts: []Account{{ID: 1, Name: "a", Tier: TierUnlimited}, {ID: 2, Name: "b", Tier: TierUnlimited}},
				Ports: []Port{
					{ID: 1, AccountID: 1, Start: 60050, End: 60050},
					{ID: 2, AccountID: 2, Start: 60000, End: 60099},
				},
			},
			wantErr: true,
		},
		{
			name: "reversed bounds rejected",
			snap: Snapshot{
				Accounts: []Account{{ID: 1, Name: "a", Tier: TierUnlimited}},
				Ports:    []Port{{ID: 1, AccountID: 1, Start: 200, End: 100}},
			},
			wantErr: true,
		},
		{
			name: "zero start rejected",
			snap: Snapshot{
				Accounts: []Account{{ID: 1, Name: "a", Tier: TierUnlimited}},
				Ports:    []Port{{ID: 1, AccountID: 1, Start: 0, End: 0}},
			},
			wantErr: true,
		},
		{
			name: "quota tier without limit",
			snap: Snapshot{
				Accounts: []Account{{ID: 1, Name: "a", Tier: TierOneShot, LimitGiB: 0}},
			},
			wantErr: true,
		},
		{
			name: "unlimited tier needs no limit",
			snap: Snapshot{
				Accounts: []Account{{ID: 1, Name: "a", Tier: TierUnlimited}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.snap.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestPortStringAndOverlap(t *testing.T) {
	if got := (Port{Start: 60006, End: 60006}).String(); got != "60006" {
		t.Errorf("single port String() = %q, want 60006", got)
	}
	if got := (Port{Start: 60000, End: 60099}).String(); got != "60000-60099" {
		t.Errorf("range String() = %q, want 60000-60099", got)
	}
	cases := []struct {
		a, b Port
		want bool
	}{
		{Port{Start: 60000, End: 60099}, Port{Start: 60099, End: 60199}, true},  // touch at 60099
		{Port{Start: 60000, End: 60099}, Port{Start: 60100, End: 60199}, false}, // adjacent
		{Port{Start: 60050, End: 60050}, Port{Start: 60000, End: 60099}, true},  // single inside range
		{Port{Start: 8080, End: 8080}, Port{Start: 9090, End: 9090}, false},     // distinct singles
	}
	for _, c := range cases {
		if got := c.a.Overlaps(c.b); got != c.want {
			t.Errorf("%s overlaps %s = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := c.b.Overlaps(c.a); got != c.want {
			t.Errorf("overlap not symmetric for %s / %s", c.a, c.b)
		}
	}
}

func TestTierSemantics(t *testing.T) {
	if !TierMonthly.HasQuota() || !TierMonthly.Resets() {
		t.Error("monthly should have quota and reset")
	}
	if !TierOneShot.HasQuota() || TierOneShot.Resets() {
		t.Error("one-shot should have quota but never reset")
	}
	if TierUnlimited.HasQuota() || TierUnlimited.Resets() {
		t.Error("unlimited should have no quota and never reset")
	}
}

func TestLimitBytes(t *testing.T) {
	a := Account{Tier: TierMonthly, LimitGiB: 2}
	if got := a.LimitBytes(); got != 2*(1<<30) {
		t.Errorf("LimitBytes = %d, want %d", got, 2*(1<<30))
	}
	u := Account{Tier: TierUnlimited, LimitGiB: 5}
	if got := u.LimitBytes(); got != 0 {
		t.Errorf("unlimited LimitBytes = %d, want 0", got)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[uint64]string{
		512:           "512 B",
		1024:          "1.00 KiB",
		1024 * 1024:   "1.00 MiB",
		3 * (1 << 30): "3.00 GiB",
	}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
