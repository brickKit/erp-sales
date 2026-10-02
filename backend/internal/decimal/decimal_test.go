package decimal

import (
	"math/big"
	"testing"
)

func TestFormat2_四舍五入远离零(t *testing.T) {
	cases := []struct {
		in   *big.Rat
		want string
	}{
		{big.NewRat(285, 1000), "0.29"},
		{big.NewRat(284, 1000), "0.28"},
		{big.NewRat(-285, 1000), "-0.29"},
		{big.NewRat(-4, 1000), "0.00"},
		{big.NewRat(5, 1), "5.00"},
		{big.NewRat(1, 3), "0.33"},
		{big.NewRat(123456789, 100), "1234567.89"},
	}
	for _, c := range cases {
		if got := Format2(c.in); got != c.want {
			t.Fatalf("Format2(%s) 期望 %s，实际 %s", c.in.RatString(), c.want, got)
		}
	}
}

func TestParse_只认普通十进制写法(t *testing.T) {
	for _, ok := range []string{"0", "1.5", "-2.25", "+3", "100.000001"} {
		if _, err := Parse(ok); err != nil {
			t.Fatalf("%q 应能解析：%v", ok, err)
		}
	}
	for _, bad := range []string{"", "1/3", "1e3", ".5", "1.", "abc", " 1"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("%q 不该被接受", bad)
		}
	}
}
