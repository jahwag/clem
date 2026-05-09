package cmd

import (
	"strings"
	"testing"
	"time"
)

// tokenExpiryDisplay encodes the user-visible truth that Claude Max access
// tokens are short-lived but auto-refreshed; without these branches the
// pre-0.8.4 cosmetic "EXPIRES 0d" alarm comes back.
func TestTokenExpiryDisplay(t *testing.T) {
	cases := []struct {
		name       string
		expiry     time.Time
		hasRefresh bool
		wantSub    string
	}{
		{"missing-everything", time.Time{}, false, "missing"},
		{"refresh-only-no-expiry", time.Time{}, true, "auto-refresh"},
		{"expired-but-refresh-present", time.Now().Add(-2 * time.Hour), true, "auto-refresh"},
		{"expired-and-no-refresh", time.Now().Add(-72 * time.Hour), false, "EXPIRED"},
		{"valid-hours", time.Now().Add(3 * time.Hour), true, "h)"},
		{"valid-days", time.Now().Add(5 * 24 * time.Hour), true, "d)"},
	}
	for _, tc := range cases {
		got := tokenExpiryDisplay(tc.expiry, tc.hasRefresh)
		if !strings.Contains(got, tc.wantSub) {
			t.Errorf("%s: got %q, want substring %q", tc.name, got, tc.wantSub)
		}
	}
}
