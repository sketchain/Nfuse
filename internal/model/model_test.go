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
				Ports:    []Port{{ID: 1, AccountID: 1, Port: 80}},
			},
		},
		{
			name: "port to unknown account",
			snap: Snapshot{
				Ports: []Port{{ID: 1, AccountID: 99, Port: 80}},
			},
			wantErr: true,
		},
		{
			name: "overlapping port across accounts",
			snap: Snapshot{
				Accounts: []Account{{ID: 1, Name: "a", Tier: TierUnlimited}, {ID: 2, Name: "b", Tier: TierUnlimited}},
				Ports:    []Port{{ID: 1, AccountID: 1, Port: 80}, {ID: 2, AccountID: 2, Port: 80}},
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
