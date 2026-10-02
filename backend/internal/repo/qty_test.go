package repo

import (
	"context"
	"errors"
	"testing"
)

// 数量是外部输入（建单请求、试算请求、赢单事件的 items[].quantity），
// 一律按十进制字符串严格校验：
//   - strconv.ParseFloat 认得的 "NaN"、"Inf"、"Infinity"、"1e3"、"0x10" 都不是
//     十进制数字。NaN 落进 NUMERIC 后排在一切数字之上，qty > 0 这类 CHECK 与
//     比较全都放行，所以必须在进库之前拒绝；
//   - 数量必须大于 0（sales_order_items 的 CHECK qty > 0；在这里先拒绝，
//     否则建单时撞上 CHECK 变成 500，试算时 0 与负数还能算出一个价）；
//   - 不能超出 NUMERIC(18,6)：小数最多 6 位（多出的位数落库时被悄悄舍掉，
//     与按原值算出的小计对不上），整数最多 12 位（落库时溢出变成 500）。

var nanClassQty = []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "Infinity", "-Infinity", "1e3", "1E3", "0x10", "1_000"}

func TestComputeSubtotal_NaN类写法报参数错误(t *testing.T) {
	for _, qty := range nanClassQty {
		if _, err := computeSubtotal(qty, "1.00", "0"); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("qty=%q 不是十进制数字，应该是参数错误，实际 %v", qty, err)
		}
	}
}

func TestCalculatePriceDryRun_数量非法时是参数错误(t *testing.T) {
	db := testDB(t)
	r := New(db, "erp_sales_rw", "erp_sales")
	bad := append([]string{
		"0", "0.000000", "-1", "-0.5", "+1", // 不是正数，或带符号
		"0.0000001", "1.1234567", // 小数超过 6 位
		"1234567890123", // 整数超过 12 位
	}, nanClassQty...)
	for _, qty := range bad {
		// 走全局兜底价（迁移播种的 GLOBAL 规则，min_quantity = 0）
		_, _, err := r.CalculatePriceDryRun(context.Background(), []PriceInput{{ProductID: uniqueSuffix("P-qty"), Qty: qty}})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("qty=%q 应该是参数错误（HTTP 400），实际 %v", qty, err)
		}
	}
}

func TestCalculatePriceDryRun_合法数量照常定价(t *testing.T) {
	db := testDB(t)
	r := New(db, "erp_sales_rw", "erp_sales")
	for _, qty := range []string{"1", "0.000001", "2.5", "123456789012", "123456789012.123456"} {
		if _, _, err := r.CalculatePriceDryRun(context.Background(), []PriceInput{{ProductID: uniqueSuffix("P-qty"), Qty: qty}}); err != nil {
			t.Fatalf("qty=%q 是合法数量，不该报错：%v", qty, err)
		}
	}
}
