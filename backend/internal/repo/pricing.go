// 定价：取 Odoo 的有序规则表模型 + metasfresh 的 price_limit 一等字段。
// 一条规则出全部结果（单价+折扣+底价），不做规则
// 叠加——"为什么是这个价"永远只有一个答案。
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"regexp"

	besdk "github.com/brickKit/be-sdk-go"

	"github.com/brickKit/erp-sales/v2/backend/internal/decimal"
)

// PriceInput 是定价的入参——本组件不 join mdm-product，categoryID 由
// 调用方（拿到 mdm-product 的 BatchGet 结果后）传进来，留空则只匹配
// GLOBAL 规则。
type PriceInput struct {
	ProductID  string
	CategoryID string
	Qty        string
}

type PriceResult struct {
	ProductID string
	UnitPrice string
	Discount  string
	TaxRate   string
	Subtotal  string
}

// findPriceTx 按 (适用范围特异度 PRODUCT>CATEGORY>GLOBAL, min_quantity
// desc, id desc) 排序，第一条命中的赢。数量必须达到
// min_quantity 才命中；生效日期区间用 CURRENT_DATE 过滤。
func findPriceTx(ctx context.Context, tx *sql.Tx, in PriceInput) (*PriceResult, error) {
	if err := validateQty(in.Qty); err != nil {
		return nil, err
	}

	row := tx.QueryRowContext(ctx, `
		SELECT unit_price, discount, tax_rate
		FROM pricelist_items
		WHERE status = 'ACTIVE'
		  AND min_quantity <= $1::numeric
		  AND (date_start IS NULL OR date_start <= CURRENT_DATE)
		  AND (date_end   IS NULL OR date_end   >= CURRENT_DATE)
		  AND (
		        (applied_on = 'PRODUCT'  AND product_id  = $2 AND $2 <> '')
		     OR (applied_on = 'CATEGORY' AND category_id = $3 AND $3 <> '')
		     OR (applied_on = 'GLOBAL')
		      )
		ORDER BY
		  CASE applied_on WHEN 'PRODUCT' THEN 0 WHEN 'CATEGORY' THEN 1 ELSE 2 END,
		  min_quantity DESC,
		  id DESC
		LIMIT 1`,
		in.Qty, in.ProductID, in.CategoryID)

	var unitPrice, discount, taxRate string
	if err := row.Scan(&unitPrice, &discount, &taxRate); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: 没有任何价格表规则命中 product_id=%s", ErrNotFound, in.ProductID)
		}
		return nil, fmt.Errorf("查 pricelist_items: %w", err)
	}

	subtotal, err := computeSubtotal(in.Qty, unitPrice, discount)
	if err != nil {
		return nil, err
	}
	return &PriceResult{ProductID: in.ProductID, UnitPrice: unitPrice, Discount: discount, TaxRate: taxRate, Subtotal: subtotal}, nil
}

// qtyPattern 是数量允许的写法：不带符号的十进制数，整数最多 12 位、小数
// 最多 6 位，正好装进 sales_order_items.qty 的 NUMERIC(18,6)。超出的位数
// 落库时会被舍掉或溢出，与按原值算出的小计对不上。
var qtyPattern = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,6})?$`)

// validateQty 校验一个数量（建单、试算与赢单转单都经过这里）：只认
// qtyPattern 的写法，且必须大于 0。"NaN"、"Inf"、"1e3" 之类写法一律拒绝：
// NaN 落进 NUMERIC 后排在一切数字之上，CHECK (qty > 0) 也拦不住它。
func validateQty(qty string) error {
	if !qtyPattern.MatchString(qty) {
		return fmt.Errorf("%w: qty 必须是大于 0 的十进制数（整数最多 12 位、小数最多 6 位）：%q", ErrInvalidArgument, qty)
	}
	q, err := decimal.Parse(qty)
	if err != nil || q.Sign() <= 0 {
		return fmt.Errorf("%w: qty 必须大于 0：%q", ErrInvalidArgument, qty)
	}
	return nil
}

// computeSubtotal = qty * unit_price * (1 - discount)，十进制精确计算，
// 最后四舍五入到分。服务端算好直接给，不让调用方自己算钱。
func computeSubtotal(qty, unitPrice, discount string) (string, error) {
	q, err := decimal.Parse(qty)
	if err != nil {
		return "", fmt.Errorf("%w: qty 不是合法数字：%q", ErrInvalidArgument, qty)
	}
	p, err := decimal.Parse(unitPrice)
	if err != nil {
		return "", fmt.Errorf("解析 unit_price: %w", err)
	}
	d, err := decimal.Parse(discount)
	if err != nil {
		return "", fmt.Errorf("解析 discount: %w", err)
	}
	rest := new(big.Rat).Sub(big.NewRat(1, 1), d)
	return decimal.Format2(new(big.Rat).Mul(new(big.Rat).Mul(q, p), rest)), nil
}

// CalculatePriceDryRun 是纯函数、不落库的试算接口——前端 / BFF 不许自己
// 算钱，金额只由服务端算。
func (r *Repo) CalculatePriceDryRun(ctx context.Context, items []PriceInput) ([]*PriceResult, string, error) {
	var results []*PriceResult
	total := new(big.Rat)
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		for _, in := range items {
			res, err := findPriceTx(ctx, tx, in)
			if err != nil {
				return err
			}
			results = append(results, res)
			subtotal, err := decimal.Parse(res.Subtotal)
			if err != nil {
				return err
			}
			total.Add(total, subtotal)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return results, decimal.Format2(total), nil
}
