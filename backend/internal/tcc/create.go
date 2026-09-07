package tcc

import (
	"context"
	"fmt"

	"github.com/brickKit/erp-sales/backend/internal/repo"
)

// CreateOrderItemInput 是 CreateOrder 的入参一项——只有 product_id/qty，
// 其余快照字段（sku/name/uom/单价/折扣/税率/小计）都在这里解析出来
// （设计计划 §2.2：全部快照，不 join）。
type CreateOrderItemInput struct {
	ProductID string
	Qty       string
}

// CreateOrder 建 DRAFT，不预留库存（设计计划 §3 契约面）。没有需要补偿
// 的外部写，所以不是真正的 TCC，但同样要先校验客户/产品、再落库——
// 跨组件读调用与本地写分离的骨架和 ConfirmOrder 一致。
func (o *Orchestrator) CreateOrder(ctx context.Context, idempotencyKey, customerID string, items []CreateOrderItemInput) (*repo.Order, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: items 不能为空", repo.ErrInvalidArgument)
	}

	customer, err := fetchActiveCustomer(ctx, customerID)
	if err != nil {
		return nil, err
	}

	productIDs := make([]string, 0, len(items))
	for _, it := range items {
		productIDs = append(productIDs, it.ProductID)
	}
	products, err := fetchActiveProducts(ctx, uniqueStrings(productIDs))
	if err != nil {
		return nil, err
	}

	priceInputs := make([]repo.PriceInput, 0, len(items))
	for _, it := range items {
		p := products[it.ProductID]
		priceInputs = append(priceInputs, repo.PriceInput{ProductID: it.ProductID, CategoryID: p.CategoryId, Qty: it.Qty})
	}
	priceResults, _, err := o.Repo.CalculatePriceDryRun(ctx, priceInputs)
	if err != nil {
		return nil, fmt.Errorf("定价失败: %w", err)
	}
	if len(priceResults) != len(items) {
		return nil, fmt.Errorf("定价结果数量（%d）与订单行数量（%d）不一致", len(priceResults), len(items))
	}

	createItems := make([]repo.CreateOrderItemInput, 0, len(items))
	for i, it := range items {
		p := products[it.ProductID]
		pr := priceResults[i]
		createItems = append(createItems, repo.CreateOrderItemInput{
			ProductID: it.ProductID, ProductSKU: p.Sku, ProductName: p.Name, UOMID: p.BaseUomId,
			Qty: it.Qty, UnitPrice: pr.UnitPrice, Discount: pr.Discount, TaxRate: pr.TaxRate, Subtotal: pr.Subtotal,
		})
	}

	return o.Repo.CreateOrder(ctx, repo.CreateOrderInput{
		IdempotencyKey: idempotencyKey, CustomerID: customerID, CustomerName: customer.Name, Items: createItems,
		// dept_id/dept_path/owner_id：阶段二占位（同 besdk.ScopeOf），见
		// repo.CreateOrderInput 字段注释。
	})
}
