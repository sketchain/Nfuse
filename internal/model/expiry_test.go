package model

import (
	"testing"
	"time"
)

func TestParseExpiry(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "2026-12-31", want: time.Date(2026, 12, 31, 0, 0, 0, 0, loc).Unix()},
		{in: " 2026-12-31 18:30 ", want: time.Date(2026, 12, 31, 18, 30, 0, 0, loc).Unix()},
		{in: "2026-12-31T18:30", want: time.Date(2026, 12, 31, 18, 30, 0, 0, loc).Unix()},
		{in: "2026-12-31 18:30:15", want: time.Date(2026, 12, 31, 18, 30, 15, 0, loc).Unix()},
		{in: "", want: 0},
		{in: "never", want: 0},
		{in: "NONE", want: 0},
		{in: "0", want: 0},
		{in: "2026/12/31", wantErr: true},
		{in: "2026-02-30", wantErr: true},
		{in: "tomorrow", wantErr: true},
		{in: "1969-12-31", wantErr: true}, // not after the epoch
	}
	for _, tc := range tests {
		got, err := ParseExpiry(tc.in, loc)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseExpiry(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("ParseExpiry(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFormatExpiryRoundTrips(t *testing.T) {
	loc := time.FixedZone("UTC-5", -5*3600)
	if got := FormatExpiry(0, loc); got != "never" {
		t.Errorf("FormatExpiry(0) = %q, want never", got)
	}
	for _, s := range []string{"2026-12-31", "2027-01-15 08:30", "2027-01-15 08:30:05"} {
		u, err := ParseExpiry(s, loc)
		if err != nil {
			t.Fatalf("ParseExpiry(%q): %v", s, err)
		}
		if got := FormatExpiry(u, loc); got != s {
			t.Errorf("FormatExpiry(ParseExpiry(%q)) = %q", s, got)
		}
	}
}

func TestExpiredAt(t *testing.T) {
	deadline := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	before, at := deadline.Add(-time.Second), deadline

	for _, tier := range []Tier{TierMonthly, TierOneShot} {
		a := Account{Tier: tier, LimitGiB: 1, ExpiresUnix: deadline.Unix()}
		if a.ExpiredAt(before) {
			t.Errorf("tier %s: expired one second before the deadline", tier)
		}
		if !a.ExpiredAt(at) {
			t.Errorf("tier %s: not expired at the deadline", tier)
		}
		if a.ExpiredAt(time.Time{}) {
			t.Errorf("tier %s: a zero reference time must never count as expired", tier)
		}
	}
	// Tier c keeps a stored expiry dormant; 0 means never.
	if (Account{Tier: TierUnlimited, ExpiresUnix: deadline.Unix()}).ExpiredAt(at) {
		t.Error("an unlimited account must never expire")
	}
	if (Account{Tier: TierMonthly, LimitGiB: 1}).ExpiredAt(at) {
		t.Error("ExpiresUnix 0 must mean never")
	}
}
