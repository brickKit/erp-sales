package tcc

import "testing"

// 信用额度预判按十进制比较：已用 0.10 + 本单 0.20 正好等于额度 0.30，
// 没有超（float64 会算成 0.30000000000000004 > 0.3，把这单误拒）。
func TestExceedsCreditLimit_十进制比较(t *testing.T) {
	cases := []struct {
		exposure, amount, limit string
		want                    bool
	}{
		{"0.10", "0.20", "0.30", false}, // 正好等于额度，不算超
		{"0.10", "0.21", "0.30", true},
		{"0", "100.00", "99.99", true},
		{"", "0.00", "0", false}, // 查不到快照（额度按 0）且本单 0 元
		{"", "0.01", "", true},   // 查不到快照：任何正金额都超
	}
	for _, c := range cases {
		if got := exceedsCreditLimit(c.exposure, c.amount, c.limit); got != c.want {
			t.Fatalf("exceedsCreditLimit(%q, %q, %q) 期望 %v，实际 %v", c.exposure, c.amount, c.limit, c.want, got)
		}
	}
}
