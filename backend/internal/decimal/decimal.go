// Package decimal 是本组件金额与数量的十进制运算：输入输出都是十进制字符串
// （契约里金额一律是字符串），中间用 math/big.Rat 精确计算，绝不经过
// 二进制浮点数。舍入只在最后一步做一次：四舍五入（远离零）到分，与
// PostgreSQL NUMERIC(18,2) 落库时的舍入一致。
package decimal

import (
	"fmt"
	"math/big"
	"regexp"
)

// 只接受普通十进制写法（可带符号和小数部分）；不接受 big.Rat 也认的
// "1/3"、"1e3" 之类写法。
var plain = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?$`)

// Parse 解析一个十进制字符串。
func Parse(s string) (*big.Rat, error) {
	if !plain.MatchString(s) {
		return nil, fmt.Errorf("不是十进制数字：%q", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("不是十进制数字：%q", s)
	}
	return r, nil
}

// ParseOrZero 同 Parse，解析不了时当作 0（用于读本地快照这类"缺失即 0"的值）。
func ParseOrZero(s string) *big.Rat {
	r, err := Parse(s)
	if err != nil {
		return new(big.Rat)
	}
	return r
}

// Format2 把 x 四舍五入（远离零）到两位小数。
func Format2(x *big.Rat) string {
	scaled := new(big.Rat).Mul(x, big.NewRat(100, 1))
	num := new(big.Int).Abs(scaled.Num())
	den := scaled.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Lsh(m, 1).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	s := q.String()
	for len(s) < 3 {
		s = "0" + s
	}
	out := s[:len(s)-2] + "." + s[len(s)-2:]
	if scaled.Sign() < 0 && q.Sign() != 0 {
		out = "-" + out
	}
	return out
}
