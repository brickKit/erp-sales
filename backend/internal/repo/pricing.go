// 定价：取 Odoo 的有序规则表模型 + metasfresh 的 price_limit 一等字段
// （设计计划 §3.2）。一条规则出全部结果（单价+折扣+底价），不做规则
// 叠加——"为什么是这个价"永远只有一个答案。
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	besdk "github.com/brickKit/be-sdk-go"
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
// desc, id desc) 排序，第一条命中的赢（设计计划 §3.2）。数量必须达到
// min_quantity 才命中；生效日期区间用 CURRENT_DATE 过滤。
func findPriceTx(ctx context.Context, tx *sql.Tx, in PriceInput) (*PriceResult, error) {
	qty, err := strconv.ParseFloat(in.Qty, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: qty 不是合法数字：%q", ErrInvalidArgument, in.Qty)
	}

	row := tx.QueryRowContext(ctx, `
		SELECT unit_price, discount, tax_rate
		FROM pricelist_items
		WHERE status = 'ACTIVE'
		  AND min_quantity <= $1
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
		qty, in.ProductID, in.CategoryID)

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

// computeSubtotal = qty * unit_price * (1 - discount)，服务端算好直接
// 给，不让调用方自己算（同 §3.2 的"前端严禁自己算钱"判据）。
func computeSubtotal(qty, unitPrice, discount string) (string, error) {
	q, err := strconv.ParseFloat(qty, 64)
	if err != nil {
		return "", fmt.Errorf("%w: qty 不是合法数字：%q", ErrInvalidArgument, qty)
	}
	p, err := strconv.ParseFloat(unitPrice, 64)
	if err != nil {
		return "", fmt.Errorf("解析 unit_price: %w", err)
	}
	d, err := strconv.ParseFloat(discount, 64)
	if err != nil {
		return "", fmt.Errorf("解析 discount: %w", err)
	}
	return strconv.FormatFloat(q*p*(1-d), 'f', 2, 64), nil
}

// CalculatePriceDryRun 是纯函数、不落库的试算接口——前端/BFF 严禁自己
// 算钱（设计计划 §3.2、§8.4）。
func (r *Repo) CalculatePriceDryRun(ctx context.Context, items []PriceInput) ([]*PriceResult, string, error) {
	var results []*PriceResult
	var total float64
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		for _, in := range items {
			res, err := findPriceTx(ctx, tx, in)
			if err != nil {
				return err
			}
			results = append(results, res)
			subtotal, err := strconv.ParseFloat(res.Subtotal, 64)
			if err != nil {
				return err
			}
			total += subtotal
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return results, strconv.FormatFloat(total, 'f', 2, 64), nil
}
