package repo

import (
	"context"
	"database/sql"
	"testing"

	besdk "github.com/brickKit/be-sdk-go"
)

// 金额按十进制计算、四舍五入（远离零）到分，与 PostgreSQL NUMERIC(18,2)
// 落库时的舍入一致；不能先转成二进制浮点数再格式化。
func TestComputeSubtotal_十进制四舍五入到分(t *testing.T) {
	cases := []struct{ qty, price, discount, want string }{
		{"1", "0.30", "0.05", "0.29"},  // 0.285 → 0.29（float64 算成 0.28）
		{"1", "0.15", "0.3", "0.11"},   // 0.105 → 0.11（float64 算成 0.10）
		{"3", "10.00", "0", "30.00"},   // 没有折扣
		{"2.5", "4.00", "0.1", "9.00"}, // 小数数量
	}
	for _, c := range cases {
		got, err := computeSubtotal(c.qty, c.price, c.discount)
		if err != nil {
			t.Fatalf("computeSubtotal(%s, %s, %s) 报错：%v", c.qty, c.price, c.discount, err)
		}
		if got != c.want {
			t.Fatalf("computeSubtotal(%s, %s, %s) 期望 %s，实际 %s", c.qty, c.price, c.discount, c.want, got)
		}
	}
}

func TestComputeSubtotal_非法数字报参数错误(t *testing.T) {
	for _, qty := range []string{"", "abc", "1/3", "1e3"} {
		if _, err := computeSubtotal(qty, "1.00", "0"); err == nil {
			t.Fatalf("qty=%q 不是十进制数字，应该报错", qty)
		}
	}
}

// 走真实价格表：一条产品级规则命中后，每行小计与合计都按十进制算到分。
func TestCalculatePriceDryRun_产品级规则按十进制算小计与合计(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")
	productID := uniqueSuffix("P-price")
	err := besdk.WithTx(ctx, db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO pricelist_items (pricelist_id, applied_on, product_id, min_quantity, unit_price, discount, tax_rate)
			SELECT id, 'PRODUCT', $1, 0, 0.30, 0.05, 0.13 FROM pricelists ORDER BY id LIMIT 1`, productID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	results, total, err := r.CalculatePriceDryRun(ctx, []PriceInput{
		{ProductID: productID, Qty: "1"}, // 0.285 → 0.29
		{ProductID: productID, Qty: "3"}, // 0.855 → 0.86
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Subtotal != "0.29" || results[1].Subtotal != "0.86" {
		t.Fatalf("期望小计 0.29 / 0.86，实际 %s / %s", results[0].Subtotal, results[1].Subtotal)
	}
	if total != "1.15" {
		t.Fatalf("期望合计 1.15，实际 %s", total)
	}
}
